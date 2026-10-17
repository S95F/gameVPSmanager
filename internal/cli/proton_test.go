package cli

import (
	"strings"
	"testing"
)

// TestProtonTitlesAreLabelledEverywhere is Phase 42 item 9's guidance: a title
// gvm runs under Proton is flagged wherever an operator browses the catalogue,
// and `gvm help proton` explains that they supply Proton themselves.
func TestProtonTitlesAreLabelledEverywhere(t *testing.T) {
	h := newTestHost(t)

	// `game list` flags the Proton title on its own row.
	code, out, errb := runWith(t, h, "", "game", "list")
	if code != 0 {
		t.Fatalf("game list: exit %d\n%s", code, errb)
	}
	var vrLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "vrising") {
			vrLine = line
			break
		}
	}
	if vrLine == "" {
		t.Fatalf("vrising is not listed:\n%s", out)
	}
	if !strings.Contains(vrLine, "Proton") {
		t.Errorf("the vrising row does not flag Proton:\n%q", vrLine)
	}

	// `game show` spells out that the operator supplies Proton.
	code, out, errb = runWith(t, h, "", "game", "show", "vrising")
	if code != 0 {
		t.Fatalf("game show vrising: exit %d\n%s", code, errb)
	}
	if !strings.Contains(out, "Windows server under Proton") {
		t.Errorf("game show does not label the Proton title:\n%s", out)
	}

	// A native title is not mislabelled.
	if _, out, _ = runWith(t, h, "", "game", "show", "valheim"); strings.Contains(out, "Proton") {
		t.Errorf("a native game was labelled Proton:\n%s", out)
	}

	// `gvm help proton` serves the topic.
	code, out, errb = runWith(t, h, "", "help", "proton")
	if code != 0 {
		t.Fatalf("help proton: exit %d\n%s", code, errb)
	}
	for _, want := range []string{"Proton", "proton_path", "never installs Proton"} {
		if !strings.Contains(out, want) {
			t.Errorf("the proton help topic is missing %q:\n%s", want, out)
		}
	}
}
