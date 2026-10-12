package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// The off-box copy, and the three rules that make it safe to have at all
// (section 13.2).
//
// RULE ONE: an off-box push failing must never fail the local backup. The
// local archive is the actual protection; the remote copy is insurance against
// losing the box. A NAS that is asleep, a key that expired, a full remote
// volume -- none of those are reasons to record a night with no backup. So
// `on_failure` defaults to warn and the engine's Push step is non-fatal.
//
// RULE TWO: there is no trust on first use. `host_key` is PINNED in the
// instance YAML and there is no known_hosts file anywhere in gvm. A backup
// push authenticates with a key that can read every world on the box; handing
// it to whatever answered on port 22 the first time is not a thing to offer as
// a convenience.
//
// RULE THREE, and the one that is a genuine security property backup.sh does
// not have: `encrypt_to` is a PUBLIC age recipient, so the VPS can produce
// backups it cannot itself decrypt. That property only holds if a configured
// recipient can never be skipped, so a recipient with no encrypter available
// is a REFUSAL and never a plaintext upload. See remote_age.go.
//
// The pipeline is streaming -- tar -> pgzip -> age -> sftp -- and never writes
// a second copy to disk. On a VPS whose world is 2.4 GB and whose volume is 8
// GB, a staged encrypted copy is the difference between a push and a full
// disk.

// Remote errors.
var (
	// ErrRemote is a remote target that cannot be used as configured. It is
	// separate from a transport failure because the operator's next move
	// differs: this one is an edit to the instance YAML.
	ErrRemote = errors.New("backup: the remote target is not usable")
	// ErrNoTransport is a configured remote with no transport wired. It is a
	// refusal rather than a skip for the same reason ErrNoEncrypter is: a
	// push that silently did nothing is worse than one that failed, because
	// the operator believes they have an off-box copy.
	ErrNoTransport = errors.New("backup: no transport is available for this remote target")
	// ErrPush is a push that was attempted and failed.
	ErrPush = errors.New("backup: the off-box push failed")
)

// Remote kinds. v1 ships exactly one, behind an interface, so S3 and B2 land
// later without touching anything above this line.
const (
	RemoteNone = "none"
	RemoteSFTP = "sftp"
)

// On-failure policies.
const (
	// OnFailureWarn is the default, and rule one above is why.
	OnFailureWarn = "warn"
	// OnFailureFail is how an operator says "I treat the off-box copy as the
	// backup". It is theirs to choose and it is not the default.
	OnFailureFail = "fail"
)

// RemoteFile is one archive as the remote reports it.
type RemoteFile struct {
	Name    string
	Bytes   int64
	ModTime time.Time
}

// Session is one connection to a remote target.
//
// Deliberately four methods and no more. A transport that could be asked to
// run a command would be a transport that could be asked to run a command, and
// the uploading half of a backup tool has no business holding that.
type Session interface {
	// Put streams r to name under the target's directory. It must not buffer
	// the whole stream: the entire point of the streaming pipeline is that a
	// 2.4 GB archive never exists twice.
	Put(ctx context.Context, name string, r io.Reader) error
	// List names what is already there, for keep_remote.
	List(ctx context.Context) ([]RemoteFile, error)
	// Remove deletes one name.
	Remove(ctx context.Context, name string) error
	Close() error
}

// Transport opens sessions. It is the seam S3 lands behind.
type Transport interface {
	// Describe is what goes in the job log: a host and a path, never a key.
	Describe() string
	Open(ctx context.Context) (Session, error)
}

// Encrypter wraps the upload stream.
//
// It is an interface and not a function so that Describe can put the recipient
// in the record: "encrypted to age1qyqs...". An operator restoring a year from
// now needs to know which key they need, and the recipient is public.
type Encrypter interface {
	Describe() string
	// Wrap returns a writer that encrypts into w. Closing it must flush and
	// finalise -- for an AEAD format that means writing the last chunk -- so
	// a caller that forgets to Close uploads an unopenable file, and the one
	// caller here closes it before reporting success.
	Wrap(w io.Writer) (io.WriteCloser, error)
}

// Remote is one instance's off-box target.
type Remote struct {
	Target spec.RemoteTarget

	// Transport is how bytes leave the box. Nil means the kind's own, which
	// for sftp in this build is an honest refusal -- see remote_sftp.go.
	Transport Transport
	// Encrypter is required when Target.EncryptTo is set and forbidden from
	// being silently absent. Nil with a recipient configured is
	// ErrNoEncrypter.
	Encrypter Encrypter
	// Open reads the local archive. Nil is os.Open.
	Open func(path string) (io.ReadCloser, error)
	Now  func() time.Time
}

