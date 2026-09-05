package platform

import (
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// NotifySocketEnv is the variable systemd sets for a Type=notify unit.
const NotifySocketEnv = "NOTIFY_SOCKET"

// maxStatusBytes caps a STATUS= payload. systemd truncates a long status
// itself, but the cap is applied here so that a runaway console line -- a
// stack trace, a progress bar with no newlines -- cannot turn every readiness
// update into a multi-kilobyte datagram.
const maxStatusBytes = 2048

// NotifyReady is the datagram that satisfies Type=notify and ends TimeoutStartSec.
func NotifyReady() string { return "READY=1" }

// NotifyStopping announces that the shutdown ladder has begun, so systemd stops
// counting the unit as running while the ladder does its slow, correct work.
func NotifyStopping() string { return "STOPPING=1" }

// NotifyStatus formats a human-readable state line for `systemctl status`.
func NotifyStatus(msg string) string { return "STATUS=" + sanitizeStatus(msg) }

// NotifyExtendTimeout formats the extension that replaces TimeoutStartSec=infinity
// (D-12). systemd parses the value as unsigned, so a negative duration would
// be rejected and the extension silently lost -- exactly the case where losing
// it kills a server mid-save. It is clamped to zero instead.
func NotifyExtendTimeout(d time.Duration) string {
	usec := d.Microseconds()
	if usec < 0 {
		usec = 0
	}
	return "EXTEND_TIMEOUT_USEC=" + strconv.FormatInt(usec, 10)
}

// sanitizeStatus makes a message safe to put in a datagram.
//
// The protocol is newline-separated KEY=value assignments in one datagram, so
// a newline inside a status turns the rest of a game's console line into
// assignments systemd will act on -- a log line containing "READY=1" would
// mark a still-booting server ready. Newlines become spaces; nothing else
// about the message is altered, because the operator has to recognise it.
func sanitizeStatus(msg string) string {
	msg = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', 0:
			return ' '
		}
		return r
	}, msg)
	msg = strings.TrimSpace(msg)
	if len(msg) > maxStatusBytes {
		msg = msg[:maxStatusBytes]
		// Truncating mid-rune would put invalid UTF-8 in the journal.
		for len(msg) > 0 && !utf8.ValidString(msg) {
			msg = msg[:len(msg)-1]
		}
	}
	return msg
}

// NotifyAddr resolves the value of NOTIFY_SOCKET to a unix socket name.
//
// systemd guarantees either an absolute path or an abstract-namespace name
// introduced by '@'; the kernel spells the abstract namespace with a leading
// NUL. Anything else is not a socket systemd handed us, and dialling it would
// mean writing readiness state into a path an unprivileged process chose.
func NotifyAddr(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	if strings.HasPrefix(raw, "@") {
		return "\x00" + raw[1:], true
	}
	if strings.HasPrefix(raw, "/") {
		return raw, true
	}
	return "", false
}

// Notifier speaks sd_notify to the manager that started this process.
//
// It is hand-rolled, twenty lines of net, because the runner's dependency
// surface is the thing standing between a game world and a supply-chain
// incident; go-systemd would pull in a D-Bus stack the runner never uses.
type Notifier struct {
	addr string
}

// NewNotifier reads NOTIFY_SOCKET. Outside systemd the result is a disabled
// Notifier whose methods are no-ops, so `gvm run` works from a shell.
func NewNotifier() *Notifier {
	return NotifierAt(os.Getenv(NotifySocketEnv))
}

// NotifierAt builds a Notifier for a raw NOTIFY_SOCKET value.
func NotifierAt(raw string) *Notifier {
	addr, ok := NotifyAddr(raw)
	if !ok {
		return &Notifier{}
	}
	return &Notifier{addr: addr}
}

// Enabled reports whether this process is running under a notify-type unit.
func (n *Notifier) Enabled() bool { return n != nil && n.addr != "" }

// Notify sends one datagram. Not being under systemd is a no-op, not an error:
// readiness reporting is how the unit talks to its manager, and the runner's
// own correctness does not depend on anyone listening.
func (n *Notifier) Notify(state string) error {
	if !n.Enabled() {
		return nil
	}
	return notifySend(n.addr, state)
}

func (n *Notifier) Ready() error            { return n.Notify(NotifyReady()) }
func (n *Notifier) Stopping() error         { return n.Notify(NotifyStopping()) }
func (n *Notifier) Status(msg string) error { return n.Notify(NotifyStatus(msg)) }

func (n *Notifier) ExtendTimeout(d time.Duration) error { return n.Notify(NotifyExtendTimeout(d)) }
