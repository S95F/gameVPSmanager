// The two commands that EXPLAIN: `gvm content diff` and `gvm content why`.
//
// Section 12.9's three-way view is the shape, and the third way is the whole
// point. A two-way diff cannot distinguish "the operator changed their mind"
// from "the disk drifted": declared-vs-locked says what somebody asked for and
// has not applied, locked-vs-observed says what the box is actually doing, and
// only both together tell an operator which of the two to fix. Project Zomboid
// makes that concrete -- it rewrites its own ini on shutdown, so the file after
// a run is evidence about what the SERVER believed, not about what gvm wrote.
//
// `why` is the reverse map. "Why is this mod not loading" is the single most
// common Project Zomboid support question in the legacy README, and the answer
// is almost always one of four things: the id is enabled and the item never
// downloaded, the id was DROPPED as a lower-scoring build variant, the item
// carries no build marker so everything inside it was enabled, or the id is
// live in Mods= and nothing on this box has ever heard of it. All four are
// recorded -- in the lock's evidence and in the observed reading -- so the
// answer is a lookup rather than an afternoon.

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/S95F/gameVPSmanager/internal/content"
)

func runContentDiff(ctx context.Context, env *Env, args []string) error {
	fs, opt := contentFlags(env, "diff", "<instance> [--against declared|lock|observed]")
	against := fs.String("against", "", "show one section only: declared, lock or observed")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagErr(err)
	}
	switch strings.TrimSpace(*against) {
	case "", "declared", "lock", "observed":
	default:
		fs.Usage()
		return fmt.Errorf("--against %q: want declared, lock or observed", *against)
	}
	v, err := openContent(ctx, env, opt, pos)
	if err != nil {
		return err
	}

	p, err := v.plan(ctx)
	if err != nil {
		return err
	}
	d := newContentDiff(v, p)
	if opt.JSON {
		return writeJSON(env.Stdout, d)
	}
	d.print(env.Stdout, v, p, strings.TrimSpace(*against))
	return nil
}

// contentDiff is the three-way comparison, as data.
//
// It is computed in one pass and rendered in another so that --json and the
// text output cannot disagree, which is the same reason `gvm status` projects
// one Observation: two renderers over two computations is two stories about
// one server.
type contentDiff struct {
	Instance string `json:"instance"`
	Block    string `json:"block"`

	DeclaredCount int `json:"declared"`
	LockedCount   int `json:"locked"`
	LockedIDs     int `json:"locked_activation_ids"`
	ObservedItems int `json:"observed_items"`
	ObservedIDs   int `json:"observed_activation_ids"`

	// DeclaredVsLocked is intent against recorded intent: what somebody asked
	// for and has not applied yet.
	DeclaredVsLocked []contentDiffRow `json:"declared_vs_locked"`
	// LockedVsObserved is recorded intent against the box: drift.
	LockedVsObserved []contentDiffRow `json:"locked_vs_observed"`

	Impact string `json:"impact"`
	Boot   bool   `json:"boot"`
	// ConfigAbsent is "the game has not generated its config yet", which is
	// not an empty mod list and must not be reported as one.
	ConfigAbsent bool     `json:"config_absent,omitempty"`
	Warnings     []string `json:"warnings,omitempty"`
}

// contentDiffRow is one finding. Sigil is the character the text output prints
// and the word the JSON carries, so a dashboard does not have to parse "+".
type contentDiffRow struct {
	Sigil  string            `json:"sigil"`
	ID     content.AcquireID `json:"id,omitempty"`
	Label  string            `json:"label,omitempty"`
	What   string            `json:"what"`
	Detail []string          `json:"detail,omitempty"`
}

const (
	sigilAdded     = "+"
	sigilRemoved   = "-"
	sigilChanged   = "~"
	sigilSame      = "="
	sigilAttention = "!"
)

