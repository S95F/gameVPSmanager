package runner

import (
	"slices"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// TestImplementedWrappersMatchTheRunner keeps spec's wrapper registry and
// BuildLaunch in step. BuildLaunch (config.go) builds only the proton wrapper
// and refuses every other kind, so spec must declare exactly proton as
// implemented — otherwise validate would green-light a launch path the runner
// cannot build, and the game would fail to start instead of being warned about
// at validate time.
func TestImplementedWrappersMatchTheRunner(t *testing.T) {
	got := spec.ImplementedWrapperKinds()
	want := []string{"proton"}
	if !slices.Equal(got, want) {
		t.Errorf("spec.ImplementedWrapperKinds() = %v, want %v (the kinds BuildLaunch builds)", got, want)
	}
}
