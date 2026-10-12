package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/S95F/gameVPSmanager/internal/archive"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// fakeSession records what a transport was asked to do, in order.
//
// The order is itself an assertion: keep_remote must run AFTER the upload, for
// the same reason local retention runs after a verified create -- a failed push
// must never reclaim the remote copies it failed to add to.
type fakeSession struct {
	mu sync.Mutex
	// ops is "put:<name>", "list", "remove:<name>", "close".
	ops  []string
	body map[string][]byte
	// Files is what List reports.
	Files []RemoteFile
	// FailPut, FailList and FailRemove inject failures.
	FailPut    error
	FailList   error
	FailRemove error
}

func (s *fakeSession) Put(ctx context.Context, name string, r io.Reader) error {
	if s.FailPut != nil {
		return s.FailPut
	}
	b, err := io.ReadAll(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops = append(s.ops, "put:"+name)
	if s.body == nil {
		s.body = map[string][]byte{}
	}
	s.body[name] = b
	return err
}

func (s *fakeSession) List(ctx context.Context) ([]RemoteFile, error) {
	s.mu.Lock()
	s.ops = append(s.ops, "list")
	s.mu.Unlock()
	if s.FailList != nil {
		return nil, s.FailList
	}
	return s.Files, nil
}

func (s *fakeSession) Remove(ctx context.Context, name string) error {
	s.mu.Lock()
	s.ops = append(s.ops, "remove:"+name)
	s.mu.Unlock()
	return s.FailRemove
}

func (s *fakeSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops = append(s.ops, "close")
	return nil
}

func (s *fakeSession) log() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ops...)
}

func (s *fakeSession) sent(name string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body[name]
}

// fakeTransport hands out one session.
type fakeTransport struct {
	S        *fakeSession
	FailOpen error
}

func (t *fakeTransport) Describe() string { return "fake://nas.lan/srv/backups/gvm/pz-main" }

func (t *fakeTransport) Open(ctx context.Context) (Session, error) {
	if t.FailOpen != nil {
		return nil, t.FailOpen
	}
	return t.S, nil
}

// markEncrypter is a stand-in for age: it brackets the stream so a test can
// tell an encrypted upload from a plaintext one, and it records whether Close
// was called -- which for a real AEAD is what writes the last chunk.
type markEncrypter struct {
	mu     sync.Mutex
	closed bool
	Fail   error
}

func (m *markEncrypter) Describe() string { return "age1fake..." }

func (m *markEncrypter) Wrap(w io.Writer) (io.WriteCloser, error) {
	if m.Fail != nil {
		return nil, m.Fail
	}
	if _, err := w.Write([]byte("AGE[")); err != nil {
		return nil, err
	}
	return &markWriter{w: w, enc: m}, nil
}

type markWriter struct {
	w   io.Writer
	enc *markEncrypter
}

func (m *markWriter) Write(p []byte) (int, error) { return m.w.Write(p) }

func (m *markWriter) Close() error {
	m.enc.mu.Lock()
	m.enc.closed = true
	m.enc.mu.Unlock()
	_, err := m.w.Write([]byte("]"))
	return err
}

func (m *markEncrypter) wasClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// verifiedRef is an archive.Ref that may authorize a push.
func verifiedRef(name string, body []byte) *archive.Ref {
	return &archive.Ref{
		Path:       "/var/backups/gvm/pz-main/" + name,
		SHA256:     "deadbeef",
		Bytes:      int64(len(body)),
		Instance:   "pz-main",
		Class:      archive.ClassRoutine,
		Reason:     "routine",
		CreatedAt:  testNow,
		VerifiedAt: testNow,
	}
}

// sftpTargetSpec is a complete, valid remote block.
func sftpTargetSpec(t *testing.T, keepRemote int, encryptTo string) spec.RemoteTarget {
	t.Helper()
	_, _, line := ed25519HostKey(0x77)
	return spec.RemoteTarget{
		Kind: RemoteSFTP, Host: "nas.lan", User: "gvmpush",
		Path: "/srv/backups/gvm/pz-main", KeyFile: "/etc/gvm/secrets/host/sftp_ed25519",
		HostKey: line, KeepRemote: keepRemote, EncryptTo: encryptTo,
	}
}

