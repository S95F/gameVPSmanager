package steamworkshop

import (
	"context"
	"fmt"
	"log/slog"
	gopath "path"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/content"
)

// Source is the Steam Workshop content source.
//
// The compile-time assertion is the point of this line: if a phase's signature
// drifts from section 12.2's contract, this file stops building rather than
// the job layer silently calling a method that no longer means what it says.
var _ content.Source = (*Source)(nil)

// Evidence an entry carries about its UPSTREAM state, as distinct from its
// files ("modinfo:…") and its Workshop tags ("tags:…").
//
// All three of these are recorded and NONE of them removes an item. That is
// the same rule as the tag table's, extended to the rest of the metadata:
// gvm does not delete a mod the operator declared because a web service had
// an opinion about it. The operator removes it, with `gvm content remove`,
// having read the report.
const (
	// EvidenceBanned is banned:true on the item's Workshop page.
	EvidenceBanned = "upstream:banned"
	// EvidenceNotFound is result 9: private, removed, or never published.
	EvidenceNotFound = "upstream:not-found"
	// EvidenceWrongApp is a consumer_app_id that is not this game's workshop
	// appid. A hard stop for a COLLECTION rewrite (that is a mistyped
	// collection id); evidence for an item the operator declared by hand,
	// because the files on disk are what the server will actually load.
	EvidenceWrongApp = "upstream:wrong-app"
)

// New builds the Workshop source.
func New(cfg Config) (*Source, error) {
	s := &Source{appID: cfg.WorkshopAppID, log: cfg.Log}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if cfg.Offline {
		// No client at all, rather than a client nobody calls: a source that
		// cannot construct a request cannot make one by accident.
		if cfg.WorkshopAppID <= 0 {
			return nil, fmt.Errorf("%w: no workshop app id", ErrConfig)
		}
		return s, nil
	}
	api, err := NewAPI(cfg)
	if err != nil {
		return nil, err
	}
	s.api = api
	return s, nil
}

// Source implements content.Source over a Steam Workshop block.
type Source struct {
	// api is nil in Offline mode, which is section 12.5's documented
	// degradation asked for on purpose rather than an error path.
	api   *API
	appID int
	log   *slog.Logger
}

// Kind is the registry's name for this source.
func (s *Source) Kind() content.SourceKind { return content.KindSteamWorkshop }

// API is the metadata client, or nil in offline mode.
func (s *Source) API() *API { return s.api }

// schemaOf validates what a caller handed over, once, for every phase.
func (s *Source) schemaOf(in *content.Input) (*content.Schema, error) {
	if in == nil {
		return nil, fmt.Errorf("%w: no input", ErrInput)
	}
	sc := in.Schema
	if sc == nil {
		return nil, fmt.Errorf("%w: no compiled schema, so there are no write keys and no "+
			"resolver to work with", ErrInput)
	}
	if sc.Source != content.KindSteamWorkshop {
		return nil, fmt.Errorf("%w: block %q is a %s block and this is the %s source",
			ErrInput, sc.Block, sc.Source, content.KindSteamWorkshop)
	}
	switch sc.Acquisition.Mechanism {
	case content.MechanismServerBoot:
	case content.MechanismSteamCMDItem:
		// Named rather than approximated. workshop_download_item needs the
		// host-wide steamcmd lock (D-30), the anonymous-UGC opt-in check, and
		// the "succeeded but produced nothing" guard from
		// docs/research/steamcmd.md -- none of which this build has. Pretending
		// otherwise would mean a plan that claims to acquire and does not.
		return nil, fmt.Errorf("%w: block %q declares %s, which this build does not implement "+
			"for the Workshop; %s is the shipped mechanism (the server downloads what the %s "+
			"key lists)", ErrMechanism, sc.Block, content.MechanismSteamCMDItem,
			content.MechanismServerBoot, sc.Acquisition.Key)
	default:
		return nil, fmt.Errorf("%w: block %q declares acquisition mechanism %q", ErrMechanism,
			sc.Block, sc.Acquisition.Mechanism)
	}
	return sc, nil
}

