package content

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/provider"
)

// Source is one content backend: Steam Workshop, Modrinth, CurseForge.
//
// Four phases, and the boundaries between them are the design. Resolve reads
// the network and writes nothing, so it is safe against a live server with
// players online and it is the half that decides whether anything has to stop
// at all. Materialize is the only half that writes bytes. Activate writes
// nothing either -- it returns LINES, and the privileged config write is a
// separate, tiny step through internal/gamecfg's surgical editors. Observe
// reads local state only, which is what makes "has someone hand-edited mods/"
// answerable with the network down and Postgres gone.
type Source interface {
	Kind() SourceKind

	// Resolve performs network READS and writes nothing.
	//
	// It MUST account for every Ref it is given: anything it cannot classify
	// goes in Plan.Unaccounted rather than being dropped. A source that
	// returns a short list without saying so has produced something
	// indistinguishable from a deliberately short list.
	Resolve(ctx context.Context, in *Input, want []Ref) (*Plan, error)

	// Materialize refuses a Plan whose Unaccounted is non-empty (its first
	// statement is p.CheckAccounted), may only write inside in.Jail, and may
	// only accept bytes whose digest appears in the plan. An unverified
	// download is a HARD FAILURE, never a warning: gvm hands these bytes to a
	// service account that executes them.
	Materialize(ctx context.Context, in *Input, p *Plan, pr Progress) (*Lock, error)

	// Activate maps the materialized set into the game's own identifiers. No
	// network, no writes. It returns lines and honours VerbatimActivation;
	// Activate (the free function below) is the shared implementation and
	// every source is expected to delegate to it.
	Activate(ctx context.Context, in *Input, l *Lock) (*Activation, error)

	// Observe reads local state only. Mandatory on every source, so adoption,
	// drift detection and "has someone hand-edited this" are one call graph
	// rather than three per-source special cases.
	Observe(ctx context.Context, in *Input) (*Observed, error)
}

// Root is one content root, opened read-only.
//
// Name is the absolute path, for diagnostics and for the lock's artifact path;
// FS is a read-only handle rooted AT that directory. A source and a resolver
// get fs.FS and never a writer: the fetcher runs as neither root nor the
// instance user (D-16) and nothing in the resolve path has any business
// writing into a game's install tree.
type Root struct {
	Name string
	FS   fs.FS
}

// Input is everything a Source is allowed to see.
//
// Deliberately not the instance struct: a source cannot reach RBAC, the
// database, systemd or another instance. Every field is a value or a narrow
// seam, so the blast radius of a source bug is this block's two lists.
type Input struct {
	// Instance and Game identify what is being synced, for the lock's header.
	Instance string
	Game     string
	// ManifestSHA256 pins the manifest this resolve was computed against, so
	// `gvm content lock --check` can fail when the manifest moves underneath
	// a recorded intent (D-17, section 12.3).
	ManifestSHA256 string

	Schema *Schema

	// Roots are the declared roots that EXIST, in declared order. A missing
	// root is skipped rather than fatal: the server writes workshop content to
	// different locations depending on version and both may be on one box.
	Roots []Root

	// Jail bounds every write Materialize may make. A content source that
	// could write outside it is a mod archive away from overwriting a world
	// (section 10.1).
	Jail *instance.Jail

	// Blobs is the content-addressed store, which hands back a path only on a
	// digest match. nil for a source that materialises nothing -- PZ's
	// server_boot acquisition never has the bytes in hand.
	Blobs provider.BlobStore

	// Upstream is metadata fetched ABOUT the declared items, keyed by
	// acquisition id. A missing entry means the API was unreachable, which
	// DEGRADES the resolve to the local heuristic and never aborts it: a
	// Steam outage must not empty a mod list (section 12.5).
	Upstream map[AcquireID]*Upstream

	// Live is the two write keys' values as the config file has them right
	// now, read by the CALLER through internal/gamecfg's surgical readers
	// before a source runs.
	//
	// Two strings rather than a file handle or a codec, and that is the point.
	// The RCON password lives in the same ini as Mods=, so a source that could
	// open the file could read a key it does not own -- and internal/content
	// would gain an import that puts a config parser inside the package that
	// holds the gate. nil means "nobody read the config", which Observe
	// reports as such rather than as an empty Mods=, because an empty Mods= is
	// the one reading that would look like a deliberate instruction to disable
	// every mod.
	Live *Live

	Log   *slog.Logger
	Clock func() time.Time
}

