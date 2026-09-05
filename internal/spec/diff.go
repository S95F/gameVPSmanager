package spec

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// ChangeKind is what happened to one field.
type ChangeKind string

const (
	ChangeAdded    ChangeKind = "added"
	ChangeRemoved  ChangeKind = "removed"
	ChangeModified ChangeKind = "modified"
)

// Symbol is the one-character marker the diff is rendered with.
func (k ChangeKind) Symbol() string {
	switch k {
	case ChangeAdded:
		return "+"
	case ChangeRemoved:
		return "-"
	default:
		return "~"
	}
}

// Class is how dangerous a change is, in the specific way that matters.
//
// The four risk classes exist so that the entries that can break a running
// server cannot be skimmed past in a list of forty. ClassCosmetic is reserved
// for fields that are only ever read by a human — a tooltip, a description —
// and ClassOther is everything else. The distinction is load-bearing: calling a
// readiness deadline "cosmetic" because it is not argv would invite exactly the
// skim the classification exists to prevent.
type Class string

const (
	ClassArgv     Class = "argv"
	ClassDestroy  Class = "destroy_globs"
	ClassPorts    Class = "ports"
	ClassShutdown Class = "shutdown"
	ClassCosmetic Class = "cosmetic"
	ClassOther    Class = "other"
)

// FieldChange is one difference, at one document path.
type FieldChange struct {
	Path  string // "spec.shutdown.steps[1].wait", the same syntax NodeIndex uses
	Kind  ChangeKind
	From  string
	To    string
	Class Class
}

// ManifestDiff is a field-level comparison of two revisions of one game.
//
// It is computed over the PARSED structs, never over the file text. A text diff
// of two manifests is dominated by comment and ordering churn, and the one line
// that changes a live server's command line looks exactly like the forty that
// reflow a doc string.
type ManifestDiff struct {
	ID       string
	From, To ManifestPin
	FromRef  string // the file each side came from, for the header
	ToRef    string
	Changes  []FieldChange

	// Unchanged counts leaf values that compared equal, which is the "= 41 other
	// fields unchanged" line: evidence that the diff looked at everything.
	Unchanged int
}

// Diff compares two manifests field by field. A nil side is treated as an empty
// manifest so that `diff <nothing> <new>` is a description rather than a panic.
func Diff(from, to *Manifest) *ManifestDiff {
	if from == nil {
		from = &Manifest{}
	}
	if to == nil {
		to = &Manifest{}
	}
	d := &ManifestDiff{
		ID:      firstNonEmpty(to.Metadata.ID, from.Metadata.ID),
		From:    PinOf(from),
		To:      PinOf(to),
		FromRef: from.SourceRef,
		ToRef:   to.SourceRef,
	}
	w := &differ{out: d}
	w.walk("", reflect.ValueOf(*from), reflect.ValueOf(*to))
	return d
}

// Empty reports whether the two manifests are field-for-field identical.
func (d *ManifestDiff) Empty() bool { return d == nil || len(d.Changes) == 0 }

// Count returns how many changes fall in class c.
func (d *ManifestDiff) Count(c Class) int {
	n := 0
	for _, ch := range d.Changes {
		if ch.Class == c {
			n++
		}
	}
	return n
}

// RequiresRestart reports whether any change only takes effect after the unit is
// restarted. It is deliberately broader than the risk classes: a readiness
// deadline is not dangerous, but telling an operator their edit is live when the
// running process still has the old value is its own kind of wrong.
func (d *ManifestDiff) RequiresRestart() bool {
	for _, ch := range d.Changes {
		for _, p := range restartPrefixes {
			if pathHasPrefix(ch.Path, p) {
				return true
			}
		}
	}
	return false
}

// String renders the diff the way `gvm manifest upgrade --dry` prints it.
func (d *ManifestDiff) String() string {
	var b strings.Builder
	b.WriteString("  " + d.ID + "  " + d.header() + "\n")
	if len(d.Changes) == 0 {
		b.WriteString("\n  no field changes")
		if d.Unchanged > 0 {
			b.WriteString(fmt.Sprintf(" (%d fields compared)", d.Unchanged))
		}
		b.WriteString("\n")
		return b.String()
	}

	width := 20
	for _, ch := range d.Changes {
		if n := len(ch.Path); n > width && n <= 36 {
			width = n
		}
	}
	b.WriteString("\n")
	for _, ch := range d.Changes {
		b.WriteString(fmt.Sprintf("  %s %-*s  %s\n", ch.Kind.Symbol(), width, ch.Path, ch.render()))
		if ch.Class == ClassArgv {
			b.WriteString("      ^ CHANGES THE LAUNCH COMMAND OF A LIVE SERVER\n")
		}
		if ch.Class == ClassDestroy {
			b.WriteString("      ^ CHANGES WHAT A REBUILD DELETES\n")
		}
	}
	if d.Unchanged > 0 {
		b.WriteString(fmt.Sprintf("  = %d other fields unchanged\n", d.Unchanged))
	}
	b.WriteString(fmt.Sprintf("\n  %s, %s, %s, %s.\n",
		plural(d.Count(ClassArgv), "argv change", "argv changes"),
		plural(d.Count(ClassDestroy), "destroy-glob change", "destroy-glob changes"),
		plural(d.Count(ClassPorts), "port change", "port changes"),
		plural(d.Count(ClassShutdown), "shutdown change", "shutdown changes")))
	if d.RequiresRestart() {
		b.WriteString("  Requires a restart to take effect.\n")
	}
	return b.String()
}

