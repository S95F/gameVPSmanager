package spec

import (
	"errors"
	"testing"
)

// TestProjectZomboidSizing is the number from section 6.6, and it is a number
// with a body count: B42 uses significant native memory OUTSIDE the Java heap
// during world streaming, so a 5g heap on an 8G box needs the 3G of headroom or
// the kernel OOM-kills the JVM mid-generation. MemoryHigh one gigabyte below
// Max is what makes the cgroup throttle and reclaim before that happens.
func TestProjectZomboidSizing(t *testing.T) {
	s := Sizing{
		Model:          MemoryModelHeapPlusHeadroom,
		HeapVar:        "jvm_xmx",
		NativeHeadroom: "3G",
		HighMargin:     "1G",
		RAMFloorMB:     8192,
	}
	got, err := s.Memory(VarValues{"jvm_xmx": "5g"})
	if err != nil {
		t.Fatal(err)
	}
	if got.High != "7G" || got.Max != "8G" {
		t.Errorf("got High=%s Max=%s, want High=7G Max=8G", got.High, got.Max)
	}
	if got.SwapMax != "0" {
		t.Errorf("MemorySwapMax = %q, want 0: a swapped JVM is a dead server long before it is an OOM", got.SwapMax)
	}
	if got.MaxBytes != 8<<30 || got.HighBytes != 7<<30 {
		t.Errorf("byte forms disagree with the strings: %+v", got)
	}
}

func TestMemoryLimits(t *testing.T) {
	for _, tc := range []struct {
		name       string
		sizing     Sizing
		vars       VarValues
		wantHigh   string
		wantMax    string
		wantErrIs  error
		wantNoCall bool
	}{
		{
			name:     "the flat model sizes from ram_floor_mb",
			sizing:   Sizing{Model: MemoryModelFlat, NativeHeadroom: "1G", HighMargin: "512M", RAMFloorMB: 2048},
			wantHigh: "2688M",
			wantMax:  "3G",
		},
		{
			name:     "the eighth-of-max margin wins when it is under the declared cap",
			sizing:   Sizing{Model: MemoryModelFlat, NativeHeadroom: "512M", HighMargin: "256M", RAMFloorMB: 2048},
			wantHigh: "2304M", // 2.5G max, margin 256M capped
			wantMax:  "2560M",
		},
		{
			name:     "an omitted high_margin keeps the 1 GiB section 6.6 hardcoded",
			sizing:   Sizing{Model: MemoryModelHeapPlusHeadroom, HeapVar: "heap", NativeHeadroom: "3G"},
			vars:     VarValues{"heap": "5G"},
			wantHigh: "7G",
			wantMax:  "8G",
		},
		{
			name:      "heap_plus_headroom with no heap_var cannot be sized",
			sizing:    Sizing{Model: MemoryModelHeapPlusHeadroom, NativeHeadroom: "1G"},
			wantErrIs: ErrSizing,
		},
		{
			name:      "a heap_var with no value cannot be sized",
			sizing:    Sizing{Model: MemoryModelHeapPlusHeadroom, HeapVar: "heap", NativeHeadroom: "1G"},
			vars:      VarValues{},
			wantErrIs: ErrSizing,
		},
		{
			name:      "a heap value that is not a size is refused, never guessed",
			sizing:    Sizing{Model: MemoryModelHeapPlusHeadroom, HeapVar: "heap", NativeHeadroom: "1G"},
			vars:      VarValues{"heap": "five gigs"},
			wantErrIs: ErrBytes,
		},
		{
			name:      "an unknown memory model is refused",
			sizing:    Sizing{Model: "guess", NativeHeadroom: "1G"},
			wantErrIs: ErrSizing,
		},
		{
			name:      "native_headroom is required",
			sizing:    Sizing{Model: MemoryModelFlat, RAMFloorMB: 2048},
			wantErrIs: ErrSizing,
		},
		{
			name:      "a sizing that yields nothing at all is refused rather than emitted",
			sizing:    Sizing{Model: MemoryModelFlat, NativeHeadroom: "0", RAMFloorMB: 0},
			wantErrIs: ErrSizing,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.sizing.Memory(tc.vars)
			if tc.wantErrIs != nil {
				if !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("err = %v, want one wrapping %v", err, tc.wantErrIs)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.High != tc.wantHigh || got.Max != tc.wantMax {
				t.Errorf("got High=%s Max=%s, want High=%s Max=%s", got.High, got.Max, tc.wantHigh, tc.wantMax)
			}
		})
	}
}

// TestMemoryHighIsAlwaysAWholeMiBAndBelowMax is the invariant every caller
// depends on: systemd accepts a byte count, but a MemoryHigh nobody can compare
// against the manifest by eye is a limit nobody checks, and a High at or above
// Max means reclaim never runs before the OOM killer does.
func TestMemoryHighIsAlwaysAWholeMiBAndBelowMax(t *testing.T) {
	for _, floor := range []int{512, 1000, 2048, 3333, 8192, 65536} {
		s := Sizing{Model: MemoryModelFlat, NativeHeadroom: "777M", HighMargin: "1G", RAMFloorMB: floor}
		got, err := s.Memory(nil)
		if err != nil {
			t.Fatalf("ram_floor_mb %d: %v", floor, err)
		}
		if got.HighBytes%mib != 0 {
			t.Errorf("ram_floor_mb %d: MemoryHigh %d is not a whole MiB", floor, got.HighBytes)
		}
		if got.HighBytes >= got.MaxBytes {
			t.Errorf("ram_floor_mb %d: High %d is not below Max %d", floor, got.HighBytes, got.MaxBytes)
		}
		if n, err := ParseBytes(got.High); err != nil || n != got.HighBytes {
			t.Errorf("ram_floor_mb %d: %q does not parse back to %d (%v)", floor, got.High, got.HighBytes, err)
		}
	}
}

func TestDiskFloorBytes(t *testing.T) {
	if got := (Sizing{DiskFloorGB: 12}).DiskFloorBytes(); got != 12<<30 {
		t.Errorf("got %d, want %d", got, int64(12)<<30)
	}
}
