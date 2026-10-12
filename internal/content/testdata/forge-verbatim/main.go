// This fixture MUST NOT COMPILE.
//
// It manufactures a "verbatim" activation line from outside the package. If
// that were possible, Verbatim() would be a flag anybody could set rather
// than a statement about where the value came from -- and the one guarantee
// an adopted server depends on, that its live Mods= list is returned
// untouched, would be forgeable by a struct literal.
package main

import (
	"fmt"

	"github.com/S95F/gameVPSmanager/internal/content"
)

func main() {
	line := content.ActivationLine{
		Key:         "Mods",
		Separator:   ";",
		verbatim:    true,
		rawRecorded: "WhateverTheCallerFeltLike",
	}
	fmt.Println(line.Value())
}
