package pz

import (
	"fmt"
	"sort"
	"strings"
)

// Steam Workshop tags[], layered over the mod.info scoring as EVIDENCE.
//
// Section 12.5's rule, and the reason this file is small: TAGS NEVER DROP AN
// ITEM. A tag is a string an author typed into a form on a web page. It can
// raise confidence in an answer the downloaded files already gave, it can
// explain an item the files say nothing about, and it can be recorded as
// disagreeing with them -- but it can never remove something the operator
// asked for, and it can never choose between two variants inside one item.
// That choice stays purely file-based in resolve.go, where the bytes are.
//
// Three consequences follow from that one rule, and each of them is a thing
// that would otherwise go silently wrong:
//
//   - An UNREACHABLE API degrades to exactly today's heuristic. TagSet's zero
//     value is "the API did not answer", so a caller that forgot to fetch
//     metadata, or whose fetch failed, gets the mod.info scoring that already
//     works offline. Steam being down must not change which mods a server
//     enables.
//   - A CONFLICT is recorded, not acted on. mod.info saying versionMin 42
//     while the page says "Build 41" means somebody's metadata is stale; gvm
//     keeps the files' answer, writes "tags:conflict" into the evidence, and
//     hands a human the sentence to act on.
//   - A PROMOTION only ever removes a warning. An item with no build marker
//     whose page is tagged for one build alone is that build's: all of its ids
//     were already enabled, and the tag is what lets gvm stop calling it
//     ambiguous. Nothing is enabled or disabled by it.

// The two tags that carry a build opinion, spelled as the Workshop page
// spells them. Matching is exact after trimming and case folding: there is no
// prefix or substring rule, because "Build 41" and "Build 42" are a closed set
// and a loose match on a free-text tag field is how an author's "Build 41
// compatible, Build 42 soon" would be read as a declaration.
const (
	tagBuild41 = "Build 41"
	tagBuild42 = "Build 42"
)

// Evidence prefixes. "tags:" for a conclusion drawn from the Workshop page,
// "modinfo:" (resolve.go) for one drawn from the downloaded files. Keeping the
// two distinct is what lets a reader tell "the files say 42" from "the page
// says 42" months later, when the two disagree and somebody has to decide
// which is stale.
const (
	// EvidenceConflict is section 12.5's `tags:conflict`: the files scored for
	// the target build and the page is tagged for the other one only.
	EvidenceConflict = "tags:conflict"
	// EvidenceUnavailable is "the API did not answer". Recorded rather than
	// left blank, because "no tags" and "no answer" lead to the same
	// resolution today and to different conversations with the operator.
	EvidenceUnavailable = "tags:unavailable"
	// EvidenceNoBuildTag is "the API answered and the page carries no build
	// tag at all". Common on single-build-era items.
	EvidenceNoBuildTag = "tags:no-build-tag"
)

// TagSet is one workshop item's tags[] array.
//
// THE ZERO VALUE IS "THE API DID NOT ANSWER", which is the whole safety
// property of this type. A composite literal somebody writes in a hurry, a
// map lookup that missed, a fetch that failed -- all of them produce a TagSet
// that contributes no opinion and leaves the file heuristic in charge. The
// alternative shape, a []string plus a bool, has a zero value that reads as
// "the API answered and the item has no tags", which is a different and
// strictly more confident claim.
type TagSet struct {
	known bool
	// builds are the build tags found, sorted and de-duplicated. A slice
	// rather than two bools so that a third build does not need a new field,
	// and never mutated after construction.
	builds []Build
	// names is every tag, in the order the page listed them, for a report.
	// Only the build tags become evidence; the rest would be noise in a
	// lockfile.
	names []string
}

// TagsFrom records the tags the API returned for one item.
//
// An empty or nil slice is a legitimate answer and is recorded as one: the API
// was reached, and the item has no tags. That is NOT the same as the zero
// value, and TagsUnavailable is how a caller says the other thing.
func TagsFrom(tags []string) TagSet {
	t := TagSet{known: true}
	seen := map[Build]bool{}
	for _, raw := range tags {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		t.names = append(t.names, name)
		if b, ok := buildTag(name); ok && !seen[b] {
			seen[b] = true
			t.builds = append(t.builds, b)
		}
	}
	sort.Slice(t.builds, func(i, j int) bool { return t.builds[i] < t.builds[j] })
	return t
}

