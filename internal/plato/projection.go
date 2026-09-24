package plato

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	lsp "go.lsp.dev/protocol"
	"gopkg.in/yaml.v3"
)

type projection struct {
	source, text string
	literals     []span
	synthetic    []span
	safe         bool
}

func makeProjection(source, host string, schemas ...map[string][]string) (projection, error) {
	literals, unstable, err := templateRegions(source)
	if err != nil {
		return projection{}, err
	}
	p := projection{source: source, literals: literals, safe: !unstable}
	output := []byte(source)
	last := 0
	for _, literal := range append(append([]span{}, literals...), span{len(source), len(source)}) {
		if literal.start > last {
			action := source[last:literal.start]
			p.synthetic = append(p.synthetic, span{last, literal.start})
			if strings.Contains(action, "{{{-") || strings.Contains(action, "-}}}") ||
				strings.Contains(action, "ToYAML") || strings.Contains(action, "{{{/*") {
				p.safe = false
			}
			for i := last; i < literal.start; i++ {
				if output[i] != '\r' && output[i] != '\n' {
					output[i] = ' '
				}
			}
			if p.safe && strings.HasPrefix(action, "{{{") {
				for i := last; i < literal.start; i++ {
					if output[i] == ' ' && source[i] != ' ' && source[i] != '\t' {
						output[i] = 'x'
						break
					}
				}
			}
		}
		last = literal.end
	}
	if host != "yaml" && host != "bash" {
		return projection{}, fmt.Errorf("unsupported host language")
	}
	if host == "yaml" {
		var allowed map[string][]string
		if len(schemas) > 0 {
			allowed = schemas[0]
		}
		maskUnconfiguredSchemaSelectors(output, &p, allowed)
	}
	if len(p.synthetic) > 0 && (host == "yaml" && yamlBlock.MatchString(source) ||
		host == "bash" && strings.Contains(source, "<<")) {
		p.safe = false
	}
	p.text = string(output)
	return p, nil
}

var (
	yamlModeline  = regexp.MustCompile(`^#\s+(?:yaml-language-server\s*:|\$schema:).*$`)
	schemaValue   = regexp.MustCompile(`\$schema(?:=|:[^\S\r\n]*)(\S+)`)
	schemaKey     = regexp.MustCompile(`^\s*['"]?\$schema['"]?\s*:\s*(\S+)`)
	schemaFlowKey = regexp.MustCompile(`[{,]\s*['"]?\$schema['"]?\s*:\s*(\S+)`)
	yamlBlock     = regexp.MustCompile(`(?m)^[ \t]*[^#\r\n]+:[ \t]*[|>][+-1-9]{0,2}[ \t]*(?:#.*)?$`)
)

func maskUnconfiguredSchemaSelectors(output []byte, p *projection, allowed map[string][]string) {
	lineStarts := []int{0}
	for i, b := range output {
		if b == '\n' {
			lineStarts = append(lineStarts, i+1)
		}
	}
	mask := func(at int) {
		output[at] = '_'
		p.synthetic = append(p.synthetic, span{at, at + 1})
	}
	for _, start := range lineStarts {
		end := len(output)
		if newline := bytes.IndexByte(output[start:], '\n'); newline >= 0 {
			end = start + newline
		}
		line := string(output[start:end])
		if yamlModeline.MatchString(line) {
			if match := schemaValue.FindStringSubmatchIndex(line); match != nil {
				value := strings.Trim(line[match[2]:match[3]], `"'},`)
				if value != "none" && !safeSchemaSelector(value, allowed) {
					at := start + match[0] + strings.Index(line[match[0]:match[1]], "$schema")
					mask(at)
				}
			}
		}
	}
	decoder := yaml.NewDecoder(bytes.NewReader(output))
	for {
		var document yaml.Node
		err := decoder.Decode(&document)
		if err == io.EOF {
			return
		}
		if err != nil {
			for _, start := range lineStarts {
				end := len(output)
				if newline := bytes.IndexByte(output[start:], '\n'); newline >= 0 {
					end = start + newline
				}
				line := string(output[start:end])
				for _, pattern := range []*regexp.Regexp{schemaKey, schemaFlowKey} {
					for _, match := range pattern.FindAllStringSubmatchIndex(line, -1) {
						if safeSchemaSelector(strings.Trim(line[match[2]:match[3]], `"'},`), allowed) {
							continue
						}
						at := start + match[0] + strings.Index(line[match[0]:match[1]], "$schema")
						if output[at] == '$' {
							mask(at)
						}
					}
				}
			}
			return
		}
		if len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode {
			continue
		}
		keys := document.Content[0].Content
		for i := 0; i+1 < len(keys); i += 2 {
			key, value := keys[i], keys[i+1]
			if key.Value != "$schema" || safeSchemaSelector(value.Value, allowed) {
				continue
			}
			start := lineStarts[key.Line-1]
			end := len(output)
			if newline := bytes.IndexByte(output[start:], '\n'); newline >= 0 {
				end = start + newline
			}
			if at := bytes.Index(output[start:end], []byte("$schema")); at >= 0 {
				mask(start + at)
			}
		}
	}
}

func safeSchemaSelector(value string, allowed map[string][]string) bool {
	u, err := url.Parse(value)
	if err != nil || !strings.HasPrefix(value, "file:///") ||
		u.Scheme != "file" || u.Host != "" || !filepath.IsAbs(u.Path) {
		return false
	}
	_, configured := allowed[value]
	return configured
}

func byteOffset(content string, pos lsp.Position) (int, bool) {
	line, col := uint32(0), uint32(0)
	for i, r := range content {
		if line == pos.Line && col == pos.Character {
			return i, true
		}
		if r == '\n' {
			line++
			col = 0
		} else if line == pos.Line {
			if r > 0xFFFF {
				col += 2
			} else {
				col++
			}
		}
		if line > pos.Line || line == pos.Line && col > pos.Character {
			return 0, false
		}
	}
	return len(content), line == pos.Line && col == pos.Character
}

func (p projection) mapDiagnostic(d lsp.Diagnostic) (lsp.Diagnostic, bool) {
	if !p.safe {
		return d, false
	}
	start, validStart := byteOffset(p.text, d.Range.Start)
	end, validEnd := byteOffset(p.text, d.Range.End)
	if !validEnd {
		end, validEnd = clampEndOfLine(p.text, d.Range.End)
	}
	if !validStart || !validEnd || start > end {
		return d, false
	}
	found := false
	for _, literal := range p.literals {
		if start >= literal.start && end <= literal.end && end > start {
			found = true
			break
		}
	}
	if !found {
		return d, false
	}
	for _, synthetic := range p.synthetic {
		if position(p.text, synthetic.start).Line <= d.Range.End.Line &&
			position(p.text, synthetic.end).Line >= d.Range.Start.Line {
			return d, false
		}
	}
	d.Range = lsp.Range{Start: position(p.source, start), End: position(p.source, end)}
	return d, true
}

func clampEndOfLine(text string, pos lsp.Position) (int, bool) {
	start := 0
	for line := uint32(0); line <= pos.Line; line++ {
		end := strings.IndexByte(text[start:], '\n')
		if end < 0 {
			end = len(text) - start
		}
		if line == pos.Line {
			last := start + end
			if pos.Character >= position(text[start:last], end).Character {
				return last, true
			}
			return 0, false
		}
		if start+end >= len(text) {
			break
		}
		start += end + 1
	}
	return 0, false
}
