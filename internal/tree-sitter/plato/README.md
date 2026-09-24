# Vendored Plato parser

`parser.c` and `tree_sitter/parser.h` are generated artifacts copied from the
workspace's `tree-sitter-plato` grammar fork (Tree-sitter generator v0.25.8).
`binding.go` exposes the generated `tree_sitter_plato` symbol without requiring
the grammar checkout during a standalone Go build. The original parser's MIT
license and attribution are preserved in `LICENSE`.

SHA-256 of this exact snapshot:

- `parser.c`: `bac0b03dbecaf1040239e7512655036940b796985579f1fec32d55a62460145b`
- `tree_sitter/parser.h`: `180b893c8734778fd32f372dfbc27bd6ad1cd2221f26150b31256ff6716320d2`

Regenerate parser artifacts in the grammar project first, then deliberately
update these generated files, checksums, and parser compatibility tests together.
The Go runtime is `github.com/tree-sitter/go-tree-sitter v0.25.0`.
