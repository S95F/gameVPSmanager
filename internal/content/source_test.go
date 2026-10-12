package content

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

func TestInputDefaultsAreUsableOnTheZeroValue(t *testing.T) {
	// A source calls these unconditionally; a nil check per call site is a nil
	// check that gets forgotten in the error path.
	var in *Input
	if in.Logger() == nil {
		t.Error("Logger() on a nil input is nil")
	}
	if in.Now().IsZero() {
		t.Error("Now() on a nil input is the zero time")
	}
	if in.UpstreamFor("1") != nil {
		t.Error("UpstreamFor on a nil input invented metadata")
	}

	at := time.Date(2026, 9, 4, 18, 22, 11, 0, time.UTC)
	have := &Input{Clock: fixedClock(at)}
	if !have.Now().Equal(at) {
		t.Errorf("Now() = %v, want the injected clock's value", have.Now())
	}
	if have.Now().Location() != time.UTC {
		t.Error("Now() is not UTC; a lock's generated_at is read on other machines")
	}
}

// TestUnreachableUpstreamIsANormalValue is section 12.5's degradation rule: a
// Steam outage must leave the resolve on the local heuristic, not empty a mod
// list.
func TestUnreachableUpstreamIsANormalValue(t *testing.T) {
	in := &Input{Upstream: map[AcquireID]*Upstream{
		"3171167894": {API: "steam_remote_storage", Title: "that DAMN Library"},
	}}
	if got := in.UpstreamFor("3171167894"); got == nil || got.Title == "" {
		t.Fatalf("UpstreamFor = %+v", got)
	}
	if got := in.UpstreamFor("2866258937"); got != nil {
		t.Errorf("UpstreamFor an item the API did not answer for = %+v, want nil", got)
	}
}

func TestActivationWritesActivationFirst(t *testing.T) {
	sc := mustCompile(t, pzBlock(), spec.ContentList{})
	a, err := Activate(sc, lockedPZ(t, sc))
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	writes := a.Writes()
	if len(writes) != 2 {
		t.Fatalf("got %d writes", len(writes))
	}
	// Activation first keeps the declared key order in a shared ini stable
	// across syncs, so a diff shows one changed value rather than two moved
	// lines.
	if !strings.HasPrefix(writes[0], "Mods=") {
		t.Errorf("writes[0] = %q, want the activation key first", writes[0])
	}
	if !strings.HasPrefix(writes[1], "WorkshopItems=") {
		t.Errorf("writes[1] = %q", writes[1])
	}
}

// TestEmptyIsAskableOnBothLines: an empty ACTIVATION write disables every mod,
// and PZ rewrites the ini on shutdown so it becomes permanent. Every caller is
// expected to be able to ask.
func TestEmptyIsAskableOnBothLines(t *testing.T) {
	acq := AcquisitionSpec{Key: "WorkshopItems", Separator: ";"}
	act := ActivationSpec{Key: "Mods", Separator: ";"}

	if !acq.Line(nil).Empty() || !act.Line(nil).Empty() {
		t.Error("a line with no ids does not report itself empty")
	}
	if act.Verbatim("  ").Empty() != true {
		t.Error("a whitespace-only verbatim line does not report itself empty")
	}
	if act.Verbatim("ThatDamnLibrary").Empty() {
		t.Error("a recorded verbatim line reports itself empty")
	}
	if acq.Line([]AcquireID{"1"}).Empty() {
		t.Error("a line with an id reports itself empty")
	}
}

func TestNamespacesReportThemselves(t *testing.T) {
	// Reported for a message or a dashboard column, never used to pick which
	// list to write: the write keys live on separate structs so that no
	// `switch ns` can transpose them.
	if got := (AcquisitionLine{}).Namespace(); got != NamespaceAcquisition {
		t.Errorf("acquisition line namespace = %q", got)
	}
	if got := (ActivationLine{}).Namespace(); got != NamespaceActivation {
		t.Errorf("activation line namespace = %q", got)
	}
}

