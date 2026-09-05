package platform

import (
	"fmt"
	"strconv"
	"strings"
)

// PortRange is an inclusive [Low, High] span of TCP/UDP port numbers.
type PortRange struct {
	Low, High int
}

// DefaultEphemeral is the kernel's compiled-in ip_local_port_range. It is the
// documented fallback for a caller that could not read the live value: the
// alternative -- treating the range as empty -- hands out ports the kernel may
// assign to an outbound connection, which is the intermittent bind failure
// section 6.7 exists to prevent.
var DefaultEphemeral = PortRange{Low: 32768, High: 60999}

// Valid reports whether r is a usable, non-empty port span.
func (r PortRange) Valid() bool {
	return r.Low >= 1 && r.High <= 65535 && r.Low <= r.High
}

// Contains reports whether p falls inside r.
//
// The zero PortRange contains nothing. A caller that failed to read the live
// range must substitute DefaultEphemeral explicitly rather than relying on
// this, so the fallback is a decision in the log and not an accident.
func (r PortRange) Contains(p int) bool {
	if !r.Valid() {
		return false
	}
	return p >= r.Low && p <= r.High
}

func (r PortRange) String() string {
	return strconv.Itoa(r.Low) + "-" + strconv.Itoa(r.High)
}

// ParsePortRange decodes the contents of
// /proc/sys/net/ipv4/ip_local_port_range: two decimal numbers separated by
// whitespace, low first.
func ParsePortRange(b []byte) (PortRange, error) {
	f := strings.Fields(string(b))
	if len(f) != 2 {
		return PortRange{}, fmt.Errorf("platform: ip_local_port_range: want 2 fields, got %d in %q", len(f), strings.TrimSpace(string(b)))
	}
	lo, err := strconv.Atoi(f[0])
	if err != nil {
		return PortRange{}, fmt.Errorf("platform: ip_local_port_range: low bound %q: %w", f[0], err)
	}
	hi, err := strconv.Atoi(f[1])
	if err != nil {
		return PortRange{}, fmt.Errorf("platform: ip_local_port_range: high bound %q: %w", f[1], err)
	}
	r := PortRange{Low: lo, High: hi}
	if !r.Valid() {
		return PortRange{}, fmt.Errorf("platform: ip_local_port_range: %d-%d is not a valid port span", lo, hi)
	}
	return r, nil
}
