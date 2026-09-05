package platform

import (
	"errors"
	"runtime"
	"testing"
)

// isLinux gates the handful of assertions that need a real kernel. Everything
// else in this package's tests runs identically on the Windows development
// machine, which is the whole point of splitting parse-from-bytes out of
// read-from-file.
func isLinux() bool { return runtime.GOOS == "linux" }

func TestUnsupportedNamesTheOperationAndUnwraps(t *testing.T) {
	err := Unsupported("statfs")
	if !errors.Is(err, ErrNotSupported) {
		t.Fatal("Unsupported does not unwrap to ErrNotSupported")
	}
	var ue *UnsupportedError
	if !errors.As(err, &ue) || ue.Op != "statfs" {
		t.Fatalf("Unsupported lost the operation name: %v", err)
	}
}

func TestHostOperationsAreUnsupportedOffLinux(t *testing.T) {
	if isLinux() {
		t.Skip("these are the real implementations on Linux")
	}
	if _, err := Statfs("."); !errors.Is(err, ErrNotSupported) {
		t.Errorf("Statfs: %v", err)
	}
	if _, err := EphemeralRange(); !errors.Is(err, ErrNotSupported) {
		t.Errorf("EphemeralRange: %v", err)
	}
	if _, err := UserExists("root"); !errors.Is(err, ErrNotSupported) {
		t.Errorf("UserExists: %v", err)
	}
	if _, err := GroupList("root"); !errors.Is(err, ErrNotSupported) {
		t.Errorf("GroupList: %v", err)
	}
	if _, err := Flock(t.TempDir() + "/job.lock"); !errors.Is(err, ErrNotSupported) {
		t.Errorf("Flock: %v", err)
	}
	if _, err := PeerCred(nil); !errors.Is(err, ErrNotSupported) {
		t.Errorf("PeerCred: %v", err)
	}
}