func TestObservedHelpers(t *testing.T) {
	o := &Observed{
		Block: "workshop",
		Items: []ObservedItem{
			{ID: "3171167894", Present: true, Activation: []ActivationID{"ThatDamnLibrary"}},
			{ID: "4000000001", Present: true, Empty: true},
			{ID: "9999999999"},
		},
		Unlisted: []ActivationID{"SomethingHandEdited"},
	}

	if it, ok := o.Item("3171167894"); !ok || !it.Materialized() {
		t.Errorf("a present non-empty item is not materialized: %+v", it)
	}
	// Present and empty is a download that started and did not finish. Not
	// the same as absent, and treating it as acquired is how a mod silently
	// never loads.
	if it, _ := o.Item("4000000001"); it.Materialized() {
		t.Error("an empty item directory counted as materialized")
	}
	if it, _ := o.Item("9999999999"); it.Materialized() {
		t.Error("an absent item counted as materialized")
	}
	if _, ok := o.Item("nope"); ok {
		t.Error("Item invented an entry")
	}
	if _, ok := (*Observed)(nil).Item("x"); ok {
		t.Error("Item on a nil Observed returned something")
	}
	// The "WorkshopItems= has 32 entries and Mods= has 33" discovery finding.
	if len(o.Unlisted) != 1 {
		t.Errorf("Unlisted = %v", o.Unlisted)
	}
}

// TestActivateNeedsASchema: without one there are no write keys, and guessing
// them is how a list lands under the wrong key.
func TestActivateNeedsASchema(t *testing.T) {
	sc := mustCompile(t, pzBlock(), spec.ContentList{})
	if _, err := Activate(nil, lockedPZ(t, sc)); err == nil {
		t.Error("Activate with no schema returned lines")
	}
	if _, err := Derive(nil, lockedPZ(t, sc)); err == nil {
		t.Error("Derive with no schema returned a line")
	}
	if _, err := Activate(sc, nil); err == nil {
		t.Error("Activate with no lock returned lines")
	}
}

// TestActivateRefusesAnEmptyDerivedLine closes the gap ActivationLine.Empty
// documents and nothing asked.
//
// An empty ACTIVATION write disables every mod, and PZ rewrites the ini on
// shutdown so it becomes permanent. Empty's own comment says "every caller is
// expected to ask" -- and until this check existed, none did: a block whose
// lock recorded zero entries returned Mods= and WorkshopItems= as a success,
// with the note "derived 0 activation id(s) from 0 item(s)". A misspelled
// instance content key produced exactly that lock, which is how a typo became
// a plan to empty both keys.
//
// The accept form is explicit and separate, because "the operator declared no
// mods" is a real state and the caller is the only thing that can tell it from
// "nothing was read".
func TestActivateRefusesAnEmptyDerivedLine(t *testing.T) {
	sc := mustCompile(t, pzBlock(), spec.ContentList{})
	l := NewLock(&Input{Instance: "pz-main", Game: "projectzomboid"})
	if err := l.Put(sc.Block, &SourceLock{Kind: KindSteamWorkshop}); err != nil {
		t.Fatal(err)
	}

	_, err := Activate(sc, l)
	if !errors.Is(err, ErrEmptyActivation) {
		t.Fatalf("Activate = %v, want ErrEmptyActivation", err)
	}
	for _, want := range []string{sc.Block, sc.Activation.Key} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}

	a, err := ActivateAllowingEmpty(sc, l)
	if err != nil {
		t.Fatalf("the explicit accept form refused: %v", err)
	}
	if a.Activate.Value() != "" || a.Acquisition.Value() != "" {
		t.Errorf("writes = %v, want both keys empty", a.Writes())
	}
	// The note must not read as an ordinary success: that sentence is what an
	// operator and a job log both see.
	if !strings.Contains(a.Note, "no items") {
		t.Errorf("note = %q; it does not say the list was empty on purpose", a.Note)
	}
}
