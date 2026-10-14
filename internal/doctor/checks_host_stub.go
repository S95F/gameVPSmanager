//go:build !linux

package doctor

import "github.com/S95F/gameVPSmanager/internal/lifecycle"

func checkRunGvmDirsImpl() lifecycle.CheckFinding {
	return lifecycle.CheckFinding{ID: "HOST-009", Severity: lifecycle.CheckSkip, Message: "dir check requires Linux"}
}