// Now is the source's clock, injectable so a lock's generated_at is assertable
// rather than "roughly now".
func (in *Input) Now() time.Time {
	if in == nil || in.Clock == nil {
		return time.Now().UTC()
	}
	return in.Clock().UTC()
}

// Logger is Input.Log, never nil.
func (in *Input) Logger() *slog.Logger {
	if in == nil || in.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return in.Log
}

// UpstreamFor is the metadata for one item, or nil. Nil is a normal answer.
func (in *Input) UpstreamFor(id AcquireID) *Upstream {
	if in == nil {
		return nil
	}
	return in.Upstream[id]
}

// Live is the config as it is on disk: the two write keys' raw values, and
// when they were read.
//
// Raw, unparsed strings, because that is what verbatim activation preserves
// and what an operator's hand edit looks like. Normalising them on the way in
// -- sorting, de-duplicating, dropping an id gvm does not recognise -- would
// destroy the evidence that there was an edit at all.
type Live struct {
	// Acquisition is the acquisition key's value: PZ's WorkshopItems=.
	Acquisition string
	// Activation is the activation key's value: PZ's Mods=.
	Activation string
	// ReadAt is when the file was read. PZ rewrites its ini on shutdown, so a
	// reading's age is part of what it means.
	ReadAt time.Time

	// Absent is "I looked and the file is not there", which is a THIRD state
	// and not the same as a nil *Live meaning "nobody looked".
	//
	// All three leave the two values above empty and they lead to three
	// different answers. Nobody looked: Resolve refuses, because a plan
	// computed without knowing what is live would read an unread config as an
	// empty one. Looked and it is absent: a fresh instance before the game has
	// generated its ini, which is reported as absent and must never be
	// reported as an empty mod list -- and which still has to produce changes,
	// or the first WorkshopItems= is never written. Looked and it is empty:
	// a real empty list, and the per-id drift rows are the right answer.
	Absent bool
}

// Progress is how a long Materialize reports to the job log and the dashboard.
// Reused from the install seam rather than redeclared: a job layer that had to
// adapt between two identical func-struct types would grow a nil check per
// call site, and the nil check is the thing that gets forgotten in the error
// path.
type Progress = provider.Progress

// AcquisitionLine is a rendered write into the ACQUISITION namespace.
type AcquisitionLine struct {
	Key       string
	File      string
	Separator string
	IDs       []AcquireID
}

// Value is the string that goes after the "=".
func (l AcquisitionLine) Value() string { return joinAcquire(l.IDs, l.Separator) }

// Namespace reports which id space this line carries, for a report.
func (l AcquisitionLine) Namespace() Namespace { return NamespaceAcquisition }

// Empty reports whether this line would write nothing.
func (l AcquisitionLine) Empty() bool { return len(l.IDs) == 0 }

// ActivationLine is a rendered write into the ACTIVATION namespace.
//
// Its value comes from one of two places and the type remembers which: derived
// ids, or a recorded verbatim string. The recorded string is kept in an
// unexported field so that no composite literal in another package can
// manufacture a "verbatim" line, which is what makes Verbatim() a statement
// about where the value came from rather than a flag someone set.
type ActivationLine struct {
	Key       string
	File      string
	Separator string
	IDs       []ActivationID

	verbatim    bool
	rawRecorded string
}

// Value is the string that goes after the "=".
//
// For a verbatim line this is the RECORDED line, byte for byte, including
// whatever spacing and ordering the operator left in it. That is the point: an
// operator who hand-pruned their activation list keeps their edit through the
// next sync.
func (l ActivationLine) Value() string {
	if l.verbatim {
		return l.rawRecorded
	}
	return joinActivate(l.IDs, l.Separator)
}

