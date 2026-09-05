package platform

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestNotifyDatagramFormatting(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"ready", NotifyReady(), "READY=1"},
		{"stopping", NotifyStopping(), "STOPPING=1"},
		{"status", NotifyStatus("booting: loading world"), "STATUS=booting: loading world"},
		{"extend 90s", NotifyExtendTimeout(90 * time.Second), "EXTEND_TIMEOUT_USEC=90000000"},
		{"extend sub-ms", NotifyExtendTimeout(1500 * time.Microsecond), "EXTEND_TIMEOUT_USEC=1500"},
		{"extend zero", NotifyExtendTimeout(0), "EXTEND_TIMEOUT_USEC=0"},
		// systemd parses the value unsigned, so a negative extension is
		// dropped entirely -- exactly when a slow save needs it most.
		{"extend negative", NotifyExtendTimeout(-5 * time.Second), "EXTEND_TIMEOUT_USEC=0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("got %q, want %q", tc.got, tc.want)
			}
		})
	}
}

// A console line is operator-supplied and game-supplied data. The notify
// protocol is newline-separated assignments in one datagram, so an unsanitised
// status containing a newline lets a log line declare the unit ready.
func TestStatusCannotInjectASecondAssignment(t *testing.T) {
	cases := []string{
		"loading\nREADY=1",
		"loading\r\nREADY=1",
		"loading\x00READY=1",
		"loading\nSTOPPING=1\nREADY=1",
	}
	for _, in := range cases {
		got := NotifyStatus(in)
		if strings.ContainsAny(got, "\n\r\x00") {
			t.Fatalf("NotifyStatus(%q) = %q still carries an assignment separator", in, got)
		}
	}
	// The text survives verbatim apart from the separators: an operator has to
	// recognise the message they are reading in `systemctl status`.
	if got := NotifyStatus("loading\nREADY=1"); got != "STATUS=loading READY=1" {
		t.Fatalf("got %q", got)
	}
}

func TestStatusIsTrimmedAndCappedOnAValidRuneBoundary(t *testing.T) {
	if got := NotifyStatus("  spaced  "); got != "STATUS=spaced" {
		t.Fatalf("got %q", got)
	}
	// A progress bar with no newline can be arbitrarily long.
	long := strings.Repeat("é", maxStatusBytes) // two bytes per rune
	got := NotifyStatus(long)
	body := strings.TrimPrefix(got, "STATUS=")
	if len(body) > maxStatusBytes {
		t.Fatalf("status body is %d bytes, over the %d cap", len(body), maxStatusBytes)
	}
	if !utf8.ValidString(body) {
		t.Fatal("status truncated mid-rune")
	}
	odd := strings.Repeat("a", maxStatusBytes-1) + "é"
	body = strings.TrimPrefix(NotifyStatus(odd), "STATUS=")
	if !utf8.ValidString(body) || len(body) > maxStatusBytes {
		t.Fatalf("odd-boundary truncation produced %d bytes, valid=%v", len(body), utf8.ValidString(body))
	}
}

func TestNotifyAddr(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		// The kernel spells the abstract namespace with a leading NUL.
		{"@/org/freedesktop/systemd1/notify/1234", "\x00/org/freedesktop/systemd1/notify/1234", true},
		{"/run/systemd/notify", "/run/systemd/notify", true},
		{"", "", false},
		// Not a socket systemd handed us; dialling it would write readiness
		// state into a path some other process chose.
		{"run/systemd/notify", "", false},
		{"tcp://example.com:9", "", false},
		{"@", "\x00", true},
	}
	for _, tc := range cases {
		got, ok := NotifyAddr(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("NotifyAddr(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// Running outside systemd is the normal case for `gvm run` from a shell and for
// every test; it must be a no-op, not an error that fails a start.
func TestNotifierOutsideSystemdIsASilentNoOp(t *testing.T) {
	n := NotifierAt("")
	if n.Enabled() {
		t.Fatal("empty NOTIFY_SOCKET produced an enabled notifier")
	}
	for _, err := range []error{n.Ready(), n.Stopping(), n.Status("x"), n.ExtendTimeout(time.Second)} {
		if err != nil {
			t.Fatalf("disabled notifier returned %v", err)
		}
	}
	if NotifierAt("relative/path").Enabled() {
		t.Fatal("a relative NOTIFY_SOCKET produced an enabled notifier")
	}
}

func TestNotifierFromEnvironment(t *testing.T) {
	t.Setenv(NotifySocketEnv, "/run/systemd/notify")
	n := NewNotifier()
	if !n.Enabled() {
		t.Fatal("NOTIFY_SOCKET was ignored")
	}
	if n.addr != "/run/systemd/notify" {
		t.Fatalf("addr = %q", n.addr)
	}
}

func TestEnabledNotifierIsUnsupportedOffLinux(t *testing.T) {
	if isLinux() {
		t.Skip("unixgram is available on Linux")
	}
	// Enabled, so the send is attempted rather than skipped: the stub must
	// be honest about being unable to speak the protocol.
	if err := NotifierAt("/run/systemd/notify").Ready(); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("Ready = %v, want ErrNotSupported", err)
	}
}
