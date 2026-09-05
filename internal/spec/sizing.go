package spec

import (
	"errors"
	"fmt"
)

// ErrSizing is the sentinel every sizing failure wraps.
var ErrSizing = errors.New("invalid sizing")

// Memory models. `heap_plus_headroom` sizes the cgroup from a heap variable
// plus the game's native footprint; `flat` sizes it from ram_floor_mb, for
// engines with no separately declared heap.
const (
	MemoryModelHeapPlusHeadroom = "heap_plus_headroom"
	MemoryModelFlat             = "flat"
)

// MemoryModels returns the models in the order a diagnostic should list them.
func MemoryModels() []string {
	return []string{MemoryModelHeapPlusHeadroom, MemoryModelFlat}
}

// defaultHighMargin is the gap between MemoryHigh and MemoryMax when a manifest
// declares none. It is 1 GiB because that is the number section 6.6 hardcoded
// before high_margin existed, so a manifest that omits the field keeps the
// behaviour it had.
const defaultHighMargin = 1 << 30

// mib is the granularity MemoryHigh is rounded to. systemd accepts a byte
// count, but a limit that reads 7516192768 in `systemctl show` is a number
// nobody can check against the manifest by eye.
const mib = 1 << 20

// MemoryLimits is the memory triple that lands in the systemd drop-in, in the
// spelling it lands in: strings, so an operator's diff shows "8G" rather than
// 8589934592 (section 6.2, Resources).
//
// MaxBytes and HighBytes are carried alongside so doctor's
// "memory_max >= heap + native_headroom" assertion does not have to re-parse
// what this function just computed.
type MemoryLimits struct {
	High    string
	Max     string
	SwapMax string

	HighBytes int64
	MaxBytes  int64
}

// Memory computes the cgroup memory limits for one instance's variable values.
//
// MemoryMax is a BACKSTOP: exceeding it invokes the kernel OOM killer, which is
// precisely the failure this arithmetic exists to prevent. Project Zomboid's
// own config.env says why the headroom is not optional:
//
//	"B42 uses significant native memory OUTSIDE the Java heap during world
//	 streaming, so the heap must leave ~3GB free or the kernel OOM-kills the
//	 JVM mid-generation."
//
// MemoryHigh is the working limit — throttle and reclaim rather than kill — and
// sits one margin below Max. The margin is an eighth of Max, capped at
// high_margin, so a small server gets a proportional gap and a large one does
// not reserve gigabytes it will never touch.
//
// Swap is pinned to 0 rather than left at the cgroup default: a swapped JVM is a
// dead server. TPS collapses long before the kernel would have OOM-killed it,
// and the symptom — a server that responds but stutters unplayably — is much
// harder to diagnose than a restart.
func (s Sizing) Memory(vars VarValues) (MemoryLimits, error) {
	base, err := s.baseBytes(vars)
	if err != nil {
		return MemoryLimits{}, err
	}

	headroom, err := sizeField("native_headroom", s.NativeHeadroom)
	if err != nil {
		return MemoryLimits{}, err
	}

	max := base + headroom
	if max <= 0 {
		return MemoryLimits{}, fmt.Errorf("%w: memory_max would be %d bytes; "+
			"set ram_floor_mb or a heap_var with a non-zero default", ErrSizing, max)
	}

	margin := max / 8
	marginCap := int64(defaultHighMargin)
	if s.HighMargin != "" {
		if marginCap, err = sizeField("high_margin", s.HighMargin); err != nil {
			return MemoryLimits{}, err
		}
	}
	if margin > marginCap {
		margin = marginCap
	}

	high := roundDownMiB(max - margin)
	return MemoryLimits{
		High:      FormatBytes(high),
		Max:       FormatBytes(max),
		SwapMax:   "0",
		HighBytes: high,
		MaxBytes:  max,
	}, nil
}

// baseBytes is the part of the limit the game itself declares, before headroom.
func (s Sizing) baseBytes(vars VarValues) (int64, error) {
	switch s.Model {
	case MemoryModelHeapPlusHeadroom:
		if s.HeapVar == "" {
			return 0, fmt.Errorf("%w: memory_model %q needs a heap_var", ErrSizing, s.Model)
		}
		v, ok := vars[s.HeapVar]
		if !ok {
			return 0, fmt.Errorf("%w: heap_var names %q, which has no value", ErrSizing, s.HeapVar)
		}
		n, err := ParseBytes(v)
		if err != nil {
			return 0, fmt.Errorf("%w: vars.%s: %w", ErrSizing, s.HeapVar, err)
		}
		return n, nil

	case MemoryModelFlat:
		return int64(s.RAMFloorMB) * mib, nil

	case "":
		return 0, fmt.Errorf("%w: memory_model is required (want %s)", ErrSizing, orList(MemoryModels()))
	}
	return 0, fmt.Errorf("%w: %q is not a memory model (want %s)", ErrSizing, s.Model, orList(MemoryModels()))
}

// DiskFloorBytes is the free space `gvm create` and doctor require before an
// install. It is stated in GB in the manifest and consumed in bytes.
func (s Sizing) DiskFloorBytes() int64 { return int64(s.DiskFloorGB) << 30 }

func sizeField(name, v string) (int64, error) {
	if v == "" {
		return 0, fmt.Errorf("%w: %s is required", ErrSizing, name)
	}
	n, err := ParseBytes(v)
	if err != nil {
		return 0, fmt.Errorf("%w: %s: %w", ErrSizing, name, err)
	}
	return n, nil
}

// roundDownMiB truncates to a whole MiB. Rounding DOWN is deliberate: rounding
// up would put MemoryHigh above the value the arithmetic chose, and the whole
// point of High is that reclaim starts before Max is anywhere near.
func roundDownMiB(n int64) int64 {
	if n < mib {
		return n
	}
	return n - n%mib
}
