package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Exit codes, from the section 7.8 table. They are named here because the
// runner is the only process that emits them and because the difference
// between 65 and 75 is the difference between systemd giving up and systemd
// retrying -- a number chosen at the call site is a restart policy chosen by
// accident.
const (
	// ExitUsage means the spec is invalid or the manifest pin does not match.
	// Restart=on-failure is configured NOT to retry it: running the same bytes
	// again produces the same refusal, and retrying would hide it behind a
	// restart loop.
	ExitUsage = 64
	// ExitDataErr means preflight failed on something absent: a directory, a
	// credential, the executable itself.
	ExitDataErr = 65
	// ExitSoftware means the runner panicked. The ladder was attempted anyway
	// (section 11.8 layer 2).
	ExitSoftware = 70
	// ExitTempFail means a previous run's game process is still alive, or
	// another runner holds the lock. This one DOES retry: the condition
	// resolves itself when the old process finally exits.
	ExitTempFail = 75
	// ExitConfig means the sandbox blocks a write the runner requires -- the
	// ProtectHome mistake, which must fail here in under a second rather than
	// silently at the first world write.
	ExitConfig = 78
)

// Failure is a preflight refusal: a check that failed, the sentence systemd's
// STATUS= carries, and the exit code that decides whether the unit retries.
//
// Status is written for `systemctl status`, which is where an operator looks
// first and often the only place they look. It names the path or the pid, not
// the check.
type Failure struct {
	Check  string
	Code   int
	Status string
	Err    error
}

func (f *Failure) Error() string {
	if f.Err != nil {
		return fmt.Sprintf("preflight %s: %s: %v", f.Check, f.Status, f.Err)
	}
	return fmt.Sprintf("preflight %s: %s", f.Check, f.Status)
}

func (f *Failure) Unwrap() error { return f.Err }

// ExitCode reports the exit status a failure should produce. Anything that is
// not a *Failure is a runner bug rather than an environment problem, so it
// takes ExitSoftware and its retry.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var f *Failure
	if errors.As(err, &f) {
		return f.Code
	}
	return ExitSoftware
}

// StatusOf reports the sd_notify STATUS= sentence for a failure.
func StatusOf(err error) string {
	if err == nil {
		return ""
	}
	var f *Failure
	if errors.As(err, &f) {
		return f.Status
	}
	return err.Error()
}

// PathFact is what one preflight path turned out to be.
//
// It is a bag of observations rather than a verdict because gathering them
// touches a filesystem and deciding on them must not: Evaluate is a pure
// function over these, so section 11.5's table is asserted on the development
// machine rather than only on a VPS with a broken sandbox.
type PathFact struct {
	Path     string
	Missing  bool
	NotDir   bool
	NotFile  bool
	NoExec   bool
	StatErr  error
	WriteErr error
}

// CredFact is the state of the credentials systemd was asked to load.
type CredFact struct {
	// Dir is $CREDENTIALS_DIRECTORY, empty when the unit does not use
	// LoadCredential= or when gvm was started by hand.
	Dir string
	// Want are the credential ids this instance needs.
	Want []string
	// Missing are the ids that were not found in Dir.
	Missing []string
	Err     error
}

// OrphanFact is section 11.7's third lock: what the previous run left pinned
// in runner.json, and whether it is still running.
type OrphanFact struct {
	Path     string
	Present  bool
	Identity Identity
	Alive    bool
	// Err is set when liveness could not be decided. It is NOT the same as
	// "dead": a /proc read that fails must refuse the start, because the
	// alternative is a second server on a live world.
	Err error
}

// LockFact is the result of trying to take the per-instance job lock.
type LockFact struct {
	Path string
	Err  error
}

// Facts is everything preflight observed, in one value.
type Facts struct {
	Instance string
	PinErr   error
	Exec     PathFact
	Install  PathFact
	Data     PathFact
	Logs     PathFact
	Run      PathFact
	Creds    CredFact
	Lock     LockFact
	Orphan   OrphanFact
}

