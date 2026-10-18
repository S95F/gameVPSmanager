// Command docscli writes docs/cli-reference.md from gvm's dispatch tables.
//
// It is what `make docs-cli` runs. The reference is generated, never
// hand-edited: TestCLIReferenceIsUpToDate fails if the committed file no longer
// matches what this prints, so a command added to the tables without
// regenerating turns the build red rather than leaving the docs quietly wrong.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/S95F/gameVPSmanager/internal/cli"
)

func main() {
	out := filepath.Join("docs", "cli-reference.md")
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	if err := os.WriteFile(out, []byte(cli.GenerateCLIReference()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "docscli:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "wrote", out)
}
