package platform

import "math"

// DiskUsage is what statfs reports about the filesystem holding a path.
//
// Avail is separate from Free because on ext4 five percent of the volume is
// reserved for root by default: a preflight that trusts Free will happily
// start a 6 GB archive on a volume where the unprivileged instance user has
// 200 MB left, and discover that halfway through the tar.
type DiskUsage struct {
	Total uint64 // bytes on the filesystem
	Free  uint64 // bytes unused, including the root reserve
	Avail uint64 // bytes an unprivileged process may actually use
}

// Required is need scaled by headroom, saturating at MaxInt64.
//
// A headroom below 1 is a caller bug that would silently weaken a preflight
// into approving a write that exactly fills the volume, so it is clamped
// rather than honoured. A non-positive need requires nothing.
func Required(need int64, headroom float64) int64 {
	if need <= 0 {
		return 0
	}
	if headroom < 1 || math.IsNaN(headroom) {
		headroom = 1
	}
	want := float64(need) * headroom
	if want >= math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(math.Ceil(want))
}

// Sufficient reports whether need bytes, scaled by headroom, fit in Avail.
func (d DiskUsage) Sufficient(need int64, headroom float64) bool {
	return uint64(Required(need, headroom)) <= d.Avail
}

// Shortfall is how many more bytes Avail would need for Sufficient to hold; it
// is zero when there is already room. The caller writes the sentence, because
// only the caller knows which path and which job is short.
func (d DiskUsage) Shortfall(need int64, headroom float64) int64 {
	req := Required(need, headroom)
	if uint64(req) <= d.Avail {
		return 0
	}
	// Reaching here means Avail is below req, and req is at most MaxInt64, so
	// the subtraction cannot overflow.
	return req - int64(d.Avail)
}
