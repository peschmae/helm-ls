package plato

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	lsp "go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

func TestBackendVersionAndLiteralMapping(t *testing.T) {
	root, _ := os.Getwd()
	s, client, source := testServer(root)
	text := "same: one\nsame: two\nname: {{{ .name }}}\n"
	p, err := makeProjection(text, "yaml")
	if err != nil {
		t.Fatal(err)
	}
	s.docs[source] = document{text: text, version: 2}
	s.host = make(map[lsp.DocumentURI]hostDiagnostics)
	s.routes = make(map[lsp.DocumentURI]route)
	old := uri.File(source.Filename() + ".plato-ls-v1.yaml")
	current := uri.File(source.Filename() + ".plato-ls-v2.yaml")
	s.routes[old] = route{source: source, version: 1, kind: "yaml", project: p}
	s.routes[current] = route{source: source, version: 2, kind: "yaml", project: p}
	diagnostic := lsp.Diagnostic{Range: lsp.Range{
		Start: lsp.Position{Line: 1, Character: 0}, End: lsp.Position{Line: 1, Character: 10},
	}, Message: "Map keys must be unique"}
	proxy := backendClient{owner: s, kind: "yaml"}
	for _, params := range []lsp.PublishDiagnosticsParams{
		{URI: old, Diagnostics: []lsp.Diagnostic{diagnostic}},
		{URI: current, Version: 3, Diagnostics: []lsp.Diagnostic{diagnostic}},
	} {
		if err := proxy.PublishDiagnostics(context.Background(), &params); err != nil {
			t.Fatal(err)
		}
	}
	if len(client.events) != 0 {
		t.Fatalf("stale diagnostics published: %+v", client.events)
	}
	if err := proxy.PublishDiagnostics(context.Background(), &lsp.PublishDiagnosticsParams{
		URI: current, Diagnostics: []lsp.Diagnostic{diagnostic},
	}); err != nil {
		t.Fatal(err)
	}
	if len(client.events) != 1 || client.events[0].Version != 2 ||
		len(client.events[0].Diagnostics) != 1 ||
		client.events[0].Diagnostics[0].Range.End.Character != 9 ||
		client.events[0].Diagnostics[0].Source != "yaml" {
		t.Fatalf("mapped diagnostics: %+v", client.events)
	}
	if err := s.DidChange(context.Background(), &lsp.DidChangeTextDocumentParams{
		TextDocument: lsp.VersionedTextDocumentIdentifier{
			TextDocumentIdentifier: lsp.TextDocumentIdentifier{URI: source}, Version: 3,
		},
		ContentChanges: []lsp.TextDocumentContentChangeEvent{{Text: "{{{ unknownFunction }}}"}},
	}); err != nil {
		t.Fatal(err)
	}
	if len(client.events) != 2 || client.events[1].Version != 3 ||
		len(client.events[1].Diagnostics) != 1 || client.events[1].Diagnostics[0].Source != "plato-ls" {
		t.Fatalf("host diagnostic not cleared on syntax error: %+v", client.events)
	}
	_ = proxy.PublishDiagnostics(context.Background(), &lsp.PublishDiagnosticsParams{
		URI: current, Diagnostics: []lsp.Diagnostic{diagnostic},
	})
	if len(client.events) != 2 {
		t.Fatalf("stale host result replaced template error: %+v", client.events)
	}
}

func TestHostClassificationPrecedenceAndShebang(t *testing.T) {
	for _, tc := range []struct {
		filename, content, kind, extension string
	}{
		{"input.yml", "#!/bin/bash\n", "yaml", ".yaml"},
		{"input.YAML", "#!/usr/bin/env sh\n", "yaml", ".yaml"},
		{"input.sh", "name: value\n", "bash", ".sh"},
		{"input", "#!/bin/bash\n", "bash", ".sh"},
		{"input", "#!/bin/sh\r\n", "bash", ".sh"},
		{"input", "#!/usr/bin/env bash -e\n", "bash", ".sh"},
		{"input", "#!/usr/bin/env -S sh -e\n", "bash", ".sh"},
		{"input", "#!/bin/zsh\n", "", ""},
		{"input.txt", "echo '#!/bin/bash'\n", "", ""},
	} {
		kind, extension := hostForFile(tc.filename, tc.content)
		if kind != tc.kind || extension != tc.extension {
			t.Errorf("%s %q: got %s %s, want %s %s", tc.filename, tc.content, kind, extension, tc.kind, tc.extension)
		}
	}
}

