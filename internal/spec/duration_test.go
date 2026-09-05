package spec

import (
	"errors"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestParseDuration(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"seconds, as every shipped manifest writes them", "3600s", time.Hour},
		{"minutes", "5m", 5 * time.Minute},
		{"compound", "1h30m", 90 * time.Minute},
		{"sub-second", "250ms", 250 * time.Millisecond},
		{"zero", "0s", 0},
		{"bare zero, the one unitless value that cannot be misread", "0", 0},
		{"surrounding whitespace", "  45s ", 45 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseDuration(tc.in)
			if err != nil {
				t.Fatalf("ParseDuration(%q) = error %v", tc.in, err)
			}
			if got.Duration() != tc.want {
				t.Errorf("ParseDuration(%q) = %v, want %v", tc.in, got.Duration(), tc.want)
			}
		})
	}
}

func TestParseDurationRejects(t *testing.T) {
	tests := []struct{ name, in string }{
		{"empty", ""},
		{"bare integer has no agreed unit", "300"},
		{"negative", "-5s"},
		{"prose", "5 minutes"},
		{"unknown unit", "5w"},
		{"systemd infinity is not a duration gvm can compare", "infinity"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseDuration(tc.in)
			if err == nil {
				t.Fatalf("ParseDuration(%q) = %v, want an error", tc.in, got)
			}
			if !errors.Is(err, ErrDuration) {
				t.Errorf("ParseDuration(%q) error %v does not wrap ErrDuration", tc.in, err)
			}
		})
	}
}

// A unitless number is the one mistake worth spelling out, because the operator
// meant something specific and gvm must not guess which.
func TestParseDurationNamesTheFixForAUnitlessNumber(t *testing.T) {
	_, err := ParseDuration("300")
	if err == nil {
		t.Fatal("ParseDuration(\"300\") succeeded")
	}
	if !strings.Contains(err.Error(), `"300s"`) {
		t.Errorf("error %q does not suggest \"300s\"", err)
	}
}

func TestDurationStringIsCanonical(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{time.Hour, "3600s"},
		{5 * time.Minute, "300s"},
		{90 * time.Minute, "5400s"},
		{45 * time.Second, "45s"},
		{250 * time.Millisecond, "250ms"},
		{1500 * time.Millisecond, "1.5s"},
	}
	for _, tc := range tests {
		if got := Duration(tc.in).String(); got != tc.want {
			t.Errorf("Duration(%v).String() = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDurationYAMLRoundTrip(t *testing.T) {
	type doc struct {
		HardDeadline Duration `yaml:"hard_deadline"`
		StallTimeout Duration `yaml:"stall_timeout"`
		Budget       Duration `yaml:"budget"`
		Wait         Duration `yaml:"wait,omitempty"`
		Explicit     Duration `yaml:"explicit_null"`
	}

	const src = `
hard_deadline: 3600s
stall_timeout: 5m
budget: "1h30m"
explicit_null:
`
	var d doc
	if err := yaml.Unmarshal([]byte(src), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.HardDeadline.Duration() != time.Hour {
		t.Errorf("hard_deadline = %v", d.HardDeadline.Duration())
	}
	if d.StallTimeout.Duration() != 5*time.Minute {
		t.Errorf("stall_timeout = %v", d.StallTimeout.Duration())
	}
	if d.Budget.Duration() != 90*time.Minute {
		t.Errorf("budget = %v", d.Budget.Duration())
	}
	if d.Explicit != 0 {
		t.Errorf("explicit null = %v, want zero", d.Explicit.Duration())
	}

	out, err := yaml.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The emitted form must be re-readable: an unsuffixed number would not be.
	var back doc
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatalf("re-unmarshal of %q: %v", out, err)
	}
	if back != d {
		t.Errorf("round trip changed the value:\n got %+v\nwant %+v\nvia %s", back, d, out)
	}
}

func TestDurationYAMLRejectsBadScalars(t *testing.T) {
	type doc struct {
		Budget Duration `yaml:"budget"`
	}
	for _, src := range []string{
		"budget: 300\n",
		"budget: -5s\n",
		"budget: [30s]\n",
		"budget: {value: 30s}\n",
	} {
		var d doc
		if err := yaml.Unmarshal([]byte(src), &d); err == nil {
			t.Errorf("unmarshal(%q) succeeded with %v, want an error", src, d.Budget.Duration())
		}
	}
}