// openBody is the Remote.Open seam: the local archive's bytes without a file.
func openBody(body []byte) func(string) (io.ReadCloser, error) {
	return func(string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
}

// TestAnOffBoxPushFailingNeverFailsTheLocalBackup.
//
// Rule one of section 13.2: the local archive is the actual protection and the
// remote copy is insurance against losing the box. A NAS that is asleep is not
// a reason to record a night with no backup, so `warn` is the default -- and an
// unrecognised value is read as the default too, because refusing to back up
// over a mistyped `fali` would be the tool enforcing its own typo.
func TestAnOffBoxPushFailingNeverFailsTheLocalBackup(t *testing.T) {
	cases := map[string]bool{"": false, OnFailureWarn: false, OnFailureFail: true, "fali": false}
	for value, want := range cases {
		r := &Remote{Target: spec.RemoteTarget{Kind: RemoteSFTP, OnFailure: value}}
		if got := r.FailsTheJob(); got != want {
			t.Errorf("on_failure=%q FailsTheJob() = %v, want %v", value, got, want)
		}
	}
}

// TestPushStreamsTheArchiveAndThenAppliesKeepRemote.
//
// The ordering is the assertion: put, then list, then remove. Retention after
// the upload, for the same reason local retention runs after a verified
// create.
func TestPushStreamsTheArchiveAndThenAppliesKeepRemote(t *testing.T) {
	body := []byte("the world, gzipped")
	name := archive.Filename("pz-main", archive.ClassRoutine, testNow)
	ref := verifiedRef(name, body)

	older := []RemoteFile{}
	for i := 1; i <= 4; i++ {
		older = append(older, RemoteFile{
			Name:  archive.Filename("pz-main", archive.ClassRoutine, testNow.Add(-time.Duration(i)*24*time.Hour)),
			Bytes: 10,
		})
	}
	sess := &fakeSession{Files: append([]RemoteFile{{Name: name}}, older...)}
	r := &Remote{
		Target:    sftpTargetSpec(t, 3, ""),
		Transport: &fakeTransport{S: sess},
		Open:      openBody(body),
		Now:       newClock(testNow).Now,
	}

	res, err := r.Push(context.Background(), ref, nil)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Name != name {
		t.Errorf("remote name = %q, want the local basename %q", res.Name, name)
	}
	if res.Encrypted {
		t.Error("an unencrypted target reported an encrypted push")
	}
	if got := sess.sent(name); !bytes.Equal(got, body) {
		t.Errorf("the remote got %q, want %q", got, body)
	}
	ops := sess.log()
	if len(ops) < 2 || ops[0] != "put:"+name || ops[1] != "list" {
		t.Fatalf("ops = %v; retention must run after the upload", ops)
	}
	// keep_remote: 3 keeps the fresh upload plus the two newest older ones,
	// so the two oldest go.
	if len(res.Removed) != 2 {
		t.Fatalf("removed %v, want the two oldest of four", res.Removed)
	}
	for _, name := range res.Removed {
		if !containsOp(ops, "remove:"+name) {
			t.Errorf("%s was reported removed and never asked for", name)
		}
	}
}

// TestKeepRemoteCountsTheFreshUpload.
//
// keep_remote: 30 means thirty archives on the NAS, not thirty plus tonight's.
func TestKeepRemoteCountsTheFreshUpload(t *testing.T) {
	fresh := archive.Filename("pz-main", archive.ClassRoutine, testNow)
	var files []RemoteFile
	files = append(files, RemoteFile{Name: fresh})
	for i := 1; i <= 5; i++ {
		files = append(files, RemoteFile{
			Name: archive.Filename("pz-main", archive.ClassRoutine, testNow.Add(-time.Duration(i)*time.Hour)),
		})
	}
	remove, kept, unmanaged := PlanRemotePrune(files, 3, fresh)
	if len(unmanaged) != 0 {
		t.Fatalf("unmanaged = %v", unmanaged)
	}
	if len(kept) != 3 {
		t.Fatalf("kept %v, want three including the fresh upload", kept)
	}
	if len(remove) != 3 {
		t.Fatalf("remove = %v, want the three oldest", remove)
	}
	if containsOp(remove, fresh) {
		t.Fatal("the archive that was just uploaded was a prune candidate")
	}
}

// TestRemoteRetentionNeverTouchesAPreserveArchiveOrSomebodyElsesFile.
//
// A remote directory full of worlds deserves the same protections as a local
// one. `preserve` is produced by the thing that is about to be irreversible --
// a rebuild, a restore, a cutover -- and is never deleted by any code path;
// `--force` does not exist out here either. Anything gvm did not write is
// reported and left alone, because deleting somebody else's backups on a
// schedule is not a feature.
func TestRemoteRetentionNeverTouchesAPreserveArchiveOrSomebodyElsesFile(t *testing.T) {
	fresh := archive.Filename("pz-main", archive.ClassRoutine, testNow)
	preserve := archive.Filename("pz-main", archive.ClassPreserve, testNow.Add(-90*24*time.Hour))
	files := []RemoteFile{
		{Name: fresh},
		{Name: preserve},
		{Name: "pz-zomboid-20240101-000000.tar.gz"}, // a bash-suite tarball
		{Name: "notes.txt"},                         // somebody's file
		{Name: archive.Filename("pz-main", archive.ClassRoutine, testNow.Add(-time.Hour))},
		{Name: archive.Filename("pz-main", archive.ClassRoutine, testNow.Add(-2*time.Hour))},
	}
	remove, kept, unmanaged := PlanRemotePrune(files, 1, fresh)

	if containsOp(remove, preserve) {
		t.Fatal("a preserve archive was a prune candidate on the remote")
	}
	if !containsOp(kept, preserve) {
		t.Errorf("the preserve archive is not in kept: %v", kept)
	}
	for _, name := range []string{"pz-zomboid-20240101-000000.tar.gz", "notes.txt"} {
		if !containsOp(unmanaged, name) {
			t.Errorf("%s should be reported as unmanaged, got %v", name, unmanaged)
		}
		if containsOp(remove, name) {
			t.Errorf("%s was a prune candidate and gvm did not write it", name)
		}
	}
	// keep_remote: 1 means only tonight's survives among the routine ones.
	if len(remove) != 2 {
		t.Errorf("remove = %v, want the two older routine archives", remove)
	}
}

// TestTheEncryptedPushIsStreamedAndFinalised.
//
// Two claims. The transport receives the WRAPPED bytes, so nothing plaintext
// leaves the box; and the encrypter is Closed before the push reports success,
// which for an AEAD format is what writes the last chunk. A stream that
// skipped it would upload a file nobody can open, discovered at restore time.
func TestTheEncryptedPushIsStreamedAndFinalised(t *testing.T) {
	body := []byte("the world, gzipped")
	name := archive.Filename("pz-main", archive.ClassRoutine, testNow)
	ref := verifiedRef(name, body)

	enc := &markEncrypter{}
	sess := &fakeSession{}
	r := &Remote{
		Target:    sftpTargetSpec(t, 0, ageVector),
		Transport: &fakeTransport{S: sess},
		Encrypter: enc,
		Open:      openBody(body),
		Now:       newClock(testNow).Now,
	}
	res, err := r.Push(context.Background(), ref, nil)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if !res.Encrypted || res.To != "age1fake..." {
		t.Fatalf("res = %+v, want an encrypted push naming its recipient", res)
	}
	if res.Name != name+AgeExt {
		t.Errorf("remote name = %q, want %q", res.Name, name+AgeExt)
	}
	got := sess.sent(res.Name)
	if want := "AGE[" + string(body) + "]"; string(got) != want {
		t.Fatalf("the remote got %q, want %q: the plaintext must never leave the box", got, want)
	}
	if !enc.wasClosed() {
		t.Error("the encrypter was not closed, so an AEAD's last chunk would be missing and the " +
			"archive would be unopenable")
	}
}

// TestAConfiguredRecipientWithNoEncrypterUploadsNothingAtAll.
//
// The security property at the push level: not merely that the refusal
// happens, but that it happens BEFORE a session is opened, so there is no
// window in which a plaintext stream could start.
func TestAConfiguredRecipientWithNoEncrypterUploadsNothingAtAll(t *testing.T) {
	body := []byte("the world")
	name := archive.Filename("pz-main", archive.ClassRoutine, testNow)
	sess := &fakeSession{}
	r := &Remote{
		Target:    sftpTargetSpec(t, 0, ageVector),
		Transport: &fakeTransport{S: sess},
		Open:      openBody(body),
		Now:       newClock(testNow).Now,
	}
	_, err := r.Push(context.Background(), verifiedRef(name, body), nil)
	if !errors.Is(err, ErrNoEncrypter) {
		t.Fatalf("Push error = %v, want ErrNoEncrypter", err)
	}
	if ops := sess.log(); len(ops) != 0 {
		t.Fatalf("the transport was touched before the refusal: %v", ops)
	}
}

// TestAnUnverifiedArchiveIsNotPushed.
//
// The same rule as prune. An off-box copy of a truncated tarball is a false
// sense of safety that outlives the local one, because the local one gets
// re-verified by the nightly gvm-job@_verify timer and the remote one does
// not.
func TestAnUnverifiedArchiveIsNotPushed(t *testing.T) {
	sess := &fakeSession{}
	r := &Remote{
		Target:    sftpTargetSpec(t, 0, ""),
		Transport: &fakeTransport{S: sess},
		Open:      openBody([]byte("x")),
		Now:       newClock(testNow).Now,
	}
	unverified := verifiedRef("pz-main-20260904-040003-routine.tar.gz", []byte("x"))
	unverified.VerifiedAt = time.Time{}

	if _, err := r.Push(context.Background(), unverified, nil); !errors.Is(err, ErrPush) {
		t.Fatalf("Push error = %v, want ErrPush", err)
	}
	if _, err := r.Push(context.Background(), nil, nil); !errors.Is(err, ErrPush) {
		t.Errorf("a nil ref was accepted: %v", err)
	}
	if ops := sess.log(); len(ops) != 0 {
		t.Fatalf("the transport was touched for an unverified archive: %v", ops)
	}
}

// TestADisabledRemoteIsSilent: most instances have none, and a push step that
// logged or errored for them would be noise on every nightly.
func TestADisabledRemoteIsSilent(t *testing.T) {
	var r *Remote
	if res, err := r.Push(context.Background(), nil, nil); err != nil || res == nil {
		t.Fatalf("a nil Remote's Push = (%v, %v)", res, err)
	}
	none := &Remote{Target: spec.RemoteTarget{Kind: RemoteNone}}
	if res, err := none.Push(context.Background(), nil, nil); err != nil || len(res.Removed) != 0 {
		t.Fatalf("kind: none Push = (%+v, %v)", res, err)
	}
}

// TestAFailedRetentionDoesNotUndoASuccessfulUpload.
//
// The bytes are off-box, which is what the push was for. A remote directory
// that is one archive over its limit is next week's problem, and failing the
// push over it would make the operator think the upload did not happen.
func TestAFailedRetentionDoesNotUndoASuccessfulUpload(t *testing.T) {
	body := []byte("the world")
	name := archive.Filename("pz-main", archive.ClassRoutine, testNow)
	sess := &fakeSession{FailList: errInjected}
	var notes []string
	r := &Remote{
		Target:    sftpTargetSpec(t, 5, ""),
		Transport: &fakeTransport{S: sess},
		Open:      openBody(body),
		Now:       newClock(testNow).Now,
	}
	res, err := r.Push(context.Background(), verifiedRef(name, body),
		func(s string) { notes = append(notes, s) })
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got := sess.sent(name); !bytes.Equal(got, body) {
		t.Fatalf("the upload did not happen: %q", got)
	}
	if res.Elapsed <= 0 && len(notes) == 0 {
		t.Error("nothing was recorded about the failed retention")
	}
	if !containsSubstring(notes, "keep_remote could not be applied") {
		t.Errorf("notes = %v, want one explaining the retention failure", notes)
	}
}

// TestATransportThatCannotOpenIsReportedWithItsDescription.
func TestATransportThatCannotOpenIsReportedWithItsDescription(t *testing.T) {
	body := []byte("x")
	name := archive.Filename("pz-main", archive.ClassRoutine, testNow)
	r := &Remote{
		Target:    sftpTargetSpec(t, 0, ""),
		Transport: &fakeTransport{FailOpen: errInjected},
		Open:      openBody(body),
		Now:       newClock(testNow).Now,
	}
	res, err := r.Push(context.Background(), verifiedRef(name, body), nil)
	if !errors.Is(err, ErrPush) {
		t.Fatalf("Push error = %v, want ErrPush", err)
	}
	if !strings.Contains(err.Error(), "fake://nas.lan") {
		t.Errorf("the failure does not name the target: %v", err)
	}
	if res.Target == "" {
		t.Error("the result does not say which target was tried")
	}
}

// TestAnEncrypterThatFailsMidStreamFailsThePushRatherThanTruncatingIt.
//
// An upload that reported success with half the archive in it would be the
// worst outcome available: the remote listing would look right and the file
// would be useless.
func TestAnEncrypterThatFailsMidStreamFailsThePushRatherThanTruncatingIt(t *testing.T) {
	body := []byte("the world")
	name := archive.Filename("pz-main", archive.ClassRoutine, testNow)
	r := &Remote{
		Target:    sftpTargetSpec(t, 0, ageVector),
		Transport: &fakeTransport{S: &fakeSession{}},
		Encrypter: &markEncrypter{Fail: errInjected},
		Open:      openBody(body),
		Now:       newClock(testNow).Now,
	}
	if _, err := r.Push(context.Background(), verifiedRef(name, body), nil); !errors.Is(err, ErrPush) {
		t.Fatalf("Push error = %v, want ErrPush", err)
	}
}

// TestRemoteNameKeepsTheLocalBasename.
//
// It is what lets archive.ParseFilename recognise gvm's own uploads in a
// remote listing, which is what makes keep_remote able to tell an archive gvm
// wrote from one somebody else put there.
func TestRemoteNameKeepsTheLocalBasename(t *testing.T) {
	ref := verifiedRef("pz-main-20260904-040003-routine.tar.gz", nil)
	if got := RemoteName(ref, false); got != "pz-main-20260904-040003-routine.tar.gz" {
		t.Errorf("RemoteName = %q", got)
	}
	if got := RemoteName(ref, true); got != "pz-main-20260904-040003-routine.tar.gz.age" {
		t.Errorf("RemoteName encrypted = %q", got)
	}
	if got := RemoteName(nil, false); got != "" {
		t.Errorf("RemoteName(nil) = %q", got)
	}
	// A Linux-produced path read on the Windows development machine: baseOf
	// handles both separators so the test and the production path agree.
	win := &archive.Ref{Path: `C:\tmp\pz-main-20260904-040003-routine.tar.gz`, VerifiedAt: testNow}
	if got := RemoteName(win, false); got != "pz-main-20260904-040003-routine.tar.gz" {
		t.Errorf("RemoteName on a native path = %q", got)
	}
}

func containsOp(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func containsSubstring(xs []string, want string) bool {
	for _, x := range xs {
		if strings.Contains(x, want) {
			return true
		}
	}
	return false
}
