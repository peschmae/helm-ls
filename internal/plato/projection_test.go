package plato

import (
	"strings"
	"testing"

	lsp "go.lsp.dev/protocol"
)

func TestProjectionExactProjectedBytes(t *testing.T) {
	action := "{{{ .value }}}"
	for _, tc := range []struct{ host, text string }{
		{"yaml", "name: \"" + action + "\"\r\n"},
		{"bash", "echo \"" + action + "\"\r\n"},
	} {
		p, err := makeProjection(tc.text, tc.host)
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Replace(tc.text, action, "x"+strings.Repeat(" ", len(action)-1), 1)
		if p.text != want || !p.safe {
			t.Fatalf("%s projection: got %q, want %q, safe=%v", tc.host, p.text, want, p.safe)
		}
	}
}

func TestOnlyUnsafeYAMLSchemaSelectorsAreNeutralized(t *testing.T) {
	for _, tc := range []struct{ source, want string }{
		{
			"# yaml-language-server: $schema=https://example.invalid/🐧.json\r\nname: x\r\n",
			"# yaml-language-server: _schema=https://example.invalid/🐧.json\r\nname: x\r\n",
		},
		{
			"# $schema: https://example.invalid/schema.json\nname: x\n",
			"# _schema: https://example.invalid/schema.json\nname: x\n",
		},
		{
			"$schema: https://example.invalid/schema.json\nname: x\n",
			"_schema: https://example.invalid/schema.json\nname: x\n",
		},
		{
			"$schema: none\nname: x\n",
			"_schema: none\nname: x\n",
		},
		{
			`{"$schema": "https://example.invalid/schema.json"}` + "\n",
			`{"_schema": "https://example.invalid/schema.json"}` + "\n",
		},
		{
			"note: |\n  $schema: https://example.invalid/schema.json\n",
			"note: |\n  $schema: https://example.invalid/schema.json\n",
		},
		{
			"# ordinary $schema=https://example.invalid/schema.json\nname: x\n",
			"# ordinary $schema=https://example.invalid/schema.json\nname: x\n",
		},
		{
			`{"$schema": "https://example.invalid/schema.json"}` + "\ninvalid: [\n",
			`{"_schema": "https://example.invalid/schema.json"}` + "\ninvalid: [\n",
		},
		{
			"# yaml-language-server: $schema=none\nname: x\n",
			"# yaml-language-server: $schema=none\nname: x\n",
		},
	} {
		p, err := makeProjection(tc.source, "yaml")
		if err != nil {
			t.Fatal(err)
		}
		if p.text != tc.want || len(p.text) != len(tc.source) {
			t.Errorf("projection: got %q, want %q", p.text, tc.want)
		}
		if len(p.text) != len(tc.source) || position(p.text, len(p.text)) != position(tc.source, len(tc.source)) {
			t.Errorf("UTF-16 position shifted: %q", tc.source)
		}
	}
}

func TestConfiguredLocalYAMLSchemaSelectorPreserved(t *testing.T) {
	source := "# yaml-language-server: $schema=file:///project/schema.json\nname: x\n"
	p, err := makeProjection(source, "yaml", map[string][]string{
		"file:///project/schema.json": {"*.yaml"},
	})
	if err != nil || p.text != source || len(p.synthetic) != 0 {
		t.Fatalf("explicit local schema: %+v, %v", p, err)
	}
	property := "$schema: file:///project/schema.json\nname: x\n"
	p, err = makeProjection(property, "yaml", map[string][]string{
		"file:///project/schema.json": {"*.yaml"},
	})
	if err != nil || p.text != property || len(p.synthetic) != 0 {
		t.Fatalf("explicit local schema property: %+v, %v", p, err)
	}
}

