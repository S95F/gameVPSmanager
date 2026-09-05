package spec

import (
	"fmt"
	"sort"
	"strings"
)

// When is the whole of the manifest's conditionality: seven predicates plus
// `not`, attached to an arg, never expressed inside a template (D-21).
//
// There is no expression language on purpose. A YAML predicate such as
// `field('versionMin') >= 42` has unspecified numeric-versus-lexicographic
// semantics against a real `versionMin=42.0.1`, and a wrong answer SILENTLY
// OMITS A MOD while the server boots normally — which would regress
// sync-mods.sh's visible `check` warning into invisible data loss.
//
// Exactly one predicate field (plus optional Not) may be set. Two is a
// validation error naming the line; the check lives with the rest of validation
// so the diagnostic can carry file:line rather than surfacing as a decode error
// with no path.
type When struct {
	VarTrue         string   `yaml:"var_true,omitempty"`
	VarSet          string   `yaml:"var_set,omitempty"` // non-empty
	VarEquals       *NameVal `yaml:"var_equals,omitempty"`
	SecretSet       string   `yaml:"secret_set,omitempty"`
	PortSet         string   `yaml:"port_set,omitempty"`
	ContentNonEmpty string   `yaml:"content_nonempty,omitempty"`
	Phase           string   `yaml:"phase,omitempty"` // install | content | normal
	Not             *When    `yaml:"not,omitempty"`
}

// NameVal is the one predicate that needs two operands.
type NameVal struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

// Predicates returns the names of the predicate fields that are set on w,
// ignoring Not. Validation uses it to enforce exactly-one and to name the
// offenders when there is more than one.
func (w *When) Predicates() []string {
	if w == nil {
		return nil
	}
	var set []string
	if w.VarTrue != "" {
		set = append(set, "var_true")
	}
	if w.VarSet != "" {
		set = append(set, "var_set")
	}
	if w.VarEquals != nil {
		set = append(set, "var_equals")
	}
	if w.SecretSet != "" {
		set = append(set, "secret_set")
	}
	if w.PortSet != "" {
		set = append(set, "port_set")
	}
	if w.ContentNonEmpty != "" {
		set = append(set, "content_nonempty")
	}
	if w.Phase != "" {
		set = append(set, "phase")
	}
	return set
}

// orderedPredicates returns the set predicates in the order the FILE wrote them
// rather than the order this struct declares them.
//
// The collision message points at the EXTRA key and names the one it collides
// with, and "extra" only means anything as "the one written second": pointing at
// the first line and naming a key three lines below it reads as a compiler bug.
func (w *When) orderedPredicates(ix *NodeIndex, path string) []string {
	preds := w.Predicates()
	if ix == nil || len(preds) < 2 {
		return preds
	}
	at := make(map[string]Pos, len(preds))
	for _, p := range preds {
		pos, _ := ix.KeyPos(path + "." + p)
		at[p] = pos
	}
	sort.SliceStable(preds, func(i, j int) bool {
		a, b := at[preds[i]], at[preds[j]]
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Col < b.Col
	})
	return preds
}

// whenFields is the complete predicate list, used in the messages that reject a
// when block, so the author is told the surface rather than left to grep.
const whenFields = "var_true var_set var_equals secret_set port_set content_nonempty phase"

// Check validates w and every nested `not`, positioned at path.
//
// Three refusals, each closing a way for a guard to be quietly wrong:
//
//   - Two predicates. Their combination is undefined — a reader would have to
//     guess whether it is an AND or the last one wins — so the second one is an
//     error naming the first.
//   - No predicate at all. A `when:` block whose only key is misspelled
//     (`var_tru:`) decodes to a zero When, and a zero When that meant "always"
//     would put an install-only `-adminpassword` on every restart's argv.
//   - An unknown phase. `phase: startup` would never match, so the arg it guards
//     silently disappears.
func (w *When) Check(ix *NodeIndex, path string) Diags {
	if w == nil {
		return nil
	}
	var out Diags

	preds := w.orderedPredicates(ix, path)
	if len(preds) > 1 {
		at := ""
		if pos, ok := ix.KeyPos(path + "." + preds[0]); ok {
			at = fmt.Sprintf(" (line %d)", pos.Line)
		}
		for _, extra := range preds[1:] {
			out = append(out, ix.Errorf(path+"."+extra,
				"when: %s collides with %s%s; exactly one predicate may be set", extra, preds[0], at).
				WithFix("split the arg in two, or nest one of them under `not:`"))
		}
	}
	if len(preds) == 0 && w.Not == nil {
		out = append(out, ix.Errorf(path, "when: has no predicate").
			WithFix("set exactly one of: "+whenFields+" (a misspelled key decodes to an empty guard that would always match)"))
	}

	if w.Phase != "" && !RenderPhase(w.Phase).Valid() {
		out = append(out, ix.Errorf(path+".phase",
			"when: %q is not a phase (want install|content|normal)", w.Phase))
	}
	if w.VarEquals != nil && w.VarEquals.Name == "" {
		out = append(out, ix.Errorf(path+".var_equals",
			"when: var_equals needs a `name`").
			WithFix("write `var_equals: {name: pz_build, value: \"42\"}`"))
	}

	return append(out, w.Not.Check(ix, path+".not")...)
}

