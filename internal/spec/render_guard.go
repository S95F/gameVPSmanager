// The template guard: a manifest is data, and this is what keeps it data.
//
// Templates are parsed with text/template and then their PARSE TREE is walked.
// A node the walk does not recognise is a refusal, not a warning, and the
// refusal happens when the manifest is LOADED — not when the field is rendered
// at 3am on a restart. The closed surface itself is documented in render.go.
//
// Parsing is not enough on its own. text/template resolves `if`, `range`,
// `with`, `template`, `printf`, `call`, `index`, `js`, `urlquery`, `len`,
// `slice`, `and`, `or` and `not` from its own builtins, which no FuncMap can
// remove — `{{if .X}}` parses cleanly against an empty FuncMap. So the builtins
// are rejected BY NAME here, and the allowlist is the only thing that admits a
// function at all.

package spec

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"text/template/parse"

	"gopkg.in/yaml.v3"
)

// ErrTemplate is the sentinel every guard refusal wraps, so a caller can tell a
// closed-grammar violation from an I/O or decode failure without matching on the
// message.
var ErrTemplate = errors.New("template outside the closed grammar")

// allowedFuncs is the complete function list with each function's arity.
//
// It is deliberately a SECOND declaration of the same set that templateFuncs in
// render.go holds. A guard that read its allowlist out of the FuncMap would
// admit whatever anyone added to the FuncMap, which is precisely the failure it
// exists to prevent; the duplication is what makes adding a function a
// two-file, reviewable act, and TestFuncMapMatchesAllowlist fails the moment the
// two disagree.
//
// The arity is here rather than left to text/template because text/template
// checks argument counts at EXECUTE time. `{{ join ";" }}` would otherwise load
// clean, pass `gvm game validate`, and fail on the restart that needed it.
var allowedFuncs = map[string]int{
	"join":      2,
	"lower":     1,
	"upper":     1,
	"quote":     1,
	"default":   2,
	"mib":       1,
	"plusBytes": 2,
}

// The variable surface, split by shape. See render.go for what each one means.
var (
	scalarVars = map[string]bool{
		"Name": true, "Game": true,
		"Install": true, "Data": true, "Home": true, "Work": true,
		"User": true, "Group": true,
		"Phase": true,
	}

	// verbVars carry operator input into a game console and exist ONLY in a verb
	// template. Elsewhere they are always empty, so `-servername {{ .Player }}`
	// would not fail — it would launch with an empty argument and generate a
	// fresh world.
	verbVars = map[string]bool{"Msg": true, "Player": true, "Reason": true, "Duration": true}

	// namedVars take exactly one author-chosen leaf. Whether the leaf is a
	// declared port/var/secret needs the manifest and is checked by validation;
	// the shape is checked here.
	namedVars = map[string]bool{"Port": true, "Var": true, "Secret": true}

	hostVars    = map[string]bool{"RamMB": true, "CPUs": true, "IP4": true}
	contentVars = map[string]bool{"Acquire": true, "Activate": true}
)

// Surface selects which variables a template may name. It is a property of
// WHERE the template lives, not of what it says: the verb operands are legal in
// spec.verbs.* and nowhere else.
type Surface uint8

const (
	SurfaceManifest Surface = iota
	SurfaceVerb
)

// SurfaceFor maps a document path to its surface. The guard walk and the
// renderer both call it, so a template cannot be checked against one surface and
// rendered against another.
func SurfaceFor(path string) Surface {
	if strings.HasPrefix(path, "spec.verbs.") || strings.HasPrefix(path, "verbs.") {
		return SurfaceVerb
	}
	return SurfaceManifest
}

// TemplateError is one refusal, positioned inside the template TEXT.
//
// Line and Col are relative to the value, not to the file: the file position
// comes from the YAML node and is attached by GuardDocument, which is the only
// place that knows both. Keeping them apart means a template checked from a
// string (a test, `gvm game lint -e`) still reports where in the string the
// problem is.
type TemplateError struct {
	Field     string
	Line, Col int
	Msg       string
	Fix       string
}

func (e *TemplateError) Error() string {
	var b strings.Builder
	if e.Field != "" {
		b.WriteString(e.Field + ": ")
	}
	if e.Line > 0 {
		b.WriteString("line " + strconv.Itoa(e.Line))
		if e.Col > 0 {
			b.WriteString(":" + strconv.Itoa(e.Col))
		}
		b.WriteString(": ")
	}
	b.WriteString(e.Msg)
	return b.String()
}