// Resolve performs network READS and writes nothing, so it is safe against a
// live server with players online (section 12.2).
//
// The order of the three readings is the design. The DISK is read first and
// unconditionally, because it is the only authority on what the server will
// actually load. The API is read second and OPTIONALLY: a failure is logged,
// recorded as a warning, and otherwise ignored, so an unreachable Steam
// degrades this to exactly the mod.info heuristic that already works offline.
// The RESOLVER runs last, over both, and it is the half that decides which
// variant inside each item is this build's.
//
// Every declared Ref is accounted for: a duplicate id, an id that is not a
// publishedfileid, and an item the resolver could say nothing about all land in
// Plan.Unaccounted, which closes the gate rather than shortening the list.
func (s *Source) Resolve(ctx context.Context, in *content.Input, want []content.Ref) (
	*content.Plan, error) {

	sc, err := s.schemaOf(in)
	if err != nil {
		return nil, err
	}
	if in.Live == nil {
		// Observe tolerates this; Resolve must not. A plan is a statement
		// about what has to CHANGE, and a plan computed without knowing what
		// is live would read an unread config as an empty one -- which on an
		// adopted server means every mod looks newly added and the activation
		// line looks like it needs writing from scratch.
		return nil, fmt.Errorf("%w: block %q was resolved without a reading of the live %s and "+
			"%s values; an unread config and an empty one are the same bytes, and only one of "+
			"them is a reason to write anything", ErrInput, sc.Block, sc.Acquisition.Key,
			sc.Activation.Key)
	}

	p := &content.Plan{
		Block:    sc.Block,
		Source:   content.KindSteamWorkshop,
		Declared: len(want),
	}

	// Pass one: classify the declared refs themselves, before any IO. A typo'd
	// id and a doubled id are both refusals, and both are cheaper to find here
	// than after a six-gigabyte walk.
	refs := make([]content.Ref, 0, len(want))
	ordinals := make(map[content.AcquireID]int, len(want))
	// first records the declared position PLUS ONE, so the zero value means
	// "not seen". Storing the position itself would make position 0 -- the
	// library that has to load before everything else -- invisible to the
	// duplicate check.
	first := make(map[content.AcquireID]int, len(want))
	for i, ref := range want {
		id := content.RefID(ref)
		switch {
		case id == "":
			p.Unaccounted = append(p.Unaccounted, content.Unaccounted{
				Reason: fmt.Sprintf("the item declared at position %d has no id", i),
				Hint:   "remove the entry, or give it a Steam Workshop publishedfileid",
			})
		case !numericID(string(id)):
			p.Unaccounted = append(p.Unaccounted, content.Unaccounted{
				ID: id,
				Reason: "it is not a Steam Workshop publishedfileid, which is the numeric id " +
					"out of the item's URL",
				Hint: "the acquisition namespace for this block is numeric; the string ids " +
					"that look like mod names belong to the ACTIVATION namespace and gvm " +
					"derives those itself",
			})
		case first[id] > 0:
			p.Unaccounted = append(p.Unaccounted, content.Unaccounted{
				ID: id,
				Reason: fmt.Sprintf("it is declared twice, at positions %d and %d",
					first[id]-1, i),
				Hint: "two entries for one id make the derived activation list depend on " +
					"which one is read, and the two can carry different notes and requires",
			})
		default:
			first[id] = i + 1
			ordinals[id] = i
			refs = append(refs, ref)
		}
	}

	// Pass two: the disk.
	obs, err := s.Observe(ctx, in)
	if err != nil {
		return nil, err
	}
	p.Observed = obs

	// Pass three: the metadata, best effort.
	ids := make([]content.AcquireID, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, content.RefID(ref))
	}
	up := s.upstream(ctx, in, ids, p)

	// Pass four: the resolver, over the two.
	items := make([]content.ResolverItem, 0, len(refs))
	for _, ref := range refs {
		id := content.RefID(ref)
		items = append(items, content.ResolverItem{
			Ordinal:  ordinals[id],
			ID:       id,
			Pin:      ref.Pin,
			Vendored: ref.Vendored,
			Upstream: up[id],
		})
	}
	var res *content.ResolverResult
	if len(items) > 0 {
		res, err = content.Resolve(ctx, sc, items, in.Roots)
		if err != nil {
			return nil, err
		}
		p.Unaccounted = append(p.Unaccounted, res.Unaccounted...)
		p.Warnings = append(p.Warnings, res.Warnings...)
	}

	refused := make(map[content.AcquireID]bool, len(p.Unaccounted))
	for _, u := range p.Unaccounted {
		if u.ID != "" {
			refused[u.ID] = true
		}
	}

	byID := map[content.AcquireID]content.ResolvedItem{}
	if res != nil {
		for _, it := range res.Items {
			byID[it.ID] = it
		}
	}

	for _, ref := range refs {
		id := content.RefID(ref)
		if refused[id] {
			continue
		}
		it, ok := byID[id]
		if !ok {
			continue // the wrapper's cross-check already recorded a reason
		}
		e := content.PlanEntry{
			Ordinal:      ordinals[id],
			Ref:          ref,
			Activation:   it.Activation,
			Dropped:      it.Dropped,
			Evidence:     it.Evidence,
			Ambiguous:    it.Ambiguous,
			Materialized: !it.Missing,
			Resolution:   resolutionFor(sc, ref),
			Upstream:     up[id],
		}
		e.Evidence, p.Warnings = s.upstreamEvidence(e.Evidence, p.Warnings, id, up[id])
		if e.Materialized {
			art, err := itemArtifact(ctx, in.Roots, id, it.Path)
			if err != nil {
				return nil, err
			}
			e.Artifact = art
			p.Bytes += art.Size
		}
		p.Entries = append(p.Entries, e)
	}

	p.Changes = planChanges(p, obs, sc)
	p.ComputeImpact(sc)
	p.Steps = planSteps(p, sc)
	return p, nil
}

