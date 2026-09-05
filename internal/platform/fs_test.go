package platform

import (
	"math"
	"testing"
)

const gib = 1 << 30

func TestRequiredAppliesHeadroom(t *testing.T) {
	cases := []struct {
		name     string
		need     int64
		headroom float64
		want     int64
	}{
		{"the 1.2 preflight factor", 10 * gib, 1.2, 12 * gib},
		{"no headroom", 100, 1, 100},
		{"rounds up rather than down", 3, 1.1, 4},
		{"nothing needed", 0, 1.2, 0},
		{"negative need", -1, 1.2, 0},
		// A headroom below 1 would approve a write that exactly fills the
		// volume; it is clamped rather than honoured.
		{"headroom below one is clamped", 100, 0.5, 100},
		{"headroom of zero is clamped", 100, 0, 100},
		{"NaN headroom is clamped", 100, math.NaN(), 100},
		{"saturates instead of wrapping", math.MaxInt64, 2, math.MaxInt64},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Required(tc.need, tc.headroom); got != tc.want {
				t.Fatalf("Required(%d, %v) = %d, want %d", tc.need, tc.headroom, got, tc.want)
			}
		})
	}
}

func TestDiskUsageSufficient(t *testing.T) {
	// Free counts the root reserve, Avail does not; an unprivileged instance
	// user gets Avail, so that is what the preflight has to use.
	d := DiskUsage{Total: 100 * gib, Free: 12 * gib, Avail: 7 * gib}

	if !d.Sufficient(5*gib, 1.2) {
		t.Error("6 GiB required against 7 GiB available was refused")
	}
	if d.Sufficient(10*gib, 1.2) {
		t.Error("12 GiB required against 7 GiB available was allowed")
	}
	if !d.Sufficient(7*gib, 1) {
		t.Error("an exact fit was refused")
	}
	if d.Sufficient(7*gib, 1.000001) {
		t.Error("an exact fit plus a byte was allowed")
	}
	// Trusting Free would have said yes here. That is the ext4 five-percent
	// reserve turning a preflight into a truncated archive.
	if d.Sufficient(10*gib, 1) {
		t.Error("the preflight used Free instead of Avail")
	}
	if !d.Sufficient(0, 1.2) {
		t.Error("a zero-byte job was refused")
	}
}

func TestDiskUsageShortfall(t *testing.T) {
	d := DiskUsage{Total: 100 * gib, Free: 12 * gib, Avail: 7 * gib}

	if got := d.Shortfall(5*gib, 1.2); got != 0 {
		t.Errorf("Shortfall with room = %d, want 0", got)
	}
	if got, want := d.Shortfall(10*gib, 1.2), int64(5*gib); got != want {
		t.Errorf("Shortfall = %d, want %d", got, want)
	}
	if got := d.Shortfall(7*gib, 1); got != 0 {
		t.Errorf("exact fit shortfall = %d, want 0", got)
	}

	empty := DiskUsage{}
	if got, want := empty.Shortfall(1, 1), int64(1); got != want {
		t.Errorf("Shortfall on a full volume = %d, want %d", got, want)
	}
	if empty.Shortfall(0, 1.2) != 0 {
		t.Error("a zero-byte job reported a shortfall")
	}
}
