package content

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/version"
)

// LockVersion is the lockfile schema version. A lock from the future is
// refused rather than decoded on a best-effort basis: a field this build does
// not understand could be the one that says "do not re-derive this list".
const LockVersion = 1

// Lock is /etc/gvm/instances/<name>.lock.json (D-03).
//
// It is INTENT, not cache. It records what the operator asked for and what was
// resolved, with digests, and losing it must cost reproducibility rather than
// correctness -- which is why observed state lives somewhere else entirely, at
// /var/lib/gvm/instances/<name>/content.observed.json. A pin is diffable,
// git-able and secret-free; a cache is none of those.
//
// 0644 root:root, no secrets, safe to paste into a support thread. The same
// bytes go inside every backup tarball's manifest.json, so a rebuild is
// reproducible from a tarball copied off the box with Postgres gone.
type Lock struct {
	Version        int       `json:"version"`
	Instance       string    `json:"instance"`
	Game           string    `json:"game"`
	ManifestSHA256 string    `json:"manifest_sha256"`
	GeneratedAt    time.Time `json:"generated_at"`
	GVMVersion     string    `json:"gvm_version"`

	// Sources is keyed by content BLOCK name, which is how the manifest and
	// the instance name the same list. Keyed rather than ordered: a block's
	// position in a manifest is not identity, and a reordered manifest must not
	// re-point an instance's locked intent at a different source.
	Sources map[string]*SourceLock `json:"sources"`
}

// SourceLock is one content block's recorded intent.
type SourceLock struct {
	Kind             SourceKind `json:"kind"`
	OrderSignificant bool       `json:"order_significant"`

	// VerbatimActivation is the adoption switch. When set, Activate returns
	// ActivationLine untouched and Derive refuses outright. That is how "the
	// live ini's Mods= list is PRESERVED VERBATIM" becomes a property of the
	// data rather than a promise in a doc -- and it is also how an operator
	// who hand-pruned their activation list keeps that edit through the next
	// sync.
	VerbatimActivation bool `json:"verbatim_activation"`

	// AcquisitionLine and ActivationLine are the rendered values, recorded.
	//
	// Both are kept even though the entries could render them, for two
	// different reasons. The acquisition line is recorded so a diff can show
	// what was actually written. The activation line is recorded because under
	// verbatim activation it is the ONLY copy of the operator's list: the
	// entries cannot reproduce a hand-pruned ordering, and re-deriving it is
	// the exact thing that must not happen.
	AcquisitionLine string `json:"acquisition_line"`
	ActivationLine  string `json:"activation_line"`

	Entries []LockEntry `json:"entries"`

	// Unaccounted carries the fail-closed gate across a process boundary.
	//
	// A lock gvm wrote can never have entries here, because Plan.Lock refuses
	// to produce one. It is recorded anyway so that a hand-edited lock, or one
	// restored from an older gvm, cannot be activated silently.
	//
	// This field is only HALF of what Activate asks a lock, and the halves are
	// split by what crosses the process boundary. Unaccounted does: a reason
	// recorded on one box is readable on another. The arithmetic half --
	// whether the entries still render the recorded acquisition_line and
	// activation_line -- needs the schema's separators, so it lives in
	// Validate and Activate calls it there.
	Unaccounted []Unaccounted `json:"unaccounted,omitempty"`
}

// LockEntry is one acquisition id, as resolved.
type LockEntry struct {
	// Ordinal is load order. It is the number a requires: violation reports,
	// and it is recorded rather than implied by position so that a lock a
	// human reordered in an editor is detectably inconsistent rather than
	// quietly re-interpreted.
	Ordinal int        `json:"ordinal"`
	Source  SourceKind `json:"source,omitempty"`
	Acquire AcquireID  `json:"acquire_id"`
	Pin     string     `json:"pin,omitempty"`
	// Note is the operator's trailing comment from mods.txt. Documentation,
	// carried as data, because "required by all KI5 vehicle mods" is the only
	// record of why an item sits at ordinal 0.
	Note     string      `json:"note,omitempty"`
	Requires []AcquireID `json:"requires,omitempty"`
	Vendored bool        `json:"vendored,omitempty"`

	// Materialized is "this item's bytes were on disk when the lock was
	// written". It is the recorded form of PlanEntry.Materialized, and it is
	// what the acquisition line's drop predicate tests.
	//
	// Recorded rather than inferred, and NOT omitempty. The two available
	// proxies are both wrong: "this entry has no activation ids" is a
	// different question in both directions (a downloaded item whose every
	// variant lost the score comparison has bytes and no ids; an undownloaded
	// item listed in mods.txt can have ids and no bytes), and "Artifact ==
	// nil" is false on the hook path, which builds its entries with
	// Materialized set and Artifact deliberately nil. An omitted false would
	// reintroduce the same ambiguity at the JSON boundary -- an absent field
	// reading as a decision nobody made -- so every entry says which it is.
	Materialized bool `json:"materialized"`

	// ActivationIDs is a SLICE, not a string, because one workshop item
	// legitimately contains several mods (sync-mods.sh's `multi` status).
	// Anything that models it one-to-one is wrong on day one.
	ActivationIDs []ActivationID `json:"activation_ids"`
	Dropped       []ActivationID `json:"dropped,omitempty"`

	Resolution ResolutionPath `json:"resolution"`
	Artifact   *Artifact      `json:"artifact,omitempty"`
	Upstream   *Upstream      `json:"upstream,omitempty"`

	Ambiguous bool     `json:"ambiguous,omitempty"`
	Evidence  []string `json:"evidence,omitempty"`
}