// Verbatim reports whether this line was recorded rather than derived.
func (l ActivationLine) Verbatim() bool { return l.verbatim }

// Namespace reports which id space this line carries.
func (l ActivationLine) Namespace() Namespace { return NamespaceActivation }

// Empty reports whether this line would write nothing. An empty ACTIVATION
// write is the catastrophe this package is shaped around -- it disables every
// mod, and PZ rewrites the ini on shutdown so it becomes permanent -- so every
// caller is expected to ask.
func (l ActivationLine) Empty() bool {
	if l.verbatim {
		return strings.TrimSpace(l.rawRecorded) == ""
	}
	return len(l.IDs) == 0
}

// Activation is what Activate returns: two lines, in two namespaces, with
// their own keys. Never one list with two renderings.
type Activation struct {
	Block string
	// Acquisition is always derived from the lock's entries: gvm owns which
	// items the server is told to fetch, including the ones that have not
	// downloaded yet (keep_missing).
	Acquisition AcquisitionLine
	// Activate is derived, or recorded verbatim.
	Activate ActivationLine
	// Note is the sentence a report prints: "33 ids derived from 33 items", or
	// "returned the recorded line untouched (activation_policy: preserve)".
	Note string
}

// Verbatim reports whether the activation half was recorded rather than
// derived.
func (a *Activation) Verbatim() bool { return a != nil && a.Activate.Verbatim() }

// Writes returns the two lines in the order a caller should apply them, which
// is activation first when both land in one file: that keeps the declared key
// order in a shared ini stable across syncs, so a diff shows one changed value
// rather than two moved lines.
func (a *Activation) Writes() []string {
	if a == nil {
		return nil
	}
	return []string{
		a.Activate.Key + "=" + a.Activate.Value(),
		a.Acquisition.Key + "=" + a.Acquisition.Value(),
	}
}

// Observed is the local reading: what is actually on this box right now.
//
// It is what `gvm content diff --against observed` compares to, what adoption
// imports, and what step 10 of the download cycle re-reads after the start --
// PZ rewrites its ini on shutdown, so the file after a run is evidence about
// what the server believed, not about what gvm wrote.
type Observed struct {
	Block  string
	ReadAt time.Time

	// AcquisitionRaw and ActivationRaw are the live config values, exactly as
	// the file has them. Raw, because that is what verbatim activation
	// preserves and what an operator's hand edit looks like.
	AcquisitionRaw string
	ActivationRaw  string
	// ConfigRead distinguishes "the config says the mod list is empty" from
	// "nobody read the config". Both leave the two Raw fields empty, and only
	// one of them is a reason to write anything: acting on the second as if it
	// were the first is how a fresh instance's reading turns into an empty
	// Mods= on an adopted server.
	ConfigRead bool
	// ConfigAbsent is "somebody looked and the file is not there".
	//
	// A separate field rather than clearing ConfigRead, because the two drive
	// different machinery and the obvious collapse breaks the bootstrap:
	// planChanges early-returns nil on !ConfigRead, so a brand-new instance
	// with no ini would produce zero changes, sync would see nothing to do,
	// and the first WorkshopItems= would never be written. So the reading is
	// still a reading, and this is the flag a report has to consult before it
	// says anything about what the live config enables.
	ConfigAbsent bool

	Acquire  []AcquireID
	Activate []ActivationID

	// Items is what the roots hold, one per acquisition id that was looked
	// for.
	Items []ObservedItem
	// Roots are the roots that existed when this reading was taken.
	Roots []string

	// Unlisted are activation ids live in the config that no observed item
	// accounts for. This is the "WorkshopItems= has 32 entries and Mods= has
	// 33" discovery finding: the operator's next question is which id, and
	// `gvm content why` answers it from here.
	Unlisted []ActivationID
}

