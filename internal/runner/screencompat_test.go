package runner

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestScreenCompatArgv(t *testing.T) {
	c := ScreenCompat{Session: "pzserver", Socket: "/run/gvm/pz-main/console.sock"}
	got, err := c.Argv()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/usr/bin/screen", "-dmS", "pzserver",
		"/usr/bin/gvm", "console", "--socket", "/run/gvm/pz-main/console.sock", "--no-color",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("argv = %q\nwant %q", got, want)
	}
}

// TestScreenCompatRefusesARelativeProgram. Nothing here resolves a program
// through PATH: the bridge runs as the instance user, and that user's PATH is
// not gvm's to trust.
func TestScreenCompatRefusesARelativeProgram(t *testing.T) {
	for _, c := range []ScreenCompat{
		{Session: "pzserver", Socket: "/run/s.sock", ScreenPath: "screen"},
		{Session: "pzserver", Socket: "/run/s.sock", GvmPath: "./gvm"},
	} {
		if _, err := c.Argv(); err == nil || !strings.Contains(err.Error(), "PATH") {
			t.Errorf("Argv(%+v) err = %v, want a refusal naming PATH", c, err)
		}
	}
}

func TestScreenSessionNames(t *testing.T) {
	for _, ok := range []string{"pzserver", "pz-main", "pz_main.2", "A1"} {
		if err := ScreenSessionOK(ok); err != nil {
			t.Errorf("ScreenSessionOK(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "pz main", "pz;rm -rf /", "pz\nmain", "pz\"main", strings.Repeat("a", 81)} {
		if err := ScreenSessionOK(bad); err == nil {
			t.Errorf("ScreenSessionOK(%q) = nil; that name cannot be typed after `screen -r`", bad)
		}
	}
}

// TestScreenSunset. A transitional mechanism with no expiry date is a
// permanent one, so the 90-day clock is in the code rather than in a README.
func TestScreenSunset(t *testing.T) {
	first := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name          string
		now           time.Time
		warn, expired bool
	}{
		{"fresh", first.Add(24 * time.Hour), false, false},
		{"one day before the warning", first.Add(ScreenSunset - ScreenWarnFrom - time.Hour), false, false},
		{"inside the warning window", first.Add(ScreenSunset - 13*24*time.Hour), true, false},
		{"on the day", first.Add(ScreenSunset), true, true},
		{"long past", first.Add(2 * ScreenSunset), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warn, expired, _ := ScreenSunsetState(tc.now, first)
			if warn != tc.warn || expired != tc.expired {
				t.Errorf("warn=%t expired=%t, want %t/%t", warn, expired, tc.warn, tc.expired)
			}
		})
	}
	// Never used: the clock has not started, so nothing is due.
	if warn, expired, rem := ScreenSunsetState(first, time.Time{}); warn || expired || rem != ScreenSunset {
		t.Errorf("unused bridge = warn %t expired %t remaining %s", warn, expired, rem)
	}
}

func TestStartScreenCompat(t *testing.T) {
	t.Run("no compat asked for", func(t *testing.T) {
		h := newHarness(t)
		if err := h.StartScreenCompat("/usr/bin/gvm"); err != nil {
			t.Fatalf("err = %v, want nil when the manifest asks for no bridge", err)
		}
	})
	t.Run("an unknown compat mode is refused", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.Manifest.Spec.Runtime.ConsoleCompat = "tmux"
		if err := h.StartScreenCompat(""); err == nil || !strings.Contains(err.Error(), "tmux") {
			t.Fatalf("err = %v, want a refusal naming the mode", err)
		}
	})
	t.Run("a failed bridge is a warning, not a fatal", func(t *testing.T) {
		// The bridge is a convenience. Refusing to run a game server because a
		// compatibility shim did not start would be the tail wagging the dog,
		// so the failure has to reach the console an operator is reading and
		// go no further.
		c := ScreenCompat{
			Session: "pzserver", Socket: "/run/s.sock",
			Start: func([]string) error { return errors.New("screen: command not found") },
		}
		if err := c.Run(); err == nil {
			t.Fatal("Run swallowed the failure")
		}
	})
	t.Run("runs the rendered argv", func(t *testing.T) {
		var got []string
		c := ScreenCompat{
			Session: "pz-main", Socket: "/run/gvm/pz-main/console.sock",
			Start: func(argv []string) error { got = argv; return nil },
		}
		if err := c.Run(); err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 || got[0] != DefaultScreenPath {
			t.Fatalf("argv = %q", got)
		}
	})
}

// exemptingProcess is a Process that can prove which cgroup members are gvm's
// own helpers, which on Linux is what procBase does.
type exemptingProcess struct {
	*fakeProcess
	around int
	ids    []Identity
}

func (p *exemptingProcess) ExemptAround(fn func() error) error {
	p.around++
	return fn()
}

func (p *exemptingProcess) SweepExempt() []Identity { return p.ids }

// The screen bridge runs `gvm console` inside the unit's OWN cgroup, and its
// session leader stays there for the life of the instance. Empty() is
// cgroup-wide, so unless the bridge's processes are exempted a textbook `quit`
// never settles, walks all the way to the SIGKILL rungs, and is recorded as a
// degraded shutdown of a world that saved perfectly.
func TestTheScreenBridgeStartsInsideTheSweepExemptionWindow(t *testing.T) {
	h := newHarness(t)
	p := &exemptingProcess{
		fakeProcess: h.proc,
		ids:         []Identity{{PID: 52001, PGID: 52001, StartTicks: 901}},
	}
	h.Runner.setProcessForTest(p)

	ran := 0
	if err := h.exemptAround(func() error { ran++; return nil }); err != nil {
		t.Fatal(err)
	}
	if ran != 1 || p.around != 1 {
		t.Fatalf("ran=%d around=%d; the bridge did not start inside the window", ran, p.around)
	}
	if got := h.SweepExempt(); len(got) != 1 || got[0].PID != 52001 {
		t.Errorf("SweepExempt = %+v", got)
	}
}

// A Process that cannot prove anything about cgroups -- every non-Linux build,
// and a runner that has not spawned yet -- must still start the bridge.
func TestTheBridgeStartsEvenWhenNothingCanBeExempted(t *testing.T) {
	h := newHarness(t)
	ran := 0
	if err := h.exemptAround(func() error { ran++; return nil }); err != nil {
		t.Fatal(err)
	}
	if ran != 1 {
		t.Fatalf("ran = %d, want 1", ran)
	}
	if got := h.SweepExempt(); got != nil {
		t.Errorf("SweepExempt = %+v, want none", got)
	}
}