func newContentDiff(v *contentView, p *content.Plan) *contentDiff {
	sl, _ := v.sourceLock()
	obs := p.Observed

	d := &contentDiff{
		Instance:      v.Name,
		Block:         v.Schema.Block,
		DeclaredCount: len(v.Refs),
		Impact:        p.Impact().String(),
		Boot:          p.Boot,
		Warnings:      p.Warnings,
	}
	if sl != nil {
		d.LockedCount = len(sl.Entries)
		d.LockedIDs = len(sl.ActivationIDs())
	}
	if obs != nil {
		d.ObservedItems = len(obs.Items)
		d.ObservedIDs = len(obs.Activate)
		// Both states, because the field means "there is no live config to
		// compare against" and there are two ways for that to be true: nobody
		// read it, or somebody looked and it is not there. It used to be
		// `!obs.ConfigRead` alone, which the CLI could never make false --
		// so the field was omitempty-omitted from every payload and a
		// dashboard read false for a config that does not exist.
		d.ConfigAbsent = !obs.ConfigRead || obs.ConfigAbsent
	}

	d.DeclaredVsLocked = declaredVsLocked(v, p, sl)
	d.LockedVsObserved = lockedVsObserved(v, p, sl, obs)
	return d
}

// declaredVsLocked compares the operator's list against the recorded one.
func declaredVsLocked(v *contentView, p *content.Plan, sl *content.SourceLock) []contentDiffRow {
	if sl == nil {
		return []contentDiffRow{{
			Sigil: sigilAttention,
			What: fmt.Sprintf("nothing is recorded as intent yet, so all %d declared item(s) "+
				"would be acquired", len(v.Refs)),
		}}
	}

	violations := make(map[content.AcquireID]content.OrderViolation)
	for _, vi := range content.CheckOrderPlan(p) {
		violations[vi.Dependent] = vi
	}

	var rows []contentDiffRow
	declared := make(map[content.AcquireID]bool, len(v.Refs))
	for i, ref := range v.Refs {
		id := content.RefID(ref)
		declared[id] = true
		e, ok := sl.Entry(id)
		if !ok {
			rows = append(rows, contentDiffRow{
				Sigil: sigilAdded, ID: id, Label: ref.Note,
				What: "declared, never acquired",
			})
			continue
		}
		if e.Ordinal != i {
			rows = append(rows, contentDiffRow{
				Sigil: sigilChanged, ID: id, Label: ref.Note,
				What:   fmt.Sprintf("ordinal %d -> %d", e.Ordinal, i),
				Detail: requiresDetail(ref, violations[id], v.Name),
			})
		}
	}
	for _, e := range sl.Entries {
		if declared[e.Acquire] {
			continue
		}
		rows = append(rows, contentDiffRow{
			Sigil: sigilRemoved, ID: e.Acquire, Label: e.Note,
			What: "locked, no longer declared",
		})
	}
	sortDiffRows(rows)
	if len(rows) == 0 {
		rows = append(rows, contentDiffRow{
			Sigil: sigilSame,
			What:  "the declared list and the lock agree, item for item and in order",
		})
	}
	return rows
}

// requiresDetail renders a moved item's declared dependencies and whether the
// new position still honours them.
//
// It is on the MOVE row deliberately: the reason to care that something slid
// from ordinal 33 to 28 is almost always a requires: edge, and an operator
// reading "ordinal 33 -> 28" with no second line has to go and check by hand.
func requiresDetail(ref content.Ref, v content.OrderViolation, inst string) []string {
	reqs := content.RefRequires(ref)
	if len(reqs) == 0 {
		return nil
	}
	if v.Kind != "" {
		return []string{fmt.Sprintf("requires %s — VIOLATED: %s", joinAcquireIDs(reqs),
			v.Error()), "fix: " + v.Fix(inst)}
	}
	return []string{fmt.Sprintf("requires %s, satisfied", joinAcquireIDs(reqs))}
}

