package doctor

import (
	"fmt"
	"os/user"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

func instCheckSingleGroupImpl(inst spec.Instance) lifecycle.CheckFinding {
	username := inst.Spec.User
	if username == "" {
		return lifecycle.CheckFinding{ID: "INST-008", Severity: lifecycle.CheckSkip, Message: "no user configured"}
	}
	u, err := user.Lookup(username)
	if err != nil {
		return lifecycle.CheckFinding{ID: "INST-008", Severity: lifecycle.CheckSkip, Message: "user not found: " + username}
	}
	gids, err := u.GroupIds()
	if err != nil {
		return lifecycle.CheckFinding{ID: "INST-008", Severity: lifecycle.CheckSkip, Message: "could not enumerate groups: " + err.Error()}
	}
	primaryGroup, _ := user.LookupGroupId(u.Gid)

	forbidden := []string{"gvm", "gvm-console", "gvm-content", "caddy"}
	var inForbidden []string
	for _, gid := range gids {
		g, err := user.LookupGroupId(gid)
		if err != nil {
			continue
		}
		for _, f := range forbidden {
			if g.Name == f {
				inForbidden = append(inForbidden, f)
			}
		}
	}

	if len(inForbidden) > 0 {
		return lifecycle.CheckFinding{
			ID:       "INST-008",
			Severity: lifecycle.CheckProblem,
			Message:  fmt.Sprintf("user %s is in forbidden group(s): %s", username, strings.Join(inForbidden, ", ")),
			Fix:      fmt.Sprintf("gpasswd -d %s <group> for each listed group", username),
		}
	}

	if len(gids) != 1 {
		pg := ""
		if primaryGroup != nil {
			pg = primaryGroup.Name
		}
		if pg != username {
			return lifecycle.CheckFinding{
				ID:       "INST-008",
				Severity: lifecycle.CheckProblem,
				Message:  fmt.Sprintf("user %s is in %d groups (want exactly 1: itself)", username, len(gids)),
				Fix:      "remove extra group memberships",
			}
		}
	}

	return lifecycle.CheckFinding{
		ID:       "INST-008",
		Severity: lifecycle.CheckOK,
		Message:  fmt.Sprintf("user %s is in exactly one group (%s)", username, username),
	}
}
