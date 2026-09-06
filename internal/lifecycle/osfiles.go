package lifecycle

import (
	"io/fs"
	"os"
)

// OSFiles is the production Files: the real filesystem, read-only.
//
// This is the ONLY file in the package that may import `os`, and noshell_test.go
// is what keeps it that way. The rule is not squeamishness about the standard
// library, it is the layering: everything gvm WRITES goes through
// internal/hostops' closed verb table, and a package that had os.WriteFile
// scattered through it would put writes outside that table one convenience at
// a time. Confining `os` to a type with three read methods and no write method
// makes the absence structural.
//
// It is a value type with no fields so that the zero value works and so that
// nothing can be configured into it -- a root, a prefix or a chroot here would
// be a second way to decide which /etc/gvm an observation came from.
type OSFiles struct{}

func (OSFiles) Stat(name string) (fs.FileInfo, error)      { return os.Stat(name) }
func (OSFiles) ReadFile(name string) ([]byte, error)       { return os.ReadFile(name) }
func (OSFiles) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(name) }