func (e *TemplateError) Unwrap() error { return ErrTemplate }

func (e *TemplateError) withFix(format string, args ...any) *TemplateError {
	e.Fix = fmt.Sprintf(format, args...)
	return e
}

// CheckTemplate parses and guards one template without rendering it. A nil
// return means the text is inside the closed grammar for that surface.
func CheckTemplate(field, text string, s Surface) *TemplateError {
	_, err := parseGuarded(field, text, s)
	return err
}

// GuardDocument rejects every template in an indexed MANIFEST that is outside
// the closed grammar, positioned at the offending scalar's own line and column.
//
// It walks the node tree and treats any scalar containing "{{" as a template,
// rather than consulting a list of which fields are templated. A hand-kept list
// silently stops guarding a field the day someone adds one, and an unguarded
// templated field is the whole hole. The cost is that prose containing "{{" —
// a `help:` string, a `doc:` — is held to the same grammar, which is a
// rewording for the author and never a running server's problem.
//
// Instance documents are NOT guarded this way: their scalars are operator data
// (a motd, a world name) where "{{" is a literal, and refusing to load an
// instance because someone typed braces in a message of the day would be gvm
// inventing a failure.
func GuardDocument(ix *NodeIndex) Diags {
	if ix == nil {
		return nil
	}
	var out Diags
	for _, path := range TemplateFields(ix) {
		n := ix.Node(path)
		te := CheckTemplate(path, n.Value, SurfaceFor(path))
		if te == nil {
			continue
		}
		msg := te.Msg
		// A block scalar's YAML position is the value's first line, so an error
		// on the template's own line 3 would otherwise point three lines high.
		// Saying which line of the value it is beats a caret that is quietly
		// wrong.
		if te.Line > 1 {
			msg += fmt.Sprintf(" (line %d of the value)", te.Line)
		}
		d := ix.Errorf(path, "%s", msg)
		if te.Fix != "" {
			d = d.WithFix("%s", te.Fix)
		}
		out = append(out, d)
	}
	return out
}

// TemplateFields returns every document path whose scalar value contains a
// template, in document order.
//
// Membership is decided by the VALUE containing "{{", never by a list of which
// fields are templated: a hand-kept list silently stops guarding a field the day
// somebody adds one, and that hole is the whole reason the guard exists. The
// walk is shared with GuardDocument so the set of guarded fields and the set
// `gvm game validate` reports a count for cannot drift.
func TemplateFields(ix *NodeIndex) []string {
	if ix == nil {
		return nil
	}
	var out []string
	for _, path := range ix.Paths() {
		n := ix.Node(path)
		if n == nil || n.Kind != yaml.ScalarNode || !strings.Contains(n.Value, "{{") {
			continue
		}
		out = append(out, path)
	}
	return out
}

// TemplateRef is one variable reference found inside a template: the root the
// closed surface names, and the author-chosen leaf where the root takes one.
//
// It exists so validation can ask whether .Var.foo is DECLARED, which the guard
// deliberately does not: the guard checks the shape of a field chain and knows
// nothing about the manifest around it. Collecting the references here rather
// than re-parsing in validate.go keeps the two readings of a template in one
// file, where they cannot drift.
type TemplateRef struct {
	Root  string // "Var", "Port", "Secret", "Content", "Host", "Name", ...
	Name  string // the leaf; empty for a scalar root such as .Install
	Field string // the third segment, only for .Content.<name>.Acquire

	Line, Col int // within the template TEXT, as TemplateError reports it
}

// String renders the reference the way it was written, for a diagnostic.
func (r TemplateRef) String() string {
	s := "." + r.Root
	if r.Name != "" {
		s += "." + r.Name
	}
	if r.Field != "" {
		s += "." + r.Field
	}
	return s
}

// TemplateRefs returns every variable reference in text, in source order.
//
// The text is guarded first, so a caller cannot use this to inspect something
// the closed grammar would refuse; a refusal comes back as the TemplateError and
// no references at all.
func TemplateRefs(field, text string, s Surface) ([]TemplateRef, *TemplateError) {
	if !strings.Contains(text, "{{") {
		return nil, nil
	}
	t, te := parseGuarded(field, text, s)
	if te != nil {
		return nil, te
	}
	var out []TemplateRef
	collectRefs(text, t.Tree.Root, &out)
	return out, nil
}

