// Package content is the two-list model (section 12).
//
// Everything a mod system gets wrong is one of three mistakes: conflating the
// id you ASK FOR with the id the game ENABLES, losing load order, or trusting
// bytes you did not hash. The first one is the expensive one, and it is the
// reason this package exists as a package rather than as a field on a source.
//
// Project Zomboid needs two non-interchangeable lists:
//
//   - ACQUISITION: numeric Steam Workshop ids. What to download. Written to
//     WorkshopItems=.
//   - ACTIVATION: the internal string ids read out of each item's mod.info.
//     What to enable. Written to Mods=.
//
// A wrong ACTIVATION entry fails SILENTLY: the item downloads, the mod never
// loads, and nothing is logged anywhere. No exit code, no log line, no warning
// on the console -- a player eventually notices a mod is missing. So the two
// are separate Go types in separate namespaces with separate write keys, and
// there is deliberately no conversion between AcquireID and ActivationID: a
// function that returns one cannot be assigned to the other, and the compiler
// says so. Modelling them as one list with two renderings is the bug.
//
// The three load-bearing refusals, each a type rather than a review comment:
//
//   - Plan.Unaccounted is a FAIL-CLOSED GATE (plan.go). Anything in the
//     declared set that could not be accounted for makes Materialize and
//     Activate refuse, because a partial list looks exactly like a deliberate
//     one.
//   - The lockfile is INTENT, not cache (lock.go, D-03), and
//     verbatim_activation makes Activate hand back the recorded line untouched
//     rather than re-deriving it.
//   - CheckOrder reports a requires: violation and NEVER reorders (order.go),
//     because load order is hand-tuned and the reasoning is in comments gvm
//     cannot read.
package content

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// AcquireID is an id in the ACQUISITION namespace: a Steam Workshop
// publishedfileid ("3171167894"), a Modrinth project/version pair, a
// CurseForge "projectID:fileID". It is what a human writes and what gvm (or
// the server itself) downloads.
//
// It is a defined type, not a string alias, for one reason: no assignment,
// return or struct literal can put one of these where an ActivationID belongs.
// That is the whole safety property of this package, and a convention could
// not provide it.
type AcquireID string

func (id AcquireID) String() string { return string(id) }

// ActivationID is an id in the ACTIVATION namespace: a mod.info `id=` value
// ("ThatDamnLibrary"), a jar filename. It is what the game enables, it is NOT
// derivable from an AcquireID, and getting it wrong is silent.
type ActivationID string

func (id ActivationID) String() string { return string(id) }

// Namespace names one of the two id spaces, for a message or a dashboard
// column. It is never a selector that picks which list to write: the write
// keys are separate fields on separate structs precisely so that no `switch
// ns` can transpose them.
type Namespace string

const (
	NamespaceAcquisition Namespace = "acquisition"
	NamespaceActivation  Namespace = "activation"
)

// Ref is the ACQUISITION namespace as the operator wrote it: the only thing a
// human edits.
//
// An alias of spec.ContentRef rather than a second struct. spec declares it
// because the instance YAML has to decode and validate it, and a content.Ref
// that had to be converted at the seam would be two structs one forgotten
// field away from disagreeing -- which for `Requires` would mean an enforced
// ordering constraint silently dropped on the way in.
type Ref = spec.ContentRef

// ID is the ref's acquisition id in its own namespace.
func RefID(r Ref) AcquireID { return AcquireID(strings.TrimSpace(r.ID)) }

// RefRequires is the ref's declared dependencies, in the acquisition
// namespace. Steam Workshop publishes no machine-readable dependency data --
// the "Required items" block is HTML -- so these are operator-declared, and
// order.go enforces them rather than guessing.
func RefRequires(r Ref) []AcquireID {
	out := make([]AcquireID, 0, len(r.Requires))
	for _, id := range r.Requires {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, AcquireID(id))
		}
	}
	return out
}

// SourceKind is a content source. The values are spec's, so a manifest's
// `source:` string and this package's registry cannot drift.
type SourceKind string

const (
	KindSteamWorkshop SourceKind = spec.SourceSteamWorkshop
	KindModrinth      SourceKind = spec.SourceModrinth
	KindCurseForge    SourceKind = spec.SourceCurseForge
)

func (k SourceKind) String() string { return string(k) }