type integrationClient struct {
	lsp.Client
	events chan lsp.PublishDiagnosticsParams
	notice chan lsp.ShowMessageParams
}

func (c *integrationClient) PublishDiagnostics(_ context.Context, params *lsp.PublishDiagnosticsParams) error {
	c.events <- *params
	return nil
}
func (c *integrationClient) ShowMessage(_ context.Context, params *lsp.ShowMessageParams) error {
	c.notice <- *params
	return nil
}

func TestRealYAMLBackend(t *testing.T) {
	executable := os.Getenv("PLATO_YAML_BACKEND")
	if executable == "" {
		t.Skip("set PLATO_YAML_BACKEND to opt into the local YAML backend integration")
	}

	root, err := os.MkdirTemp(".", "backend-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.MkdirAll(filepath.Join(root, "templates"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plato.yaml"), []byte("plato:\n  source: templates\n"), 0600); err != nil {
		t.Fatal(err)
	}
	absolute, _ := filepath.Abs(root)
	executable, _ = filepath.Abs(executable)
	c := &integrationClient{events: make(chan lsp.PublishDiagnosticsParams, 16), notice: make(chan lsp.ShowMessageParams, 8)}
	s := &server{client: c, docs: make(map[lsp.DocumentURI]document),
		host: make(map[lsp.DocumentURI]hostDiagnostics), routes: make(map[lsp.DocumentURI]route),
		backends: make(map[string]*backend), settings: settings{Root: absolute, YAMLBackend: executable}}
	t.Cleanup(s.closeBackends)
	u := uri.File(filepath.Join(absolute, "templates", "sample.yaml"))
	if err := s.DidOpen(context.Background(), &lsp.DidOpenTextDocumentParams{TextDocument: lsp.TextDocumentItem{
		URI: u, Version: 1, Text: "same: one\nsame: two\nname: {{{ .name }}}\n",
	}}); err != nil {
		t.Fatal(err)
	}
	expectEvent := func(version uint32, withError bool) {
		t.Helper()
		timer := time.NewTimer(12 * time.Second)
		defer timer.Stop()
		for {
			select {
			case event := <-c.events:
				t.Logf("backend event: version=%d diagnostics=%d", event.Version, len(event.Diagnostics))
				for _, d := range event.Diagnostics {
					t.Logf("diagnostic source=%s class=%s", d.Source, d.Message)
				}
				if event.URI != u || event.Version != version {
					continue
				}
				if withError && len(event.Diagnostics) == 1 && strings.EqualFold(event.Diagnostics[0].Source, "yaml") {
					if event.Diagnostics[0].Range.Start.Line != 1 {
						t.Fatalf("wrong YAML source line: %+v", event.Diagnostics)
					}
					return
				}
				if !withError && len(event.Diagnostics) == 0 {
					return
				}
			case msg := <-c.notice:
				t.Logf("backend notice: %s", msg.Message)
				if strings.Contains(msg.Message, "unavailable") {
					t.Fatalf("backend unavailable: %s", msg.Message)
				}
			case <-timer.C:
				t.Fatal("timed out waiting for mapped YAML backend diagnostics")
			}
		}
	}
	expectEvent(1, true)
	if err := s.DidChange(context.Background(), &lsp.DidChangeTextDocumentParams{
		TextDocument: lsp.VersionedTextDocumentIdentifier{
			TextDocumentIdentifier: lsp.TextDocumentIdentifier{URI: u}, Version: 2,
		},
		ContentChanges: []lsp.TextDocumentContentChangeEvent{{Text: "same: one\nname: {{{ .name }}}\n"}},
	}); err != nil {
		t.Fatal(err)
	}
	expectEvent(2, false)
}