func collectRefs(text string, n parse.Node, out *[]TemplateRef) {
	switch v := n.(type) {
	case nil:
		return
	case *parse.ListNode:
		if v == nil {
			return
		}
		for _, c := range v.Nodes {
			collectRefs(text, c, out)
		}
	case *parse.ActionNode:
		collectRefs(text, v.Pipe, out)
	case *parse.PipeNode:
		if v == nil {
			return
		}
		for _, c := range v.Cmds {
			for _, a := range c.Args {
				collectRefs(text, a, out)
			}
		}
	case *parse.FieldNode:
		r := TemplateRef{Root: v.Ident[0]}
		if len(v.Ident) > 1 {
			r.Name = v.Ident[1]
		}
		if len(v.Ident) > 2 {
			r.Field = v.Ident[2]
		}
		r.Line, r.Col = textPos(text, int(v.Position()))
		*out = append(*out, r)
	}
}

// parseGuarded is the ONLY way a template is turned into something executable.
// Rendering re-runs it every time rather than trusting a cache, so a string that
// never went through the load-time walk cannot be rendered by accident.
func parseGuarded(field, text string, s Surface) (*template.Template, *TemplateError) {
	t, err := template.New(field).Funcs(templateFuncs).Option("missingkey=error").Parse(text)
	if err != nil {
		return nil, parseFailure(field, err)
	}
	if te := guardNode(field, text, t.Tree.Root, s); te != nil {
		return nil, te
	}
	// {{define}} contributes nothing to this template's own output, so the walk
	// above never sees its body — it would be unguarded text sitting in a
	// *template.Template, one ExecuteTemplate away from running. There is no
	// template namespace in a manifest, so the honest answer is to refuse it.
	for _, assoc := range t.Templates() {
		if assoc.Name() == field || assoc.Tree == nil {
			continue
		}
		te := &TemplateError{Field: field, Line: 1, Msg: fmt.Sprintf(
			"{{define %q}} is not allowed: a manifest cannot define another template", assoc.Name())}
		if assoc.Tree.Root != nil {
			te.Line, te.Col = textPos(text, int(assoc.Tree.Root.Position()))
		}
		return nil, te
	}
	return t, nil
}

// parseFailure reshapes text/template's own error. Its format is
// "template: <name>:<line>: <msg>", and the name is the document path the caller
// already prints, so leaving it in place would print the path twice in one line.
func parseFailure(field string, err error) *TemplateError {
	msg := err.Error()
	line := 1
	if prefix := "template: " + field + ":"; strings.HasPrefix(msg, prefix) {
		rest := msg[len(prefix):]
		if i := strings.IndexByte(rest, ':'); i > 0 {
			if n, convErr := strconv.Atoi(rest[:i]); convErr == nil {
				line, msg = n, strings.TrimSpace(rest[i+1:])
			}
		}
	}
	te := &TemplateError{Field: field, Line: line, Msg: msg}
	if strings.Contains(msg, "not defined") {
		te.Fix = "the complete function list is " + funcList()
	}
	return te
}

func guardNode(field, text string, n parse.Node, s Surface) *TemplateError {
	switch v := n.(type) {
	case nil:
		return nil
	case *parse.ListNode:
		if v == nil {
			return nil
		}
		for _, c := range v.Nodes {
			if te := guardNode(field, text, c, s); te != nil {
				return te
			}
		}
		return nil

	case *parse.TextNode:
		return nil

	case *parse.ActionNode:
		return guardPipe(field, text, v.Pipe, s)

	case *parse.IfNode, *parse.RangeNode, *parse.WithNode,
		*parse.BreakNode, *parse.ContinueNode:
		return posError(field, text, n, "%s is not allowed: a manifest template has no control flow", keyword(n)).
			withFix("spec.runtime.args is the only place the schema takes a `when:` guard, and it is checked at load time and shows up in the diff; a config key is a plain string, so render it unconditionally and use `default` for the empty case")

	case *parse.TemplateNode:
		// {{block}} parses to the same node: it defines a template and invokes
		// it in one breath, so both spellings land here.
		return posError(field, text, n, "{{template}}/{{block}} is not allowed: a manifest cannot include another template").
			withFix("inline the text; there is no template namespace to reference")

	default:
		return posError(field, text, n, "unsupported template construct %T", n)
	}
}

