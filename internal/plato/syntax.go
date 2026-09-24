package plato

import (
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"unicode/utf16"

	"github.com/Masterminds/sprig/v3"
	lsp "go.lsp.dev/protocol"
)

var errorLine = regexp.MustCompile(`:(\d+):`)

func syntaxDiagnostics(content string, w workspace) []lsp.Diagnostic {
	if w.problem != "" {
		return []lsp.Diagnostic{diagnostic(content, 0, 0, w.problem)}
	}
	if w.left != "{{{" || w.right != "}}}" {
		return []lsp.Diagnostic{diagnostic(content, 0, 0, "Unsupported Plato delimiters: only {{{ and }}} are supported")}
	}
	funcs := template.FuncMap{}
	for name := range sprig.TxtFuncMap() {
		funcs[name] = func(...any) any { return nil }
	}
	for _, name := range []string{"PLATO", "IPofCIDR", "MKPasswd", "ToYAML", "SemverCheck", "HtpasswdBcrypt", "HtpasswdSHA", "filepath"} {
		funcs[name] = func(...any) any { return nil }
	}
	_, err := template.New("input").Delims("{{{", "}}}").Funcs(funcs).Parse(content)
	if err == nil {
		return nil
	}
	message := "Invalid Plato template syntax"
	if strings.Contains(err.Error(), "function ") && strings.Contains(err.Error(), "not defined") {
		message = "Unknown template function"
	}
	line := 1
	if match := errorLine.FindStringSubmatch(err.Error()); len(match) > 1 {
		line, _ = strconv.Atoi(match[1])
	}
	startLine := 0
	for i := 1; i < line; i++ {
		next := strings.IndexByte(content[startLine:], '\n')
		if next < 0 {
			break
		}
		startLine += next + 1
	}
	start := strings.LastIndex(content[:startLine], "{{{")
	if here := strings.Index(content[startLine:], "{{{"); here >= 0 && (start < 0 || strings.Index(content[start+3:startLine], "}}}") >= 0) {
		start = startLine + here
	}
	if start < 0 {
		start = startLine
	}
	end := len(content)
	if close := strings.Index(content[start:], "}}}"); close >= 0 {
		end = start + close + 3
	} else if next := strings.IndexByte(content[start:], '\n'); next >= 0 {
		end = start + next
	}
	return []lsp.Diagnostic{diagnostic(content, start, end, message)}
}

func diagnostic(content string, start, end int, message string) lsp.Diagnostic {
	return lsp.Diagnostic{
		Range:    lsp.Range{Start: position(content, start), End: position(content, end)},
		Severity: lsp.DiagnosticSeverityError,
		Source:   "plato-ls",
		Message:  message,
	}
}

func position(content string, offset int) lsp.Position {
	if offset > len(content) {
		offset = len(content)
	}
	p := lsp.Position{}
	for _, r := range content[:offset] {
		if r == '\n' {
			p.Line++
			p.Character = 0
		} else {
			p.Character += uint32(utf16.RuneLen(r))
		}
	}
	return p
}