// ObservedItem is one acquisition id as the filesystem has it.
type ObservedItem struct {
	ID AcquireID
	// Present is "there is a directory for this id under some root".
	Present bool
	// Empty is "the directory is there and holds nothing" -- a download that
	// started and did not finish, which is NOT the same as absent and must not
	// be reported as acquired.
	Empty bool
	Path  string

	Activation []ActivationID
	Dropped    []ActivationID
	Evidence   []string
	Ambiguous  bool
}

// Materialized reports whether this item's bytes are on disk. Present and
// non-empty: a directory the server created and then failed to fill is not
// materialized, and treating it as such is how a mod silently never loads.
func (it ObservedItem) Materialized() bool { return it.Present && !it.Empty }

// Item returns the observed state of one acquisition id.
func (o *Observed) Item(id AcquireID) (ObservedItem, bool) {
	if o == nil {
		return ObservedItem{}, false
	}
	for _, it := range o.Items {
		if it.ID == id {
			return it, true
		}
	}
	return ObservedItem{}, false
}

// Activate is the shared implementation of Source.Activate's contract.
//
// It lives here, as one function, rather than in each source, because the two
// rules it enforces are the ones a per-source copy would drift on:
//
//  1. The fail-closed gate. A lock whose declared set was not fully accounted
//     for cannot be activated. Writing a partial activation list starts a
//     server with mods missing, and on PZ the server rewrites the ini on
//     shutdown, so the partial list becomes the new truth and there is no
//     second pass that could put it back.
//  2. verbatim_activation. The recorded line is returned UNTOUCHED and is
//     never re-derived, which is how "the live ini's Mods= list is preserved
//     verbatim" becomes a property of the data rather than a promise in a doc.
//  3. An empty DERIVED activation line is a refusal, not a success. See
//     ActivateAllowingEmpty for the one way to get one.
func Activate(sc *Schema, l *Lock) (*Activation, error) { return activate(sc, l, false) }

// ActivateAllowingEmpty is Activate for a caller that has established that an
// empty activation list is what the operator asked for.
//
// It exists as a second function rather than as a flag because the judgement it
// represents cannot be made in this package. "The instance declares no mods"
// and "the block was never read" produce the same zero-entry lock, and only
// the caller knows which it is holding: internal/hook's resolver takes this
// form when the declared acquisition list is empty, which is intent, and takes
// Activate -- and its refusal -- otherwise.
//
// Deliberately NOT given a config reader of its own to answer "is the live line
// already empty". That would make this package read the file it is supposed to
// be returning lines for, and the answer would still not distinguish the two
// cases: an adopted server whose operator removed every mod has a non-empty
// live line and an empty declared list, which is legitimate.
func ActivateAllowingEmpty(sc *Schema, l *Lock) (*Activation, error) {
	return activate(sc, l, true)
}