// PushResult is what the push did, for the job record.
type PushResult struct {
	Target string `json:"target,omitempty"`
	Name   string `json:"name,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
	// Encrypted says the stream was wrapped, and To names the recipient.
	Encrypted bool   `json:"encrypted"`
	To        string `json:"encrypted_to,omitempty"`
	// Removed are the remote archives keep_remote reclaimed.
	Removed []string `json:"removed,omitempty"`
	// Kept and Unmanaged mirror local retention's vocabulary: Unmanaged is
	// everything in the remote directory that gvm did not write, and it is
	// never deleted.
	Kept      []string      `json:"kept,omitempty"`
	Unmanaged []string      `json:"unmanaged,omitempty"`
	Elapsed   time.Duration `json:"elapsed"`
}

// Enabled reports whether this instance has an off-box target at all.
func (r *Remote) Enabled() bool {
	if r == nil {
		return false
	}
	k := strings.TrimSpace(r.Target.Kind)
	return k != "" && k != RemoteNone
}

// FailsTheJob reports whether a push failure fails the backup.
//
// The default is warn, and the empty string means the default. An unrecognised
// value also means the default: rule one is the safer side, and refusing to
// back up because `on_failure: fali` was mistyped would be the tool enforcing
// its own typo.
func (r *Remote) FailsTheJob() bool {
	return r != nil && strings.TrimSpace(r.Target.OnFailure) == OnFailureFail
}

func (r *Remote) now() time.Time {
	if r != nil && r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Remote) open(path string) (io.ReadCloser, error) {
	if r != nil && r.Open != nil {
		return r.Open(path)
	}
	return os.Open(path)
}

// Validate refuses a target that cannot be used, before anything is uploaded.
func (r *Remote) Validate() error {
	if !r.Enabled() {
		return nil
	}
	t := r.Target
	switch strings.TrimSpace(t.Kind) {
	case RemoteSFTP:
	default:
		return fmt.Errorf("%w: %q is not a remote kind this build knows (want %s or %s)",
			ErrRemote, t.Kind, RemoteNone, RemoteSFTP)
	}

	var errs []error
	if strings.TrimSpace(t.Host) == "" {
		errs = append(errs, fmt.Errorf("%w: no host", ErrRemote))
	}
	if strings.TrimSpace(t.User) == "" {
		errs = append(errs, fmt.Errorf("%w: no user", ErrRemote))
	}
	if strings.TrimSpace(t.Path) == "" {
		errs = append(errs, fmt.Errorf("%w: no path; an upload with no directory would land "+
			"wherever the remote's login happens to start", ErrRemote))
	}
	if strings.TrimSpace(t.KeyFile) == "" {
		errs = append(errs, fmt.Errorf("%w: no key_file; there is no password authentication "+
			"here, because a password for an account that can read every world on the box does "+
			"not belong in a YAML file", ErrRemote))
	}
	if strings.TrimSpace(t.HostKey) == "" {
		// The TOFU refusal, spelled out. This is the one validation in the
		// block that an operator will be tempted to work around, so the
		// message says what to paste and where to get it.
		errs = append(errs, fmt.Errorf("%w: no host_key. There is no trust-on-first-use and no "+
			"known_hosts in gvm: the key that can read every world on this box is not handed to "+
			"whatever answers on port 22. Paste the line from `ssh-keyscan -t ed25519 %s` after "+
			"checking it against the server itself", ErrRemote, t.Host))
	} else if _, err := ParseHostKey(t.HostKey); err != nil {
		errs = append(errs, err)
	}
	if to := strings.TrimSpace(t.EncryptTo); to != "" {
		if _, err := ParseRecipient(to); err != nil {
			errs = append(errs, err)
		}
	}
	if t.KeepRemote < 0 {
		errs = append(errs, fmt.Errorf("%w: keep_remote is %d", ErrRemote, t.KeepRemote))
	}
	return errors.Join(errs...)
}

// transport is the transport to use, or the reason there is none.
func (r *Remote) transport() (Transport, error) {
	if r.Transport != nil {
		return r.Transport, nil
	}
	switch strings.TrimSpace(r.Target.Kind) {
	case RemoteSFTP:
		return newSFTPTarget(r.Target)
	default:
		return nil, fmt.Errorf("%w: %q", ErrNoTransport, r.Target.Kind)
	}
}

// encrypter is the encrypter to use, or the reason the push must not proceed.
//
// The asymmetry here is the security property. No recipient means no
// encryption, which is a choice the operator made. A recipient with no
// encrypter means the operator asked for something this build cannot do, and
// the only safe answer is to refuse: uploading the world in the clear to the
// path they configured for encrypted backups would be gvm quietly deciding
// that their threat model did not matter.
func (r *Remote) encrypter() (Encrypter, error) {
	to := strings.TrimSpace(r.Target.EncryptTo)
	if to == "" {
		return nil, nil
	}
	rec, err := ParseRecipient(to)
	if err != nil {
		return nil, err
	}
	if r.Encrypter == nil {
		return nil, fmt.Errorf("%w: %s is configured as an encryption recipient and this build "+
			"has no age encrypter wired, so the archive would have been uploaded in the clear",
			ErrNoEncrypter, rec.Short())
	}
	return r.Encrypter, nil
}

// RemoteName is what the archive is called on the remote.
//
// It is the local basename, with `.age` appended when the stream is encrypted.
// Keeping the local name is what lets archive.ParseFilename recognise gvm's own
// uploads in the remote listing -- which is what makes keep_remote able to tell
// an archive gvm wrote from one somebody else put there.
func RemoteName(ref *archive.Ref, encrypted bool) string {
	if ref == nil {
		return ""
	}
	name := baseOf(ref.Path)
	if encrypted {
		name += AgeExt
	}
	return name
}

// Push streams the archive off-box and applies keep_remote.
//
// note receives the observations that belong in the job log. The returned
// *PushResult is populated even on failure, because "it got as far as opening
// the session" and "it never resolved the host" are different nights.
func (r *Remote) Push(ctx context.Context, ref *archive.Ref, note func(string)) (*PushResult, error) {
	if note == nil {
		note = func(string) {}
	}
	res := &PushResult{}
	if !r.Enabled() {
		return res, nil
	}
	if ref == nil || !ref.Verified() {
		// The same rule as prune, for the same reason: an unverified archive
		// is not a receipt for anything, and an off-box copy of a truncated
		// tarball is a false sense of safety that outlives the local one.
		return res, fmt.Errorf("%w: there is no verified archive to push", ErrPush)
	}
	if err := r.Validate(); err != nil {
		return res, err
	}
	enc, err := r.encrypter()
	if err != nil {
		return res, err
	}
	tr, err := r.transport()
	if err != nil {
		return res, err
	}
	res.Target = tr.Describe()
	res.Encrypted = enc != nil
	if enc != nil {
		res.To = enc.Describe()
	}
	res.Name = RemoteName(ref, res.Encrypted)

	start := r.now()
	defer func() { res.Elapsed = r.now().Sub(start) }()

	sess, err := tr.Open(ctx)
	if err != nil {
		return res, fmt.Errorf("%w: %s: %w", ErrPush, res.Target, err)
	}
	defer func() {
		if cerr := sess.Close(); cerr != nil {
			note(fmt.Sprintf("the remote session did not close cleanly: %v", cerr))
		}
	}()

	if err := r.put(ctx, sess, ref, enc, res); err != nil {
		return res, err
	}
	note(fmt.Sprintf("pushed %s (%d bytes) to %s%s", res.Name, res.Bytes, res.Target,
		encryptedSuffix(res)))

	// Retention runs only after a successful upload, which is the same
	// ordering the local prune has and for the same reason: a failed push
	// must never reclaim the remote copies it failed to add to.
	if err := r.retain(ctx, sess, res, note); err != nil {
		// Reported and not fatal. The bytes are off-box, which is what the
		// push was for; a remote directory that is one archive over its limit
		// is next week's problem.
		note(fmt.Sprintf("the remote copy is in place but keep_remote could not be applied: %v", err))
	}
	return res, nil
}

func encryptedSuffix(res *PushResult) string {
	if !res.Encrypted {
		return ""
	}
	return ", encrypted to " + res.To
}

// put streams one archive, through the encrypter when there is one.
//
// The encrypted path is an io.Pipe rather than a temporary file, and that is
// the whole of "never a second copy on disk": the reader the transport pulls
// from is the far end of a pipe whose near end is the age writer wrapping the
// local file. Nothing is ever materialised.
func (r *Remote) put(ctx context.Context, sess Session, ref *archive.Ref, enc Encrypter, res *PushResult) error {
	src, err := r.open(ref.Path)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrPush, ref.Path, err)
	}
	defer src.Close()

	if enc == nil {
		res.Bytes = ref.Bytes
		if err := sess.Put(ctx, res.Name, src); err != nil {
			return fmt.Errorf("%w: %s: %w", ErrPush, res.Name, err)
		}
		return nil
	}

	pr, pw := io.Pipe()
	// The copy runs on its own goroutine because Put consumes the reader;
	// every exit path closes the pipe writer with a reason, so a transport
	// that gives up mid-upload unblocks the copy instead of leaking it.
	done := make(chan error, 1)
	go func() {
		w, werr := enc.Wrap(pw)
		if werr != nil {
			_ = pw.CloseWithError(werr)
			done <- werr
			return
		}
		_, cerr := io.Copy(w, src)
		if cerr != nil {
			_ = w.Close()
			_ = pw.CloseWithError(cerr)
			done <- cerr
			return
		}
		// Close BEFORE closing the pipe: an AEAD format's last chunk is
		// written by Close, and a stream that skipped it uploads a file
		// nobody can open -- which would be discovered at restore time, which
		// is the worst possible time.
		if cerr := w.Close(); cerr != nil {
			_ = pw.CloseWithError(cerr)
			done <- cerr
			return
		}
		done <- pw.Close()
	}()

	perr := sess.Put(ctx, res.Name, pr)
	// Unblock the copier if Put returned early, then wait for it: the
	// encrypter must be finished before the push is reported as done.
	_ = pr.CloseWithError(perr)
	cerr := <-done

	if perr != nil {
		return fmt.Errorf("%w: %s: %w", ErrPush, res.Name, perr)
	}
	if cerr != nil {
		return fmt.Errorf("%w: encrypting %s: %w", ErrPush, res.Name, cerr)
	}
	return nil
}

// retain applies keep_remote.
func (r *Remote) retain(ctx context.Context, sess Session, res *PushResult, note func(string)) error {
	keep := r.Target.KeepRemote
	if keep <= 0 {
		// Zero means no limit, exactly as the local policy's Keep does. A
		// policy that keeps nothing is always a typo and this is not the
		// place to act on one.
		return nil
	}
	files, err := sess.List(ctx)
	if err != nil {
		return err
	}
	remove, kept, unmanaged := PlanRemotePrune(files, keep, res.Name)
	res.Kept, res.Unmanaged = kept, unmanaged
	if len(unmanaged) > 0 {
		note(fmt.Sprintf("%d file(s) in the remote directory were not written by gvm and were "+
			"left alone: %s", len(unmanaged), strings.Join(unmanaged, ", ")))
	}
	var errs []error
	for _, name := range remove {
		if err := sess.Remove(ctx, name); err != nil {
			errs = append(errs, fmt.Errorf("removing %s: %w", name, err))
			continue
		}
		res.Removed = append(res.Removed, name)
	}
	return errors.Join(errs...)
}

// PlanRemotePrune decides what keep_remote reclaims. It is pure.
//
// It applies the SAME three rules as local retention, because a remote
// directory full of worlds deserves the same protections as a local one:
//
//   - the archive just uploaded is never a candidate;
//   - a `preserve` archive is never deleted, by any code path, and `--force`
//     does not exist out here either;
//   - anything gvm did not write -- a bash-suite tarball, a file somebody
//     scp'd in -- is reported and left alone, because deleting somebody
//     else's backups on a schedule is not a feature.
//
// Recognition is archive.ParseFilename on the local name, with the `.age`
// suffix stripped first, which is why RemoteName keeps the local basename.
func PlanRemotePrune(files []RemoteFile, keep int, fresh string) (remove, kept, unmanaged []string) {
	type entry struct {
		name string
		at   time.Time
	}
	var routine []entry

	for _, f := range files {
		name := f.Name
		local := strings.TrimSuffix(name, AgeExt)
		_, at, class, ok := archive.ParseFilename(local)
		switch {
		case name == fresh:
			kept = append(kept, name)
		case !ok:
			unmanaged = append(unmanaged, name)
		case class == archive.ClassPreserve:
			kept = append(kept, name)
		case class != archive.ClassRoutine:
			// An unrecognised class must never default to routine. That is
			// the one direction in which a mistake out here is as
			// unrecoverable as it is locally.
			unmanaged = append(unmanaged, name)
		default:
			routine = append(routine, entry{name: name, at: at})
		}
	}

	// Newest first, by the timestamp in the NAME rather than by the remote's
	// mtime: an rsync, a restore from the NAS's own snapshots, or a filesystem
	// with a coarse clock all move mtimes around, and the name is what gvm
	// wrote.
	slices.SortStableFunc(routine, func(a, b entry) int {
		switch {
		case a.at.After(b.at):
			return -1
		case b.at.After(a.at):
			return 1
		default:
			return strings.Compare(b.name, a.name)
		}
	})
	// `keep` counts the archives that survive, and the fresh upload is one of
	// them even though it is not in this list: keeping N plus the new one
	// would make keep_remote: 30 mean 31.
	budget := keep - 1
	for i, e := range routine {
		if i < budget {
			kept = append(kept, e.name)
			continue
		}
		remove = append(remove, e.name)
	}
	return remove, kept, unmanaged
}

// baseOf is the last path element, for a path that may be slash- or
// backslash-separated. Written here rather than reached for from path/filepath
// because the archive's Path is produced on Linux and read in a test on
// Windows, and filepath.Base would disagree with itself between them.
func baseOf(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}