// SourceKinds returns the known sources, in ship order (section 12.8:
// Modrinth first, CurseForge second).
func SourceKinds() []SourceKind {
	return []SourceKind{KindSteamWorkshop, KindModrinth, KindCurseForge}
}

// Valid reports whether k is a source this schema knows. Implementation is a
// separate question, asked of the source registry.
func (k SourceKind) Valid() bool {
	for _, s := range SourceKinds() {
		if s == k {
			return true
		}
	}
	return false
}

// Acquisition mechanisms. The values are spec's.
//
// MechanismServerBoot is the one that shapes section 12.6's pipeline: the
// SERVER downloads its own items from the acquisition key, so acquiring means
// writing the key while stopped and then booting, and there is no steamcmd
// lock to take.
const (
	MechanismServerBoot   = spec.AcquireServerBoot
	MechanismSteamCMDItem = spec.AcquireSteamCMDItem
	MechanismHTTP         = spec.AcquireHTTP
)

// ArtifactKind distinguishes the two shapes of thing an entry can point at.
//
// A single `SHA256 string` cannot describe a Steam Workshop item, which is a
// DIRECTORY the server itself downloaded and gvm never had the bytes of. Being
// honest about that is what lets one Source interface serve server_boot
// acquisition and HTTP acquisition without either one lying about what it
// verified.
type ArtifactKind string

const (
	// ArtifactFile is one blob, one digest, in the content-addressed store,
	// hardlinked into place. gvm downloaded it and can re-hash it.
	ArtifactFile ArtifactKind = "file"
	// ArtifactTree is a directory gvm did not download. Verified by presence
	// plus non-emptiness plus a tree-sha256 over a canonical listing
	// (digest.go). The cheap change signal is the API's time_updated; the full
	// tree digest is recomputed only on `gvm content verify --deep`.
	ArtifactTree ArtifactKind = "tree"
)

// Artifact is what an entry resolved to on disk.
type Artifact struct {
	Kind ArtifactKind `json:"kind"`
	Path string       `json:"path"`
	// Digest is "sha256:…" for a file and "tree-sha256:…" for a tree. The
	// prefix is what keeps an unverifiable identity from being mistaken for a
	// verified one; see digest.go.
	Digest Digest `json:"digest,omitempty"`
	Size   int64  `json:"size,omitempty"`
	// Members is the file count, for a tree. Zero on a file artifact.
	Members int `json:"members,omitempty"`
}

// Upstream is the metadata a source fetched ABOUT an item, as evidence. It is
// never authority: section 12.5's rule is that tags never drop an item, so an
// unreachable API degrades to the local file heuristic instead of emptying a
// mod list.
type Upstream struct {
	API string `json:"api,omitempty"` // "steam_remote_storage"
	// TimeUpdated is the cheap change signal for a tree artifact.
	TimeUpdated   time.Time `json:"time_updated,omitempty"`
	Title         string    `json:"title,omitempty"`
	Tags          []string  `json:"tags,omitempty"`
	ConsumerAppID int       `json:"consumer_app_id,omitempty"`
	FileSize      int64     `json:"file_size,omitempty"`
	Banned        bool      `json:"banned,omitempty"`
	BanReason     string    `json:"ban_reason,omitempty"`
	// UpstreamOrder is the collection's own sortorder. Recorded for display
	// and NEVER used for load order: a collection cannot know that that DAMN
	// Library has to load before the vehicle mods that need it.
	UpstreamOrder int `json:"upstream_order,omitempty"`
}

// ResolutionPath is how an entry was satisfied, as a value rather than as
// nested error handling. For CurseForge the fallback ladder IS the feature
// (section 12.8), and a typed result is what makes "resolved 247 of 250 (api
// 231, cache 12, …)" a report instead of a log archaeology exercise.
type ResolutionPath string

const (
	ResolutionAPI         ResolutionPath = "api"
	ResolutionDownloadURL ResolutionPath = "download_url"
	ResolutionCache       ResolutionPath = "cache"
	ResolutionManual      ResolutionPath = "manual"
	ResolutionMirror      ResolutionPath = "mirror_modrinth"
	ResolutionServerBoot  ResolutionPath = "server_boot"
	ResolutionVendored    ResolutionPath = "vendored"
	ResolutionAdopted     ResolutionPath = "adopted"
	// ResolutionBlocked is rung 6: the author opted out of API distribution.
	// The resolver records it and CONTINUES -- it never aborts on the first
	// blocked mod, and it never synthesizes a CDN URL (section 12.8).
	ResolutionBlocked ResolutionPath = "blocked"
)