// upstream fetches metadata and never lets a failure matter.
//
// Section 12.5's rule in one function: whatever the caller already holds wins
// (it may have come from a batch somebody else paid for), the API fills the
// gaps, and an API that cannot be reached leaves the map short. A short map is
// a resolve that degrades to the mod.info heuristic, which is exactly today's
// behaviour -- Steam being down must not change which mods a server enables.
func (s *Source) upstream(ctx context.Context, in *content.Input, ids []content.AcquireID,
	p *content.Plan) map[content.AcquireID]*content.Upstream {

	out := make(map[content.AcquireID]*content.Upstream, len(ids))
	var missing []content.AcquireID
	for _, id := range ids {
		if u := in.UpstreamFor(id); u != nil {
			out[id] = u
			continue
		}
		missing = append(missing, id)
	}
	if len(missing) == 0 {
		return out
	}
	if s.api == nil {
		p.Warnings = append(p.Warnings, fmt.Sprintf("no Workshop metadata was fetched for %d "+
			"item(s): this source is offline, so every item was scored from its downloaded "+
			"files alone", len(missing)))
		return out
	}

	det, err := s.api.Details(ctx, missing)
	if err != nil {
		// Logged and recorded, never returned. This is the whole degradation.
		s.log.Warn("workshop metadata unavailable; scoring from the downloaded files alone",
			"block", p.Block, "items", len(missing), "error", err)
		p.Warnings = append(p.Warnings, fmt.Sprintf("Workshop metadata could not be fetched "+
			"for %d item(s), so they were scored from their downloaded files alone (which is "+
			"the offline behaviour, not a failure): %v", len(missing), err))
		return out
	}
	for id, u := range det.Upstream {
		out[id] = u
	}
	// Requested order, not map order. These sentences land in `gvm content
	// sync --dry-run` and in the --json payload, and an operator diffing two
	// consecutive dry-runs of an unchanged instance must see no change -- Go
	// randomises map iteration, so ranging det.Refused directly produced a
	// different order most runs. det.Absent below is a slice for the same
	// reason. det.Refused stays a map because Reason indexes it by id.
	for _, id := range missing {
		if _, ok := det.Refused[id]; !ok {
			continue
		}
		p.Warnings = append(p.Warnings, fmt.Sprintf("%s: %s. It stays declared and stays "+
			"listed -- gvm does not remove an item the operator asked for on the strength of "+
			"an API answer", id, det.Reason(id)))
	}
	for _, id := range det.Absent {
		p.Warnings = append(p.Warnings, fmt.Sprintf("%s: the Workshop answered the batch "+
			"without mentioning it, so it was scored from its downloaded files alone", id))
	}
	return out
}

