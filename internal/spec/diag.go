package spec

import (
	"fmt"
	"strconv"
	"strings"
)

// Severity separates a finding that must stop an apply from one that must only
// be seen. There is no "info": a message nobody has to act on is noise in a
// stream whose whole value is that everything in it matters.
type Severity string

const (
	SevError   Severity = "error"
	SevWarning Severity = "warning"
)

// Diag is one finding about one file, positioned. Section 8.6 requires the
// position, and requires it to come from the file rather than from a struct: a
// message that cannot say where it applies makes the operator grep.
type Diag struct {
	File      string
	Line, Col int
	Path      string // "spec.shutdown.steps[1].signal"
	Msg       string
	Fix       string // one actionable sentence, printed indented
	Sev       Severity
}

// String renders the diagnostic the way it is printed. An error prints bare, in
// the compiler's own file:line:col shape, so an editor's error parser jumps to
// it; a warning is labelled, because an unlabelled warning in a list of errors
// is read as an error and acted on as one.
func (d Diag) String() string {
	var b strings.Builder
	b.WriteString(d.File)
	if d.Line > 0 {
		b.WriteString(":" + strconv.Itoa(d.Line))
		if d.Col > 0 {
			b.WriteString(":" + strconv.Itoa(d.Col))
		}
	}
	if b.Len() > 0 {
		b.WriteString(": ")
	}
	if d.Sev == SevWarning {
		b.WriteString("warning: ")
	}
	if d.Path != "" {
		b.WriteString(d.Path + ": ")
	}
	b.WriteString(d.Msg)
	if d.Fix != "" {
		b.WriteString("\n    " + d.Fix)
	}
	return b.String()
}

// Diags is an ordered set of findings. Order is the order they were produced,
// which for the ordered checks of 8.6 is also the order that makes sense to fix
// them in.
type Diags []Diag

// HasErrors reports whether anything in ds must stop the operation. Callers of
// Load MUST consult it: a manifest can decode cleanly and still be unusable.
func (ds Diags) HasErrors() bool {
	for _, d := range ds {
		if d.Sev == SevError {
			return true
		}
	}
	return false
}

// Errors returns only the error-severity findings.
func (ds Diags) Errors() Diags {
	var out Diags
	for _, d := range ds {
		if d.Sev == SevError {
			out = append(out, d)
		}
	}
	return out
}

// String renders one finding per line (a Fix adds its own indented line).
func (ds Diags) String() string {
	parts := make([]string, 0, len(ds))
	for _, d := range ds {
		parts = append(parts, d.String())
	}
	return strings.Join(parts, "\n")
}

// Errorf builds an error diagnostic positioned at path, falling back to the
// nearest ancestor the file actually contains. It is a method on the index
// rather than a free function so that no diagnostic can be constructed without
// having somewhere to look the position up — an unpositioned diagnostic is the
// failure mode 8.6 exists to prevent.
func (ix *NodeIndex) Errorf(path, format string, args ...any) Diag {
	return ix.diag(SevError, path, fmt.Sprintf(format, args...))
}

// Warnf builds a warning diagnostic positioned at path.
func (ix *NodeIndex) Warnf(path, format string, args ...any) Diag {
	return ix.diag(SevWarning, path, fmt.Sprintf(format, args...))
}

func (ix *NodeIndex) diag(sev Severity, path, msg string) Diag {
	d := Diag{Path: path, Msg: msg, Sev: sev}
	if ix == nil {
		return d
	}
	d.File = ix.File
	// The value position, not the key: a diagnostic almost always concerns what
	// was written, and pointing at the key hides the offending text one token to
	// the right of the caret.
	pos, _ := ix.At(path)
	d.Line, d.Col = pos.Line, pos.Col
	return d
}

// WithFix returns a copy of d carrying an actionable sentence. It is separate
// from construction so a check that has no useful fix cannot be pressured into
// inventing one.
func (d Diag) WithFix(format string, args ...any) Diag {
	d.Fix = fmt.Sprintf(format, args...)
	return d
}
