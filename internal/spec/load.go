package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Well-known control-plane locations (section 5). They are constants rather than
// build-time variables because an operator who moves them has moved half the
// system; the packaging owns these paths.
const (
	EtcDir          = "/etc/gvm"
	EtcGamesDir     = "/etc/gvm/games"
	EtcInstancesDir = "/etc/gvm/instances"
	ShareGamesDir   = "/usr/share/gvm/games"
)

var (
	// ErrManifestNotFound means no source has a file for this id at all.
	ErrManifestNotFound = errors.New("no manifest for game id")

	// ErrNoUsableManifest means files exist but none may be used — every
	// candidate was an unapproved override (D-19).
	ErrNoUsableManifest = errors.New("no usable manifest")

	// ErrOverrideUnapproved is what an OverrideAuthorizer returns for an
	// /etc/gvm/games override with no recorded sha256 approval (D-19).
	ErrOverrideUnapproved = errors.New("manifest override is not approved")

	// ErrOverrideShaMismatch is what PinAuthorizer returns for an override whose
	// bytes are not the ones a pin recorded: the file changed after create.
	ErrOverrideShaMismatch = errors.New("manifest override does not match the pin")

	// ErrBadGameID means the id could not name a manifest file.
	ErrBadGameID = errors.New("invalid game id")

	// ErrNullListElement means a YAML sequence contained an empty element,
	// which yaml.v3 would silently drop.
	ErrNullListElement = errors.New("empty list element")
)

// gameIDRe is check 1 of section 8.6, and is also the only thing standing
// between a caller-supplied id and the filesystem: an id is concatenated with
// ".yaml" and looked up in every source, so "../../etc/shadow" must never get
// as far as a read.
var gameIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,31}$`)

// ValidGameID reports whether s is a legal metadata.id.
func ValidGameID(s string) bool { return gameIDRe.MatchString(s) }

// OverrideAuthorizer decides whether an operator override in /etc/gvm/games may
// be used. It is satisfied by the store (which checks game_manifest_approvals)
// or by RootCLIAuthorizer when the operator passes --unapproved-override.
//
// A manifest is argv plus env plus destruction globs, so publishing one is
// owner-only and gated by a recorded approval of the exact sha256 (D-19).
type OverrideAuthorizer interface {
	ApproveOverride(id, sha256 string) error
}

// RootCLIAuthorizer approves any override. It exists for one caller: a root
// operator who passed --unapproved-override, having been shown the sha.
type RootCLIAuthorizer struct{}

// ApproveOverride approves unconditionally.
func (RootCLIAuthorizer) ApproveOverride(string, string) error { return nil }

// DenyOverrides refuses every override. It is what a nil authorizer means, so
// "no approval system was wired up" resolves to the packaged manifest with a
// warning rather than to an unchecked override.
type DenyOverrides struct{}

// ApproveOverride always refuses.
func (DenyOverrides) ApproveOverride(id, sha string) error {
	return fmt.Errorf("%w: %s (sha256 %s)", ErrOverrideUnapproved, id, shortSHA(sha))
}

// PinAuthorizer approves exactly the override bytes a pin already recorded, and
// no others. It exists for the first `gvm apply` after `gvm create
// --unapproved-override`: root authorised those exact bytes at create, the pin
// in the instance YAML records their sha256, and apply re-authorises nothing
// new — an override edited after create has a different sha and is refused, so
// the D-17 pin and the D-19 approval gate are both intact (U11). A zero SHA256
// approves nothing.
type PinAuthorizer struct{ SHA256 string }

// ApproveOverride approves iff the override's sha256 equals the pin's.
func (p PinAuthorizer) ApproveOverride(_, sha string) error {
	if p.SHA256 != "" && strings.EqualFold(sha, p.SHA256) {
		return nil
	}
	return fmt.Errorf("%w: sha256 %s differs from the pin; the file changed after create",
		ErrOverrideShaMismatch, shortSHA(sha))
}