// upstreamEvidence records what the metadata said about one item.
//
// Evidence, plus a sentence for the report. Never a refusal and never a
// removal: see the comment on the Evidence constants.
func (s *Source) upstreamEvidence(evidence, warnings []string, id content.AcquireID,
	u *content.Upstream) ([]string, []string) {

	if u == nil {
		return evidence, warnings
	}
	if u.Banned {
		evidence = append(evidence, EvidenceBanned)
		reason := strings.TrimSpace(u.BanReason)
		if reason == "" {
			reason = "no reason given"
		}
		warnings = append(warnings, fmt.Sprintf("%s is BANNED on the Workshop (%s). Its bytes "+
			"may already be on disk and will still load; it will not download again, and gvm "+
			"will not remove it for you", id, reason))
	}
	if u.ConsumerAppID != 0 && s.appID > 0 && u.ConsumerAppID != s.appID {
		evidence = append(evidence, EvidenceWrongApp)
		warnings = append(warnings, fmt.Sprintf("%s belongs to app %d and this game's workshop "+
			"appid is %d. Check the id -- it is declared by hand, so this is most likely a "+
			"copy-paste from another game's collection", id, u.ConsumerAppID, s.appID))
	}
	return evidence, warnings
}

// resolutionFor records HOW an entry's bytes arrive, as a value rather than as
// a comment somewhere.
func resolutionFor(sc *content.Schema, ref content.Ref) content.ResolutionPath {
	if ref.Vendored {
		// Operator bytes: never fetched, never pruned.
		return content.ResolutionVendored
	}
	if sc.ServerBoot() {
		// The SERVER downloads these. gvm never has the bytes, which is why
		// the artifact is a tree with no digest until `verify --deep`.
		return content.ResolutionServerBoot
	}
	return content.ResolutionAPI
}

// itemArtifact describes one acquired item's tree.
//
// Presence and non-emptiness, counted and sized, with NO digest: gvm did not
// download these bytes and cannot claim to have verified them. Recording an
// empty Digest rather than a plausible-looking one is what keeps an
// unverifiable identity from being mistaken for a verified one -- the tree
// digest is `gvm content verify --deep`, and it is minutes of IO over a 6 GB
// workshop tree.
func itemArtifact(ctx context.Context, roots []content.Root, id content.AcquireID,
	path string) (*content.Artifact, error) {

	for _, r := range roots {
		if r.FS == nil {
			continue
		}
		// An EXACT match on the path the resolver built, not a prefix test. A
		// prefix test matches root "/srv/x" against an item under "/srv/xy",
		// which would size an item against a filesystem handle rooted
		// somewhere else and report zero members for a tree that is fully
		// downloaded.
		if path != gopath.Join(r.Name, string(id)) {
			continue
		}
		t, err := content.VerifyTree(ctx, r.FS, string(id), "", false)
		if err != nil {
			return nil, fmt.Errorf("verify acquired item %s under %s: %w", id, r.Name, err)
		}
		return &content.Artifact{
			Kind:    content.ArtifactTree,
			Path:    path,
			Size:    t.Bytes,
			Members: t.Members,
		}, nil
	}
	// The resolver found it and this walk cannot: a root was swapped out
	// underneath the resolve. Refused rather than recorded without a size,
	// because the size is the disk pre-flight.
	return nil, fmt.Errorf("%w: item %s resolved to %q, which is under none of the declared "+
		"roots", ErrInput, id, path)
}

