package doctor

import (
	"context"
	"fmt"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// instCheckManifestPin asserts INST-001: the instance's pinned manifest SHA256
// and revision match the loaded manifest.
func instCheckManifestPin(inst spec.Instance, m *spec.Manifest) lifecycle.DoctorCheck {
	return lifecycle.DoctorCheck{
		ID:      "INST-001",
		Summary: "manifest pin matches shipped manifest",
		Run: func(_ context.Context, _ lifecycle.CheckEnv) lifecycle.CheckFinding {
			pin := inst.Spec.Manifest
			if pin.SHA256 != "" && pin.SHA256 != m.SHA256 {
				return lifecycle.CheckFinding{
					ID:       "INST-001",
					Severity: lifecycle.CheckProblem,
					Message:  fmt.Sprintf("manifest pin SHA256 %s != loaded %s", pin.SHA256[:8], m.SHA256[:8]),
					Fix:      "run: gvm apply " + inst.Metadata.Name,
				}
			}
			if pin.Revision != 0 && pin.Revision != m.Metadata.Revision {
				return lifecycle.CheckFinding{
					ID:       "INST-001",
					Severity: lifecycle.CheckProblem,
					Message:  fmt.Sprintf("manifest pin revision %d != loaded %d", pin.Revision, m.Metadata.Revision),
					Fix:      "run: gvm apply " + inst.Metadata.Name,
				}
			}
			return lifecycle.CheckFinding{
				ID:       "INST-001",
				Severity: lifecycle.CheckOK,
				Message:  fmt.Sprintf("manifest pin rev %d sha %s matches", m.Metadata.Revision, shortSHA(m.SHA256)),
			}
		},
	}
}

// instCheckMemory asserts INST-002: MemoryMax >= jvm_xmx + native_headroom.
// When it fails it prints the config.env OOM sentence VERBATIM (§6.6).
const oomSentence = "B42 uses significant native memory OUTSIDE the Java heap — if MemoryMax is set to the heap size or just above it, the kernel OOM-kills the JVM mid-generation"

func instCheckMemory(inst spec.Instance, m *spec.Manifest) lifecycle.DoctorCheck {
	return lifecycle.DoctorCheck{
		ID:      "INST-002",
		Summary: "MemoryMax >= jvm_xmx + native_headroom",
		Run: func(_ context.Context, _ lifecycle.CheckEnv) lifecycle.CheckFinding {
			memMax := inst.Spec.Resources.MemoryMax
			jvmXmx := inst.Spec.Vars["jvm_xmx"]
			headroom := inst.Spec.Vars["native_headroom"]

			if memMax == "" || jvmXmx == "" || headroom == "" {
				return lifecycle.CheckFinding{ID: "INST-002", Severity: lifecycle.CheckSkip, Message: "sizing vars not set"}
			}

			maxBytes := parseMemBytes(memMax)
			xmxBytes := parseMemBytes(jvmXmx)
			hrBytes := parseMemBytes(headroom)

			if maxBytes == 0 || xmxBytes == 0 || hrBytes == 0 {
				return lifecycle.CheckFinding{ID: "INST-002", Severity: lifecycle.CheckSkip, Message: "could not parse memory values"}
			}

			needed := xmxBytes + hrBytes
			if maxBytes < needed {
				return lifecycle.CheckFinding{
					ID:       "INST-002",
					Severity: lifecycle.CheckProblem,
					Message: fmt.Sprintf("MemoryMax=%s but jvm_xmx=%s + native_headroom=%s needs >= %s\n     %s",
						memMax, jvmXmx, headroom, formatBytes(needed), oomSentence),
					Fix: fmt.Sprintf("set resources.memory_max to at least %s in the instance spec", formatBytes(needed)),
				}
			}
			return lifecycle.CheckFinding{
				ID:       "INST-002",
				Severity: lifecycle.CheckOK,
				Message:  fmt.Sprintf("MemoryMax=%s >= jvm_xmx=%s + native_headroom=%s", memMax, jvmXmx, headroom),
			}
		},
	}
}

func instCheckTimeouts(inst spec.Instance, m *spec.Manifest) lifecycle.DoctorCheck {
	return lifecycle.DoctorCheck{
		ID:      "INST-003",
		Summary: "TimeoutStopSec >= shutdown.budget",
		Run: func(_ context.Context, _ lifecycle.CheckEnv) lifecycle.CheckFinding {
			budget := m.Spec.Shutdown.Budget
			if budget == 0 {
				return lifecycle.CheckFinding{ID: "INST-003", Severity: lifecycle.CheckSkip, Message: "no shutdown budget in manifest"}
			}
			return lifecycle.CheckFinding{
				ID:       "INST-003",
				Severity: lifecycle.CheckSkip,
				Message:  "TimeoutStopSec check requires systemd introspection (Phase 14)",
			}
		},
	}
}

func instCheckSpecApplied(inst spec.Instance) lifecycle.DoctorCheck {
	return lifecycle.DoctorCheck{
		ID:      "INST-005",
		Summary: "spec has been applied (no pending changes)",
		Run: func(_ context.Context, _ lifecycle.CheckEnv) lifecycle.CheckFinding {
			return lifecycle.CheckFinding{ID: "INST-005", Severity: lifecycle.CheckSkip, Message: "requires Postgres mirror (Phase 14)"}
		},
	}
}

func instCheckWorkshopTrees(inst spec.Instance, m *spec.Manifest) lifecycle.DoctorCheck {
	return lifecycle.DoctorCheck{
		ID:      "INST-009",
		Summary: "all required workshop content trees are present and non-empty",
		Run: func(_ context.Context, _ lifecycle.CheckEnv) lifecycle.CheckFinding {
			if len(inst.Spec.Content) == 0 {
				return lifecycle.CheckFinding{ID: "INST-009", Severity: lifecycle.CheckSkip, Message: "no content lists defined"}
			}
			missing := 0
			for _, cl := range inst.Spec.Content {
				for _, ref := range cl.Acquire {
					if ref.Vendored {
						continue
					}
					_ = ref.ID
					missing++
				}
			}
			_ = missing
			return lifecycle.CheckFinding{ID: "INST-009", Severity: lifecycle.CheckSkip, Message: "workshop tree check requires resolved data path"}
		},
	}
}

func instCheckOrderViolations(inst spec.Instance) lifecycle.DoctorCheck {
	return lifecycle.DoctorCheck{
		ID:      "INST-010",
		Summary: "no requires: violation in content order",
		Run: func(_ context.Context, _ lifecycle.CheckEnv) lifecycle.CheckFinding {
			for _, cl := range inst.Spec.Content {
				if msg := checkRefsOrder(cl.Acquire); msg != "" {
					return lifecycle.CheckFinding{
						ID:       "INST-010",
						Severity: lifecycle.CheckProblem,
						Message:  msg,
						Fix:      "reorder the content list so dependencies precede dependents",
					}
				}
			}
			return lifecycle.CheckFinding{ID: "INST-010", Severity: lifecycle.CheckOK, Message: "no requires: violations"}
		},
	}
}

// checkRefsOrder returns a non-empty violation message if any item's requires
// list names an item that appears AFTER it in the slice.
func checkRefsOrder(refs []spec.ContentRef) string {
	pos := map[string]int{}
	for i, r := range refs {
		pos[r.ID] = i
	}
	for i, r := range refs {
		for _, dep := range r.Requires {
			depPos, ok := pos[dep]
			if !ok {
				continue
			}
			if depPos > i {
				return fmt.Sprintf("item %q (ordinal %d) requires %q (ordinal %d) which comes later",
					r.ID, i+1, dep, depPos+1)
			}
		}
	}
	return ""
}

func instCheckSingleGroup(inst spec.Instance) lifecycle.DoctorCheck {
	return lifecycle.DoctorCheck{
		ID:      "INST-008",
		Summary: "instance user is a member of exactly one group",
		Run: func(_ context.Context, _ lifecycle.CheckEnv) lifecycle.CheckFinding {
			return instCheckSingleGroupImpl(inst)
		},
	}
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func parseMemBytes(s string) int64 {
	if len(s) == 0 {
		return 0
	}
	unit := s[len(s)-1:]
	numStr := s[:len(s)-1]
	var mult int64 = 1
	switch strings.ToUpper(unit) {
	case "G":
		mult = 1024 * 1024 * 1024
	case "M":
		mult = 1024 * 1024
	case "K":
		mult = 1024
	default:
		numStr = s
	}
	n := int64(0)
	for _, c := range numStr {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return n * mult
}

func formatBytes(n int64) string {
	if n >= 1024*1024*1024 {
		return fmt.Sprintf("%dG", n/(1024*1024*1024))
	}
	if n >= 1024*1024 {
		return fmt.Sprintf("%dM", n/(1024*1024))
	}
	return fmt.Sprintf("%d", n)
}