// Source is one place manifests come from. FS is rooted at the directory that
// holds <id>.yaml, so the embedded floor and a real directory are the same shape
// and the resolution order is testable without touching /etc.
type Source struct {
	Origin Origin
	Ref    string // what a human sees: "/etc/gvm/games", "embedded games"
	FS     fs.FS

	osDir bool // Ref is an OS directory, so display joins with the OS separator
}

// DirSource is a source backed by a real directory.
func DirSource(origin Origin, dir string) Source {
	return Source{Origin: origin, Ref: dir, FS: os.DirFS(dir), osDir: true}
}

// EmbeddedSource is the floor: the manifests compiled into the binary. It is
// always present so `gvm doctor` and `gvm adopt discover` still work on a box
// whose /usr/share was mangled.
func EmbeddedSource(fsys fs.FS, ref string) Source {
	if ref == "" {
		ref = "embedded"
	}
	return Source{Origin: OriginEmbedded, Ref: ref, FS: fsys}
}

func (s Source) refFor(id string) string {
	name := id + ".yaml"
	if s.osDir {
		return filepath.Join(s.Ref, name)
	}
	if s.Ref == "" {
		return name
	}
	return s.Ref + "/" + name
}

// Catalog resolves a game id across the three shadowing layers.
type Catalog struct {
	sources []Source
}

// NewCatalog orders sources by Origin.Rank, so callers cannot get the shadowing
// order wrong by listing them in the wrong order. Sources with no FS are
// dropped: the embedded floor does not exist until the games/ tree is compiled
// in, and a nil FS there must degrade to "one fewer layer", not to a panic.
func NewCatalog(sources ...Source) *Catalog {
	c := &Catalog{}
	for _, s := range sources {
		if s.FS == nil {
			continue
		}
		c.sources = append(c.sources, s)
	}
	sort.SliceStable(c.sources, func(i, j int) bool {
		return c.sources[i].Origin.Rank() > c.sources[j].Origin.Rank()
	})
	return c
}

// SystemCatalog is the catalog of a real box: operator override, then the
// packaged manifests, then the embedded floor. Pass a nil embedded FS before the
// games/ tree exists.
func SystemCatalog(embedded fs.FS) *Catalog {
	return NewCatalog(
		DirSource(OriginOverride, EtcGamesDir),
		DirSource(OriginPackage, ShareGamesDir),
		EmbeddedSource(embedded, "embedded games"),
	)
}

// Candidate is one layer's answer for a game id, decoded and checksummed. It is
// what `gvm game which` prints: the winner AND the losers, because three
// shadowing layers with no way to ask which one won is a 2am trap.
type Candidate struct {
	Origin   Origin
	Ref      string // the file, as a human should see it
	SHA256   string
	Raw      []byte
	Revision int

	// Manifest is nil when the file did not decode; Err says why and Diags say
	// where. They are held here rather than returned with the rest because a
	// broken layer that never gets consulted must not make the winner's
	// diagnostics look like errors.
	Manifest *Manifest
	Err      error
	Diags    Diags

	// Skipped is non-empty when the file decoded but may not be used, and says
	// why in one line. Only an override is ever skipped (D-19).
	Skipped string

	Winner bool
}