// Materialize records the plan as recorded intent, and writes no bytes.
//
// For a server_boot acquisition that is not a shortcut, it is the truth: the
// SERVER downloads its own items from the acquisition key, so acquiring means
// writing that key while stopped and then booting (section 12.6 steps 4-5),
// and both of those belong to the job layer that holds the instance lock.
// There is nothing here for in.Jail to bound, and nothing that could accept an
// unverified byte, because no byte is accepted at all.
//
// What it does do is CHECK AGAIN. Resolve may have run minutes ago with players
// online, and an item that was on disk then can be gone now -- a mod unsubscribed
// in-game, a disk that filled, a root unmounted. Every entry the plan calls
// materialized is re-verified for presence and non-emptiness here, at the
// moment the intent is recorded.
func (s *Source) Materialize(ctx context.Context, in *content.Input, p *content.Plan,
	pr content.Progress) (*content.Lock, error) {

	// The gate, first statement, before anything is read or recorded.
	if err := content.Materializable(p); err != nil {
		return nil, err
	}
	sc, err := s.schemaOf(in)
	if err != nil {
		return nil, err
	}
	if p.Block != sc.Block {
		return nil, fmt.Errorf("%w: a plan for block %q cannot be materialized against block "+
			"%q", ErrInput, p.Block, sc.Block)
	}

	pr.Step(fmt.Sprintf("verifying %d acquired item(s) under %d root(s)", len(p.Entries),
		len(in.Roots)))
	var done int64
	for i := range p.Entries {
		e := &p.Entries[i]
		if !e.Materialized {
			// Not an error: on a server_boot source the id stays listed so the
			// server retries the download on its next boot, and keep_missing
			// is what makes that possible.
			continue
		}
		if e.Artifact == nil {
			return nil, fmt.Errorf("%w: entry %s is recorded as materialized with no artifact, "+
				"so there is nothing to re-verify", ErrInput, e.AcquireID())
		}
		art, err := itemArtifact(ctx, in.Roots, e.AcquireID(), e.Artifact.Path)
		if err != nil {
			return nil, fmt.Errorf("%w: %s was on disk when the plan was made and is not now; "+
				"nothing was written: %w", content.ErrUnaccounted, e.AcquireID(), err)
		}
		e.Artifact = art
		done += art.Size
		pr.Bytes(done, p.Bytes)
	}

	sl, err := p.Lock(sc)
	if err != nil {
		return nil, err
	}
	if sc.VerbatimActivation {
		// The adoption switch, recorded. The live line is the ONLY copy of the
		// operator's list -- the entries cannot reproduce a hand-pruned
		// ordering -- so a lock that recorded nothing here would be a lock
		// whose Activate refuses, or worse, writes an empty Mods=.
		live := ""
		if in.Live != nil {
			live = in.Live.Activation
		}
		if strings.TrimSpace(live) == "" {
			return nil, fmt.Errorf("%w: block %q is set to preserve the live %s line and there "+
				"is nothing to preserve; the live list must be recorded first (writing the "+
				"empty line would disable every mod on an adopted server), and importing it is "+
				"not available in this build", content.ErrInvalidLock, sc.Block, sc.Activation.Key)
		}
		sl.ActivationLine = live
	}

	l := content.NewLock(in)
	if err := l.Put(sc.Block, sl); err != nil {
		return nil, err
	}
	if err := l.ValidateAgainst(sc); err != nil {
		return nil, err
	}
	pr.Step(fmt.Sprintf("recorded %d item(s) as intent", len(sl.Entries)))
	return l, nil
}

