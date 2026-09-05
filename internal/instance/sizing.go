package instance

import (
	"errors"
	"fmt"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Defaults for the cgroup knobs a manifest does not size per game.
//
// CPUWeight and IOWeight are 100 -- the systemd default -- because gvm's
// slices exist to bound one instance's blast radius, not to rank instances
// against each other. An operator who wants a priority order sets it in the
// instance YAML, where it is visible in a diff.
//
// TasksMax and NoFile back the manifest's own limits block when it is silent.
// A game server with a default TasksMax of 4096 has never run out; the value
// exists so that a fork bomb inside a mod hits a wall.
const (
	DefaultCPUWeight = 100
	DefaultIOWeight  = 100
	DefaultTasksMax  = 4096
	DefaultNoFile    = 65535
)

// oomSentence is Project Zomboid's config.env, verbatim. It is a constant
// because section 6.6 requires doctor to print it -- unedited -- to whoever
// just set memory_max below heap + headroom. A paraphrase would lose the one
// detail that makes the number make sense: the memory is outside the heap, so
// no JVM flag can bound it.
const oomSentence = "B42 uses significant native memory OUTSIDE the Java heap during world " +
	"streaming, so the heap must leave ~3GB free or the kernel OOM-kills the JVM mid-generation."

// ErrResources is the sentinel every sizing failure here wraps. Failures that
// come from the manifest's own arithmetic keep wrapping spec.ErrSizing too.
var ErrResources = errors.New("instance: invalid resources")

// ComputeResources renders one instance's cgroup limits from its pinned
// manifest and its variable values (section 6.6).
//
// The memory arithmetic itself is spec.Sizing.Memory: it is manifest-shaped
// data, it is validated where manifests are validated, and it is already
// tested against every shipped manifest. Reimplementing it here would give the
// project two answers to "how big is this server", which is one more than the
// number of servers that survive them disagreeing.
//
// What this function adds is the rest of the drop-in's resource block, and the
// decision about what to do when a manifest declares no limits.
func ComputeResources(m *spec.Manifest, vars spec.VarValues) (spec.Resources, error) {
	if m == nil {
		return spec.Resources{}, fmt.Errorf("%w: no manifest", ErrResources)
	}
	mem, err := m.Spec.Sizing.Memory(vars)
	if err != nil {
		return spec.Resources{}, err
	}

	lim := m.Spec.Runtime.Limits
	r := spec.Resources{
		CPUWeight:     DefaultCPUWeight,
		IOWeight:      DefaultIOWeight,
		MemoryHigh:    mem.High,
		MemoryMax:     mem.Max,
		MemorySwapMax: mem.SwapMax,
		TasksMax:      lim.Tasks,
		NoFile:        lim.NoFile,
	}
	if r.TasksMax <= 0 {
		r.TasksMax = DefaultTasksMax
	}
	if r.NoFile <= 0 {
		r.NoFile = DefaultNoFile
	}
	return r, nil
}

// CheckResources is doctor's assertion (section 6.6): whatever is written in
// the instance YAML today must still leave the game its native headroom.
//
// It re-parses rather than trusting ComputeResources' output because the point
// is to catch a hand-edited instance file. MemoryMax is a BACKSTOP -- crossing
// it means the kernel OOM killer, which is precisely the failure the headroom
// exists to prevent -- so a memory_max below heap + native_headroom is not a
// smaller server, it is a server that dies during world generation.
func CheckResources(m *spec.Manifest, vars spec.VarValues, have spec.Resources) error {
	if m == nil {
		return fmt.Errorf("%w: no manifest", ErrResources)
	}
	want, err := m.Spec.Sizing.Memory(vars)
	if err != nil {
		return err
	}
	haveMax, err := spec.ParseBytes(have.MemoryMax)
	if err != nil {
		return fmt.Errorf("%w: memory_max %q: %w", ErrResources, have.MemoryMax, err)
	}
	if haveMax < want.MaxBytes {
		return fmt.Errorf("%w: memory_max is %s but this game needs %s "+
			"(heap plus native_headroom).\n\n    %s",
			ErrResources, have.MemoryMax, want.Max, oomSentence)
	}
	haveHigh, err := spec.ParseBytes(have.MemoryHigh)
	if err != nil {
		return fmt.Errorf("%w: memory_high %q: %w", ErrResources, have.MemoryHigh, err)
	}
	// MemoryHigh above MemoryMax is not merely useless: it silently disables
	// the throttle-and-reclaim tier, so the first sign of memory pressure is
	// the OOM kill rather than a slow tick.
	if haveHigh > haveMax {
		return fmt.Errorf("%w: memory_high %s is above memory_max %s, which disables reclaim "+
			"and leaves the OOM killer as the only limit", ErrResources, have.MemoryHigh, have.MemoryMax)
	}
	return nil
}