// Evaluate turns observations into the first refusal, or nil.
//
// The order is the order an operator can act on. A manifest pin mismatch comes
// first because it invalidates every path below it -- the directories being
// checked are the ones the WRONG manifest names. World safety comes last
// because it is the check whose failure is temporary, and reporting "orphan
// still running" for an instance whose install directory does not exist would
// send the operator hunting a process that never started.
func Evaluate(f Facts) error {
	if f.PinErr != nil {
		return &Failure{
			Check:  "manifest_pin",
			Code:   ExitUsage,
			Status: "manifest.lock.yaml does not match the instance's pin; refusing to run new argv against a live world",
			Err:    f.PinErr,
		}
	}
	if err := checkExec(f.Exec); err != nil {
		return err
	}
	for _, c := range []struct {
		check string
		fact  PathFact
	}{
		{"install_dir", f.Install},
		{"data_dir", f.Data},
		{"log_dir", f.Logs},
		{"run_dir", f.Run},
	} {
		if err := checkDir(c.check, c.fact); err != nil {
			return err
		}
	}
	if err := checkCreds(f.Creds); err != nil {
		return err
	}
	if err := checkLock(f.Lock); err != nil {
		return err
	}
	return checkOrphan(f.Orphan)
}

func checkExec(p PathFact) error {
	switch {
	case p.StatErr != nil && !p.Missing:
		return &Failure{Check: "exec", Code: ExitDataErr,
			Status: "cannot stat " + p.Path, Err: p.StatErr}
	case p.Missing:
		return &Failure{Check: "exec", Code: ExitDataErr,
			Status: p.Path + " does not exist; the install is incomplete or the manifest's exec is wrong"}
	case p.NotFile:
		return &Failure{Check: "exec", Code: ExitDataErr,
			Status: p.Path + " is not a regular file"}
	case p.NoExec:
		return &Failure{Check: "exec", Code: ExitDataErr,
			Status: p.Path + " is not executable"}
	}
	return nil
}

func checkDir(check string, p PathFact) error {
	switch {
	case p.StatErr != nil && !p.Missing:
		return &Failure{Check: check, Code: ExitDataErr,
			Status: "cannot stat " + p.Path, Err: p.StatErr}
	case p.Missing:
		return &Failure{Check: check, Code: ExitDataErr,
			Status: p.Path + " does not exist"}
	case p.NotDir:
		return &Failure{Check: check, Code: ExitDataErr,
			Status: p.Path + " is not a directory"}
	case p.NoExec:
		return &Failure{Check: check, Code: ExitDataErr,
			Status: p.Path + " is not searchable by this user"}
	case p.WriteErr != nil:
		// The directory is there and cannot be written to. Under systemd that
		// is almost always ProtectHome= or a missing ReadWritePaths= entry,
		// which is a unit-file mistake and must not be retried in a loop.
		return &Failure{Check: check, Code: ExitConfig,
			Status: p.Path + " is not writable; check ReadWritePaths= and ProtectHome= on the unit", Err: p.WriteErr}
	}
	return nil
}

func checkCreds(c CredFact) error {
	if len(c.Want) == 0 {
		return nil
	}
	if c.Dir == "" {
		return &Failure{Check: "credentials", Code: ExitDataErr,
			Status: "the unit passed no credentials, but this instance needs " + join(c.Want) +
				"; add LoadCredential= lines and re-apply"}
	}
	if c.Err != nil {
		return &Failure{Check: "credentials", Code: ExitDataErr,
			Status: "cannot read the credentials directory " + c.Dir, Err: c.Err}
	}
	if len(c.Missing) > 0 {
		return &Failure{Check: "credentials", Code: ExitDataErr,
			Status: "credential " + join(c.Missing) + " is missing from " + c.Dir}
	}
	return nil
}

func checkLock(l LockFact) error {
	switch {
	case l.Err == nil:
		return nil
	case errors.Is(l.Err, platform.ErrLocked):
		// A second supervisor on one world is the failure that destroys it, so
		// this exits 75 and lets systemd retry: whichever runner holds the
		// lock will eventually release it.
		return &Failure{Check: "job_lock", Code: ExitTempFail,
			Status: "another gvm runner already holds " + l.Path + " for this instance", Err: l.Err}
	default:
		return &Failure{Check: "job_lock", Code: ExitConfig,
			Status: "cannot take the instance lock " + l.Path, Err: l.Err}
	}
}

