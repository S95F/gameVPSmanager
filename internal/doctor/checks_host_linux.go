package doctor

import (
	"io/fs"
	"os"
	"strings"
	"syscall"

	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

func checkRunGvmDirsImpl() lifecycle.CheckFinding {
	var issues []string

	if fi, err := os.Stat("/run/gvm"); err != nil {
		if os.IsNotExist(err) {
			return lifecycle.CheckFinding{
				ID:       "HOST-009",
				Severity: lifecycle.CheckSkip,
				Message:  "/run/gvm does not exist (tmpfiles not run?)",
			}
		}
		return lifecycle.CheckFinding{ID: "HOST-009", Severity: lifecycle.CheckProblem, Message: err.Error()}
	} else {
		mode := fi.Mode()
		if mode&0o777 != 0o755 {
			issues = append(issues, "/run/gvm mode "+octal(mode&0o777)+", want 0755")
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			if st.Uid != 0 {
				issues = append(issues, "/run/gvm uid not root")
			}
		}
	}

	if fi, err := os.Stat("/run/gvm/web"); err != nil {
		if os.IsNotExist(err) {
			issues = append(issues, "/run/gvm/web does not exist")
		}
	} else {
		mode := fi.Mode()
		if mode&0o7777 != 0o2750 {
			issues = append(issues, "/run/gvm/web mode "+octal(mode&0o7777)+", want 02750")
		}
	}

	if len(issues) == 0 {
		return lifecycle.CheckFinding{
			ID:       "HOST-009",
			Severity: lifecycle.CheckOK,
			Message:  "/run/gvm 0755 root:root; /run/gvm/web 2750 gvm:caddy",
		}
	}
	return lifecycle.CheckFinding{
		ID:       "HOST-009",
		Severity: lifecycle.CheckProblem,
		Message:  strings.Join(issues, "; "),
		Fix:      "run: systemd-tmpfiles --create",
	}
}

func octal(mode fs.FileMode) string {
	digits := "01234567"
	b := make([]byte, 4)
	b[0] = digits[(mode>>9)&7]
	b[1] = digits[(mode>>6)&7]
	b[2] = digits[(mode>>3)&7]
	b[3] = digits[mode&7]
	return "0" + string(b)
}