// NewLock builds an empty lock header for one instance.
func NewLock(in *Input) *Lock {
	return &Lock{
		Version:        LockVersion,
		Instance:       in.instanceName(),
		Game:           in.gameID(),
		ManifestSHA256: in.manifestSHA(),
		GeneratedAt:    in.Now(),
		GVMVersion:     version.Version,
		Sources:        map[string]*SourceLock{},
	}
}

func (in *Input) instanceName() string {
	if in == nil {
		return ""
	}
	return in.Instance
}

func (in *Input) gameID() string {
	if in == nil {
		return ""
	}
	return in.Game
}

func (in *Input) manifestSHA() string {
	if in == nil {
		return ""
	}
	return in.ManifestSHA256
}

// Put records one block's resolved intent.
func (l *Lock) Put(block string, sl *SourceLock) error {
	if l == nil {
		return fmt.Errorf("%w: there is no lock to write into", ErrInvalidLock)
	}
	if strings.TrimSpace(block) == "" {
		return fmt.Errorf("%w: a source lock belongs to a named content block", ErrInvalidLock)
	}
	if sl == nil {
		return fmt.Errorf("%w: block %q has nothing to record", ErrInvalidLock, block)
	}
	if l.Sources == nil {
		l.Sources = map[string]*SourceLock{}
	}
	l.Sources[block] = sl
	return nil
}

// SourceFor returns one block's recorded intent, or an error naming what is
// missing.
//
// An error rather than a nil-and-ok pair: every caller of this is about to
// write a mod list, and a nil source lock read as "no mods" would write an
// empty one.
func (l *Lock) SourceFor(block string) (*SourceLock, error) {
	if l == nil {
		return nil, fmt.Errorf("%w: there is no lock; run `gvm content sync` to record one",
			ErrInvalidLock)
	}
	sl, ok := l.Sources[block]
	if !ok || sl == nil {
		have := l.Blocks()
		if len(have) == 0 {
			return nil, fmt.Errorf("%w: the lock records no content blocks at all",
				ErrInvalidLock)
		}
		return nil, fmt.Errorf("%w: the lock records no block %q (it has %s)", ErrInvalidLock,
			block, strings.Join(have, ", "))
	}
	return sl, nil
}

