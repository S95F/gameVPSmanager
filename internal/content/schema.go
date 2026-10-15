package content

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// AcquisitionSpec is the ACQUISITION half of one content block: how the bytes
// arrive and which config key lists what to fetch.
//
// It is a separate struct from ActivationSpec, with its own Key and its own
// Separator, rather than one struct used twice. A single `WriteKey` field
// parameterised by namespace is the shape that lets a bug write the activation
// ids under WorkshopItems=, which downloads nothing and enables nothing while
// looking entirely plausible in the ini.
type AcquisitionSpec struct {
	// Mechanism is server_boot | steamcmd_item | http. server_boot means the
	// SERVER downloads what this key lists, which is why the key is written
	// while the server is stopped (section 12.6 step 4).
	Mechanism string
	Key       string // "WorkshopItems"
	File      string // a manifest config_files[].name
	Separator string // ";" for PZ
	// KeepMissing keeps an id that has not downloaded listed, so the server
	// retries it on the next boot. Dropping it would mean an item that failed
	// once never downloads again.
	KeepMissing bool
	BootTimeout time.Duration
	// VerifyDirNonEmpty is the positive acquisition signal: a non-empty
	// directory under a declared root. rebuild.sh's NOT_DOWNLOADED check.
	VerifyDirNonEmpty bool
}

// Line renders the acquisition write. It takes AcquireIDs and nothing else, so
// there is no call site at which an activation id could arrive here.
func (a AcquisitionSpec) Line(ids []AcquireID) AcquisitionLine {
	return AcquisitionLine{
		Key:       a.Key,
		File:      a.File,
		Separator: a.Separator,
		IDs:       append([]AcquireID(nil), ids...),
	}
}

// ActivationModePresence is the activation mode in which an item is enabled by
// its presence in a content root, with no config key to write (Minecraft jars).
const ActivationModePresence = "presence"

// ActivationSpec is the ACTIVATION half: which config key enables what, and
// which resolver maps acquisition ids onto activation ids.
type ActivationSpec struct {
	// Mode is "" (key-based) or ActivationModePresence (enabled by file
	// presence in a root). Presence leaves Key, Separator and Resolver empty.
	Mode      string
	Key       string // "Mods"
	File      string
	Separator string
	// Resolver is a name from the closed registry (D-21, resolver.go).
	Resolver string
	// Vars are the manifest's resolver_vars, already rendered. A resolver sees
	// these and the declared roots and nothing else.
	Vars map[string]string
}

// Line renders the activation write from derived ids.
func (a ActivationSpec) Line(ids []ActivationID) ActivationLine {
	return ActivationLine{
		Key:       a.Key,
		File:      a.File,
		Separator: a.Separator,
		IDs:       append([]ActivationID(nil), ids...),
	}
}

// Verbatim renders the activation write from the RECORDED line, untouched.
//
// The ids are deliberately not populated: a verbatim line is a string the
// operator owns, and parsing it back into ids would invite a later caller to
// "normalise" it -- re-sorting it, de-duplicating it, dropping an id gvm does
// not recognise -- which is exactly the edit this switch exists to protect.
func (a ActivationSpec) Verbatim(recorded string) ActivationLine {
	return ActivationLine{
		Key:         a.Key,
		File:        a.File,
		Separator:   a.Separator,
		verbatim:    true,
		rawRecorded: recorded,
	}
}

// Schema is one content block, compiled and validated: the manifest's half and
// the instance's half, reconciled once so that every later step reads fields
// instead of re-deriving them from two YAML documents.
type Schema struct {
	// Block is the content block's name, which is the key both the manifest
	// and the instance use. The two halves are matched by NAME, never by
	// position: a manifest with a workshop block and an http block must not
	// have one instance list applied to whichever happens to be first.
	Block            string
	Source           SourceKind
	OrderSignificant bool

	Acquisition AcquisitionSpec
	Activation  ActivationSpec

	WorkshopAppID int
	// Roots are the declared content roots, RENDERED to absolute paths by the
	// caller. The manifest templates them and this package never renders:
	// rendering needs the instance's vars, ports and secrets, and a package
	// that could render could read a secret into a path.
	Roots          []string
	MetadataAPI    string
	CollectionSync bool
	OwnedKeys      []string

	// Impact is the manifest's map, decoded. Every declared value parsed
	// successfully or Compile failed; there is no entry here that silently
	// became ImpactNone.
	Impact map[ChangeKind]Impact

	// VerbatimActivation is the instance's activation_policy: preserve. It is
	// an INSTANCE fact, not a manifest one: adoption sets it on the one
	// instance whose live Mods= must survive, and the same manifest serves a
	// freshly created instance that derives.
	VerbatimActivation bool

	// Collection is the Steam collection id to sync from, when the instance
	// names one.
	Collection     string
	CollectionMode string

	// GameVersion is the target Minecraft version string (e.g. "1.21.4").
	// Populated by the job layer for Modrinth content blocks so the source
	// can filter and re-verify versions against the right MC release.
	GameVersion string

	// Loader is the mod loader ("fabric", "neoforge", "forge", "paper", etc.).
	// Populated alongside GameVersion for Modrinth content blocks.
	Loader string
}