func checkOrphan(o OrphanFact) error {
	switch {
	case o.Err != nil:
		return &Failure{Check: "orphan", Code: ExitTempFail,
			Status: "cannot tell whether the previous run's game process is still alive; refusing to start a second server on this world",
			Err:    o.Err}
	case o.Present && o.Alive:
		// The sentence is section 11.7's, verbatim, because it is what an
		// operator greps for.
		return &Failure{Check: "orphan", Code: ExitTempFail,
			Status: fmt.Sprintf("orphaned game process %d still running", o.Identity.PID)}
	}
	return nil
}

func join(list []string) string {
	switch len(list) {
	case 0:
		return ""
	case 1:
		return list[0]
	}
	out := list[0]
	for _, s := range list[1 : len(list)-1] {
		out += ", " + s
	}
	return out + " and " + list[len(list)-1]
}

// Request is what Gather needs to observe a host.
type Request struct {
	Instance string

	// Manifest is the manifest the runner is about to run, and Pin is what the
	// instance says it should be. Verifying the loaded value rather than
	// re-reading manifest.lock.yaml is deliberate: the check has to cover the
	// bytes that will actually build the argv.
	Manifest *spec.Manifest
	Pin      spec.ManifestPin

	Exec       string
	InstallDir string
	DataDir    string
	LogDir     string
	RunDir     string

	JobLock    string
	RunnerJSON string

	// Secrets are the credential ids the instance needs from
	// $CREDENTIALS_DIRECTORY.
	Secrets []string
	// CredentialsDir overrides the environment, for tests.
	CredentialsDir string

	// Flock and Alive are the two host seams. nil takes the platform
	// implementation, which is Linux-only.
	Flock func(path string) (*platform.FileLock, error)
	Alive func(pid int, startTicks uint64) (bool, error)
}

// Preflight runs section 11.5's checks and, on success, returns the held job
// lock. The caller holds it for the whole life of the runner and releases it
// as the last thing it does.
//
// On failure the lock is released here: a refused start must not leave the
// instance locked against the next attempt.
func Preflight(req Request) (*platform.FileLock, error) {
	facts, lock := Gather(req)
	if err := Evaluate(facts); err != nil {
		_ = lock.Unlock()
		return nil, err
	}
	return lock, nil
}

// Gather observes the host. It returns the lock it took (possibly nil) along
// with the facts, so a caller that refuses on some other fact can still
// release it.
func Gather(req Request) (Facts, *platform.FileLock) {
	f := Facts{Instance: req.Instance}
	// Unconditional. Making the check depend on a pin being present would mean
	// an instance that was never applied -- or a caller that forgot to fill
	// the field -- silently gets no check at all, which is the one outcome
	// D-17 exists to prevent.
	f.PinErr = req.Pin.Verify(req.Manifest)
	f.Exec = statFile(req.Exec)
	f.Install = statDir(req.InstallDir, false)
	f.Data = statDir(req.DataDir, true)
	f.Logs = statDir(req.LogDir, true)
	f.Run = statDir(req.RunDir, true)
	f.Creds = gatherCreds(req)

	// The lock is taken here rather than left to the caller because it is a
	// fact about the host like any other: whether this world already has a
	// supervisor. An empty path is a refusal, not a skip -- world safety must
	// not be switchable off by leaving a field blank.
	var held *platform.FileLock
	if req.JobLock == "" {
		f.Lock = LockFact{Err: errors.New("runner: preflight was given no job lock path")}
	} else {
		fn := req.Flock
		if fn == nil {
			fn = platform.Flock
		}
		var err error
		held, err = fn(req.JobLock)
		f.Lock = LockFact{Path: req.JobLock, Err: err}
	}

	f.Orphan = gatherOrphan(req)
	return f, held
}

func gatherCreds(req Request) CredFact {
	c := CredFact{Dir: req.CredentialsDir, Want: req.Secrets}
	if c.Dir == "" {
		c.Dir = os.Getenv("CREDENTIALS_DIRECTORY")
	}
	if len(c.Want) == 0 || c.Dir == "" {
		return c
	}
	for _, id := range c.Want {
		fi, err := os.Stat(filepath.Join(c.Dir, id))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			c.Missing = append(c.Missing, id)
		case err != nil:
			c.Err = err
			return c
		case !fi.Mode().IsRegular():
			c.Missing = append(c.Missing, id)
		}
	}
	return c
}

