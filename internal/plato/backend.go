package plato

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.lsp.dev/jsonrpc2"
	lsp "go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"go.uber.org/zap"
)

type hostDiagnostics struct {
	version     int32
	diagnostics []lsp.Diagnostic
}

type route struct {
	source  lsp.DocumentURI
	version int32
	epoch   uint64
	kind    string
	key     string
	project projection
}

type backend struct {
	command  *exec.Cmd
	conn     jsonrpc2.Conn
	server   lsp.Server
	opened   map[lsp.DocumentURI]int32
	closed   atomic.Bool
	stopOnce sync.Once
}

type backendStream struct {
	stdout io.ReadCloser
	stdin  io.WriteCloser
}

func (stream backendStream) Read(p []byte) (int, error)  { return stream.stdout.Read(p) }
func (stream backendStream) Write(p []byte) (int, error) { return stream.stdin.Write(p) }
func (stream backendStream) Close() error {
	_ = stream.stdin.Close()
	return stream.stdout.Close()
}

type backendClient struct {
	lsp.Client
	owner    *server
	kind     string
	settings settings
}

func (client backendClient) PublishDiagnostics(ctx context.Context, params *lsp.PublishDiagnosticsParams) error {
	s := client.owner
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.routes[params.URI]
	if !ok || r.epoch != s.epoch || r.kind != client.kind ||
		params.Version != 0 && uint32(r.version) != params.Version {
		return nil
	}
	doc, ok := s.docs[r.source]
	if !ok || doc.version != r.version {
		return nil
	}
	mapped := []lsp.Diagnostic{}
	for _, d := range params.Diagnostics {
		if result, ok := r.project.mapDiagnostic(d); ok {
			if result.Source == "" {
				result.Source = client.kind
			}
			mapped = append(mapped, result)
		}
	}
	s.host[r.source] = hostDiagnostics{version: doc.version, diagnostics: mapped}
	return s.publish(ctx, r.source)
}

func (client backendClient) Configuration(_ context.Context, params *lsp.ConfigurationParams) ([]any, error) {
	result := make([]any, len(params.Items))
	yamlSettings := map[string]any{
		"schemaStore": map[string]any{"enable": false},
		"schemas":     client.settings.YAMLSchemas, "validate": true,
	}
	for i, item := range params.Items {
		switch item.Section {
		case "":
			result[i] = map[string]any{"yaml": yamlSettings}
		case "yaml":
			result[i] = yamlSettings
		case "yaml.schemaStore":
			result[i] = map[string]any{"enable": false}
		case "yaml.schemas":
			result[i] = client.settings.YAMLSchemas
		case "yaml.validate":
			result[i] = true
		case "bashIde":
			result[i] = map[string]any{"shellcheckPath": client.settings.ShellCheckPath}
		default:
			result[i] = map[string]any{}
		}
	}
	return result, nil
}
func (backendClient) WorkspaceFolders(context.Context) ([]lsp.WorkspaceFolder, error) {
	return nil, nil
}
func (backendClient) RegisterCapability(context.Context, *lsp.RegistrationParams) error {
	return nil
}
func (backendClient) UnregisterCapability(context.Context, *lsp.UnregistrationParams) error {
	return nil
}
func (backendClient) WorkDoneProgressCreate(context.Context, *lsp.WorkDoneProgressCreateParams) error {
	return nil
}
func (backendClient) Progress(context.Context, *lsp.ProgressParams) error { return nil }
func (backendClient) LogMessage(context.Context, *lsp.LogMessageParams) error {
	return nil
}
func (backendClient) ShowMessage(context.Context, *lsp.ShowMessageParams) error {
	return nil
}
func (backendClient) Telemetry(context.Context, interface{}) error { return nil }