// Compile reconciles the manifest's content block with the instance's list.
//
// It is where the two-namespace rules become refusals rather than review
// comments, and it runs before anything reads a file or opens a socket, so a
// mis-declared block is a load-time error on a server that is still running
// whatever it was running.
//
// roots are the block's rendered roots; the caller renders them because
// rendering is the instance's business (see Schema.Roots).
func Compile(block spec.ContentSpec, list spec.ContentList, roots []string) (*Schema, error) {
	var errs []error
	name := strings.TrimSpace(block.Name)
	if name == "" {
		errs = append(errs, fmt.Errorf("%w: a content block needs a name; it is the key the "+
			"instance's own list is matched by", ErrInvalidSchema))
	}

	src := SourceKind(strings.TrimSpace(block.Source))
	if !src.Valid() {
		errs = append(errs, fmt.Errorf("%w: %q is not a content source (want %s)",
			ErrInvalidSchema, src, strings.Join(sourceNames(), "|")))
	}
	if ls := strings.TrimSpace(list.Source); ls != "" && SourceKind(ls) != src {
		// The instance naming a different source than the manifest's block is
		// not a cosmetic mismatch: the instance's ids are in whichever
		// namespace its source uses, and resolving workshop ids as Modrinth
		// slugs would produce an empty activation list with no error.
		errs = append(errs, fmt.Errorf("%w: block %q is %s in the manifest and %s in the "+
			"instance", ErrInvalidSchema, name, src, ls))
	}

	// Presence mode: the item's existence in a content root is its activation,
	// so there is no acquisition or activation list to write to a config key.
	// Minecraft mods and plugins are jars dropped in a folder; a write_key here
	// would be a key nothing reads. Any mode other than the two known values is
	// a load-time error rather than a silently key-based block.
	presence := false
	switch strings.TrimSpace(block.Activation.Mode) {
	case "":
	case ActivationModePresence:
		presence = true
	default:
		errs = append(errs, fmt.Errorf("%w: block %q declares activation mode %q (want %q or omit)",
			ErrInvalidSchema, name, block.Activation.Mode, ActivationModePresence))
	}

	acq, aerrs := compileAcquisition(name, block.Acquisition, presence)
	errs = append(errs, aerrs...)
	act, verrs := compileActivation(name, block.Activation, presence)
	errs = append(errs, verrs...)

	// THE central check. Two namespaces, two write keys. One key cannot carry
	// both lists: whichever write landed second would overwrite the first, so
	// the server would be told either to download nothing or to enable
	// nothing, and the ini would look fine either way.
	if acq.Key != "" && acq.Key == act.Key && acq.File == act.File {
		errs = append(errs, fmt.Errorf("%w: block %q writes both the acquisition and the "+
			"activation namespace to %s in %q; they are different id namespaces and one key "+
			"cannot hold both", ErrInvalidSchema, name, acq.Key, acq.File))
	}

	impact, ierrs := compileImpact(name, block.Impact)
	errs = append(errs, ierrs...)

	if len(block.OwnedKeys) > 0 {
		// owned_keys is the declarative promise that every other key in the
		// file is provably left alone. A write key missing from it is a write
		// the surgical editor will refuse later, in the middle of a stopped
		// server's sync window, which is the worst possible time to find out.
		owned := make(map[string]bool, len(block.OwnedKeys))
		for _, k := range block.OwnedKeys {
			owned[strings.TrimSpace(k)] = true
		}
		for _, k := range []string{acq.Key, act.Key} {
			if k != "" && !owned[k] {
				errs = append(errs, fmt.Errorf("%w: block %q writes %s but does not declare it "+
					"in owned_keys, so the write would be refused", ErrInvalidSchema, name, k))
			}
		}
	}

	switch strings.TrimSpace(list.ActivationPolicy) {
	case spec.ActivationPreserve, spec.ActivationDerive, "":
	default:
		errs = append(errs, fmt.Errorf("%w: %q is not an activation policy (want %s|%s)",
			ErrInvalidSchema, list.ActivationPolicy, spec.ActivationPreserve, spec.ActivationDerive))
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	sc := &Schema{
		Block:              name,
		Source:             src,
		OrderSignificant:   block.OrderSignificant,
		Acquisition:        acq,
		Activation:         act,
		WorkshopAppID:      block.WorkshopAppID,
		Roots:              append([]string(nil), roots...),
		MetadataAPI:        strings.TrimSpace(block.MetadataAPI),
		CollectionSync:     block.CollectionSync,
		OwnedKeys:          append([]string(nil), block.OwnedKeys...),
		Impact:             impact,
		VerbatimActivation: strings.TrimSpace(list.ActivationPolicy) == spec.ActivationPreserve,
		Collection:         strings.TrimSpace(list.Collection),
		CollectionMode:     strings.TrimSpace(list.CollectionSync),
	}
	return sc, nil
}

func compileAcquisition(block string, in spec.ContentAcquisition, presence bool) (AcquisitionSpec, []error) {
	var errs []error
	out := AcquisitionSpec{
		Mechanism:         strings.TrimSpace(in.Mechanism),
		Key:               strings.TrimSpace(in.WriteKey),
		File:              strings.TrimSpace(in.WriteFile),
		Separator:         in.Separator,
		KeepMissing:       in.KeepMissing,
		BootTimeout:       time.Duration(in.BootTimeout),
		VerifyDirNonEmpty: in.Verify.DirNonEmpty,
	}
	switch out.Mechanism {
	case MechanismServerBoot, MechanismSteamCMDItem, MechanismHTTP:
	default:
		errs = append(errs, fmt.Errorf("%w: block %q declares acquisition mechanism %q (want "+
			"%s|%s|%s)", ErrInvalidSchema, block, out.Mechanism,
			MechanismServerBoot, MechanismSteamCMDItem, MechanismHTTP))
	}
	if presence {
		// No write_key, no separator: the download lands as files in the roots,
		// and there is no list to write. A key here would be a contradiction.
		return out, errs
	}
	if out.Key == "" {
		errs = append(errs, fmt.Errorf("%w: block %q declares no acquisition write_key, so "+
			"there is nothing to tell the server to download", ErrInvalidSchema, block))
	}
	errs = append(errs, checkSeparator(block, NamespaceAcquisition, out.Key, out.Separator)...)
	return out, errs
}

func compileActivation(block string, in spec.ContentActivation, presence bool) (ActivationSpec, []error) {
	var errs []error
	out := ActivationSpec{
		Mode:      strings.TrimSpace(in.Mode),
		Key:       strings.TrimSpace(in.WriteKey),
		File:      strings.TrimSpace(in.WriteFile),
		Separator: in.Separator,
		Resolver:  strings.TrimSpace(in.Resolver),
	}
	if len(in.ResolverVars) > 0 {
		out.Vars = make(map[string]string, len(in.ResolverVars))
		for k, v := range in.ResolverVars {
			out.Vars[k] = v
		}
	}
	if presence {
		// Activation is the file's presence in a root; there is no key to write
		// and no resolver to map ids. A resolver declared alongside presence is
		// a contradiction, so it is refused rather than silently ignored.
		if out.Resolver != "" {
			errs = append(errs, fmt.Errorf("%w: block %q is activation mode %q and also names a "+
				"resolver; presence activation enables by file presence, not by a derived list",
				ErrInvalidSchema, block, ActivationModePresence))
		}
		return out, errs
	}
	if out.Key == "" {
		errs = append(errs, fmt.Errorf("%w: block %q declares no activation write_key, so "+
			"nothing would ever be enabled", ErrInvalidSchema, block))
	}
	errs = append(errs, checkSeparator(block, NamespaceActivation, out.Key, out.Separator)...)

	if out.Resolver != "" {
		// D-21's closed registry, checked against the RUNTIME table rather
		// than only against the schema's name list: a manifest that validated
		// against a name nothing implements would fail in the middle of a
		// sync, after the server had already been stopped.
		if _, ok := GetResolver(out.Resolver); !ok {
			d := fmt.Errorf("%w: block %q names activation resolver %q; this build implements "+
				"%s", ErrNoResolver, block, out.Resolver, strings.Join(ResolverNames(), "|"))
			errs = append(errs, d)
		}
	}
	return out, errs
}

// checkSeparator refuses a separator that cannot render a list.
//
// An absent one is refused rather than defaulted: PZ's lists are
// semicolon-separated and a comma breaks them with NO error, which is a
// popular way to disable every mod at once by accident. Guessing here is how
// gvm would make that mistake on the operator's behalf.
func checkSeparator(block string, ns Namespace, key, sep string) []error {
	switch {
	case sep == "":
		return []error{fmt.Errorf("%w: block %q declares no %s separator for %s, and the wrong "+
			"one disables every mod with no error", ErrInvalidSchema, block, ns, key)}
	case strings.TrimSpace(sep) == "":
		return []error{fmt.Errorf("%w: block %q declares a whitespace-only %s separator for %s",
			ErrInvalidSchema, block, ns, key)}
	}
	return nil
}

// compileImpact decodes the manifest's impact map.
//
// Both halves are strict. An unknown KEY is refused because `impact: {remvoe:
// rebuild}` would leave removals at their default; an unparseable VALUE is
// refused because ImpactNone is the zero value and a typo would turn a
// world-affecting change into a no-op that takes no archive.
func compileImpact(block string, in map[string]string) (map[ChangeKind]Impact, []error) {
	if len(in) == 0 {
		return nil, nil
	}
	var errs []error
	out := make(map[ChangeKind]Impact, len(in))
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic error order
	for _, k := range keys {
		kind := ChangeKind(strings.TrimSpace(k))
		if !kind.Valid() {
			errs = append(errs, fmt.Errorf("%w: block %q declares an impact for %q, which is "+
				"not a change kind (want %s)", ErrInvalidSchema, block, k,
				strings.Join(changeKindNames(), "|")))
			continue
		}
		imp, err := ParseImpact(in[k])
		if err != nil {
			errs = append(errs, fmt.Errorf("block %q impact.%s: %w", block, k, err))
			continue
		}
		out[kind] = imp
	}
	return out, errs
}

// ImpactFor is the manifest's declared impact for one change kind.
//
// A kind the manifest says nothing about falls back to ImpactRestart rather
// than ImpactNone: the conservative direction is the one that stops the server
// and writes the list, and a content change that silently took effect without
// one is a server running a list it never loaded.
func (s *Schema) ImpactFor(k ChangeKind) Impact {
	if k == ChangeNote {
		// Structural, not declarable: a note is documentation. The manifest
		// cannot escalate it, because there is nothing for a restart to do.
		return ImpactNone
	}
	if s != nil {
		if imp, ok := s.Impact[k]; ok {
			return imp
		}
	}
	return ImpactRestart
}

// ServerBoot reports whether the SERVER acquires its own content. It is the
// condition behind section 12.6's escalation: an unmaterialized item on such a
// source needs a boot between the stops, because the bytes are not there yet
// and nothing else will fetch them.
func (s *Schema) ServerBoot() bool {
	return s != nil && s.Acquisition.Mechanism == MechanismServerBoot
}

// NeedsSteamCMDLock reports whether a sync of this block must take the
// host-wide steamcmd lock (D-30).
//
// Only mechanism steamcmd_item does. PZ's server_boot needs no steamcmd at
// all, so N Project Zomboid instances can sync concurrently -- taking the host
// lock for them would serialise a 40-minute window for no reason.
func (s *Schema) NeedsSteamCMDLock() bool {
	return s != nil && s.Acquisition.Mechanism == MechanismSteamCMDItem
}

func sourceNames() []string {
	out := make([]string, 0, len(SourceKinds()))
	for _, k := range SourceKinds() {
		out = append(out, string(k))
	}
	return out
}

func changeKindNames() []string {
	out := make([]string, 0, len(ChangeKinds()))
	for _, k := range ChangeKinds() {
		out = append(out, string(k))
	}
	return out
}