// Activate maps the materialized set into the game's own identifiers.
//
// It delegates to content.Activate and adds nothing. The two rules there -- the
// fail-closed gate and verbatim activation -- are the ones a per-source copy
// would drift on, and the consequence of drifting on the second one is an
// operator's hand-pruned Mods= replaced without being asked.
func (s *Source) Activate(_ context.Context, in *content.Input, l *content.Lock) (
	*content.Activation, error) {

	sc, err := s.schemaOf(in)
	if err != nil {
		return nil, err
	}
	return content.Activate(sc, l)
}

// planChanges diffs the declared list against what is LIVE.
//
// Against observed rather than against the lock, deliberately: the question
// Resolve answers is "does anything have to stop", and only the live config and
// the disk can answer that. A lock is recorded intent, and an instance can
// disagree with its own lock in both directions -- PZ rewrites its ini on
// shutdown, so the file after a run is evidence about what the SERVER believed.
// `gvm content diff` renders the three-way view from Plan.Observed and the lock
// it loads itself.
func planChanges(p *content.Plan, obs *content.Observed, sc *content.Schema) []content.Change {
	if obs == nil || !obs.ConfigRead {
		return nil
	}

	livePos := make(map[content.AcquireID]int, len(obs.Acquire))
	for i, id := range obs.Acquire {
		if _, dup := livePos[id]; !dup {
			livePos[id] = i
		}
	}
	liveActive := make(map[content.ActivationID]bool, len(obs.Activate))
	for _, a := range obs.Activate {
		liveActive[a] = true
	}

	declared := make(map[content.AcquireID]bool, len(p.Entries))
	var out []content.Change
	added := make(map[content.AcquireID]bool, len(p.Entries))

	for _, e := range p.Entries {
		id := e.AcquireID()
		declared[id] = true
		_, live := livePos[id]
		switch {
		case !live:
			added[id] = true
			out = append(out, content.Change{
				Kind:           content.ChangeAdd,
				ID:             id,
				To:             fmt.Sprintf("ordinal %d", e.Ordinal),
				Unmaterialized: !e.Materialized,
				Why:            fmt.Sprintf("not listed in the live %s", sc.Acquisition.Key),
			})
		case !e.Materialized:
			// Already listed and still not on disk. Reported as an add
			// because that is what has to happen for it to run: the bytes
			// have to arrive, which on a server_boot source means a boot.
			// Saying nothing here would make `gvm content sync` a no-op on an
			// instance whose operator is waiting for a mod to appear.
			added[id] = true
			out = append(out, content.Change{
				Kind:           content.ChangeAdd,
				ID:             id,
				Unmaterialized: true,
				Why: fmt.Sprintf("already listed in %s and its bytes are still not on disk",
					sc.Acquisition.Key),
			})
		}
	}

	for _, id := range obs.Acquire {
		if declared[id] {
			continue
		}
		out = append(out, content.Change{
			Kind: content.ChangeRemove,
			ID:   id,
			From: fmt.Sprintf("live position %d", livePos[id]),
			// WorldData is left false: no manifest field declares a workshop
			// item world-affecting yet, and section 12.6 keeps that per-item
			// and opt-in rather than a blanket scare. When the field exists,
			// this is where it is read.
			Why: fmt.Sprintf("in the live %s and no longer declared", sc.Acquisition.Key),
		})
	}

	if sc.OrderSignificant {
		out = append(out, reorderChanges(p, obs, declared)...)
	}

	if !sc.VerbatimActivation {
		for _, e := range p.Entries {
			if added[e.AcquireID()] || len(e.Activation) == 0 {
				continue
			}
			var missing []string
			for _, a := range e.Activation {
				if !liveActive[a] {
					missing = append(missing, string(a))
				}
			}
			if len(missing) == 0 {
				continue
			}
			// Section 12.6 step 10's finding, as data: the server pulled a new
			// version of an already-listed item and its internal ids moved.
			// update.sh's blind second restart, replaced by its data-driven
			// descendant.
			out = append(out, content.Change{
				Kind: content.ChangeActivation,
				ID:   e.AcquireID(),
				To:   strings.Join(missing, ", "),
				Why: fmt.Sprintf("the live %s does not enable %d of this item's %d id(s)",
					sc.Activation.Key, len(missing), len(e.Activation)),
			})
		}
	}
	return out
}