func gatherOrphan(req Request) OrphanFact {
	o := OrphanFact{Path: req.RunnerJSON}
	if req.RunnerJSON == "" {
		return o
	}
	rf, err := ReadRunnerFile(req.RunnerJSON)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return o
	case err != nil:
		// A runner.json that cannot be parsed is not permission to start: it
		// may name a live process. RuntimeDirectoryPreserve=restart is why the
		// file is here at all, so a corrupt one is a real event.
		o.Err = err
		return o
	}
	o.Present = true
	o.Identity = rf.Process
	if !rf.Process.Valid() {
		o.Err = fmt.Errorf("runner: %s pins %s, which is not a usable process identity", req.RunnerJSON, rf.Process)
		return o
	}
	alive := req.Alive
	if alive == nil {
		alive = platform.Alive
	}
	live, err := alive(rf.Process.PID, rf.Process.StartTicks)
	if err != nil {
		o.Err = err
		return o
	}
	o.Alive = live
	return o
}

// execBitsMeaningful says whether this OS reports a POSIX mode worth judging.
// Windows synthesises 0666/0777 from the read-only attribute, so an execute
// bit read there says nothing about the Linux host the file is destined for.
var execBitsMeaningful = runtime.GOOS != "windows"

func statFile(p string) PathFact {
	f := PathFact{Path: p}
	if p == "" {
		f.Missing = true
		return f
	}
	fi, err := os.Stat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		f.Missing = true
		return f
	case err != nil:
		f.StatErr = err
		return f
	}
	if !fi.Mode().IsRegular() {
		f.NotFile = true
		return f
	}
	f.NoExec = execBitsMeaningful && fi.Mode().Perm()&0o111 == 0
	return f
}

// statDir stats a directory and, when writable is set, proves the claim by
// creating and removing a file. A mode check is not enough: the mode says
// nothing about a read-only bind mount, a full filesystem or systemd's
// ProtectHome=, which are the three ways this actually fails in production.
func statDir(p string, writable bool) PathFact {
	f := PathFact{Path: p}
	if p == "" {
		f.Missing = true
		return f
	}
	fi, err := os.Stat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		f.Missing = true
		return f
	case err != nil:
		f.StatErr = err
		return f
	}
	if !fi.IsDir() {
		f.NotDir = true
		return f
	}
	f.NoExec = execBitsMeaningful && fi.Mode().Perm()&0o111 == 0
	if writable {
		f.WriteErr = writeProbe(p)
	}
	return f
}

// writeProbe creates and deletes a file. The name is prefixed with a dot so a
// probe left behind by a runner that was killed mid-preflight does not show up
// in a game's own directory listing.
func writeProbe(dir string) error {
	fh, err := os.CreateTemp(dir, ".gvm-preflight-*")
	if err != nil {
		return err
	}
	name := fh.Name()
	defer os.Remove(name)
	if _, err := fh.Write([]byte("gvm")); err != nil {
		fh.Close()
		return err
	}
	return fh.Close()
}

// RunnerFile is the identity pin written at spawn and read by the next start.
//
// It is the file section 11.7's third lock reads: if the pinned process is
// still alive, this runner refuses to spawn rather than racing a live world.
// RuntimeDirectoryPreserve=restart is what keeps it across a restart.
type RunnerFile struct {
	Proto    int      `json:"proto"`
	Instance string   `json:"instance"`
	Game     string   `json:"game"`
	Unit     string   `json:"unit,omitempty"`
	Process  Identity `json:"process"`
	// StartedAt is the runner's own view of when the child was spawned. The
	// authoritative pin is StartTicks; this is for humans.
	StartedAt   string `json:"started_at,omitempty"`
	ConsoleMode string `json:"console_mode,omitempty"`
	ConsoleLog  string `json:"console_log,omitempty"`
	ManifestSHA string `json:"manifest_sha256,omitempty"`
}

// RunnerFileProto is the schema version of runner.json.
const RunnerFileProto = 1

// ReadRunnerFile decodes runner.json.
func ReadRunnerFile(p string) (RunnerFile, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return RunnerFile{}, err
	}
	var rf RunnerFile
	if err := json.Unmarshal(b, &rf); err != nil {
		return RunnerFile{}, fmt.Errorf("runner: %s: %w", p, err)
	}
	return rf, nil
}

// WriteRunnerFile writes runner.json atomically.
func WriteRunnerFile(p string, rf RunnerFile) error {
	rf.Proto = RunnerFileProto
	b, err := json.MarshalIndent(rf, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(p, append(b, '\n'), 0o640)
}