// Resolve returns every layer's candidate for id, highest rank first, annotated
// with which one wins and why the others did not.
//
// auth may be nil, which means DenyOverrides.
func (c *Catalog) Resolve(id string, auth OverrideAuthorizer) ([]Candidate, Diags, error) {
	if !ValidGameID(id) {
		return nil, nil, fmt.Errorf("%w: %q: want %s", ErrBadGameID, id, gameIDRe)
	}
	if auth == nil {
		auth = DenyOverrides{}
	}

	var (
		cands []Candidate
		diags Diags
	)
	for _, s := range c.sources {
		ref := s.refFor(id)
		raw, err := fs.ReadFile(s.FS, id+".yaml")
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			// An unreadable layer is not a reason to refuse to run a game that a
			// lower layer can still supply: /etc being unreadable must not take
			// the packaged manifest down with it.
			diags = append(diags, Diag{
				File: ref, Sev: SevWarning,
				Msg: fmt.Sprintf("cannot be read (%v); this layer is skipped", err),
			})
			continue
		}

		cd := Candidate{Origin: s.Origin, Ref: ref, Raw: raw, SHA256: sha256Hex(raw)}
		m, mDiags, err := parseManifest(ref, raw)
		cd.Diags = mDiags
		if err != nil {
			cd.Err = err
		} else {
			m.Origin = s.Origin
			cd.Manifest = m
			cd.Revision = m.Metadata.Revision
		}
		cands = append(cands, cd)
	}
	if len(cands) == 0 {
		return nil, diags, fmt.Errorf("%w: %q", ErrManifestNotFound, id)
	}

	for i := range cands {
		cd := &cands[i]
		if cd.Err != nil {
			// Deliberately NOT a fall-through to the packaged manifest. An
			// unapproved override is skipped because approval depends on a
			// database that may legitimately be down; a syntax error is the
			// operator's own edit, visible to them, and quietly running
			// different argv instead is the surprise this design exists to
			// prevent. Live instances are unaffected either way: they run the
			// pinned copy (D-17).
			return cands, append(diags, cd.Diags...), fmt.Errorf("%s: %w", cd.Ref, cd.Err)
		}
		if cd.Origin == OriginOverride {
			if err := auth.ApproveOverride(id, cd.SHA256); err != nil {
				cd.Skipped = overrideSkipReason(err)
				diags = append(diags, Diag{
					File: cd.Ref, Sev: SevWarning,
					Msg: fmt.Sprintf("override %s: %s; using the next layer instead",
						shortSHA(cd.SHA256), cd.Skipped),
					Fix: "approving a custom manifest is not available in this build; run " +
						"`sudo gvm create <name> --game " + id + " --unapproved-override` to " +
						"authorise these exact bytes for one instance",
				})
				continue
			}
		}
		cd.Winner = true
		return cands, diags, nil
	}
	return cands, diags, fmt.Errorf("%w: %q: every candidate was skipped", ErrNoUsableManifest, id)
}

// Load returns the winning manifest for id, with its provenance filled in.
//
// Callers MUST check diags.HasErrors() even when err is nil: a manifest can
// decode cleanly and still carry findings that must stop an apply.
func (c *Catalog) Load(id string, auth OverrideAuthorizer) (*Manifest, Diags, error) {
	cands, diags, err := c.Resolve(id, auth)
	if err != nil {
		return nil, diags, err
	}
	for i := range cands {
		if cands[i].Winner {
			return cands[i].Manifest, diags, nil
		}
	}
	return nil, diags, fmt.Errorf("%w: %q", ErrNoUsableManifest, id)
}

// List returns every game id any source offers, sorted and de-duplicated.
//
// A file that is not <id>.yaml for a legal id is ignored rather than reported:
// the directory also holds whatever an operator's editor left behind, and a
// half-written .swp file is not a catalog error.
func (c *Catalog) List() ([]string, error) {
	seen := map[string]bool{}
	var firstErr error
	for _, s := range c.sources {
		ents, err := fs.ReadDir(s.FS, ".")
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", s.Ref, err)
			}
			continue
		}
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			id, ok := strings.CutSuffix(e.Name(), ".yaml")
			if !ok || !ValidGameID(id) {
				continue
			}
			seen[id] = true
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, firstErr
}

// parseManifest is the one decode path: catalog, pin and `gvm game validate`
// all go through it, so a file that is refused in one place is refused in all
// three.
func parseManifest(ref string, raw []byte) (*Manifest, Diags, error) {
	m, _, diags, err := parseManifestIndexed(ref, raw)
	return m, diags, err
}