// reorderChanges reports load-order violations between the declared list and
// the live one.
//
// It compares RELATIVE order over the ids both lists share, not absolute
// positions. Removing one item from the middle of a 33-item list shifts every
// later index by one, and reporting 20 reorders for one removal is a report
// that gets skimmed -- which is how the one real reorder in it gets missed.
func reorderChanges(p *content.Plan, obs *content.Observed,
	declared map[content.AcquireID]bool) []content.Change {

	liveSeen := map[content.AcquireID]bool{}
	var liveOrder []content.AcquireID
	for _, id := range obs.Acquire {
		if declared[id] && !liveSeen[id] {
			liveSeen[id] = true
			liveOrder = append(liveOrder, id)
		}
	}
	var declaredOrder []content.AcquireID
	ordinalOf := map[content.AcquireID]int{}
	for _, e := range p.Entries {
		id := e.AcquireID()
		if liveSeen[id] {
			declaredOrder = append(declaredOrder, id)
			ordinalOf[id] = e.Ordinal
		}
	}

	var out []content.Change
	for i, id := range declaredOrder {
		if i < len(liveOrder) && liveOrder[i] == id {
			continue
		}
		was := -1
		for j, other := range liveOrder {
			if other == id {
				was = j
				break
			}
		}
		out = append(out, content.Change{
			Kind: content.ChangeReorder,
			ID:   id,
			From: fmt.Sprintf("live place %d", was),
			To:   fmt.Sprintf("ordinal %d", ordinalOf[id]),
			Why:  "load order is hand-tuned, so the declared order is the one that is written",
		})
	}
	return out
}

// planSteps renders section 12.6's ladder for `gvm content sync --dry-run`.
//
// Generated from the computed impact rather than printed unconditionally,
// because the deviation from update.sh is the interesting part: the activation
// write happens while the server is STOPPED, so the final start loads the
// correct list first time and there is no blind second restart.
func planSteps(p *content.Plan, sc *content.Schema) []string {
	steps := []string{"plan: read-only, safe with players online"}
	switch p.Impact() {
	case content.ImpactNone:
		return append(steps, "nothing to do")
	case content.ImpactRebuild:
		steps = append(steps, "archive: world-affecting change, archive-before-destroy")
	}
	steps = append(steps, "stop: shutdown ladder")
	if p.Boot {
		steps = append(steps,
			fmt.Sprintf("write acquisition: %s (stopped, so the server cannot discard it)",
				sc.Acquisition.Key),
			fmt.Sprintf("acquisition boot: wait for ready, budget %s", sc.Acquisition.BootTimeout),
			"stop: clean ladder again")
	}
	steps = append(steps, "observe and resolve: scan roots, read mod.info, cross-check tags")
	if sc.VerbatimActivation {
		steps = append(steps, fmt.Sprintf("activation: the recorded %s line is preserved "+
			"verbatim and is NOT rewritten", sc.Activation.Key))
	} else {
		steps = append(steps, fmt.Sprintf("write activation: %s, plus the lock",
			sc.Activation.Key))
	}
	return append(steps, "start",
		fmt.Sprintf("post-start observe: re-read %s, record drift", sc.Activation.Key))
}