// TagsUnavailable records that the metadata API could not be reached for this
// item.
//
// It is the same value as the zero TagSet and exists so a call site can SAY SO
// -- `pz.TagsUnavailable()` at the point where a nil upstream is handled reads
// as a decision, where `pz.TagSet{}` reads as a forgotten field.
func TagsUnavailable() TagSet { return TagSet{} }

// buildTag decodes one tag into a build opinion.
func buildTag(name string) (Build, bool) {
	switch {
	case strings.EqualFold(name, tagBuild41):
		return BuildB41, true
	case strings.EqualFold(name, tagBuild42):
		return BuildB42, true
	}
	return 0, false
}

// Known reports whether the API answered. False means the resolve degrades to
// the mod.info heuristic, which is the documented behaviour and not a failure.
func (t TagSet) Known() bool { return t.known }

// Has reports whether the page carries that build's tag. Always false on an
// unanswered TagSet, so a caller cannot accidentally read silence as a denial.
func (t TagSet) Has(b Build) bool {
	for _, have := range t.builds {
		if have == b {
			return true
		}
	}
	return false
}

// Builds are the build tags found, ascending.
func (t TagSet) Builds() []Build { return append([]Build(nil), t.builds...) }

// Names is every tag the page listed, in page order. Display and diagnostics
// only: the ones that mean something are in Builds.
func (t TagSet) Names() []string { return append([]string(nil), t.names...) }

// Evidence renders this tag set as lockfile evidence strings.
//
// Only build tags appear, as "tags:Build 42". The unanswered and
// no-build-tag cases get their own markers rather than nothing at all,
// because "gvm decided this with no help from Steam" is the single most useful
// thing to know when an inclusion looks surprising a year later.
func (t TagSet) Evidence() []string {
	if !t.known {
		return []string{EvidenceUnavailable}
	}
	if len(t.builds) == 0 {
		return []string{EvidenceNoBuildTag}
	}
	out := make([]string, 0, len(t.builds))
	for _, b := range t.builds {
		out = append(out, "tags:Build "+b.String())
	}
	return out
}

func (t TagSet) String() string {
	if !t.known {
		return "tags unavailable"
	}
	if len(t.names) == 0 {
		return "no tags"
	}
	return strings.Join(t.names, ", ")
}

// TagVerdict is section 12.5's table, applied to one item.
//
// It carries a Status rather than an id list on purpose: there is no field
// here through which an id could be added or removed, so "tags never drop an
// item" is a property of the return type and not of the code that reads it.
type TagVerdict struct {
	// Status is the item's status after the tags were considered. It is only
	// ever the same as the file status or a PROMOTION of `check` to `ok`/
	// `multi`; nothing in this file can demote an item, and nothing can turn a
	// resolved item back into a missing one.
	Status ItemStatus
	// Evidence is the tag half of the entry's evidence list, to be appended to
	// the "modinfo:…" markers the file scan produced.
	Evidence []string
	// Conflict is "the files and the page disagree". The files win; this is
	// the record that they had to.
	Conflict bool
	// Promoted is "the page resolved an ambiguity the files could not". The
	// ids were already enabled either way.
	Promoted bool
	// Warning is the sentence `gvm doctor` and `gvm content diff` print, or ""
	// when the tags had nothing to say. It is a message and never a decision:
	// no caller branches on it.
	Warning string
}

// Ambiguous reports whether the item is still enabled-with-a-warning after the
// tags were considered.
func (v TagVerdict) Ambiguous() bool { return v.Status == StatusCheck }