func TestExactHostProjectionAndDiagnosticPolicy(t *testing.T) {
	type expected struct {
		name, host, source, projected string
		safe                          bool
		literal                       lsp.Range
		mapLiteral                    bool
		placeholder                   lsp.Range
	}
	expression := "{{{ .name }}}"
	mask := func(action string, placeholder bool) string {
		if placeholder {
			return "x" + strings.Repeat(" ", len(action)-1)
		}
		return strings.Repeat(" ", len(action))
	}
	trim := "{{{- .name -}}}"
	toYAML := "{{{ ToYAML . 2 }}}"
	ifAction, rangeAction, endAction := "{{{ if .ok }}}", "{{{ range .items }}}", "{{{ end }}}"
	cases := []expected{
		{
			name: "YAML trimming", host: "yaml",
			source:    "key: good\n" + trim + "\nnext: value\n",
			projected: "key: good\n" + mask(trim, false) + "\nnext: value\n",
			safe:      false, literal: lsp.Range{Start: lsp.Position{Line: 2}, End: lsp.Position{Line: 2, Character: 4}},
		},
		{
			name: "nested branches", host: "yaml",
			source: ifAction + "\n" + rangeAction + "\nitem: yes\n" + endAction + endAction + "\n",
			projected: mask(ifAction, false) + "\n" + mask(rangeAction, false) +
				"\nitem: yes\n" + mask(endAction, false) + mask(endAction, false) + "\n",
			safe: false, literal: lsp.Range{Start: lsp.Position{Line: 2}, End: lsp.Position{Line: 2, Character: 4}},
		},
		{
			name: "generated ToYAML structure", host: "yaml",
			source:    "key: good\n" + toYAML + "\nnext: value\n",
			projected: "key: good\n" + mask(toYAML, false) + "\nnext: value\n",
			safe:      false, literal: lsp.Range{Start: lsp.Position{Line: 2}, End: lsp.Position{Line: 2, Character: 4}},
		},
		{
			name: "YAML block scalar", host: "yaml",
			source:    "note: |\n  hello " + expression + "\n  literal\n",
			projected: "note: |\n  hello " + mask(expression, true) + "\n  literal\n",
			safe:      false, literal: lsp.Range{Start: lsp.Position{Line: 2, Character: 2}, End: lsp.Position{Line: 2, Character: 9}},
		},
		{
			name: "quoted Bash substitution", host: "bash",
			source:    "#!/bin/bash\nprintf '%s' \"pre" + expression + "post\"\necho $bad\n",
			projected: "#!/bin/bash\nprintf '%s' \"pre" + mask(expression, true) + "post\"\necho $bad\n",
			safe:      true, mapLiteral: true,
			literal:     lsp.Range{Start: lsp.Position{Line: 2, Character: 5}, End: lsp.Position{Line: 2, Character: 9}},
			placeholder: lsp.Range{Start: lsp.Position{Line: 1, Character: 0}, End: lsp.Position{Line: 1, Character: 6}},
		},
		{
			name: "single-quoted Bash substitution", host: "bash",
			source:    "echo '" + expression + "'\necho $bad\n",
			projected: "echo '" + mask(expression, true) + "'\necho $bad\n",
			safe:      true, mapLiteral: true,
			literal:     lsp.Range{Start: lsp.Position{Line: 1, Character: 5}, End: lsp.Position{Line: 1, Character: 9}},
			placeholder: lsp.Range{Start: lsp.Position{Line: 0}, End: lsp.Position{Line: 0, Character: 4}},
		},
		{
			name: "Bash heredoc", host: "bash",
			source:    "cat <<'EOF'\nvalue=" + expression + "\nEOF\necho $bad\n",
			projected: "cat <<'EOF'\nvalue=" + mask(expression, true) + "\nEOF\necho $bad\n",
			safe:      false,
			literal:   lsp.Range{Start: lsp.Position{Line: 3, Character: 5}, End: lsp.Position{Line: 3, Character: 9}},
		},
		{
			name: "Unicode CRLF", host: "yaml",
			source:    "🐧: " + expression + "\r\nbad: value\r\n",
			projected: "🐧: " + mask(expression, true) + "\r\nbad: value\r\n",
			safe:      true, mapLiteral: true,
			literal:     lsp.Range{Start: lsp.Position{Line: 1}, End: lsp.Position{Line: 1, Character: 3}},
			placeholder: lsp.Range{Start: lsp.Position{Line: 0}, End: lsp.Position{Line: 0, Character: 2}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			projected, err := makeProjection(tc.source, tc.host)
			if err != nil {
				t.Fatal(err)
			}
			if projected.text != tc.projected || projected.safe != tc.safe ||
				len(projected.text) != len(tc.source) {
				t.Fatalf("projection got %q safe=%v; want %q safe=%v", projected.text, projected.safe, tc.projected, tc.safe)
			}
			diagnostic := lsp.Diagnostic{Range: tc.literal}
			mapped, ok := projected.mapDiagnostic(diagnostic)
			if ok != tc.mapLiteral || ok && mapped.Range != tc.literal {
				t.Fatalf("literal diagnostic map: got %+v mapped=%v", mapped.Range, ok)
			}
			if tc.placeholder != (lsp.Range{}) {
				if _, ok := projected.mapDiagnostic(lsp.Diagnostic{Range: tc.placeholder}); ok {
					t.Fatal("placeholder-dependent diagnostic was mapped")
				}
			}
		})
	}
}