func TestRealBashBackendAvailability(t *testing.T) {
	executable := os.Getenv("PLATO_BASH_BACKEND")
	if executable == "" {
		t.Skip("set PLATO_BASH_BACKEND to opt into the local Bash backend integration")
	}
	root, err := os.MkdirTemp(".", "backend-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	absolute, _ := filepath.Abs(root)
	executable, _ = filepath.Abs(executable)
	c := &integrationClient{events: make(chan lsp.PublishDiagnosticsParams, 16), notice: make(chan lsp.ShowMessageParams, 8)}
	s := &server{client: c, docs: make(map[lsp.DocumentURI]document),
		host: make(map[lsp.DocumentURI]hostDiagnostics), routes: make(map[lsp.DocumentURI]route),
		backends: make(map[string]*backend), settings: settings{Root: absolute, BashBackend: executable}}
	t.Cleanup(s.closeBackends)
	u := uri.File(filepath.Join(absolute, "templates", "sample"))
	if err := s.DidOpen(context.Background(), &lsp.DidOpenTextDocumentParams{TextDocument: lsp.TextDocumentItem{
		URI: u, Version: 1, Text: "#!/usr/bin/env bash\necho ok\nvalue=\"{{{ .value }}}\"\n",
	}}); err != nil {
		t.Fatal(err)
	}
	select {
	case notice := <-c.notice:
		if !strings.Contains(notice.Message, "bash backend connected") {
			t.Fatalf("unexpected backend notice: %s", notice.Message)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("Bash backend did not connect")
	}
	if _, err := exec.LookPath("shellcheck"); err != nil {
		select {
		case event := <-c.events:
			if len(event.Diagnostics) != 1 ||
				!strings.Contains(event.Diagnostics[0].Message, "ShellCheck unavailable") {
				t.Fatalf("missing ShellCheck not reported: %+v", event.Diagnostics)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("missing ShellCheck diagnostic not published")
		}
	} else {
		if err := s.DidChange(context.Background(), &lsp.DidChangeTextDocumentParams{
			TextDocument: lsp.VersionedTextDocumentIdentifier{
				TextDocumentIdentifier: lsp.TextDocumentIdentifier{URI: u}, Version: 2,
			},
			ContentChanges: []lsp.TextDocumentContentChangeEvent{{
				Text: "#!/usr/bin/env bash\necho $undefined_var\nvalue=\"{{{ .value }}}\"\n",
			}},
		}); err != nil {
			t.Fatal(err)
		}
		timer := time.NewTimer(12 * time.Second)
		defer timer.Stop()
		found := false
		for !found {
			select {
			case event := <-c.events:
				if event.Version != 2 {
					continue
				}
				for _, d := range event.Diagnostics {
					if d.Range.Start.Line == 1 && d.Source != "plato-ls" {
						if d.Range != (lsp.Range{
							Start: lsp.Position{Line: 1, Character: 5},
							End:   lsp.Position{Line: 1, Character: 19},
						}) {
							t.Fatalf("wrong ShellCheck source range: %+v", d.Range)
						}
						found = true
						break
					}
				}
			case <-timer.C:
				t.Fatal("ShellCheck did not report a literal Bash error at the source line")
			}
		}
		if err := s.DidChange(context.Background(), &lsp.DidChangeTextDocumentParams{
			TextDocument: lsp.VersionedTextDocumentIdentifier{
				TextDocumentIdentifier: lsp.TextDocumentIdentifier{URI: u}, Version: 3,
			},
			ContentChanges: []lsp.TextDocumentContentChangeEvent{{
				Text: "#!/usr/bin/env bash\necho ok\n",
			}},
		}); err != nil {
			t.Fatal(err)
		}
		clearTimer := time.NewTimer(5 * time.Second)
		defer clearTimer.Stop()
	clearLoop:
		for {
			select {
			case event := <-c.events:
				if event.Version != 3 {
					continue
				}
				if len(event.Diagnostics) != 0 {
					t.Fatalf("Bash correction did not clear diagnostics: %+v", event)
				}
				break clearLoop
			case <-clearTimer.C:
				t.Fatal("Bash correction did not publish cleared diagnostics")
			}
		}
	}
	if err := s.DidClose(context.Background(), &lsp.DidCloseTextDocumentParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: u},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRealYAMLBackendExplicitLocalSchema(t *testing.T) {
	executable := os.Getenv("PLATO_YAML_BACKEND")
	if executable == "" {
		t.Skip("set PLATO_YAML_BACKEND to opt into the local schema integration")
	}
	root, err := os.MkdirTemp(".", "backend-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	absolute, _ := filepath.Abs(root)
	schema := filepath.Join(absolute, "schema.json")
	if err := os.WriteFile(schema, []byte(`{"type":"object","properties":{"name":{"type":"integer"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	executable, _ = filepath.Abs(executable)
	c := &integrationClient{events: make(chan lsp.PublishDiagnosticsParams, 16), notice: make(chan lsp.ShowMessageParams, 8)}
	s := &server{client: c, docs: make(map[lsp.DocumentURI]document),
		host: make(map[lsp.DocumentURI]hostDiagnostics), routes: make(map[lsp.DocumentURI]route),
		backends: make(map[string]*backend), settings: settings{Root: absolute, YAMLBackend: executable,
			YAMLSchemas: map[string][]string{string(uri.File(schema)): {"*.yaml"}}}}
	t.Cleanup(s.closeBackends)
	u := uri.File(filepath.Join(absolute, "templates", "sample.yaml"))
	if err := s.DidOpen(context.Background(), &lsp.DidOpenTextDocumentParams{TextDocument: lsp.TextDocumentItem{
		URI: u, Version: 1, Text: "name: wrong\n",
	}}); err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(12 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-c.events:
			for _, d := range event.Diagnostics {
				if strings.Contains(d.Message, "Expected") {
					if d.Range.Start.Line != 0 {
						t.Fatalf("schema diagnostic mapped to wrong source line: %+v", d)
					}
					return
				}
			}
		case <-timer.C:
			t.Fatal("explicit local YAML schema diagnostic was not mapped")
		}
	}
}

func TestRealYAMLBackendDoesNotFetchUnconfiguredSchema(t *testing.T) {
	executable := os.Getenv("PLATO_YAML_BACKEND")
	if executable == "" {
		t.Skip("set PLATO_YAML_BACKEND to opt into the remote-schema isolation check")
	}
	var requests atomic.Int32
	schemaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"object"}`))
	}))
	defer schemaServer.Close()
	root, err := os.MkdirTemp(".", "backend-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	absolute, _ := filepath.Abs(root)
	executable, _ = filepath.Abs(executable)
	c := &integrationClient{events: make(chan lsp.PublishDiagnosticsParams, 16), notice: make(chan lsp.ShowMessageParams, 8)}
	s := &server{client: c, docs: make(map[lsp.DocumentURI]document),
		host: make(map[lsp.DocumentURI]hostDiagnostics), routes: make(map[lsp.DocumentURI]route),
		backends: make(map[string]*backend), settings: settings{Root: absolute, YAMLBackend: executable}}
	t.Cleanup(s.closeBackends)
	u := uri.File(filepath.Join(absolute, "templates", "sample.yaml"))
	for version, source := range []string{
		"# yaml-language-server: $schema=" + schemaServer.URL + "/schema.json\nsame: one\nsame: two\n",
		"$schema: " + schemaServer.URL + "/schema.json\nsame: one\nsame: two\n",
	} {
		v := int32(version + 1)
		if v == 1 {
			err = s.DidOpen(context.Background(), &lsp.DidOpenTextDocumentParams{
				TextDocument: lsp.TextDocumentItem{URI: u, Version: v, Text: source},
			})
		} else {
			err = s.DidChange(context.Background(), &lsp.DidChangeTextDocumentParams{
				TextDocument: lsp.VersionedTextDocumentIdentifier{
					TextDocumentIdentifier: lsp.TextDocumentIdentifier{URI: u}, Version: v,
				},
				ContentChanges: []lsp.TextDocumentContentChangeEvent{{Text: source}},
			})
		}
		if err != nil {
			t.Fatal(err)
		}
		timer := time.NewTimer(12 * time.Second)
		found := false
		for !found {
			select {
			case event := <-c.events:
				if event.Version == uint32(v) {
					for _, d := range event.Diagnostics {
						if d.Range.Start.Line == 2 && strings.EqualFold(d.Source, "yaml") {
							found = true
						}
					}
				}
			case <-timer.C:
				t.Fatal("YAML backend did not validate unchanged literal after schema masking")
			}
		}
		timer.Stop()
		if got := requests.Load(); got != 0 {
			t.Fatalf("unconfigured schema URL was fetched %d times", got)
		}
	}
}

func TestConfiguredMissingBackendReportsFailure(t *testing.T) {
	root, _ := os.Getwd()
	c := &integrationClient{events: make(chan lsp.PublishDiagnosticsParams, 4), notice: make(chan lsp.ShowMessageParams, 4)}
	s := &server{client: c, docs: make(map[lsp.DocumentURI]document),
		host: make(map[lsp.DocumentURI]hostDiagnostics), routes: make(map[lsp.DocumentURI]route),
		backends: make(map[string]*backend), settings: settings{
			Root: root, YAMLBackend: filepath.Join(root, "nonexistent-yaml-backend"),
		}}
	u := uri.File(filepath.Join(root, "templates", "sample.yaml"))
	if err := s.DidOpen(context.Background(), &lsp.DidOpenTextDocumentParams{TextDocument: lsp.TextDocumentItem{
		URI: u, Version: 1, Text: "name: x\n",
	}}); err != nil {
		t.Fatal(err)
	}

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-c.events:
			if len(event.Diagnostics) == 1 && strings.Contains(event.Diagnostics[0].Message, "backend unavailable") {
				return
			}
		case <-timer.C:
			t.Fatal("missing configured backend was reported as clean")
		}
	}
}