// ResolutionPaths returns every rung, in ladder order.
func ResolutionPaths() []ResolutionPath {
	return []ResolutionPath{
		ResolutionAPI, ResolutionDownloadURL, ResolutionCache, ResolutionManual,
		ResolutionMirror, ResolutionServerBoot, ResolutionVendored, ResolutionAdopted,
		ResolutionBlocked,
	}
}

// Valid reports whether p is a known rung. An entry with an unknown one is a
// lock gvm did not write.
func (p ResolutionPath) Valid() bool {
	for _, k := range ResolutionPaths() {
		if k == p {
			return true
		}
	}
	return false
}

func (p ResolutionPath) String() string { return string(p) }

// Impact is how far a content change has to go to take effect (section 12.6).
//
// The constant ORDER is the severity order and Max depends on it: a plan's
// impact is the maximum over its changes. Reshuffling these values silently
// downgrades a rebuild to a restart, which is an archive that never gets
// taken, so the order is part of the contract rather than an implementation
// detail.
type Impact int

const (
	// ImpactNone is metadata only: a note, a label. Nothing restarts.
	ImpactNone Impact = iota
	// ImpactRestart is stop, write activation, start.
	ImpactRestart
	// ImpactDownloadCycle adds an acquisition boot between the stops.
	ImpactDownloadCycle
	// ImpactRebuild is archive-before-destroy: world-affecting, and it
	// requires typed confirmation.
	ImpactRebuild
)

// Impact strings. These are the manifest's `impact:` values, so they are the
// decode table and the encode table at once.
const (
	impactNone          = "none"
	impactRestart       = "restart"
	impactDownloadCycle = "download_cycle"
	impactRebuild       = "rebuild"
)

func (i Impact) String() string {
	switch i {
	case ImpactNone:
		return impactNone
	case ImpactRestart:
		return impactRestart
	case ImpactDownloadCycle:
		return impactDownloadCycle
	case ImpactRebuild:
		return impactRebuild
	}
	return fmt.Sprintf("impact(%d)", int(i))
}

// Valid reports whether i is one of the four.
func (i Impact) Valid() bool { return i >= ImpactNone && i <= ImpactRebuild }

// Impacts returns the four, in severity order.
func Impacts() []Impact {
	return []Impact{ImpactNone, ImpactRestart, ImpactDownloadCycle, ImpactRebuild}
}

// ParseImpact decodes a manifest `impact:` value.
//
// It returns an error and never a default. ImpactNone is the zero value, so a
// typo'd "rebuid" that fell through to the zero value would mean a
// world-affecting removal that takes no archive and asks for no confirmation
// -- the one failure in this file nobody could see until the world was gone.
func ParseImpact(s string) (Impact, error) {
	switch strings.TrimSpace(s) {
	case impactNone:
		return ImpactNone, nil
	case impactRestart:
		return ImpactRestart, nil
	case impactDownloadCycle:
		return ImpactDownloadCycle, nil
	case impactRebuild:
		return ImpactRebuild, nil
	}
	return ImpactNone, fmt.Errorf("%w: %q is not an impact (want %s|%s|%s|%s)",
		ErrInvalidSchema, s, impactNone, impactRestart, impactDownloadCycle, impactRebuild)
}

// Max returns the more severe of the two. A plan's impact is a fold of this
// over its changes.
func (i Impact) Max(o Impact) Impact {
	if o > i {
		return o
	}
	return i
}

// ChangeKind names one kind of edit, and is the key of the manifest's
// `impact:` map.
type ChangeKind string

const (
	ChangeAdd     ChangeKind = "add"
	ChangeRemove  ChangeKind = "remove"
	ChangeReorder ChangeKind = "reorder"
	ChangeUpdate  ChangeKind = "update"
	ChangeRepin   ChangeKind = "repin"
	// ChangeNote is documentation only: a note or a label moved. It is not in
	// the manifest's impact map because its impact is structurally none.
	ChangeNote ChangeKind = "note"
	// ChangeActivation is step 10's finding: the SERVER pulled a new version
	// of an already-listed item and the item's internal ids changed. That is
	// update.sh's blind second restart, replaced by its data-driven
	// descendant.
	ChangeActivation ChangeKind = "activation"
)

