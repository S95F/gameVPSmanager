package spec

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ErrDuration is the sentinel every duration decode failure wraps.
var ErrDuration = errors.New("invalid duration")

// Duration is a time.Duration that reads and writes YAML as a unit-suffixed
// string: "3600s", "5m", "1h30m", "250ms".
//
// A bare number is REFUSED. Every duration in this schema is a timeout, budget
// or delay, and systemd, the JVM and the shell suite this replaces disagree
// about whether a naked integer means seconds or milliseconds; a manifest that
// says "300" cannot be read correctly, only guessed at, and the guess would be
// wrong in the direction of a server that appears to hang.
type Duration time.Duration

// Duration returns the wrapped value, so arithmetic and comparisons against
// time.Duration read normally at the call site.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// String renders the canonical form: whole seconds where the value is a whole
// number of seconds, which is what every shipped manifest writes, and
// time.Duration's own form otherwise. So "5m" canonicalises to "300s" rather
// than to Go's "5m0s", and a manifest round-trips through gvm without the diff
// filling up with cosmetic unit churn.
func (d Duration) String() string {
	v := time.Duration(d)
	if v == 0 {
		return "0s"
	}
	if v%time.Second == 0 {
		return strconv.FormatInt(int64(v/time.Second), 10) + "s"
	}
	return v.String()
}

// ParseDuration accepts Go's duration grammar and nothing else: a bare integer
// is rejected for the reason on Duration, and a negative value is rejected
// because no field in this schema has a meaning for one. A bare "0" is the sole
// exception, because zero seconds and zero hours are the same duration and
// there is nothing to misread.
func ParseDuration(s string) (Duration, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, fmt.Errorf("%w: empty", ErrDuration)
	}
	v, err := time.ParseDuration(t)
	if err != nil {
		if _, numErr := strconv.ParseInt(t, 10, 64); numErr == nil {
			return 0, fmt.Errorf("%w: %q has no unit; write %q for seconds", ErrDuration, t, t+"s")
		}
		return 0, fmt.Errorf("%w: %q: want a unit-suffixed duration such as 3600s, 5m or 1h30m", ErrDuration, t)
	}
	if v < 0 {
		return 0, fmt.Errorf("%w: %q: negative", ErrDuration, t)
	}
	return Duration(v), nil
}

// UnmarshalYAML decodes a scalar. An explicit null decodes to zero, which is how
// an omitted optional duration already reads, so `timeout:` with nothing after
// it and no `timeout:` key at all cannot mean two different things.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Tag == "!!null" {
		*d = 0
		return nil
	}
	// The raw scalar text is used rather than n.Decode into a string, because
	// yaml.v3 refuses to put an !!int node into a string and that refusal would
	// swallow the one diagnostic that matters here: "300" is missing its unit.
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("%w: line %d: want a duration string such as 3600s", ErrDuration, n.Line)
	}
	v, err := ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = v
	return nil
}

// MarshalYAML emits the canonical string form. It is deliberately a string and
// not a plain scalar count: an unsuffixed number written back into a manifest
// would not survive being read again.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }
