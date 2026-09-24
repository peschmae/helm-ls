package plato

import (
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"sync"

	"go.lsp.dev/jsonrpc2"
	lsp "go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"go.uber.org/zap"
)

type document struct {
	text    string
	version int32
}

type server struct {
	lsp.Server
	mu       sync.Mutex
	exitOnce sync.Once
	client   lsp.Client
	exited   chan struct{}
	folders  []string
	settings settings
	epoch    uint64
	docs     map[lsp.DocumentURI]document
	host     map[lsp.DocumentURI]hostDiagnostics
	backends map[string]*backend
	routes   map[lsp.DocumentURI]route
	syncMu   sync.Mutex
}

func Serve(stream io.ReadWriteCloser) {
	s := &server{
		docs: make(map[lsp.DocumentURI]document), host: make(map[lsp.DocumentURI]hostDiagnostics),
		backends: make(map[string]*backend), routes: make(map[lsp.DocumentURI]route), exited: make(chan struct{}),
	}
	logger := zap.NewNop()
	defer logger.Sync()
	s.mu.Lock()
	conn := jsonrpc2.NewConn(compatibleStream{jsonrpc2.NewStream(stream)})
	client := lsp.ClientDispatcher(conn, logger)
	s.client = client
	s.mu.Unlock()
	ctx := lsp.WithClient(context.Background(), client)
	dispatch := lsp.ServerHandler(s, jsonrpc2.MethodNotFoundHandler)
	conn.Go(ctx, lsp.Handlers(func(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
		switch req.Method() {
		case "initialize", "initialized", "shutdown", "exit",
			"textDocument/didOpen", "textDocument/didChange", "textDocument/didSave", "textDocument/didClose",
			"workspace/didChangeConfiguration", "workspace/didChangeWorkspaceFolders":
			return dispatch(ctx, reply, req)
		default:
			return jsonrpc2.MethodNotFoundHandler(ctx, reply, req)
		}
	}))
	select {
	case <-conn.Done():
	case <-s.exited:
	}
	s.closeBackends()
}

// protocol v0.12 rejects exit with params:{}, which some LSP clients send.
type compatibleStream struct{ jsonrpc2.Stream }

func (s compatibleStream) Read(ctx context.Context) (jsonrpc2.Message, int64, error) {
	message, n, err := s.Stream.Read(ctx)
	if notification, ok := message.(*jsonrpc2.Notification); ok &&
		notification.Method() == "exit" && strings.TrimSpace(string(notification.Params())) == "{}" {
		var withoutParams jsonrpc2.Notification
		if json.Unmarshal([]byte(`{"jsonrpc":"2.0","method":"exit"}`), &withoutParams) == nil {
			return &withoutParams, n, err
		}
	}
	return message, n, err
}

func (s *server) Initialize(_ context.Context, params *lsp.InitializeParams) (*lsp.InitializeResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	config, err := parseSettings(params.InitializationOptions)
	if err != nil {
		return nil, err
	}
	s.settings = config
	for _, folder := range params.WorkspaceFolders {
		if u, err := uri.Parse(folder.URI); err == nil && pathForURI(u) != "" {
			s.folders = append(s.folders, u.Filename())
		}
	}
	if len(s.folders) == 0 && pathForURI(params.RootURI) != "" {
		s.folders = append(s.folders, params.RootURI.Filename())
	}
	return &lsp.InitializeResult{Capabilities: lsp.ServerCapabilities{
		TextDocumentSync: lsp.TextDocumentSyncOptions{OpenClose: true, Change: lsp.TextDocumentSyncKindFull, Save: &lsp.SaveOptions{}},
		Workspace: &lsp.ServerCapabilitiesWorkspace{WorkspaceFolders: &lsp.ServerCapabilitiesWorkspaceFolders{
			Supported: true, ChangeNotifications: true,
		}},
	}}, nil
}

func (s *server) Initialized(ctx context.Context, _ *lsp.InitializedParams) error {
	s.mu.Lock()
	client := s.client
	configured := s.settings.YAMLBackend != "" || s.settings.BashBackend != ""
	s.mu.Unlock()
	message := "Plato template syntax diagnostics only; configure YAML/Bash backend paths to enable best-effort host validation"
	if configured {
		message = "Plato template diagnostics are active; configured YAML/Bash host validation is best-effort and never rendered output"
	}
	return client.ShowMessage(ctx, &lsp.ShowMessageParams{
		Type: lsp.MessageTypeInfo, Message: message,
	})
}
func (s *server) Shutdown(context.Context) error { return nil }
func (s *server) Exit(context.Context) error {
	s.mu.Lock()
	exited := s.exited
	s.mu.Unlock()
	if exited != nil {
		s.exitOnce.Do(func() { close(exited) })
	}
	return nil
}

