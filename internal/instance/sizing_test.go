package instance

import (
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/games"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// shipped loads a real manifest out of the embedded floor. The arithmetic is
// only worth testing against the numbers that actually ship: a hand-built
// Sizing literal would keep passing after somebody edited projectzomboid.yaml.
func shipped(t *testing.T, id string) *spec.Manifest {
	t.Helper()
	raw, err := fs.ReadFile(games.FS, id+".yaml")
	if err != nil {
		t.Fatalf("read %s.yaml: %v", id, err)
	}
	m, diags, err := spec.ValidateBytes("games/"+id+".yaml", raw)
	if err != nil {
		t.Fatalf("%s.yaml: %v\n%s", id, err, diags)
	}
	return m
}

// The number section 6.6 puts in writing: 5g heap + 3G native headroom, with a
// 1G high margin, is MemoryHigh=7G MemoryMax=8G. This is the adopted Project
// Zomboid server's actual configuration.
func TestProjectZomboidSizesToSevenAndEight(t *testing.T) {
	m := shipped(t, "projectzomboid")
	got, err := ComputeResources(m, spec.VarValues{"jvm_xmx": "5g"})
	if err != nil {
		t.Fatalf("ComputeResources = %v", err)
	}
	if got.MemoryHigh != "7G" || got.MemoryMax != "8G" {
		t.Errorf("MemoryHigh/MemoryMax = %s/%s, want 7G/8G", got.MemoryHigh, got.MemoryMax)
	}
	// A swapped JVM is a dead server: TPS collapses long before the kernel
	// would have OOM-killed it, and "responds but stutters unplayably" is far
	// harder to diagnose than a restart.
	if got.MemorySwapMax != "0" {
		t.Errorf("MemorySwapMax = %q, want %q", got.MemorySwapMax, "0")
	}
	// The manifest's own limits, not the package defaults.
	if got.NoFile != 100000 {
		t.Errorf("NoFile = %d, want the manifest's 100000", got.NoFile)
	}
	if got.TasksMax != 4096 {
		t.Errorf("TasksMax = %d, want the manifest's 4096", got.TasksMax)
	}
	if got.CPUWeight != DefaultCPUWeight || got.IOWeight != DefaultIOWeight {
		t.Errorf("CPUWeight/IOWeight = %d/%d, want %d/%d",
			got.CPUWeight, got.IOWeight, DefaultCPUWeight, DefaultIOWeight)
	}
}

// The suffix survives the round trip. `memory_max: 8G` that renders as
// 8589934592 in the drop-in is a diff nobody can check against the manifest by
// eye, which is how a wrong limit ships unnoticed.
func TestEveryShippedManifestSizesToAReadableString(t *testing.T) {
	for _, id := range []string{"projectzomboid", "valheim", "palworld", "7daystodie", "corekeeper", "generic-steam"} {
		t.Run(id, func(t *testing.T) {
			m := shipped(t, id)
			vars := spec.VarValues{}
			for name, v := range m.Spec.Vars {
				vars[name] = v.Default
			}
			got, err := ComputeResources(m, vars)
			if err != nil {
				t.Fatalf("ComputeResources = %v", err)
			}
			for _, s := range []string{got.MemoryHigh, got.MemoryMax} {
				if s == "" {
					t.Fatalf("empty memory limit in %+v", got)
				}
				if _, err := spec.ParseBytes(s); err != nil {
					t.Errorf("%q does not parse back: %v", s, err)
				}
			}
			if err := CheckResources(m, vars, got); err != nil {
				t.Errorf("the resources this package just computed fail its own doctor "+
					"assertion: %v", err)
			}
		})
	}
}

// A manifest that declares no limits still gets bounded: an unbounded TasksMax
// means a fork bomb inside a mod takes the host, not the instance.
func TestMissingLimitsFallBackRatherThanBecomingZero(t *testing.T) {
	m := shipped(t, "valheim")
	m.Spec.Runtime.Limits = spec.Limits{}
	got, err := ComputeResources(m, spec.VarValues{})
	if err != nil {
		t.Fatal(err)
	}
	if got.TasksMax != DefaultTasksMax {
		t.Errorf("TasksMax = %d, want %d; zero means unlimited in a systemd drop-in",
			got.TasksMax, DefaultTasksMax)
	}
	if got.NoFile != DefaultNoFile {
		t.Errorf("NoFile = %d, want %d", got.NoFile, DefaultNoFile)
	}
}

func TestComputeResourcesRefusesWhatItCannotSize(t *testing.T) {
	if _, err := ComputeResources(nil, nil); !errors.Is(err, ErrResources) {
		t.Errorf("ComputeResources(nil, nil) = %v, want ErrResources", err)
	}
	m := shipped(t, "projectzomboid")
	// heap_var names jvm_xmx and nothing supplies it: sizing from a missing
	// heap would silently produce a cgroup limit of exactly the headroom.
	_, err := ComputeResources(m, spec.VarValues{})
	if !errors.Is(err, spec.ErrSizing) {
		t.Errorf("a missing heap var gave %v, want spec.ErrSizing", err)
	}
	_, err = ComputeResources(m, spec.VarValues{"jvm_xmx": "five gigs"})
	if !errors.Is(err, spec.ErrSizing) {
		t.Errorf("an unparseable heap var gave %v, want spec.ErrSizing", err)
	}
}

// This is doctor's check, and the sentence it prints is the reason the number
// exists. Losing the sentence would leave the operator with an arithmetic
// complaint and no idea why the headroom is not optional.
func TestCheckResourcesPrintsTheReasonForTheHeadroom(t *testing.T) {
	m := shipped(t, "projectzomboid")
	vars := spec.VarValues{"jvm_xmx": "5g"}
	good, err := ComputeResources(m, vars)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckResources(m, vars, good); err != nil {
		t.Fatalf("computed resources failed their own check: %v", err)
	}

	// The hand edit doctor exists to catch: someone set memory_max to the
	// heap size, reasoning that 5g of heap needs 5g of memory.
	handEdited := good
	handEdited.MemoryMax = "5G"
	err = CheckResources(m, vars, handEdited)
	if err == nil {
		t.Fatal("memory_max below heap + native_headroom was accepted")
	}
	if !strings.Contains(err.Error(), "OUTSIDE the Java heap") {
		t.Errorf("the diagnostic does not carry config.env's sentence: %v", err)
	}
	if !strings.Contains(err.Error(), "8G") {
		t.Errorf("the diagnostic does not say what the limit should be: %v", err)
	}
}

// MemoryHigh above MemoryMax is not a smaller mistake than the one above: it
// removes the throttle-and-reclaim tier entirely, so the first symptom of
// memory pressure is the OOM kill.
func TestCheckResourcesRejectsHighAboveMax(t *testing.T) {
	m := shipped(t, "projectzomboid")
	vars := spec.VarValues{"jvm_xmx": "5g"}
	r, err := ComputeResources(m, vars)
	if err != nil {
		t.Fatal(err)
	}
	r.MemoryHigh = "9G"
	if err := CheckResources(m, vars, r); err == nil {
		t.Fatal("memory_high above memory_max was accepted")
	} else if !strings.Contains(err.Error(), "reclaim") {
		t.Errorf("the diagnostic does not explain what is lost: %v", err)
	}
}

func TestCheckResourcesReportsAnUnparseableLimit(t *testing.T) {
	m := shipped(t, "projectzomboid")
	vars := spec.VarValues{"jvm_xmx": "5g"}
	r, err := ComputeResources(m, vars)
	if err != nil {
		t.Fatal(err)
	}
	bad := r
	bad.MemoryMax = "8 gigs"
	if err := CheckResources(m, vars, bad); err == nil {
		t.Error("an unparseable memory_max was accepted")
	}
	bad = r
	bad.MemoryHigh = "lots"
	if err := CheckResources(m, vars, bad); err == nil {
		t.Error("an unparseable memory_high was accepted")
	}
	if err := CheckResources(nil, vars, r); !errors.Is(err, ErrResources) {
		t.Errorf("CheckResources(nil, ...) = %v, want ErrResources", err)
	}
}