// ChangeKinds returns every kind, in the order a report should read them.
func ChangeKinds() []ChangeKind {
	return []ChangeKind{
		ChangeAdd, ChangeRemove, ChangeReorder, ChangeUpdate, ChangeRepin,
		ChangeNote, ChangeActivation,
	}
}

// Valid reports whether k is a known change kind.
func (k ChangeKind) Valid() bool {
	for _, c := range ChangeKinds() {
		if c == k {
			return true
		}
	}
	return false
}

func (k ChangeKind) String() string { return string(k) }

// The package's refusals. Each is a sentinel because the operator's next move
// differs, and because a caller deciding whether to retry must not have to
// match on message text.
var (
	// ErrUnaccounted is the fail-closed gate. Something in the declared set
	// could not be accounted for, so nothing is written. This is rebuild.sh's
	// RAW_COUNT != COUNT check as a type: that script refused to rewrite
	// mods.txt off a bad parse and left the file untouched, because a partial
	// list looks exactly like a deliberate one.
	ErrUnaccounted = errors.New("content: the declared set is not fully accounted for")
	// ErrVerbatim is a caller asking to RE-DERIVE an activation list that the
	// lock records as verbatim. The recorded line is an operator's hand-pruned
	// edit, or an adopted server's live Mods=; re-deriving it would discard
	// their work silently, so the derive path refuses and the caller is
	// expected to use the recorded line or run `gvm content promote`.
	ErrVerbatim = errors.New("content: activation is verbatim and must not be re-derived")
	// ErrEmptyActivation is a derived activation line with nothing in it.
	//
	// Its own sentinel because the operator's next move is unlike every other
	// refusal here: there is nothing wrong with the lock, the question is
	// whether the empty list was MEANT. An instance that declares no mods is a
	// legal state and a caller that knows so says so (ActivateAllowingEmpty);
	// an instance whose list was never read produces identical bytes, and
	// writing them disables every mod permanently on a game that rewrites its
	// own config on shutdown.
	ErrEmptyActivation = errors.New("content: the derived activation list is empty")
	// ErrInvalidLock is a lockfile gvm cannot trust: bad version, duplicate
	// ordinals, a recorded line that disagrees with the entries it was
	// supposedly rendered from.
	ErrInvalidLock = errors.New("content: invalid lockfile")
	// ErrInvalidSchema is a manifest or instance content block that cannot be
	// compiled into a usable schema -- the two namespaces sharing a write key,
	// a missing separator, an unknown impact.
	ErrInvalidSchema = errors.New("content: invalid content schema")
	// ErrNoResolver is an activation resolver name that nothing implements.
	// Closed registry, D-21: a name a manifest may declare must exist.
	ErrNoResolver = errors.New("content: no such activation resolver")
	// ErrManifestMoved is `gvm content lock --check` failing: the lock was
	// computed against a different manifest revision than the one on disk.
	ErrManifestMoved = errors.New("content: the manifest moved under the lock")
	// ErrDigest is a tree or file digest that does not match what was
	// recorded. Terminal: retrying cannot change it.
	ErrDigest = errors.New("content: digest mismatch")
)

// joinAcquire renders an acquisition line. Separate from joinActivate, with no
// shared generic helper, so that no call site can pass the wrong namespace's
// ids to the wrong key: the argument types do not match.
func joinAcquire(ids []AcquireID, sep string) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = string(id)
	}
	return strings.Join(parts, sep)
}

// joinActivate renders an activation line. See joinAcquire.
func joinActivate(ids []ActivationID, sep string) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = string(id)
	}
	return strings.Join(parts, sep)
}

// dedupeActivate removes repeats while keeping the FIRST occurrence's
// position.
//
// One workshop item legitimately contains several mods, and two items can ship
// the same library id. First-occurrence order is load order: moving a repeated
// id to its last mention would move a library below the mod that needs it.
func dedupeActivate(ids []ActivationID) []ActivationID {
	seen := make(map[ActivationID]bool, len(ids))
	out := make([]ActivationID, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