func guardPipe(field, text string, p *parse.PipeNode, s Surface) *TemplateError {
	if p == nil {
		return nil
	}
	if len(p.Decl) > 0 {
		return posError(field, text, p, "template variable assignment is not allowed").
			withFix("a manifest template has no state; write the value where it is used")
	}
	for i, cmd := range p.Cmds {
		if te := guardCommand(field, text, cmd, s, i > 0); te != nil {
			return te
		}
	}
	return nil
}

// guardCommand checks one stage. piped is true for every stage after the first,
// which is where the previous stage's value arrives as a trailing argument.
//
// A pipeline is allowed only when every stage is one of the seven functions,
// so `{{ .Var.name | upper }}` works and `{{ .Var.name | printf "%s" }}` and
// `{{ .Var.name | env }}` do not. Composition of pure, allowlisted string
// functions reaches nothing the surface does not already expose.
func guardCommand(field, text string, c *parse.CommandNode, s Surface, piped bool) *TemplateError {
	if c == nil || len(c.Args) == 0 {
		return nil
	}
	head := c.Args[0]

	switch h := head.(type) {
	case *parse.IdentifierNode:
		want, ok := allowedFuncs[h.Ident]
		if !ok {
			return posError(field, text, h, "function %q is not on the allowlist", h.Ident).
				withFix("the complete function list is %s", funcList())
		}
		got := len(c.Args) - 1
		if piped {
			got++
		}
		if got != want {
			return posError(field, text, h, "%s takes %d argument(s), got %d", h.Ident, want, got)
		}

	case *parse.FieldNode:
		if piped {
			return posError(field, text, h, "a pipeline stage must be one of the seven functions, not a field").
				withFix("the complete function list is %s", funcList())
		}
		if te := guardFieldNode(field, text, h, s); te != nil {
			return te
		}
		if len(c.Args) > 1 {
			return posError(field, text, h, "%s is a value, not a function; it takes no arguments", h.String())
		}
		return nil

	case *parse.StringNode, *parse.NumberNode, *parse.BoolNode:
		if piped {
			return posError(field, text, head, "a pipeline stage must be one of the seven functions, not a literal")
		}
		if len(c.Args) > 1 {
			return posError(field, text, head, "a literal takes no arguments")
		}
		return nil

	case *parse.PipeNode:
		if te := guardPipe(field, text, h, s); te != nil {
			return te
		}
		if len(c.Args) > 1 {
			return posError(field, text, head, "the result of a call takes no arguments")
		}
		return nil

	case *parse.DotNode:
		return posError(field, text, head, "{{.}} renders the entire variable set, including every secret").
			withFix("name the value you want, e.g. {{ .Var.server_name }}")

	case *parse.VariableNode:
		return posError(field, text, head, "template variables (%s) are not allowed", h.String())

	case *parse.ChainNode:
		return posError(field, text, head, "a chained field expression (%s) is not allowed", h.String())

	case *parse.NilNode:
		return posError(field, text, head, "nil is not a value a manifest template can use")

	default:
		return posError(field, text, head, "unsupported template construct %T", head)
	}

	for _, a := range c.Args[1:] {
		if te := guardArg(field, text, a, s); te != nil {
			return te
		}
	}
	return nil
}

func guardArg(field, text string, n parse.Node, s Surface) *TemplateError {
	switch v := n.(type) {
	case *parse.FieldNode:
		return guardFieldNode(field, text, v, s)
	case *parse.StringNode, *parse.NumberNode, *parse.BoolNode:
		return nil
	case *parse.PipeNode:
		return guardPipe(field, text, v, s)
	case *parse.IdentifierNode:
		return posError(field, text, n, "a function name (%s) cannot be used as a value", v.Ident)
	case *parse.DotNode:
		return posError(field, text, n, "{{.}} renders the entire variable set, including every secret").
			withFix("name the value you want, e.g. {{ .Var.server_name }}")
	case *parse.VariableNode:
		return posError(field, text, n, "template variables (%s) are not allowed", v.String())
	case *parse.ChainNode:
		return posError(field, text, n, "a chained field expression (%s) is not allowed", v.String())
	case *parse.NilNode:
		return posError(field, text, n, "nil is not a value a manifest template can use")
	default:
		return posError(field, text, n, "unsupported template construct %T", n)
	}
}