// parseManifestIndexed is parseManifest, keeping the position index. Validation
// needs it — every diagnostic of section 8.6 carries a line and column — and
// building it a second time would parse the file twice for geometry that was
// just thrown away.
func parseManifestIndexed(ref string, raw []byte) (*Manifest, *NodeIndex, Diags, error) {
	ix, err := IndexBytes(ref, raw)
	if err != nil {
		return nil, nil, Diags{{File: ref, Sev: SevError, Msg: err.Error()}}, err
	}
	if diags, err := nullListDiags(ix); err != nil {
		return nil, ix, diags, err
	}

	var m Manifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, ix, Diags{{File: ref, Sev: SevError, Msg: err.Error()}}, err
	}

	// The template guard runs at LOAD, not at render. A manifest whose argv
	// contains {{env "AWS_SECRET"}} must never become a usable *Manifest that a
	// later step could execute; refusing it here means catalog, pin and
	// `gvm game validate` all refuse the same file (D-21).
	if diags := GuardDocument(ix); len(diags) > 0 {
		return nil, ix, diags, fmt.Errorf("%s: %s: %w", ref, diags[0].Path, ErrTemplate)
	}

	m.SourceRef = ref
	m.SHA256 = sha256Hex(raw)
	m.Raw = bytes.Clone(raw)
	return &m, ix, nil, nil
}

// LoadInstance reads one /etc/gvm/instances/<name>.yaml. It shares the null-list
// guard with the manifest path for the same reason: an acquire list that loses
// an element does not fail, it silently unloads a mod.
func LoadInstance(path string) (*Instance, Diags, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	return ParseInstance(path, raw)
}

// ParseInstance is LoadInstance without the read.
//
// It exists because the readers of an instance spec are not all holding a
// filesystem: the lifecycle observer reaches /etc through an injected seam so
// that its logic is testable on the Windows development machine, and a loader
// that could only start from a path would have forced it to fake one. ref is
// what diagnostics are attributed to and is normally the path.
func ParseInstance(ref string, raw []byte) (*Instance, Diags, error) {
	path := ref
	ix, err := IndexBytes(path, raw)
	if err != nil {
		return nil, Diags{{File: path, Sev: SevError, Msg: err.Error()}}, err
	}
	if diags, err := nullListDiags(ix); err != nil {
		return nil, diags, err
	}
	var in Instance
	if err := yaml.Unmarshal(raw, &in); err != nil {
		return nil, Diags{{File: path, Sev: SevError, Msg: err.Error()}}, err
	}
	return &in, nil, nil
}

// nullListDiags turns every empty sequence element in a document into a
// positioned error. It is a refusal rather than a warning because the decoded
// struct provably is not what the file says: there is no reading of a dropped
// port, argv element or acquire id that is safe to continue from.
func nullListDiags(ix *NodeIndex) (Diags, error) {
	nulls := ix.NullSeqEntries()
	if len(nulls) == 0 {
		return nil, nil
	}
	var diags Diags
	for _, p := range nulls {
		diags = append(diags, ix.Errorf(p, "list element is empty").
			WithFix(`yaml drops an empty element silently; delete the line, or write "" if a blank value is meant`))
	}
	return diags, fmt.Errorf("%s: %s: %w", ix.File, nulls[0], ErrNullListElement)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// shortSHA is what goes in operator-facing text. The full digest is available
// wherever a decision is recorded; twelve hex characters is enough to compare
// two lines of `gvm game which` by eye and short enough that people read it.
func shortSHA(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

func overrideSkipReason(err error) string {
	if errors.Is(err, ErrOverrideUnapproved) {
		return "not approved"
	}
	if errors.Is(err, ErrOverrideShaMismatch) {
		return "sha256 differs from the pin: the file changed after create"
	}
	// The approval check itself failed — Postgres down, socket gone. D-19 wants
	// the box to keep booting its games off the packaged manifest, so this is
	// the same outcome with a different sentence.
	return "approval could not be checked: " + err.Error()
}