// LayerTags applies the Steam tags to one item's file-scored result.
//
// The order of the branches is the order of section 12.5's table, and each one
// is a row of it:
//
//	score  tags                     outcome
//	3      has Build 42             ok
//	3      Build 41 only            ok, CONFLICT recorded, doctor warns
//	1      has Build 42             ok
//	0      Build 42 only            ok -- PROMOTED, nothing dropped
//	0      both 41 and 42           check, all ids enabled, warned (2866258937)
//	0      Build 41 only, target 42 check + a hard doctor warning
//	0      API unreachable          check -- exactly today's behaviour
//
// item is passed by value and nothing in it is modified: a function that could
// edit ModIDs is a function that could drop one.
func LayerTags(item ItemResult, tags TagSet, target Build) TagVerdict {
	v := TagVerdict{Status: item.Status, Evidence: tags.Evidence()}

	// The bytes are not on disk. Tags cannot conjure a download and are not
	// allowed to paper over a missing one: an item reported as acquired
	// because its Workshop page looks healthy is a mod that silently never
	// loads.
	if item.Status == StatusMissing || item.Status == StatusEmpty {
		return v
	}

	other := otherBuild(target)
	hasTarget := tags.Has(target)
	hasOther := other != 0 && tags.Has(other)

	if item.Score > 0 {
		// The downloaded files carried a marker for the target build, so the
		// answer is already made and the tags cannot change it. A page tagged
		// for the OTHER build only is the interesting case: one of the two
		// sides is stale, and which one is a judgement about an author's
		// habits that gvm has no way to make. Record it and move on.
		if tags.Known() && !hasTarget && hasOther {
			v.Conflict = true
			v.Evidence = append(v.Evidence, EvidenceConflict)
			v.Warning = fmt.Sprintf("%s: the downloaded files score %d for Build %s (%s) and "+
				"the Workshop page is tagged Build %s only. The files win and the item stays "+
				"enabled -- one of the two is stale, so check the workshop page",
				item.WorkshopID, item.Score, target, strings.Join(item.Markers, ", "), other)
		}
		return v
	}

	// Score 0: no build marker anywhere in the item, so every id inside it is
	// already enabled and flagged. Everything below either explains that or
	// leaves it alone. Nothing below changes the id list.
	switch {
	case !tags.Known():
		// The documented degradation. No evidence beyond "tags:unavailable",
		// no change of status, and therefore no way for a Steam outage to
		// alter which mods a server enables.
		return v

	case hasTarget && !hasOther:
		// Promoted. A single-build item's ids are all for that build, so there
		// was never an ambiguity to warn about -- the files just could not say
		// so. The ids are untouched; only the warning goes away.
		v.Promoted = true
		v.Status = StatusOK
		if len(item.ModIDs) > 1 {
			v.Status = StatusMulti
		}
		return v

	case hasTarget && hasOther:
		// Both builds on the page and nothing in the files to choose between
		// them. This is item 2866258937, and the answer is the bash suite's:
		// enable everything inside it and say so.
		v.Warning = fmt.Sprintf("%s has no build marker in its files and its Workshop page is "+
			"tagged for both builds, so every mod id inside it is enabled (%s) and a Build %s "+
			"variant may have come with it. Check it against the workshop page",
			item.WorkshopID, strings.Join(item.ModIDs, ", "), other)
		return v

	case hasOther:
		// The hard one: a Build 41-only item on a Build 42 server. Still
		// enabled -- the operator asked for it, and refusing would be gvm
		// removing a mod on the strength of a web form -- but this is the
		// warning that deserves to be loud.
		v.Warning = fmt.Sprintf("%s is tagged Build %s ONLY on the Workshop and this server "+
			"targets Build %s. Its files carry no build marker, so all of its ids are enabled "+
			"(%s); if it has not been updated for Build %s it will not load, and gvm will not "+
			"remove it on the strength of a tag", item.WorkshopID, other, target,
			strings.Join(item.ModIDs, ", "), target)
		return v

	default:
		// The API answered and the page says nothing about builds. Same
		// outcome as an unreachable API, different evidence, so the two are
		// distinguishable in a report.
		return v
	}
}

// otherBuild is the build a target is not, or 0 when the target is not one of
// the two this file knows.
//
// Returning 0 rather than guessing keeps an unknown target (Build(99), which
// resolve.go already scores everything zero for) from being told it conflicts
// with Build 41.
func otherBuild(target Build) Build {
	switch target {
	case BuildB42:
		return BuildB41
	case BuildB41:
		return BuildB42
	}
	return 0
}