func activate(sc *Schema, l *Lock, allowEmpty bool) (*Activation, error) {
	if sc == nil {
		return nil, fmt.Errorf("%w: no compiled schema, so there are no write keys to activate "+
			"into", ErrInvalidSchema)
	}
	sl, err := l.SourceFor(sc.Block)
	if err != nil {
		return nil, err
	}
	// The gate, before anything is rendered: a refusal that happened after the
	// lines were built would be one `if` away from a caller using them.
	if err := sl.CheckAccounted(); err != nil {
		return nil, err
	}
	// And the ARITHMETIC half of the same gate, which the lock's
	// CheckAccounted does not carry: Unaccounted crosses the process boundary,
	// but "do the entries still render the lines gvm recorded" needs the
	// schema's separators and so can only be asked here. A lock whose entries
	// array was truncated while its two recorded lines were left intact passes
	// Unmarshal and passes CheckAccounted, and the evidence that it used to be
	// longer is sitting in the same file. Section 12.2's RAW_COUNT != COUNT
	// generalisation is exactly this, and without it a hand-edited or
	// foreign-build lock activates into a short Mods= -- which on PZ becomes
	// permanent, because the server rewrites the ini on shutdown.
	if err := sl.Validate(sc.Block, sc.Acquisition.Separator, sc.Activation.Separator,
		sc.Acquisition.KeepMissing); err != nil {
		return nil, err
	}

	out := &Activation{
		Block:       sc.Block,
		Acquisition: sc.Acquisition.Line(sl.AcquireIDs(sc.Acquisition.KeepMissing)),
	}

	if sl.VerbatimActivation {
		recorded := sl.ActivationLine
		if strings.TrimSpace(recorded) == "" {
			// "Preserve" with nothing recorded does not mean "write an empty
			// list" -- it means the recording step never happened. Writing the
			// empty line would disable every mod on an ADOPTED server, which
			// is the single worst thing this package could do.
			return nil, fmt.Errorf("%w: block %q records verbatim activation but the recorded "+
				"%s line is empty; the live list must be recorded before activating, and "+
				"importing it is not available in this build", ErrInvalidLock, sc.Block,
				sc.Activation.Key)
		}
		out.Activate = sc.Activation.Verbatim(recorded)
		out.Note = fmt.Sprintf("returned the recorded %s line untouched (%d bytes); "+
			"activation_policy is %q, so it was not re-derived",
			sc.Activation.Key, len(recorded), "preserve")
		return out, nil
	}

	line, err := Derive(sc, l)
	if err != nil {
		return nil, err
	}
	if line.Empty() {
		// The one question ActivationLine.Empty was written to be asked, asked
		// at the chokepoint. Writing this line disables every mod, and on PZ
		// the server rewrites the ini on shutdown, so there is no second pass
		// that could put it back. "derived 0 activation id(s) from 0 item(s)"
		// returned as a success is how a lock nobody meant to be empty becomes
		// an empty Mods=.
		if !allowEmpty {
			return nil, fmt.Errorf("%w: block %q records %d item(s) and derives no activation "+
				"id at all, so writing %s would disable every mod. If the instance really "+
				"declares no content for this block that is intent and the caller says so "+
				"explicitly; otherwise the list was never read -- check that the instance's "+
				"`spec.content` key names this block", ErrEmptyActivation, sc.Block,
				len(sl.Entries), sc.Activation.Key)
		}
		out.Activate = line
		// Phrased so it can never be mistaken for an ordinary success in a job
		// log: the whole failure this guards was a reassuring sentence about a
		// list nobody read.
		out.Note = fmt.Sprintf("this block declares no items, so %s and %s were both derived "+
			"EMPTY on purpose", sc.Activation.Key, sc.Acquisition.Key)
		return out, nil
	}
	out.Activate = line
	out.Note = fmt.Sprintf("derived %d activation id(s) from %d item(s)",
		len(line.IDs), len(sl.Entries))
	return out, nil
}

// Derive re-derives the activation line from the lock's entries, in ordinal
// order, de-duplicated by first occurrence.
//
// It REFUSES on a verbatim lock, with ErrVerbatim. That refusal is the whole
// mechanism: a caller that wants the operator's hand-pruned list calls
// Activate and gets it; a caller that tries to recompute it is told no, rather
// than quietly replacing an edit gvm cannot see the reasoning for.
func Derive(sc *Schema, l *Lock) (ActivationLine, error) {
	if sc == nil {
		return ActivationLine{}, fmt.Errorf("%w: no compiled schema", ErrInvalidSchema)
	}
	sl, err := l.SourceFor(sc.Block)
	if err != nil {
		return ActivationLine{}, err
	}
	if sl.VerbatimActivation {
		return ActivationLine{}, fmt.Errorf("%w: block %q records verbatim activation; the "+
			"recorded line is used as-is. Taking ownership of it to re-derive is not available "+
			"in this build", ErrVerbatim, sc.Block)
	}
	if err := sl.CheckAccounted(); err != nil {
		return ActivationLine{}, err
	}
	// The same arithmetic half as Activate's: this is reachable on its own
	// (`gvm content promote`, a job that re-derives), and a one-condition gate
	// here would be the hole Activate's second condition closed.
	if err := sl.Validate(sc.Block, sc.Acquisition.Separator, sc.Activation.Separator,
		sc.Acquisition.KeepMissing); err != nil {
		return ActivationLine{}, err
	}
	return sc.Activation.Line(sl.ActivationIDs()), nil
}
