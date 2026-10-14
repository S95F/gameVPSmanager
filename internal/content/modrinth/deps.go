package modrinth

import (
	"fmt"

	"github.com/S95F/gameVPSmanager/internal/content"
)

// depError reports a dependency issue.
type depError struct {
	projectID string
	depType   string
}

func (e *depError) Error() string {
	return fmt.Sprintf("modrinth dependency %q is %s", e.projectID, e.depType)
}

// checkDeps validates dependencies for a Modrinth version.
// It returns an error for incompatible deps and skips embedded ones.
// Required and optional deps are noted but not auto-resolved (section 12.8).
func checkDeps(deps []modrinthDep) error {
	for _, dep := range deps {
		switch dep.DependencyType {
		case "incompatible":
			return &depError{projectID: dep.ProjectID, depType: "incompatible"}
		case "embedded":
			// embedded deps are bundled inside the parent JAR; NEVER download separately
			continue
		case "required", "optional":
			// Required deps are noted in PlanEntry.Evidence, not auto-resolved.
			// The operator declares all mods explicitly.
			continue
		}
	}
	return nil
}

// depKindLabel returns a human-readable dependency type label.
func depKindLabel(depType string) string {
	switch depType {
	case "required":
		return "required dependency"
	case "optional":
		return "optional dependency"
	case "incompatible":
		return "incompatible with"
	case "embedded":
		return "embedded (bundled)"
	}
	return depType
}

var _ = content.ArtifactFile // import guard
