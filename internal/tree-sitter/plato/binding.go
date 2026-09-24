package plato

// #cgo CFLAGS: -std=c11 -fPIC
// #include "tree_sitter/parser.h"
// const TSLanguage *tree_sitter_plato(void);
import "C"

import "unsafe"

func Language() unsafe.Pointer {
	return unsafe.Pointer(C.tree_sitter_plato())
}
