package plato

import (
	"reflect"
	"testing"
)

func TestPlatoGrammarLiteralSpans(t *testing.T) {
	content := "name: {{{ .name }}}\n{{{ if .ok }}}ready{{{ end }}}\n"
	spans, err := literalSpans(content)
	if err != nil {
		t.Fatal(err)
	}
	var literals []string
	for _, part := range spans {
		literals = append(literals, content[part.start:part.end])
	}
	t.Logf("literal nodes: %#v", literals)
	if !reflect.DeepEqual(literals, []string{"name: ", "\n", "ready", "\n"}) {
		t.Fatalf("literal nodes: %#v", literals)
	}
}