func (d *ManifestDiff) header() string {
	switch {
	case d.From.Revision != d.To.Revision:
		return fmt.Sprintf("revision %d -> %d   (%s)", d.From.Revision, d.To.Revision, d.ToRef)
	case d.From.SHA256 != d.To.SHA256:
		// Same revision, different bytes: someone edited in place. Say so here,
		// because every other line of the diff looks like an ordinary upgrade.
		return fmt.Sprintf("revision %d, edited in place (sha256 %s -> %s)   (%s)",
			d.To.Revision, shortSHA(d.From.SHA256), shortSHA(d.To.SHA256), d.ToRef)
	default:
		return fmt.Sprintf("revision %d   (%s)", d.To.Revision, d.ToRef)
	}
}

func (c FieldChange) render() string {
	switch c.Kind {
	case ChangeAdded:
		return c.To
	case ChangeRemoved:
		return c.From
	default:
		return c.From + " -> " + c.To
	}
}

type differ struct{ out *ManifestDiff }

func (w *differ) add(path string, kind ChangeKind, from, to string) {
	w.out.Changes = append(w.out.Changes, FieldChange{
		Path: path, Kind: kind, From: from, To: to, Class: classify(path),
	})
}

// walk compares two values of the same type, emitting one change per differing
// leaf and one change per added or removed composite. Recursing into an added
// element would report a fail_marker's pattern, fatal flag and hint as three
// separate additions of a thing that was never there.
func (w *differ) walk(path string, a, b reflect.Value) {
	switch a.Kind() {
	case reflect.Pointer, reflect.Interface:
		switch {
		case a.IsNil() && b.IsNil():
			w.out.Unchanged++
		case a.IsNil():
			w.add(path, ChangeAdded, "", compact(b))
		case b.IsNil():
			w.add(path, ChangeRemoved, compact(a), "")
		default:
			w.walk(path, a.Elem(), b.Elem())
		}

	case reflect.Struct:
		for _, f := range yamlFields(a.Type()) {
			w.walk(joinPath(path, f.name), a.Field(f.index), b.Field(f.index))
		}

	case reflect.Map:
		for _, k := range unionKeys(a, b) {
			av, bv := a.MapIndex(k), b.MapIndex(k)
			p := path + "[" + keyString(k) + "]"
			switch {
			case !av.IsValid():
				w.add(p, ChangeAdded, "", compact(bv))
			case !bv.IsValid():
				w.add(p, ChangeRemoved, compact(av), "")
			default:
				w.walk(p, av, bv)
			}
		}

	case reflect.Slice, reflect.Array:
		// Positional, not keyed by name. An element inserted at the head
		// therefore reports every later index as changed — noisy, but honest,
		// and the classification of those entries is still correct, which is
		// what the operator is being asked to decide on.
		n := min(a.Len(), b.Len())
		for i := range n {
			w.walk(path+"["+strconv.Itoa(i)+"]", a.Index(i), b.Index(i))
		}
		for i := n; i < a.Len(); i++ {
			w.add(path+"["+strconv.Itoa(i)+"]", ChangeRemoved, compact(a.Index(i)), "")
		}
		for i := n; i < b.Len(); i++ {
			w.add(path+"["+strconv.Itoa(i)+"]", ChangeAdded, "", compact(b.Index(i)))
		}

	default:
		af, bf := formatLeaf(a), formatLeaf(b)
		if af == bf {
			w.out.Unchanged++
			return
		}
		w.add(path, ChangeModified, af, bf)
	}
}

func joinPath(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
}

type structField struct {
	name  string
	index int
}

// yamlFields lists the fields yaml.v3 would encode, in declaration order, so a
// diff path is a path that exists in the file.
//
// `,inline` is not handled because this schema has none; a future inline field
// would show up as a nested path rather than a flattened one, which is wrong but
// visibly wrong.
func yamlFields(t reflect.Type) []structField {
	out := make([]structField, 0, t.NumField())
	for i := range t.NumField() {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		tag := f.Tag.Get("yaml")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		out = append(out, structField{name: name, index: i})
	}
	return out
}

