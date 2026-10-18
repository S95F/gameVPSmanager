package hook

import (
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// mc.accept_eula is the one place gvm writes eula=true, so D-22 (gvm never
// accepts a licence for the operator) has to hold here and not only at
// `gvm create`: an adopted or hand-edited instance can reach post_install
// without ever passing create's gate.

func TestAcceptEULAWritesOnlyWhenAccepted(t *testing.T) {
	e := newEnv(t, "minecraft-paper.yaml", spec.VarValues{"accept_eula": "true"})
	if err := e.run("mc.accept_eula"); err != nil {
		t.Fatalf("mc.accept_eula: %v", err)
	}
	if !e.exists("data/eula.txt") {
		t.Fatal("eula.txt was not written although accept_eula=true")
	}
	if got := e.read("data/eula.txt"); !strings.Contains(got, "eula=true") {
		t.Errorf("eula.txt = %q; want it to contain eula=true", got)
	}
}

func TestAcceptEULARefusesWhenNotAccepted(t *testing.T) {
	// The var defaults to "false" in the manifest; model the operator who never
	// set it by passing it explicitly false.
	e := newEnv(t, "minecraft-paper.yaml", spec.VarValues{"accept_eula": "false"})
	if err := e.run("mc.accept_eula"); err != nil {
		t.Fatalf("mc.accept_eula returned an error; it should survive with a problem: %v", err)
	}
	if e.exists("data/eula.txt") {
		t.Fatal("eula.txt was written without the operator accepting the EULA (D-22 violation)")
	}
	probs := strings.Join(e.problems(), "\n")
	if !strings.Contains(probs, "accept_eula") {
		t.Errorf("problems = %q; want one naming the accept_eula var", probs)
	}
}
