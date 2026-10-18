package secrets

import (
	"errors"
	"testing"
)

func TestSourceRoundTrips(t *testing.T) {
	s := newTestStore(t)
	ref := InstanceRef("pz-main", "rcon")

	for _, src := range []Source{SourceGenerated, SourceOperator, SourceAdopted} {
		if err := s.WriteSource(ref, src); err != nil {
			t.Fatalf("WriteSource(%q): %v", src, err)
		}
		got, ok, err := s.Source(ref)
		if err != nil || !ok {
			t.Fatalf("Source after WriteSource(%q): ok=%v err=%v", src, ok, err)
		}
		if got != src {
			t.Errorf("Source = %q, want %q", got, src)
		}
	}
}

func TestSourceAbsentIsNotAnError(t *testing.T) {
	s := newTestStore(t)
	ref := InstanceRef("pz-main", "rcon")
	got, ok, err := s.Source(ref)
	if err != nil {
		t.Fatalf("Source of an absent sidecar errored: %v", err)
	}
	if ok {
		t.Errorf("Source reported ok=true with no sidecar written (got %q)", got)
	}
}

func TestWriteSourceRejectsUnknownOrigin(t *testing.T) {
	s := newTestStore(t)
	ref := InstanceRef("pz-main", "rcon")
	if err := s.WriteSource(ref, Source("imagined")); !errors.Is(err, ErrSource) {
		t.Errorf("WriteSource of an unknown origin = %v, want ErrSource", err)
	}
}

// TestSourceSidecarIsNotAListedSecret: the `.source` file must never show up in
// List as a secret of its own — the '.' in its name is what keeps it out.
func TestSourceSidecarIsNotAListedSecret(t *testing.T) {
	s := newTestStore(t)
	ref := InstanceRef("pz-main", "rcon")
	if err := s.Write(ref, New("Xk4mQ9pLzR2vT7wB")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := s.WriteSource(ref, SourceOperator); err != nil {
		t.Fatalf("WriteSource: %v", err)
	}
	refs, err := s.List(ScopeInstance, "pz-main")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 || refs[0].Name != "rcon" {
		t.Fatalf("List returned %v, want exactly [rcon]: the .source sidecar leaked into the listing", refs)
	}
}

func TestDeleteSourceIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ref := InstanceRef("pz-main", "rcon")
	if err := s.DeleteSource(ref); err != nil {
		t.Errorf("DeleteSource of an absent sidecar errored: %v", err)
	}
	if err := s.WriteSource(ref, SourceGenerated); err != nil {
		t.Fatalf("WriteSource: %v", err)
	}
	if err := s.DeleteSource(ref); err != nil {
		t.Fatalf("DeleteSource: %v", err)
	}
	if _, ok, _ := s.Source(ref); ok {
		t.Error("Source still reports a sidecar after DeleteSource")
	}
}