// lockedVsObserved compares the recorded intent against what the box is doing.
func lockedVsObserved(v *contentView, p *content.Plan, sl *content.SourceLock,
	obs *content.Observed) []contentDiffRow {

	if obs == nil {
		return nil
	}
	// Either no reading at all, or a reading that found no file. The second
	// case is what the CLI actually produces and what this guard could never
	// see: every path here hands Observe a non-nil Live, so ConfigRead was
	// unconditionally true and this early return was dead code -- while the
	// section below emitted a row per locked activation id saying the live
	// config does not enable it, about a file that does not exist.
	if !obs.ConfigRead || obs.ConfigAbsent {
		return []contentDiffRow{{
			Sigil: sigilAttention,
			What: fmt.Sprintf("the server has not generated its config yet, so there is no live "+
				"%s to compare against. That is NOT an empty mod list", v.Schema.Activation.Key),
		}}
	}

	live := make(map[content.ActivationID]bool, len(obs.Activate))
	for _, a := range obs.Activate {
		live[a] = true
	}

	var rows []contentDiffRow
	// Every activation id the lock records has to be live in the activation
	// key, and the ones that are not are named. "all present" as a single line
	// is the answer on a healthy box and is worth printing: a diff that says
	// nothing about the half that is fine reads as a diff that did not look.
	present, missing := 0, map[content.AcquireID][]string{}
	var order []content.AcquireID
	if sl != nil {
		for _, e := range sl.Entries {
			for _, a := range e.ActivationIDs {
				if live[a] {
					present++
					continue
				}
				if _, seen := missing[e.Acquire]; !seen {
					order = append(order, e.Acquire)
				}
				missing[e.Acquire] = append(missing[e.Acquire], a.String())
			}
		}
	}
	if len(missing) == 0 && present > 0 {
		rows = append(rows, contentDiffRow{
			Sigil: sigilSame,
			What: fmt.Sprintf("all %d activation id(s) present in %s", present,
				v.Schema.Activation.Key),
		})
	}
	for _, id := range order {
		rows = append(rows, contentDiffRow{
			Sigil: sigilAttention, ID: id,
			What: fmt.Sprintf("%s does not enable %s", v.Schema.Activation.Key,
				strings.Join(missing[id], ", ")),
		})
	}

	// The ambiguous items, with their evidence. Enabled WITH A WARNING and
	// never refused: section 12.5's rule is that evidence never drops an item,
	// so this row exists to be read, not to be acted on by gvm.
	for _, e := range ambiguousEntries(sl, obs) {
		rows = append(rows, contentDiffRow{
			Sigil: sigilAttention, ID: e.id, Label: e.note,
			What: "ambiguous: " + strings.Join(e.ids, ", "),
			Detail: []string{
				"evidence: " + strings.Join(e.evidence, ", "),
				"all of them enabled, unchanged from the bash suite",
			},
		})
	}

	// Not materialized: the bytes are not on disk. On a server_boot source
	// this is what an acquisition boot is for, and the id stays listed so the
	// server retries it.
	for _, e := range p.Entries {
		if e.Materialized {
			continue
		}
		rows = append(rows, contentDiffRow{
			Sigil: sigilAttention, ID: e.AcquireID(), Label: e.Ref.Note,
			What: "not on disk; it stays listed in " + v.Schema.Acquisition.Key +
				" so the server retries it",
		})
	}

	// The "WorkshopItems= has 32 entries and Mods= has 33" discovery finding.
	if len(obs.Unlisted) > 0 {
		rows = append(rows, contentDiffRow{
			Sigil: sigilAttention,
			What: fmt.Sprintf("%d id(s) live in %s that no item on this box accounts for: %s",
				len(obs.Unlisted), v.Schema.Activation.Key, joinActivationIDs(obs.Unlisted)),
			Detail: []string{fmt.Sprintf("gvm content why %s <id>   explains one", v.Name)},
		})
	}
	return rows
}

// ambiguousItem is one flagged item, taken from the lock when there is one and
// from the observed reading otherwise.
//
// The lock first, because its evidence includes the tags the sync consulted
// and a local reading has only the files. Adoption has no lock yet, which is
// exactly when the observed half has to answer.
type ambiguousItem struct {
	id       content.AcquireID
	note     string
	ids      []string
	evidence []string
}

