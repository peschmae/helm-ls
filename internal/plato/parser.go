package plato

import (
	"fmt"

	plato "github.com/mrjosh/helm-ls/internal/tree-sitter/plato"
	ts "github.com/tree-sitter/go-tree-sitter"
)

type span struct{ start, end int }

func literalSpans(content string) ([]span, error) {
	spans, _, err := templateRegions(content)
	return spans, err
}

func templateRegions(content string) ([]span, bool, error) {
	parser := ts.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(ts.NewLanguage(plato.Language())); err != nil {
		return nil, false, fmt.Errorf("Plato grammar is incompatible with the Go Tree-sitter runtime: %w", err)
	}
	tree := parser.Parse([]byte(content), nil)
	if tree == nil {
		return nil, false, fmt.Errorf("Plato grammar did not produce a syntax tree")
	}
	defer tree.Close()
	var spans []span
	unstable := tree.RootNode().HasError()
	var visit func(*ts.Node)
	visit = func(node *ts.Node) {
		switch node.Kind() {
		case "if_action", "range_action", "with_action", "define_action", "block_action",
			"template_action", "break_action", "continue_action":
			unstable = true
		}
		if node.Kind() == "text" || node.Kind() == "yaml_no_injection_text" {
			spans = append(spans, span{int(node.StartByte()), int(node.EndByte())})
			return
		}
		for i := uint(0); i < node.ChildCount(); i++ {
			visit(node.Child(i))
		}
	}
	visit(tree.RootNode())
	return spans, unstable, nil
}
