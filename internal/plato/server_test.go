package plato

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	lsp "go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

type collectingClient struct {
	lsp.Client
	events []lsp.PublishDiagnosticsParams
	notice []lsp.ShowMessageParams
}

func (c *collectingClient) PublishDiagnostics(_ context.Context, params *lsp.PublishDiagnosticsParams) error {
	c.events = append(c.events, *params)
	return nil
}

func (c *collectingClient) ShowMessage(_ context.Context, params *lsp.ShowMessageParams) error {
	c.notice = append(c.notice, *params)
	return nil
}

func testServer(root string) (*server, *collectingClient, lsp.DocumentURI) {
	client := &collectingClient{}
	s := &server{client: client, docs: map[lsp.DocumentURI]document{}, settings: settings{Root: root}}
	return s, client, uri.File(filepath.Join(root, "templates", "test.txt"))
}

func TestLifecycleAndUTF16Ranges(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	s, client, u := testServer(root)
	ctx := context.Background()
	broken := "name: 🐧 {{{ mystery .key }}}\n"
	if err := s.DidOpen(ctx, &lsp.DidOpenTextDocumentParams{TextDocument: lsp.TextDocumentItem{
		URI: u, Version: 1, Text: broken,
	}}); err != nil {
		t.Fatal(err)
	}
	got := client.events[0].Diagnostics
	if len(got) != 1 || got[0].Message != "Unknown template function" || got[0].Source != "plato-ls" ||
		got[0].Range.Start != (lsp.Position{Line: 0, Character: 9}) {
		t.Fatalf("diagnostics: %+v", got)
	}
	valid := "name: 🐧 {{{ .key | default \"x\" }}}\n{{{ PLATO }}} {{{ filepath }}} {{{ ToYAML . 2 }}}"
	if err := s.DidChange(ctx, &lsp.DidChangeTextDocumentParams{
		TextDocument:   lsp.VersionedTextDocumentIdentifier{TextDocumentIdentifier: lsp.TextDocumentIdentifier{URI: u}, Version: 2},
		ContentChanges: []lsp.TextDocumentContentChangeEvent{{Text: valid}},
	}); err != nil {
		t.Fatal(err)
	}
	if len(client.events[1].Diagnostics) != 0 || client.events[1].Version != 2 {
		t.Fatalf("change: %+v", client.events[1])
	}
	if err := s.DidClose(ctx, &lsp.DidCloseTextDocumentParams{TextDocument: lsp.TextDocumentIdentifier{URI: u}}); err != nil {
		t.Fatal(err)
	}
	if len(client.events[2].Diagnostics) != 0 || len(s.docs) != 0 {
		t.Fatalf("close: %+v", client.events[2])
	}
}

func TestSyntaxAndSelection(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	w := settings{Root: root}.forFile(filepath.Join(root, "templates", "test.yaml"), nil)
	for _, tc := range []struct {
		text    string
		invalid bool
	}{
		{"{{{if .x}}}yes{{{end}}}", false},
		{"{{{- if .x }}}}\r\n{{{ range .items }}}{{{ .name }}}{{{ end }}}{{{- end }}}", false},
		{"{{{/* comment */}}} {{{ .missing.value }}}", false},
		{"{{{# invalid }}}", true},
		{"{{{ if .x }}}", true},
		{"prefix {{{", true},
	} {
		got := syntaxDiagnostics(tc.text, w)
		if (len(got) == 1) != tc.invalid {
			t.Fatalf("syntax %q: %+v", tc.text, got)
		}
	}
	if w.eligible(filepath.Join(root, "templates", "thing.sops_enc"), "") ||
		w.eligible(filepath.Join(root, "templates", "thing.gem"), "") ||
		w.eligible(filepath.Join(root, "elsewhere", "thing.yaml"), "") ||
		w.eligible(filepath.Join(root, "templates", "thing.yaml"), "a\x00b") {
		t.Fatal("excluded file selected")
	}
	if got := syntaxDiagnostics("{{{ .x }}}", workspace{left: "{{", right: "}}"}); len(got) != 1 ||
		!strings.Contains(got[0].Message, "Unsupported") {
		t.Fatalf("custom delimiters: %+v", got)
	}
	s := settings{Root: root, Source: "other", ExcludedPaths: []string{"private/*"}}
	w = s.forFile(filepath.Join(root, "other", "private", "x.yaml"), nil)
	if w.eligible(filepath.Join(root, "other", "private", "x.yaml"), "") {
		t.Fatal("path exclusion ignored")
	}
}