func unionKeys(a, b reflect.Value) []reflect.Value {
	seen := map[string]reflect.Value{}
	for _, m := range []reflect.Value{a, b} {
		if !m.IsValid() || m.IsNil() {
			continue
		}
		for _, k := range m.MapKeys() {
			seen[keyString(k)] = k
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]reflect.Value, 0, len(names))
	for _, n := range names {
		out = append(out, seen[n])
	}
	return out
}

func keyString(k reflect.Value) string {
	if k.Kind() == reflect.String {
		return k.String()
	}
	return fmt.Sprint(k.Interface())
}

var stringerType = reflect.TypeOf((*fmt.Stringer)(nil)).Elem()

// formatLeaf renders a scalar the way the operator wrote it: strings quoted, so
// a trailing space or an empty value is visible, and everything else bare.
// Duration and the other Stringer types render in their canonical spelling
// ("300s"), not as a nanosecond count nobody would recognise.
func formatLeaf(v reflect.Value) string {
	if !v.IsValid() {
		return ""
	}
	if v.Kind() != reflect.Pointer && v.Type().Implements(stringerType) {
		return v.Interface().(fmt.Stringer).String()
	}
	switch v.Kind() {
	case reflect.String:
		return strconv.Quote(v.String())
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(v.Uint(), 10)
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'g', -1, 64)
	default:
		return fmt.Sprint(v.Interface())
	}
}

// compact renders a whole value on one line, in the flow spelling of the file it
// came from: {pattern: "OutOfMemoryError", fatal: true}. Zero fields are omitted
// so that an added element shows what was written, not the schema around it.
func compact(v reflect.Value) string {
	if !v.IsValid() {
		return ""
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return "null"
		}
		return compact(v.Elem())
	case reflect.Struct:
		var parts []string
		for _, f := range yamlFields(v.Type()) {
			fv := v.Field(f.index)
			if fv.IsZero() {
				continue
			}
			parts = append(parts, f.name+": "+compact(fv))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case reflect.Map:
		var parts []string
		for _, k := range unionKeys(v, reflect.Value{}) {
			parts = append(parts, keyString(k)+": "+compact(v.MapIndex(k)))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case reflect.Slice, reflect.Array:
		parts := make([]string, 0, v.Len())
		for i := range v.Len() {
			parts = append(parts, compact(v.Index(i)))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		return formatLeaf(v)
	}
}

// classRules map a document path to its risk class. Longest prefix wins.
var classRules = []struct {
	prefix string
	class  Class
}{
	// The literal launch command and the environment it runs in.
	{"spec.runtime.exec", ClassArgv},
	{"spec.runtime.cwd", ClassArgv},
	{"spec.runtime.args", ClassArgv},
	{"spec.runtime.env", ClassArgv},
	{"spec.runtime.wrapper", ClassArgv},

	// What a rebuild is allowed to delete, and what it must never touch.
	{"spec.backup.destroy_protected", ClassDestroy},
	{"spec.backup.world_paths", ClassDestroy},

	{"spec.ports", ClassPorts},
	{"spec.port_stride", ClassPorts},

	{"spec.shutdown", ClassShutdown},

	// Read only by humans.
	{"metadata.name", ClassCosmetic},
	{"metadata.doc", ClassCosmetic},
	{"metadata.tags", ClassCosmetic},
}

// cosmeticSuffixes catch the per-item prose fields, which appear under too many
// parents to enumerate as prefixes.
var cosmeticSuffixes = []string{".help", ".note", ".why", ".hint", ".doc", ".prompt"}

// restartPrefixes are the paths whose values are baked into the unit or read by
// the process at startup.
var restartPrefixes = []string{
	"spec.runtime",
	"spec.readiness",
	"spec.shutdown",
	"spec.ports",
	"spec.port_stride",
	"spec.control",
	"spec.config_files",
	"spec.sizing",
}

func classify(path string) Class {
	best, bestLen := ClassOther, -1
	for _, r := range classRules {
		if pathHasPrefix(path, r.prefix) && len(r.prefix) > bestLen {
			best, bestLen = r.class, len(r.prefix)
		}
	}
	if best != ClassOther {
		return best
	}
	for _, s := range cosmeticSuffixes {
		if strings.HasSuffix(path, s) {
			return ClassCosmetic
		}
	}
	return ClassOther
}

// pathHasPrefix matches whole segments, so "spec.ports" matches
// "spec.ports[1].default" and never a field that merely starts with the same
// letters.
func pathHasPrefix(path, prefix string) bool {
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := path[len(prefix):]
	return rest == "" || rest[0] == '.' || rest[0] == '['
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