func startBackend(kind, executable, root string, owner *server, config settings) (*backend, error) {
	args := []string{"--stdio"}
	if kind == "bash" {
		args = []string{"start"}
	}
	command := exec.Command(executable, args...)
	command.Stderr = io.Discard
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	b := &backend{command: command, opened: make(map[lsp.DocumentURI]int32)}
	_, conn, downstream := lsp.NewClient(context.Background(), backendClient{owner: owner, kind: kind, settings: config},
		jsonrpc2.NewStream(backendStream{stdout, stdin}), zap.NewNop())
	b.conn, b.server = conn, downstream
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = downstream.Initialize(ctx, &lsp.InitializeParams{
		ProcessID: int32(os.Getpid()), RootURI: uri.File(root),
		ClientInfo: &lsp.ClientInfo{Name: "plato-ls"},
	})
	if err == nil {
		err = downstream.Initialized(ctx, &lsp.InitializedParams{})
	}
	if err == nil && kind == "yaml" {
		err = downstream.DidChangeConfiguration(ctx, &lsp.DidChangeConfigurationParams{
			Settings: map[string]any{"yaml": map[string]any{
				"schemaStore": map[string]any{"enable": false}, "schemas": config.YAMLSchemas, "validate": true,
			}},
		})
	}
	if err != nil {
		b.stop()
		return nil, err
	}
	return b, nil
}

func (b *backend) stop() {
	b.stopOnce.Do(func() {
		b.closed.Store(true)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = b.server.Shutdown(ctx)
		_ = b.server.Exit(ctx)
		_ = b.conn.Close()
		if b.command.Process != nil {
			_ = b.command.Process.Kill()
			_ = b.command.Wait()
		}
	})
}

func hostForFile(filename, content string) (kind, extension string) {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".yaml", ".yml":
		return "yaml", ".yaml"
	case ".sh", ".bash":
		return "bash", ".sh"
	}
	first, _, _ := strings.Cut(content, "\n")
	fields := strings.Fields(strings.TrimPrefix(first, "#!"))
	if !strings.HasPrefix(first, "#!") || len(fields) == 0 {
		return "", ""
	}
	interpreter := fields[0]
	if strings.HasSuffix(interpreter, "/bash") || strings.HasSuffix(interpreter, "/sh") {
		return "bash", ".sh"
	}
	if strings.HasSuffix(interpreter, "/env") {
		args := fields[1:]
		if len(args) > 0 && args[0] == "-S" {
			args = args[1:]
		}
		if len(args) > 0 && (args[0] == "bash" || args[0] == "sh") {
			return "bash", ".sh"
		}
	}
	return "", ""
}

func (s *server) syncHost(u lsp.DocumentURI) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	s.mu.Lock()
	doc, exists := s.docs[u]
	settings := s.settings
	epoch := s.epoch
	folders := append([]string{}, s.folders...)
	s.mu.Unlock()
	filename := pathForURI(u)
	kind, extension := hostForFile(filename, doc.text)
	if !exists || kind == "" {
		return
	}
	path := settings.YAMLBackend
	if kind == "bash" {
		path = settings.BashBackend
	}
	if path == "" {
		return
	}
	w := settings.forFile(filename, folders)
	if !w.eligible(filename, doc.text) || len(syntaxDiagnostics(doc.text, w)) != 0 {
		return
	}
	p, err := makeProjection(doc.text, kind, settings.YAMLSchemas)
	if err != nil {
		s.hostFailure(u, doc.version, kind+" projection unavailable: incompatible Plato grammar")
		return
	}
	if !p.safe {
		s.hostFailure(u, doc.version, kind+" validation unavailable for control flow, trimming, generated content, block scalars, or heredocs")
		return
	}
	key := kind + "|" + w.root + "|" + path
	b := s.backends[key]
	if b == nil {
		b, err = startBackend(kind, path, w.root, s, settings)
		if err != nil {
			s.hostFailure(u, doc.version, fmt.Sprintf("%s backend unavailable: check the configured executable and stdio startup", kind))
			return
		}
		s.backends[key] = b
		go s.watchBackend(key, b)
		_ = s.client.ShowMessage(context.Background(), &lsp.ShowMessageParams{
			Type: lsp.MessageTypeInfo, Message: kind + " backend connected; host diagnostics are best-effort",
		})
	}

	generated := uri.File(fmt.Sprintf("%s.plato-ls-v%d%s", filename, doc.version, extension))
	s.mu.Lock()
	current, ok := s.docs[u]
	if !ok || current.version != doc.version || s.epoch != epoch {
		s.mu.Unlock()
		return
	}
	var prior lsp.DocumentURI
	for id, candidate := range s.routes {
		if candidate.source == u && candidate.key == key {
			prior = id
			delete(s.routes, id)
		}
	}
	s.routes[generated] = route{source: u, version: doc.version, epoch: epoch, kind: kind, key: key, project: p}
	s.mu.Unlock()
	if prior != "" {
		_ = b.server.DidClose(context.Background(), &lsp.DidCloseTextDocumentParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: prior},
		})
		delete(b.opened, prior)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, open := b.opened[generated]; !open {
		err = b.server.DidOpen(ctx, &lsp.DidOpenTextDocumentParams{TextDocument: lsp.TextDocumentItem{
			URI: generated, LanguageID: lsp.LanguageIdentifier(kind), Version: doc.version, Text: p.text,
		}})
	} else {
		err = b.server.DidChange(ctx, &lsp.DidChangeTextDocumentParams{
			TextDocument: lsp.VersionedTextDocumentIdentifier{
				TextDocumentIdentifier: lsp.TextDocumentIdentifier{URI: generated}, Version: doc.version,
			},
			ContentChanges: []lsp.TextDocumentContentChangeEvent{{Text: p.text}},
		})
	}
	if err != nil {
		s.hostFailure(u, doc.version, kind+" backend disconnected")
		delete(s.backends, key)
		b.stop()
		return
	}
	b.opened[generated] = doc.version
}