func (s *server) DidOpen(ctx context.Context, params *lsp.DidOpenTextDocumentParams) error {
	s.mu.Lock()
	u := params.TextDocument.URI
	s.docs[u] = document{text: params.TextDocument.Text, version: params.TextDocument.Version}
	delete(s.host, u)
	err := s.publish(ctx, u)
	s.mu.Unlock()
	go s.syncHost(u)
	return err
}

func (s *server) DidChange(ctx context.Context, params *lsp.DidChangeTextDocumentParams) error {
	s.mu.Lock()
	u := params.TextDocument.URI
	doc, ok := s.docs[u]
	if !ok || params.TextDocument.Version <= doc.version {
		s.mu.Unlock()
		return nil
	}
	for _, change := range params.ContentChanges {
		doc.text = change.Text
	}
	doc.version = params.TextDocument.Version
	s.docs[u] = doc
	delete(s.host, u)
	err := s.publish(ctx, u)
	s.mu.Unlock()
	go s.syncHost(u)
	return err
}

func (s *server) DidSave(ctx context.Context, params *lsp.DidSaveTextDocumentParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.docs[params.TextDocument.URI]; ok {
		return s.publish(ctx, params.TextDocument.URI)
	}
	return nil
}

func (s *server) DidClose(ctx context.Context, params *lsp.DidCloseTextDocumentParams) error {
	s.mu.Lock()
	delete(s.docs, params.TextDocument.URI)
	delete(s.host, params.TextDocument.URI)
	err := s.client.PublishDiagnostics(ctx, &lsp.PublishDiagnosticsParams{
		URI: params.TextDocument.URI, Diagnostics: []lsp.Diagnostic{},
	})
	s.mu.Unlock()
	go s.closeHost(params.TextDocument.URI)
	return err
}

func (s *server) DidChangeConfiguration(ctx context.Context, params *lsp.DidChangeConfigurationParams) error {
	config, err := parseSettings(params.Settings)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.settings = config
	s.epoch++
	s.host = make(map[lsp.DocumentURI]hostDiagnostics)
	err = s.republish(ctx)
	s.mu.Unlock()
	go s.reconfigureBackends()
	return err
}

func (s *server) DidChangeWorkspaceFolders(ctx context.Context, params *lsp.DidChangeWorkspaceFoldersParams) error {
	s.mu.Lock()
	for _, removed := range params.Event.Removed {
		if u, err := uri.Parse(removed.URI); err == nil {
			for i, folder := range s.folders {
				if folder == pathForURI(u) {
					s.folders = append(s.folders[:i], s.folders[i+1:]...)
					break
				}
			}
		}
	}
	for _, added := range params.Event.Added {
		if u, err := uri.Parse(added.URI); err == nil && pathForURI(u) != "" {
			s.folders = append(s.folders, u.Filename())
		}
	}
	s.host = make(map[lsp.DocumentURI]hostDiagnostics)
	s.epoch++
	err := s.republish(ctx)
	s.mu.Unlock()
	go s.reconfigureBackends()
	return err
}

func (s *server) republish(ctx context.Context) error {
	for u := range s.docs {
		if err := s.publish(ctx, u); err != nil {
			return err
		}
	}
	return nil
}

func (s *server) publish(ctx context.Context, u lsp.DocumentURI) error {
	doc := s.docs[u]
	diagnostics := []lsp.Diagnostic{}
	if filename := pathForURI(u); filename != "" {
		w := s.settings.forFile(filename, s.folders)
		if w.eligible(filename, doc.text) {
			diagnostics = append(diagnostics, syntaxDiagnostics(doc.text, w)...)
			if kind, _ := hostForFile(filename, doc.text); kind == "bash" && s.settings.BashBackend != "" {
				shellcheck := s.settings.ShellCheckPath
				if shellcheck == "" {
					shellcheck = "shellcheck"
				}
				if _, err := exec.LookPath(shellcheck); err != nil {
					warning := diagnostic(doc.text, 0, 0, "ShellCheck unavailable: configure shellcheckPath or install ShellCheck; Bash validation is incomplete")
					warning.Severity = lsp.DiagnosticSeverityWarning
					diagnostics = append(diagnostics, warning)
				}
			}
		}
	}
	if cached, ok := s.host[u]; ok && cached.version == doc.version {
		diagnostics = append(diagnostics, cached.diagnostics...)
	}
	return s.client.PublishDiagnostics(ctx, &lsp.PublishDiagnosticsParams{
		URI: u, Version: uint32(doc.version), Diagnostics: diagnostics,
	})
}