func TestProjectionMappingPolicy(t *testing.T) {
	for _, host := range []string{"yaml", "bash"} {
		source := "bad: [\r\nemoji: 🐧\r\nvalue: {{{ .name }}}\r\n"
		p, err := makeProjection(source, host)
		if err != nil {
			t.Fatal(err)
		}
		if p.text[:len("bad: [\r\nemoji: 🐧\r\nvalue: ")] != "bad: [\r\nemoji: 🐧\r\nvalue: " ||
			strings.Contains(p.text, "{{{") || len(p.text) != len(source) {
			t.Fatalf("projected %s: %q", host, p.text)
		}
		literal := lsp.Diagnostic{Range: lsp.Range{
			Start: lsp.Position{Line: 0, Character: 0}, End: lsp.Position{Line: 0, Character: 3},
		}}
		mapped, ok := p.mapDiagnostic(literal)
		if !ok || mapped.Range != literal.Range {
			t.Fatalf("literal %s: %+v, %v", host, mapped, ok)
		}
		onPlaceholder := lsp.Diagnostic{Range: lsp.Range{
			Start: lsp.Position{Line: 2, Character: 0}, End: lsp.Position{Line: 2, Character: 5},
		}}
		if _, ok := p.mapDiagnostic(onPlaceholder); ok {
			t.Fatalf("placeholder-dependent diagnostic mapped for %s", host)
		}
	}
}

func TestProjectionUTF16AfterMaskedMultibyte(t *testing.T) {
	source := "a: {{{ \"🐧\" }}}\nmisspelled: x\n"
	p, err := makeProjection(source, "yaml")
	if err != nil {
		t.Fatal(err)
	}
	d := lsp.Diagnostic{Range: lsp.Range{
		Start: lsp.Position{Line: 1, Character: 0}, End: lsp.Position{Line: 1, Character: 10},
	}}
	mapped, ok := p.mapDiagnostic(d)
	if !ok || mapped.Range != d.Range {
		t.Fatalf("UTF-16 projection map: %+v, %v", mapped, ok)
	}
}

func TestCRLFUnicodeLiteralDiagnosticAfterAction(t *testing.T) {
	source := "value: {{{ \"🐧\" }}}\r\n🦊bad: [\r\n"
	p, err := makeProjection(source, "yaml")
	if err != nil {
		t.Fatal(err)
	}
	d := lsp.Diagnostic{Range: lsp.Range{
		Start: lsp.Position{Line: 1, Character: 2}, End: lsp.Position{Line: 1, Character: 7},
	}}
	if mapped, ok := p.mapDiagnostic(d); !ok || mapped.Range != d.Range {
		t.Fatalf("CRLF / Unicode diagnostic: %+v, %v", mapped, ok)
	}
}

func TestMapDuplicateYAMLLiteralKey(t *testing.T) {
	p, err := makeProjection("same: one\nsame: two\nname: {{{ .name }}}\n", "yaml")
	if err != nil {
		t.Fatal(err)
	}
	d := lsp.Diagnostic{Range: lsp.Range{
		Start: lsp.Position{Line: 1, Character: 0}, End: lsp.Position{Line: 1, Character: 10},
	}}
	want := d.Range
	want.End.Character = 9
	if mapped, ok := p.mapDiagnostic(d); !ok || mapped.Range != want {
		t.Fatalf("duplicate key should map: %+v, %#v, %v", p, mapped, ok)
	}
}

func TestTrimAndControlDisableHostMapping(t *testing.T) {
	for _, source := range []string{
		"bad: [\n{{{- .name }}}", "bad: [\n{{{ if .ok }}}yes{{{ end }}}",
		"bad: [\n{{{ ToYAML . 2 }}}",
	} {
		p, err := makeProjection(source, "yaml")
		if err != nil {
			t.Fatal(err)
		}
		if p.safe {
			t.Fatalf("unsafe projection enabled: %q", source)
		}
		d := lsp.Diagnostic{Range: lsp.Range{
			Start: lsp.Position{Line: 0, Character: 0}, End: lsp.Position{Line: 0, Character: 3},
		}}
		if _, ok := p.mapDiagnostic(d); ok {
			t.Fatalf("unsafe diagnostic mapped: %q", source)
		}
	}
}