func (s *server) watchBackend(key string, b *backend) {
	<-b.conn.Done()
	if b.closed.Load() {
		return
	}
	s.syncMu.Lock()
	if s.backends[key] != b {
		s.syncMu.Unlock()
		return
	}
	delete(s.backends, key)
	s.mu.Lock()
	var affected []route
	for id, r := range s.routes {
		if r.key == key {
			affected = append(affected, r)
			delete(s.routes, id)
		}
	}
	s.mu.Unlock()
	b.stop()
	s.syncMu.Unlock()
	for _, r := range affected {
		s.hostFailure(r.source, r.version, r.kind+" backend disconnected; check the configured executable")
	}
}

func (s *server) hostFailure(u lsp.DocumentURI, version int32, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if doc, ok := s.docs[u]; ok && doc.version == version {
		d := diagnostic(doc.text, 0, 0, message)
		d.Severity = lsp.DiagnosticSeverityWarning
		s.host[u] = hostDiagnostics{version: version, diagnostics: []lsp.Diagnostic{d}}
		_ = s.publish(context.Background(), u)
	}
}

func (s *server) closeHost(u lsp.DocumentURI) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	s.mu.Lock()
	if _, open := s.docs[u]; open {
		s.mu.Unlock()
		return
	}
	var generated lsp.DocumentURI
	var r route
	for id, candidate := range s.routes {
		if candidate.source == u {
			generated, r = id, candidate
			delete(s.routes, id)
			break
		}
	}
	s.mu.Unlock()
	if generated != "" {
		if b := s.backends[r.key]; b != nil {
			_ = b.server.DidClose(context.Background(), &lsp.DidCloseTextDocumentParams{
				TextDocument: lsp.TextDocumentIdentifier{URI: generated},
			})
			delete(b.opened, generated)
		}
	}
}

func (s *server) reconfigureBackends() {
	s.syncMu.Lock()
	for _, b := range s.backends {
		b.stop()
	}
	s.backends = make(map[string]*backend)
	s.mu.Lock()
	s.routes = make(map[lsp.DocumentURI]route)
	var docs []lsp.DocumentURI
	for u := range s.docs {
		docs = append(docs, u)
	}
	s.mu.Unlock()
	s.syncMu.Unlock()
	for _, u := range docs {
		go s.syncHost(u)
	}
}

func (s *server) closeBackends() {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	for _, b := range s.backends {
		b.stop()
	}
}