func ambiguousEntries(sl *content.SourceLock, obs *content.Observed) []ambiguousItem {
	var out []ambiguousItem
	seen := map[content.AcquireID]bool{}
	if sl != nil {
		for _, e := range sl.Entries {
			if !e.Ambiguous {
				continue
			}
			seen[e.Acquire] = true
			out = append(out, ambiguousItem{
				id: e.Acquire, note: e.Note,
				ids: activationStrings(e.ActivationIDs), evidence: e.Evidence,
			})
		}
	}
	for _, it := range obs.Items {
		if !it.Ambiguous || seen[it.ID] {
			continue
		}
		out = append(out, ambiguousItem{
			id: it.ID, ids: activationStrings(it.Activation), evidence: it.Evidence,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

func activationStrings(ids []content.ActivationID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}

func sortDiffRows(rows []contentDiffRow) {
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
}

func (d *contentDiff) print(w io.Writer, v *contentView, p *content.Plan, only string) {
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  declared\t%s\t%s\n", v.Paths.Spec, plural(d.DeclaredCount, "item"))
	if d.LockedCount == 0 && v.Lock == nil {
		fmt.Fprintf(tw, "  locked\t%s\tnot written yet\n", v.Paths.ContentLock)
	} else {
		fmt.Fprintf(tw, "  locked\t%s\t%s, %s\n", v.Paths.ContentLock,
			plural(d.LockedCount, "item"), plural(d.LockedIDs, "activation id"))
	}
	fmt.Fprintf(tw, "  observed\tlive config + %s\t%s, %s\n",
		plural(len(v.Roots), "workshop root"), plural(d.ObservedItems, "item"),
		plural(d.ObservedIDs, "activation id"))
	_ = tw.Flush()

	if only == "" || only == "lock" || only == "declared" {
		fmt.Fprint(w, "\n  DECLARED vs LOCKED\n")
		printDiffRows(w, d.DeclaredVsLocked)
	}
	if only == "" || only == "observed" {
		fmt.Fprint(w, "\n  LOCKED vs OBSERVED\n")
		printDiffRows(w, d.LockedVsObserved)
	}
	v.printRootNotes(w)
	if v.LiveAbsent {
		// The distinction the whole observed half turns on: a config the game
		// has not written yet is not an empty mod list, and only one of the two
		// is a reason to write anything.
		fmt.Fprintf(w, "\n  ! the server has not generated its own config yet, so the live\n"+
			"    %s and %s values are absent rather than empty\n",
			v.Schema.Acquisition.Key, v.Schema.Activation.Key)
	}

	if len(p.Unaccounted) > 0 {
		fmt.Fprintf(w, "\n  REFUSING: %d of %d declared item(s) could not be accounted for\n",
			len(p.Unaccounted), p.Declared)
		for _, u := range p.Unaccounted {
			fmt.Fprintf(w, "    ! %s\n", u.String())
		}
	}

	fmt.Fprintf(w, "\n  impact: %s", d.Impact)
	if d.Boot {
		n := len(p.Unmaterialized())
		fmt.Fprintf(w, "   %s %s an acquisition boot (budget %s)", plural(n, "item"),
			needs(n), shortDuration(v.Schema.Acquisition.BootTimeout))
	}
	fmt.Fprintln(w)
	// A diff that is refusing (unaccounted items) prints no apply line at all:
	// there is nothing to apply, and this build cannot apply it anyway. When the
	// plan is clean, the line says plainly that applying is not in this build
	// and points at the read-only dry run (U3).
	if len(p.Unaccounted) == 0 {
		fmt.Fprintf(w, "  applying mod changes is not available in this build; "+
			"gvm content sync %s --dry-run shows what it would do\n", v.Name)
	}
	for _, warn := range d.Warnings {
		fmt.Fprintf(w, "\n  note: %s\n", warn)
	}
}

func printDiffRows(w io.Writer, rows []contentDiffRow) {
	if len(rows) == 0 {
		fmt.Fprint(w, "    (nothing to compare)\n")
		return
	}
	for _, r := range rows {
		head := r.What
		if r.Label != "" {
			head = r.Label + "   " + r.What
		}
		if r.ID != "" {
			fmt.Fprintf(w, "    %s %s  %s\n", r.Sigil, r.ID, head)
		} else {
			fmt.Fprintf(w, "    %s %s\n", r.Sigil, head)
		}
		for _, line := range r.Detail {
			fmt.Fprintf(w, "                  %s\n", line)
		}
	}
}

// ---------------------------------------------------------------------------
// why

func runContentWhy(ctx context.Context, env *Env, args []string) error {
	fs, opt := contentFlags(env, "why", "<instance> <activation-id>")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagErr(err)
	}
	if len(pos) != 2 {
		fs.Usage()
		return errors.New("name an instance and one activation id (the mod.info id=, " +
			"not the numeric workshop id)")
	}
	v, err := openContent(ctx, env, opt, pos[:1])
	if err != nil {
		return err
	}
	want := content.ActivationID(strings.TrimSpace(pos[1]))

	obs, err := v.observe(ctx)
	if err != nil {
		return err
	}
	ans := explainActivation(v, obs, want)
	if opt.JSON {
		return writeJSON(env.Stdout, ans)
	}
	printWhy(env.Stdout, v, ans)
	if !ans.Found {
		// Non-zero, because "nothing on this box has ever heard of that id" is
		// an answer a script should be able to act on. An operator who typed
		// the numeric workshop id instead of the mod.info id is the common
		// case, and the message says so.
		return fmt.Errorf("%s is not an activation id this instance knows", want)
	}
	return nil
}

// whyAnswer is `gvm content why`'s finding.
type whyAnswer struct {
	ActivationID content.ActivationID `json:"activation_id"`
	Found        bool                 `json:"found"`
	// Enabled is "this id is in the list the game loads". An id can be known
	// and NOT enabled, which is the answer to the most common question: it was
	// dropped as a lower-scoring build variant.
	Enabled   bool              `json:"enabled"`
	AcquireID content.AcquireID `json:"acquire_id,omitempty"`
	Title     string            `json:"title,omitempty"`
	Note      string            `json:"note,omitempty"`
	Ordinal   int               `json:"ordinal"`
	Status    string            `json:"status,omitempty"`
	Why       string            `json:"why,omitempty"`
	Siblings  []string          `json:"siblings,omitempty"`
	Dropped   []string          `json:"dropped,omitempty"`
	Evidence  []string          `json:"evidence,omitempty"`
	// LiveInConfig is "the activation key currently names it", read off the
	// file rather than inferred from the lock: Project Zomboid rewrites its
	// ini on shutdown, so the two legitimately disagree.
	LiveInConfig bool   `json:"live_in_config"`
	Source       string `json:"source"`
	Artifact     string `json:"artifact,omitempty"`
}

// explainActivation answers in four shapes, in the order an operator's
// question narrows.
//
// The lock is consulted first because it is INTENT and carries the evidence
// the sync recorded, including the Workshop tags a local reading cannot see.
// The observed reading answers for an adopted instance that has no lock yet,
// and for an id that is live in the config and belongs to nothing.
func explainActivation(v *contentView, obs *content.Observed,
	want content.ActivationID) *whyAnswer {

	ans := &whyAnswer{ActivationID: want, Source: string(v.Schema.Source), Ordinal: -1}
	for _, a := range obs.Activate {
		if a == want {
			ans.LiveInConfig = true
			break
		}
	}

	sl, _ := v.sourceLock()
	if sl != nil {
		if e, ok := sl.Owning(want); ok {
			fillFromLockEntry(ans, e, want, true)
			return ans
		}
		for _, e := range sl.Entries {
			for _, d := range e.Dropped {
				if d != want {
					continue
				}
				fillFromLockEntry(ans, e, want, false)
				return ans
			}
		}
	}

	for _, it := range obs.Items {
		for _, a := range it.Activation {
			if a != want {
				continue
			}
			fillFromObserved(ans, it, want, true)
			return ans
		}
		for _, d := range it.Dropped {
			if d != want {
				continue
			}
			fillFromObserved(ans, it, want, false)
			return ans
		}
	}
	return ans
}

func fillFromLockEntry(ans *whyAnswer, e content.LockEntry, want content.ActivationID,
	enabled bool) {

	ans.Found = true
	ans.Enabled = enabled
	ans.AcquireID = e.Acquire
	ans.Note = e.Note
	ans.Ordinal = e.Ordinal
	ans.Evidence = e.Evidence
	ans.Dropped = activationStrings(e.Dropped)
	ans.Siblings = siblingsOf(e.ActivationIDs, want)
	if e.Upstream != nil {
		ans.Title = e.Upstream.Title
	}
	if e.Artifact != nil {
		ans.Artifact = e.Artifact.Path
	}
	switch {
	case !enabled:
		ans.Status = "dropped"
		ans.Why = whyDropped
	case e.Ambiguous:
		ans.Status = "check"
		ans.Why = whyCheck
	case len(e.ActivationIDs) > 1:
		ans.Status = "multi"
		ans.Why = "enabled with the other mods in the same item; one workshop item " +
			"legitimately contains several"
	default:
		ans.Status = "ok"
		ans.Why = whyOK
	}
}

func fillFromObserved(ans *whyAnswer, it content.ObservedItem, want content.ActivationID,
	enabled bool) {

	ans.Found = true
	ans.Enabled = enabled
	ans.AcquireID = it.ID
	ans.Ordinal = -1
	ans.Evidence = it.Evidence
	ans.Dropped = activationStrings(it.Dropped)
	ans.Siblings = siblingsOf(it.Activation, want)
	ans.Artifact = it.Path
	switch {
	case !enabled:
		ans.Status = "dropped"
		ans.Why = whyDropped
	case it.Ambiguous:
		ans.Status = "check"
		ans.Why = whyCheck
	default:
		ans.Status = "ok"
		ans.Why = whyOK
	}
}

// The four sentences `why` answers with, declared once so the lock path and
// the observed path cannot word the same finding differently.
const (
	whyOK      = "enabled because the item's own files identify it as this build's variant"
	whyCheck   = "enabled because the item carries no build marker, so every id inside it was kept"
	whyDropped = "NOT enabled: it was dropped as a lower-scoring build variant of this item"
)

func siblingsOf(ids []content.ActivationID, want content.ActivationID) []string {
	var out []string
	for _, id := range ids {
		if id != want {
			out = append(out, id.String())
		}
	}
	return out
}

func printWhy(w io.Writer, v *contentView, ans *whyAnswer) {
	fmt.Fprintf(w, "\n  %s\n", ans.ActivationID)
	if !ans.Found {
		fmt.Fprintf(w, "    nothing on this box accounts for it: no locked item and no item "+
			"under %s contains a mod.info with that id\n", plural(len(v.Roots), "workshop root"))
		if ans.LiveInConfig {
			// The discovery finding, for one id: it is enabled and nothing
			// produced it. An item removed from the list whose id was left
			// behind looks exactly like this.
			fmt.Fprintf(w, "    it IS live in %s, which means the server is being told to "+
				"enable a mod this box cannot produce\n", v.Schema.Activation.Key)
		}
		fmt.Fprintf(w, "    this takes the ACTIVATION id (a mod.info id=), not the numeric "+
			"workshop id; `gvm content list %s` shows both\n", v.Name)
		return
	}

	item := fmt.Sprintf("    from %s item %s", v.Schema.Source, ans.AcquireID)
	if label := firstNonEmpty(ans.Title, ans.Note); label != "" {
		item += fmt.Sprintf("  %q", label)
	}
	fmt.Fprintln(w, item)

	if ans.Ordinal >= 0 {
		fmt.Fprintf(w, "    ordinal %d, %s (status: %s)\n", ans.Ordinal, ans.Why, ans.Status)
	} else {
		fmt.Fprintf(w, "    %s (status: %s)\n", ans.Why, ans.Status)
	}
	if len(ans.Siblings) > 0 {
		fmt.Fprintf(w, "    sibling%s in the same item: %s\n", pluralS(len(ans.Siblings)),
			strings.Join(ans.Siblings, ", "))
	}
	if !ans.Enabled && len(ans.Siblings) > 0 {
		fmt.Fprintf(w, "    the variant that won is %s\n", strings.Join(ans.Siblings, ", "))
	}
	if len(ans.Evidence) > 0 {
		fmt.Fprintf(w, "    evidence: %s\n", strings.Join(ans.Evidence, "  "))
	}
	fmt.Fprintf(w, "    live in %s: %s\n", v.Schema.Activation.Key, yesNo(ans.LiveInConfig))
	if ans.Artifact != "" {
		fmt.Fprintf(w, "    on disk: %s\n", ans.Artifact)
	}
}

func firstNonEmpty(xs ...string) string {
	for _, s := range xs {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