// Eval answers whether the arg or key this guard is attached to applies.
//
// A nil When applies. An EMPTY When is an error rather than "true": Check
// refuses one at load, and reaching Eval with one means validation was skipped,
// which is not a moment to guess.
//
// `not` composes with a predicate as a conjunction — `{phase: normal, not:
// {var_true: dev_mode}}` is "a normal start, and not dev mode" — and stands
// alone as a plain negation.
func (w *When) Eval(d *RenderData) (bool, error) {
	if w == nil {
		return true, nil
	}
	if d == nil {
		return false, fmt.Errorf("when: no values bound")
	}

	preds := w.Predicates()
	if len(preds) > 1 {
		return false, fmt.Errorf("when: %d predicates are set (%s); exactly one may be",
			len(preds), strings.Join(preds, ", "))
	}
	if len(preds) == 0 && w.Not == nil {
		return false, fmt.Errorf("when: has no predicate; set one of: %s", whenFields)
	}

	ok := true
	if len(preds) == 1 {
		var err error
		if ok, err = w.evalOne(d); err != nil {
			return false, err
		}
	}
	if w.Not != nil {
		neg, err := w.Not.Eval(d)
		if err != nil {
			return false, fmt.Errorf("when.not: %w", err)
		}
		ok = ok && !neg
	}
	return ok, nil
}

func (w *When) evalOne(d *RenderData) (bool, error) {
	switch {
	case w.VarTrue != "":
		v, ok := d.Var[w.VarTrue]
		if !ok {
			return false, fmt.Errorf("when.var_true: no var named %q", w.VarTrue)
		}
		return parseWhenBool(w.VarTrue, v)

	case w.VarSet != "":
		// Absence is a legitimate answer to "is it set", so a missing var is
		// false here and an error only in var_true, where the value is the
		// question.
		return d.Var[w.VarSet] != "", nil

	case w.VarEquals != nil:
		if w.VarEquals.Name == "" {
			return false, fmt.Errorf("when.var_equals: needs a name")
		}
		return d.Var[w.VarEquals.Name] == w.VarEquals.Value, nil

	case w.SecretSet != "":
		return d.Secret[w.SecretSet] != "", nil

	case w.PortSet != "":
		// A materialised port is never 0: the allocator writes a real number or
		// the port is absent, so zero means "declared but never allocated".
		return d.Port[w.PortSet] != 0, nil

	case w.ContentNonEmpty != "":
		return !d.Content[w.ContentNonEmpty].Empty(), nil

	case w.Phase != "":
		if !RenderPhase(w.Phase).Valid() {
			return false, fmt.Errorf("when.phase: %q is not a phase (want install|content|normal)", w.Phase)
		}
		return d.Phase == RenderPhase(w.Phase), nil
	}
	return false, fmt.Errorf("when: has no predicate; set one of: %s", whenFields)
}

// parseWhenBool reads a bool var value from the closed set the dashboard's bool
// fields and the shipped manifests write.
//
// An unrecognised value is an ERROR, never false. Treating `Public: "yes please"`
// as false would drop the argument it guards and the server would start
// perfectly — the invisible failure D-21 exists to prevent.
func parseWhenBool(name, v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off", "":
		return false, nil
	}
	return false, fmt.Errorf("when.var_true: var %q is %q, which is not a boolean "+
		"(want true|false|1|0|yes|no|on|off)", name, v)
}
