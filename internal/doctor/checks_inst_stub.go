//go:build !linux

package doctor

import (
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

func instCheckSingleGroupImpl(inst spec.Instance) lifecycle.CheckFinding {
	return lifecycle.CheckFinding{ID: "INST-008", Severity: lifecycle.CheckSkip, Message: "group check requires Linux"}
}