func TestWorkspaceConfig(t *testing.T) {
	base, err := os.MkdirTemp(".", "workspace-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	if err := os.WriteFile(filepath.Join(base, "plato.yaml"), []byte("plato:\n  source: input\n  delimiters:\n    left: '[['\n    right: ']]'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	absolute, _ := filepath.Abs(base)
	filename := filepath.Join(absolute, "input", "example.sh")
	w := (settings{}).forFile(filename, []string{absolute})
	if !w.eligible(filename, "") || w.source != filepath.Join(absolute, "input") ||
		!strings.Contains(syntaxDiagnostics("", w)[0].Message, "Unsupported") {
		t.Fatalf("config: %+v", w)
	}
	if err := os.MkdirAll(filepath.Join(base, "input"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename+".symlink", nil, 0600); err != nil {
		t.Fatal(err)
	}
	if w.eligible(filename, "") {
		t.Fatal("symlink marker companion selected")
	}
	if err := os.Remove(filename + ".symlink"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "input", "file.gem"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if w.eligible(filepath.Join(absolute, "input", "file.gem"), "") {
		t.Fatal("binary extension selected")
	}
	if got := (settings{}).forFile(filename, nil); got.source != filepath.Join(absolute, "input") {
		t.Fatalf("ancestor discovery: %+v", got)
	}
}

func TestConfigurationRepublishesOpenDocuments(t *testing.T) {
	root, _ := os.Getwd()
	s, client, u := testServer(root)
	ctx := context.Background()
	_ = s.DidOpen(ctx, &lsp.DidOpenTextDocumentParams{TextDocument: lsp.TextDocumentItem{
		URI: u, Version: 1, Text: "{{{ unknown }}}",
	}})
	if err := s.DidChangeConfiguration(ctx, &lsp.DidChangeConfigurationParams{
		Settings: map[string]any{"root": root, "excludedPaths": []string{"test.txt"}},
	}); err != nil {
		t.Fatal(err)
	}
	if len(client.events) != 2 || len(client.events[0].Diagnostics) != 1 || len(client.events[1].Diagnostics) != 0 {
		t.Fatalf("configuration events: %+v", client.events)
	}
}

func TestBackendEnvironmentIsOptInAndSettingsErrorsAreExplicit(t *testing.T) {
	t.Setenv("PLATO_LS_YAML_BACKEND", "/project/yaml-language-server")
	t.Setenv("PLATO_LS_BASH_BACKEND", "/project/bash-language-server")
	t.Setenv("PLATO_LS_SHELLCHECK_PATH", "/project/shellcheck")
	defaults, err := parseSettings(nil)
	if err != nil {
		t.Fatal(err)
	}
	if defaults.YAMLBackend != "/project/yaml-language-server" ||
		defaults.BashBackend != "/project/bash-language-server" ||
		defaults.ShellCheckPath != "/project/shellcheck" {
		t.Fatalf("environment settings: %+v", defaults)
	}
	override, err := parseSettings(map[string]any{"yamlBackend": "/explicit/yaml-language-server"})
	if err != nil || override.YAMLBackend != "/explicit/yaml-language-server" {
		t.Fatalf("explicit settings override: %+v, %v", override, err)
	}
	disabled, err := parseSettings(map[string]any{"yamlBackend": ""})
	if err != nil || disabled.YAMLBackend != "" {
		t.Fatalf("explicit disable ignored: %+v, %v", disabled, err)
	}
	if _, err := parseSettings(map[string]any{"yamlBackend": 5}); err == nil {
		t.Fatal("invalid backend setting silently ignored")
	}
	if _, err := parseSettings(map[string]any{"yamlSchemas": map[string][]string{
		"https://example.invalid/schema.json": {"*.yaml"},
	}}); err == nil {
		t.Fatal("remote schema accepted")
	}
	if _, err := parseSettings(map[string]any{"yamlSchemas": map[string][]string{
		"file:///project/schema.json": {"*.yaml"},
	}}); err != nil {
		t.Fatalf("local schema rejected: %v", err)
	}
	s := &server{settings: defaults, client: &collectingClient{}, docs: map[lsp.DocumentURI]document{}}
	if err := s.DidChangeConfiguration(context.Background(), &lsp.DidChangeConfigurationParams{
		Settings: map[string]any{"bashBackend": 5},
	}); err == nil || s.settings.YAMLBackend != defaults.YAMLBackend ||
		s.settings.BashBackend != defaults.BashBackend {
		t.Fatalf("invalid settings changed active configuration: %+v, %v", s.settings, err)
	}
}

func TestHostValidationIsExplicitlyUnavailable(t *testing.T) {
	root, _ := os.Getwd()
	s, client, _ := testServer(root)
	if err := s.Initialized(context.Background(), &lsp.InitializedParams{}); err != nil {
		t.Fatal(err)
	}
	if len(client.notice) != 1 || client.notice[0].Type != lsp.MessageTypeInfo ||
		!strings.Contains(client.notice[0].Message, "configure YAML/Bash backend paths") {
		t.Fatalf("host availability message: %+v", client.notice)
	}
	for _, name := range []string{"config.yaml", "script.sh"} {
		u := uri.File(filepath.Join(root, "templates", name))
		if err := s.DidOpen(context.Background(), &lsp.DidOpenTextDocumentParams{TextDocument: lsp.TextDocumentItem{
			URI: u, Version: 1, Text: "{{{ .known }}}",
		}}); err != nil {
			t.Fatal(err)
		}
		got := client.events[len(client.events)-1].Diagnostics
		if len(got) != 0 {
			t.Fatalf("%s: %+v", name, got)
		}
	}
	u := uri.File(filepath.Join(root, "templates", "config.yaml"))
	if err := s.DidChange(context.Background(), &lsp.DidChangeTextDocumentParams{
		TextDocument:   lsp.VersionedTextDocumentIdentifier{TextDocumentIdentifier: lsp.TextDocumentIdentifier{URI: u}, Version: 2},
		ContentChanges: []lsp.TextDocumentContentChangeEvent{{Text: "{{{ missingFunction }}}"}},
	}); err != nil {
		t.Fatal(err)
	}
	got := client.events[len(client.events)-1].Diagnostics
	if len(got) != 1 || got[0].Message != "Unknown template function" {
		t.Fatalf("syntax: %+v", got)
	}
	if err := s.DidClose(context.Background(), &lsp.DidCloseTextDocumentParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: u},
	}); err != nil {
		t.Fatal(err)
	}
	if got := client.events[len(client.events)-1].Diagnostics; len(got) != 0 {
		t.Fatalf("closed host diagnostics: %+v", got)
	}
}

func TestWorkspaceFolderChangeRepublishes(t *testing.T) {
	base, err := os.MkdirTemp(".", "workspace-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	if err := os.WriteFile(filepath.Join(base, "plato.yaml"), []byte("plato:\n  source: templates\n"), 0600); err != nil {
		t.Fatal(err)
	}
	absolute, _ := filepath.Abs(base)
	u := uri.File(filepath.Join(absolute, "templates", "test.txt"))
	client := &collectingClient{}
	s := &server{client: client, docs: make(map[lsp.DocumentURI]document), folders: []string{filepath.Join(absolute, "different")}}
	ctx := context.Background()
	if err := s.DidOpen(ctx, &lsp.DidOpenTextDocumentParams{TextDocument: lsp.TextDocumentItem{
		URI: u, Text: "{{{ badFunction }}}", Version: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	if len(client.events[0].Diagnostics) != 0 {
		t.Fatalf("outside workspace: %+v", client.events[0].Diagnostics)
	}
	if err := s.DidChangeWorkspaceFolders(ctx, &lsp.DidChangeWorkspaceFoldersParams{
		Event: lsp.WorkspaceFoldersChangeEvent{Added: []lsp.WorkspaceFolder{{URI: string(uri.File(absolute))}}},
	}); err != nil {
		t.Fatal(err)
	}
	if len(client.events[1].Diagnostics) != 1 {
		t.Fatalf("added workspace: %+v", client.events[1].Diagnostics)
	}
	if err := s.DidChangeWorkspaceFolders(ctx, &lsp.DidChangeWorkspaceFoldersParams{
		Event: lsp.WorkspaceFoldersChangeEvent{Removed: []lsp.WorkspaceFolder{{URI: string(uri.File(absolute))}}},
	}); err != nil {
		t.Fatal(err)
	}
	if len(client.events[2].Diagnostics) != 0 {
		t.Fatalf("removed workspace: %+v", client.events[2].Diagnostics)
	}
}

func TestFramedStdio(t *testing.T) {
	root, _ := os.Getwd()
	source := filepath.Join(root, "templates", "test.txt")
	left, right := net.Pipe()
	done := make(chan struct{})
	go func() { Serve(left); close(done) }()
	defer func() { _ = right.Close(); <-done }()
	send := func(message any) {
		t.Helper()
		body, err := json.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintf(right, "Content-Length: %d\r\n\r\n%s", len(body), body); err != nil {
			t.Fatal(err)
		}
	}
	receive := func() map[string]any {
		t.Helper()
		var header string
		for !strings.HasSuffix(header, "\r\n\r\n") {
			var b [1]byte
			if _, err := right.Read(b[:]); err != nil {
				t.Fatal(err)
			}
			header += string(b[:])
		}
		var length int
		if _, err := fmt.Sscanf(header, "Content-Length: %d", &length); err != nil {
			t.Fatal(err)
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(right, body); err != nil {
			t.Fatal(err)
		}
		var parsed map[string]any
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
		"processId": 0, "rootUri": uri.File(root), "initializationOptions": map[string]any{"root": root},
	}})
	if result := receive(); result["id"] != float64(1) || result["result"] == nil {
		t.Fatalf("initialize: %v", result)
	}
	send(map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}})
	if event := receive(); event["method"] != "window/showMessage" {
		t.Fatalf("availability message: %v", event)
	}
	send(map[string]any{"jsonrpc": "2.0", "method": "textDocument/didOpen", "params": map[string]any{
		"textDocument": map[string]any{"uri": uri.File(source), "languageId": "plato", "version": 1, "text": "{{{ broken }}}"},
	}})
	if event := receive(); event["method"] != "textDocument/publishDiagnostics" ||
		len(event["params"].(map[string]any)["diagnostics"].([]any)) != 1 {
		t.Fatalf("open: %v", event)
	}
	send(map[string]any{"jsonrpc": "2.0", "method": "textDocument/didChange", "params": map[string]any{
		"textDocument":   map[string]any{"uri": uri.File(source), "version": 2},
		"contentChanges": []any{map[string]any{"text": "{{{ .valid }}}"}},
	}})
	if event := receive(); len(event["params"].(map[string]any)["diagnostics"].([]any)) != 0 ||
		event["params"].(map[string]any)["version"] != float64(2) {
		t.Fatalf("change: %v", event)
	}
	send(map[string]any{"jsonrpc": "2.0", "method": "textDocument/didSave", "params": map[string]any{
		"textDocument": map[string]any{"uri": uri.File(source)},
	}})
	if event := receive(); len(event["params"].(map[string]any)["diagnostics"].([]any)) != 0 ||
		event["params"].(map[string]any)["version"] != float64(2) {
		t.Fatalf("save: %v", event)
	}
	send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "textDocument/hover", "params": map[string]any{
		"textDocument": map[string]any{"uri": uri.File(source)},
		"position":     map[string]any{"line": 0, "character": 0},
	}})
	if reply := receive(); reply["id"] != float64(3) ||
		reply["error"].(map[string]any)["code"] != float64(-32601) {
		t.Fatalf("unsupported feature was not rejected safely: %v", reply)
	}
	send(map[string]any{"jsonrpc": "2.0", "method": "textDocument/didClose", "params": map[string]any{
		"textDocument": map[string]any{"uri": uri.File(source)},
	}})
	if event := receive(); len(event["params"].(map[string]any)["diagnostics"].([]any)) != 0 {
		t.Fatalf("close: %v", event)
	}
	send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "shutdown", "params": nil})
	if result := receive(); result["id"] != float64(2) {
		t.Fatalf("shutdown: %v", result)
	}
	send(map[string]any{"jsonrpc": "2.0", "method": "exit", "params": map[string]any{}})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not exit after notification")
	}
}