// Blocks lists the recorded block names, sorted.
func (l *Lock) Blocks() []string {
	if l == nil {
		return nil
	}
	out := make([]string, 0, len(l.Sources))
	for n := range l.Sources {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// CheckManifest is `gvm content lock --check`'s manifest half.
//
// The lock pins the manifest revision it was computed against, because a
// manifest is argv plus env plus which keys gvm owns: a lock computed against
// revision 7 and activated under revision 9 may be writing a key the new
// revision does not own, or missing one it added.
func (l *Lock) CheckManifest(sha256 string) error {
	if l == nil {
		return fmt.Errorf("%w: there is no lock", ErrInvalidLock)
	}
	want := strings.TrimSpace(sha256)
	switch {
	case want == "":
		return fmt.Errorf("%w: no manifest digest to check against", ErrManifestMoved)
	case l.ManifestSHA256 == "":
		return fmt.Errorf("%w: the lock records no manifest digest, so it predates this check; "+
			"run `gvm content lock --refresh`", ErrManifestMoved)
	case !strings.EqualFold(l.ManifestSHA256, want):
		return fmt.Errorf("%w: the lock was computed against manifest %s and the pinned "+
			"manifest is now %s; run `gvm content lock --refresh` after reviewing the change. "+
			"Moving an instance to a newer manifest revision is not available in this build",
			ErrManifestMoved, short(l.ManifestSHA256), short(want))
	}
	return nil
}

// AcquireIDs is the acquisition line's ids, in ordinal order.
//
// keepMissing false drops entries whose bytes are not on disk. The default is
// true for a reason: an id that has not downloaded stays listed so the server
// retries it on the next boot, and dropping it means it is never requested
// again, so it is never downloaded, so the next resolve fails identically.
//
// The predicate is LockEntry.Materialized, which is literally Plan.AcquireIDs'
// predicate recorded. One line has to have one rendering: Plan.Lock writes the
// acquisition_line from the plan, Validate cross-checks it from the entries,
// and Activate writes it from the entries again. Two predicates for one
// question made gvm refuse the lock it had itself written.
func (sl *SourceLock) AcquireIDs(keepMissing bool) []AcquireID {
	if sl == nil {
		return nil
	}
	entries := sl.ordered()
	out := make([]AcquireID, 0, len(entries))
	seen := make(map[AcquireID]bool, len(entries))
	for _, e := range entries {
		if e.Acquire == "" || seen[e.Acquire] {
			continue
		}
		if !keepMissing && !e.Materialized {
			continue
		}
		seen[e.Acquire] = true
		out = append(out, e.Acquire)
	}
	return out
}

// ActivationIDs is every entry's activation ids, in ordinal order,
// de-duplicated by FIRST occurrence -- which is load order: moving a repeated
// library id to its last mention would put it below the mod that needs it.
func (sl *SourceLock) ActivationIDs() []ActivationID {
	if sl == nil {
		return nil
	}
	var out []ActivationID
	for _, e := range sl.ordered() {
		out = append(out, e.ActivationIDs...)
	}
	return dedupeActivate(out)
}

// Entry returns one recorded entry by acquisition id.
func (sl *SourceLock) Entry(id AcquireID) (LockEntry, bool) {
	if sl == nil {
		return LockEntry{}, false
	}
	for _, e := range sl.Entries {
		if e.Acquire == id {
			return e, true
		}
	}
	return LockEntry{}, false
}

// Owning returns the entry that contributed an activation id, which is the
// reverse map `gvm content why` is built on: an operator holding a string id
// out of a live Mods= needs to know which workshop item put it there.
func (sl *SourceLock) Owning(id ActivationID) (LockEntry, bool) {
	if sl == nil {
		return LockEntry{}, false
	}
	for _, e := range sl.ordered() {
		for _, a := range e.ActivationIDs {
			if a == id {
				return e, true
			}
		}
	}
	return LockEntry{}, false
}

// Ambiguous lists the entries enabled with a warning.
func (sl *SourceLock) Ambiguous() []AcquireID {
	if sl == nil {
		return nil
	}
	var out []AcquireID
	for _, e := range sl.ordered() {
		if e.Ambiguous {
			out = append(out, e.Acquire)
		}
	}
	return out
}

// CheckAccounted is the lock's copy of the fail-closed gate -- the half of it
// that a lockfile can carry on its own.
//
// Activate calls it before rendering anything, and then calls Validate for the
// other half: a truncated entries array leaves nothing in Unaccounted, and
// what catches it is the two recorded lines no longer rendering. A lock gvm
// wrote passes both; a hand-edited one, or one written by a build with a
// different entry shape, can fail either, and the right answer there is to
// refuse rather than to write whatever survived the edit.
func (sl *SourceLock) CheckAccounted() error {
	if sl == nil {
		return fmt.Errorf("%w: there is nothing recorded for this block", ErrUnaccounted)
	}
	if n := len(sl.Unaccounted); n > 0 {
		return fmt.Errorf("%w: %s", ErrUnaccounted,
			unaccountedDetail("(locked)", n, n+len(sl.Entries), sl.Unaccounted))
	}
	return nil
}

// Validate is the structural check Load runs.
//
// The line cross-checks are the interesting half and they are deliberately
// asymmetric. The acquisition line must always equal what the entries render,
// because gvm owns it. The activation line must equal what the entries render
// ONLY when the lock is not verbatim: under verbatim activation the recorded
// line is the operator's, it is expected to differ, and a validator that
// "corrected" it would destroy the thing the switch protects.
//
// keepMissing is the schema's acquisition policy, threaded in rather than
// assumed, because it is what Plan.Lock rendered the recorded line WITH. It is
// inert when acqSep is empty, which is how Lock.Validate skips the line checks
// it has no schema for -- so that call site is not a second source of truth for
// the policy.
func (sl *SourceLock) Validate(block, acqSep, actSep string, keepMissing bool) error {
	if sl == nil {
		return fmt.Errorf("%w: block %q records nothing", ErrInvalidLock, block)
	}
	var errs []error
	if !sl.Kind.Valid() {
		errs = append(errs, fmt.Errorf("%w: block %q records source %q", ErrInvalidLock, block,
			sl.Kind))
	}

	seenID := make(map[AcquireID]int, len(sl.Entries))
	seenOrd := make(map[int]AcquireID, len(sl.Entries))
	for i, e := range sl.Entries {
		if e.Acquire == "" {
			errs = append(errs, fmt.Errorf("%w: block %q entry[%d] has no acquire_id",
				ErrInvalidLock, block, i))
			continue
		}
		if first, dup := seenID[e.Acquire]; dup {
			errs = append(errs, fmt.Errorf("%w: block %q records %s twice, at entry[%d] and "+
				"entry[%d]", ErrInvalidLock, block, e.Acquire, first, i))
		} else {
			seenID[e.Acquire] = i
		}
		if other, dup := seenOrd[e.Ordinal]; dup {
			// Two entries at one ordinal means load order is not a total
			// order, so "libraries first" is no longer expressible.
			errs = append(errs, fmt.Errorf("%w: block %q gives ordinal %d to both %s and %s",
				ErrInvalidLock, block, e.Ordinal, other, e.Acquire))
		} else {
			seenOrd[e.Ordinal] = e.Acquire
		}
		if e.Ordinal < 0 {
			errs = append(errs, fmt.Errorf("%w: block %q entry %s has ordinal %d",
				ErrInvalidLock, block, e.Acquire, e.Ordinal))
		}
		if e.Resolution != "" && !e.Resolution.Valid() {
			errs = append(errs, fmt.Errorf("%w: block %q entry %s records resolution %q",
				ErrInvalidLock, block, e.Acquire, e.Resolution))
		}
		if e.Artifact != nil {
			errs = append(errs, validateArtifact(block, e.Acquire, e.Artifact)...)
		}
	}

	if acqSep != "" {
		if want := joinAcquire(sl.AcquireIDs(keepMissing), acqSep); want != sl.AcquisitionLine {
			errs = append(errs, fmt.Errorf("%w: block %q records an acquisition_line that its "+
				"own entries do not render (recorded %d bytes, entries render %d); the lock has "+
				"been edited by something other than gvm", ErrInvalidLock, block,
				len(sl.AcquisitionLine), len(want)))
		}
	}
	if actSep != "" && !sl.VerbatimActivation {
		if want := joinActivate(sl.ActivationIDs(), actSep); want != sl.ActivationLine {
			errs = append(errs, fmt.Errorf("%w: block %q records an activation_line that its "+
				"own entries do not render, and it is not marked verbatim; set "+
				"activation_policy: preserve if the recorded line is deliberate",
				ErrInvalidLock, block))
		}
	}
	return errors.Join(errs...)
}

func validateArtifact(block string, id AcquireID, a *Artifact) []error {
	var errs []error
	switch a.Kind {
	case ArtifactFile, ArtifactTree:
	default:
		errs = append(errs, fmt.Errorf("%w: block %q entry %s records artifact kind %q (want "+
			"%s|%s)", ErrInvalidLock, block, id, a.Kind, ArtifactFile, ArtifactTree))
	}
	if a.Digest != "" {
		if _, err := ParseDigest(a.Digest); err != nil {
			errs = append(errs, fmt.Errorf("block %q entry %s: %w", block, id, err))
		}
	}
	if a.Size < 0 || a.Members < 0 {
		errs = append(errs, fmt.Errorf("%w: block %q entry %s records size %d and %d members",
			ErrInvalidLock, block, id, a.Size, a.Members))
	}
	return errs
}

// Validate checks the whole document.
func (l *Lock) Validate() error {
	if l == nil {
		return fmt.Errorf("%w: there is no lock", ErrInvalidLock)
	}
	var errs []error
	switch {
	case l.Version == 0:
		errs = append(errs, fmt.Errorf("%w: no version; this is not a gvm content lock",
			ErrInvalidLock))
	case l.Version > LockVersion:
		errs = append(errs, fmt.Errorf("%w: version %d was written by a newer gvm (this build "+
			"understands %d); a field this build would ignore could be the one that says not to "+
			"re-derive a list", ErrInvalidLock, l.Version, LockVersion))
	}
	if strings.TrimSpace(l.Instance) == "" {
		errs = append(errs, fmt.Errorf("%w: the lock names no instance", ErrInvalidLock))
	}
	for _, block := range l.Blocks() {
		// Separators are a schema fact and are not in the lock, so the line
		// cross-check runs in ValidateAgainst where the schema is in hand.
		// Separators empty, so both line cross-checks are skipped and
		// keepMissing is inert here.
		errs = append(errs, l.Sources[block].Validate(block, "", "", true))
	}
	return errors.Join(errs...)
}

// ValidateAgainst is Validate plus the checks that need the compiled schema:
// the recorded lines, and that the lock's source kind is the manifest's.
func (l *Lock) ValidateAgainst(sc *Schema) error {
	if err := l.Validate(); err != nil {
		return err
	}
	if sc == nil {
		return fmt.Errorf("%w: no compiled schema", ErrInvalidSchema)
	}
	sl, err := l.SourceFor(sc.Block)
	if err != nil {
		return err
	}
	var errs []error
	if sl.Kind != sc.Source {
		errs = append(errs, fmt.Errorf("%w: block %q is locked as %s and the manifest declares "+
			"%s", ErrInvalidLock, sc.Block, sl.Kind, sc.Source))
	}
	if sl.VerbatimActivation != sc.VerbatimActivation {
		// Not auto-corrected in either direction: taking over a recorded
		// activation line is an explicit act, and derive -> preserve would mean
		// freezing a list nobody recorded deliberately. Changing activation
		// ownership is not available in this build, so the only move offered is
		// restoring the policy the lock was written with.
		errs = append(errs, fmt.Errorf("%w: block %q is locked with verbatim_activation:%t and "+
			"the instance declares activation_policy for %t; restore the policy in "+
			"%s.yaml. Changing activation ownership is not available in this build",
			ErrInvalidLock, sc.Block, sl.VerbatimActivation, sc.VerbatimActivation, l.Instance))
	}
	errs = append(errs, sl.Validate(sc.Block, sc.Acquisition.Separator,
		sc.Activation.Separator, sc.Acquisition.KeepMissing))
	return errors.Join(errs...)
}

// ordered returns the entries in ordinal order without disturbing the
// recorded slice.
//
// A copy, not a sort in place: this is called from read paths, including
// concurrent ones, and sorting the caller's slice underneath them would be a
// data race that only shows up under load.
func (sl *SourceLock) ordered() []LockEntry {
	out := append([]LockEntry(nil), sl.Entries...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Ordinal < out[j].Ordinal })
	return out
}

// Marshal renders the lock as the bytes that go to disk and into a backup
// tarball's manifest.json.
//
// Indented, because this file is meant to be read, diffed and pasted into a
// support thread; and with a trailing newline, because a text file without one
// makes every later diff show a modified last line.
func Marshal(l *Lock) ([]byte, error) {
	if l == nil {
		return nil, fmt.Errorf("%w: there is no lock to encode", ErrInvalidLock)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	// HTML escaping off: a mod title legitimately contains & and <, and
	// & in a file an operator is meant to read is noise.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(l); err != nil {
		return nil, fmt.Errorf("encode content lock: %w", err)
	}
	return buf.Bytes(), nil
}

// Unmarshal decodes a lock and validates it.
//
// DisallowUnknownFields is deliberately NOT set: the version check is the
// compatibility gate, and refusing a lock because a newer gvm added a
// cosmetic field would make a downgrade unrecoverable. A newer VERSION is
// refused; a newer field inside a version this build understands is ignored.
func Unmarshal(data []byte) (*Lock, error) {
	var l Lock
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidLock, err)
	}
	if err := l.Validate(); err != nil {
		return nil, err
	}
	return &l, nil
}

// Load reads a lockfile from disk.
//
// A missing file is reported with fs.ErrNotExist unwrapped, because "this
// instance has no content lock yet" is a normal state -- a fresh instance, or
// one whose game declares no content -- and the caller distinguishes it with
// errors.Is rather than by string matching.
func Load(path string) (*Lock, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("read content lock %s: %w", path, err)
	}
	l, err := Unmarshal(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return l, nil
}

// LoadFor reads the lockfile of a named instance (D-03's path).
//
// There is deliberately no Save here. The content lock is written by
// hostops.FileContentLock, which owns its mode (0644), its parent directory
// and its .bak rotation; a second writer in this package would be a second
// copy of that policy, and the two would drift the first time one of them was
// changed. This package produces the bytes (Marshal) and the privileged writer
// puts them down.
func LoadFor(name string) (*Lock, error) {
	return Load(instance.PathsFor(name).ContentLock)
}

// LockPathFor is where this instance's lock lives, for a message that has to
// name the file.
func LockPathFor(name string) string { return instance.PathsFor(name).ContentLock }

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12] + "…"
	}
	return sha
}