// guardFieldNode checks the SHAPE of a field chain against the closed variable
// list. Whether .Var.foo names a declared var needs the manifest and belongs to
// validation; whether .Var.foo.bar is even a thing does not, and catching it
// here means the message can name the whole surface.
func guardFieldNode(field, text string, n *parse.FieldNode, s Surface) *TemplateError {
	id := n.Ident
	if len(id) == 0 {
		return posError(field, text, n, "empty field reference")
	}
	root := id[0]

	switch {
	case scalarVars[root]:
		if len(id) != 1 {
			return posError(field, text, n, ".%s is a string and has no fields", root)
		}

	case verbVars[root]:
		if s != SurfaceVerb {
			return posError(field, text, n,
				".%s exists only in a verb template (spec.verbs.*); here it is always empty", root).
				withFix("verb operands carry operator input into a game console and have no value outside a verb")
		}
		if len(id) != 1 {
			return posError(field, text, n, ".%s is a string and has no fields", root)
		}

	case namedVars[root]:
		if len(id) != 2 {
			return posError(field, text, n, ".%s takes exactly one name: .%s.<name>", root, root).
				withFix("naming .%s alone would render the whole set", root)
		}

	case root == "Host":
		if len(id) != 2 || !hostVars[id[1]] {
			return posError(field, text, n, ".Host has exactly .RamMB, .CPUs and .IP4")
		}

	case root == "Content":
		if len(id) != 3 || !contentVars[id[2]] {
			return posError(field, text, n,
				".Content takes a block name and one of .Acquire or .Activate: .Content.<name>.Acquire")
		}

	default:
		return posError(field, text, n, "there is no template variable .%s", root).
			withFix("the complete variable list is %s", varList())
	}
	return nil
}

// keyword names the construct the way an author wrote it, so the message reads
// "{{if}} is not allowed" rather than "*parse.IfNode is not allowed".
func keyword(n parse.Node) string {
	switch n.(type) {
	case *parse.IfNode:
		return "{{if}}"
	case *parse.RangeNode:
		return "{{range}}"
	case *parse.WithNode:
		return "{{with}}"
	case *parse.BreakNode:
		return "{{break}}"
	case *parse.ContinueNode:
		return "{{continue}}"
	case *parse.TemplateNode:
		return "{{template}}"
	default:
		return fmt.Sprintf("%T", n)
	}
}

func posError(field, text string, n parse.Node, format string, args ...any) *TemplateError {
	line, col := textPos(text, int(n.Position()))
	return &TemplateError{
		Field: field,
		Line:  line,
		Col:   col,
		Msg:   fmt.Sprintf(format, args...),
	}
}

// textPos converts a parse.Pos byte offset into a one-based line and column.
func textPos(text string, off int) (line, col int) {
	if off < 0 {
		off = 0
	}
	if off > len(text) {
		off = len(text)
	}
	head := text[:off]
	line = 1 + strings.Count(head, "\n")
	if i := strings.LastIndexByte(head, '\n'); i >= 0 {
		return line, off - i
	}
	return line, off + 1
}

func funcList() string {
	names := make([]string, 0, len(allowedFuncs))
	for n := range allowedFuncs {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, " ")
}

func varList() string {
	return ".Name .Game .Install .Data .Home .Work .User .Group .Phase " +
		".Port.<name> .Var.<name> .Secret.<name> " +
		".Content.<name>.Acquire .Content.<name>.Activate " +
		".Host.RamMB .Host.CPUs .Host.IP4 " +
		"(.Msg .Player .Reason .Duration in verbs only)"
}

// treeTaint reports which taints a template's expansion carries.
//
// It reads the tree instead of watching the values go past, and that is EXACT
// rather than conservative: with no control flow, every action in a template is
// unconditionally expanded, so naming .Secret.x and expanding .Secret.x are the
// same statement. A runtime hook would also have to be goroutine-safe and would
// go quiet the day a value happened to be empty.
func treeTaint(n parse.Node) Taint {
	var t Taint
	switch v := n.(type) {
	case nil:
		return t
	case *parse.ListNode:
		if v == nil {
			return t
		}
		for _, c := range v.Nodes {
			t |= treeTaint(c)
		}
	case *parse.ActionNode:
		t |= pipeTaint(v.Pipe)
	case *parse.FieldNode:
		if len(v.Ident) > 0 && v.Ident[0] == "Secret" {
			t |= TaintSecret
		}
	case *parse.PipeNode:
		t |= pipeTaint(v)
	}
	return t
}

func pipeTaint(p *parse.PipeNode) Taint {
	var t Taint
	if p == nil {
		return t
	}
	for _, c := range p.Cmds {
		for _, a := range c.Args {
			t |= treeTaint(a)
		}
	}
	return t
}
