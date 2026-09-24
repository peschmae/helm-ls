package plato

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	lsp "go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

func TestUnsafeHostProjectionReportsWithheldValidation(t *testing.T) {
	root, _ := os.Getwd()
	c := &integrationClient{events: make(chan lsp.PublishDiagnosticsParams, 4), notice: make(chan lsp.ShowMessageParams, 4)}
	s := &server{client: c, docs: make(map[lsp.DocumentURI]document),
		host: make(map[lsp.DocumentURI]hostDiagnostics), routes: make(map[lsp.DocumentURI]route),
		backends: make(map[string]*backend), settings: settings{
			Root: root, YAMLBackend: filepath.Join(root, "nonexistent-yaml-backend"),
		}}
	u := uri.File(filepath.Join(root, "templates", "sample.yaml"))
	if err := s.DidOpen(context.Background(), &lsp.DidOpenTextDocumentParams{TextDocument: lsp.TextDocumentItem{
		URI: u, Version: 1, Text: "note: |\n  {{{ .name }}}\n",
	}}); err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-c.events:
			if len(event.Diagnostics) == 1 &&
				strings.Contains(event.Diagnostics[0].Message, "block scalars") {
				return
			}
		case <-timer.C:
			t.Fatal("unsafe block-scalar projection was reported as clean")
		}
	}
}
