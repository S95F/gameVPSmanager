package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDoctorTotalsLineAndHelpPointer: the text output ends with the totals and
// points at the troubleshooting topic, so a clean run says "0 problems" and a
// dirty one says how dirty and where to read next.
func TestDoctorTotalsLineAndHelpPointer(t *testing.T) {
	h := newTestHost(t)
	_, out, errb := runWith(t, h, "", "doctor")
	combined := out + errb
	if !strings.Contains(combined, "problem") {
		t.Errorf("no totals line:\n%s", combined)
	}
	if !strings.Contains(combined, "see: gvm help troubleshooting") {
		t.Errorf("no troubleshooting pointer:\n%s", combined)
	}
}

// TestDoctorJSON: --json emits valid JSON with findings and totals.
func TestDoctorJSON(t *testing.T) {
	h := newTestHost(t)
	_, out, _ := runWith(t, h, "", "doctor", "--json")
	var got struct {
		Findings []struct {
			ID       string `json:"id"`
			Severity string `json:"severity"`
			Message  string `json:"message"`
		} `json:"findings"`
		Totals struct {
			Problems int `json:"problems"`
			Warnings int `json:"warnings"`
			Notes    int `json:"notes"`
		} `json:"totals"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("doctor --json is not valid JSON: %v\n%s", err, out)
	}
	if len(got.Findings) == 0 {
		t.Error("no findings in JSON")
	}
	// Every finding carries an id and a severity.
	for _, f := range got.Findings {
		if f.ID == "" || f.Severity == "" {
			t.Errorf("finding missing id/severity: %+v", f)
		}
	}
}

// TestDoctorFirewallReadsSkipped: the firewall section is Skip, not a problem,
// on a default host — the "Why now" of the phase.
func TestDoctorFirewallReadsSkipped(t *testing.T) {
	h := newTestHost(t)
	_, out, _ := runWith(t, h, "", "doctor", "--json")
	var got struct {
		Findings []struct {
			ID       string `json:"id"`
			Severity string `json:"severity"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	for _, f := range got.Findings {
		if strings.HasPrefix(f.ID, "FW-") && f.Severity != "skip" {
			t.Errorf("%s is %s, want skip (gvm does not manage the firewall)", f.ID, f.Severity)
		}
	}
}
