package edgeops

import (
	"context"
	"errors"
	"strings"
	"testing"

	"time"

	"github.com/S95F/gameVPSmanager/internal/edge"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// fakeSystem records every effect in order and scripts the failures.
type fakeSystem struct {
	files map[string][]byte
	calls []string

	validateErr error
	validateOut string
	reloadErr   error
	adminBody   []byte
	adminErr    error
	// reloadErrOnce fails only the FIRST reload, so a rollback's own reload
	// succeeds.
	reloadErrOnce bool
	reloads       int
}

func newFake() *fakeSystem {
	return &fakeSystem{
		files:     map[string][]byte{},
		adminBody: []byte(`{"servers":{"srv0":{}}}`),
	}
}

func (f *fakeSystem) ReadFile(path string) ([]byte, error) {
	f.calls = append(f.calls, "read:"+path)
	b, ok := f.files[path]
	if !ok {
		return nil, ErrNotExist
	}
	return b, nil
}

func (f *fakeSystem) WriteFile(path string, data []byte, _ uint32) error {
	f.calls = append(f.calls, "write:"+path)
	f.files[path] = append([]byte(nil), data...)
	return nil
}

func (f *fakeSystem) Rename(oldPath, newPath string) error {
	f.calls = append(f.calls, "rename:"+oldPath+"->"+newPath)
	b, ok := f.files[oldPath]
	if !ok {
		return errors.New("rename: no such file")
	}
	f.files[newPath] = b
	delete(f.files, oldPath)
	return nil
}

func (f *fakeSystem) Remove(path string) error {
	f.calls = append(f.calls, "remove:"+path)
	delete(f.files, path)
	return nil
}

func (f *fakeSystem) Validate(_ context.Context, path string) (string, error) {
	f.calls = append(f.calls, "validate:"+path)
	return f.validateOut, f.validateErr
}

func (f *fakeSystem) ReloadCaddy(context.Context) error {
	f.reloads++
	f.calls = append(f.calls, "reload")
	if f.reloadErr != nil && (!f.reloadErrOnce || f.reloads == 1) {
		return f.reloadErr
	}
	return nil
}

func (f *fakeSystem) AdminGet(_ context.Context, sock, path string) ([]byte, error) {
	f.calls = append(f.calls, "admin:"+path)
	return f.adminBody, f.adminErr
}

func managedHost() spec.HostConfig {
	h := spec.DefaultHostConfig()
	h.Spec.Edge.Mode = "caddy_managed"
	h.Spec.Edge.Site = "gvm.example.com"
	h.Spec.Edge.TLS.Email = "ops@example.com"
	return h
}

func plan(t *testing.T, h *spec.HostConfig) *edge.Plan {
	t.Helper()
	p, err := edge.ComputePlan(h)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

const configPath = "/etc/gvm/caddy/Caddyfile"

func TestApplyHappyPath(t *testing.T) {
	f := newFake()
	f.files[configPath] = []byte("# old config\n")
	h := managedHost()

	res, err := Apply(context.Background(), f, &h, plan(t, &h))
	if err != nil {
		t.Fatalf("Apply: %v\n%s", err, strings.Join(res.Log, "\n"))
	}
	if !res.Changed || res.RolledBack {
		t.Errorf("Changed=%v RolledBack=%v", res.Changed, res.RolledBack)
	}

	// Order: stage, validate the STAGED file, back up, swap, reload, verify.
	want := []string{
		"read:" + configPath,
		"write:" + configPath + ".new",
		"validate:" + configPath + ".new",
		"write:" + configPath + ".bak",
		"rename:" + configPath + ".new->" + configPath,
		"reload",
		"admin:/config/apps/http",
	}
	if strings.Join(f.calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls:\n  got  %v\n  want %v", f.calls, want)
	}
	if !strings.Contains(string(f.files[configPath]), "gvm.example.com") {
		t.Error("the live file is not the rendered plan")
	}
	if string(f.files[configPath+".bak"]) != "# old config\n" {
		t.Error("the backup is not the previous config")
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	f := newFake()
	h := managedHost()
	if _, err := Apply(context.Background(), f, &h, plan(t, &h)); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	res, err := Apply(context.Background(), f, &h, plan(t, &h))
	if err != nil || res.Changed {
		t.Fatalf("second apply: changed=%v err=%v", res.Changed, err)
	}
	for _, c := range f.calls {
		if c == "reload" {
			t.Error("a no-op apply reloaded Caddy")
		}
	}
}

// TestApplyValidationFailureTouchesNothing: a config caddy refuses never
// reaches the live path, and the staged file is cleaned up.
func TestApplyValidationFailureTouchesNothing(t *testing.T) {
	f := newFake()
	f.files[configPath] = []byte("# old config\n")
	f.validateErr = errors.New("exit 1")
	f.validateOut = "adapting config: unrecognized directive"
	h := managedHost()

	_, err := Apply(context.Background(), f, &h, plan(t, &h))
	if err == nil || !strings.Contains(err.Error(), "unrecognized directive") {
		t.Fatalf("err = %v, want caddy's own words", err)
	}
	if string(f.files[configPath]) != "# old config\n" {
		t.Error("the live config changed on a validation failure")
	}
	if _, staged := f.files[configPath+".new"]; staged {
		t.Error("the staged file was left behind")
	}
	if f.reloads != 0 {
		t.Error("caddy was reloaded on a validation failure")
	}
}

// TestApplyRollsBackWhenTheConfigDoesNotComeUp: reload succeeded, the admin
// API never reported servers — the previous config comes back (§17.2).
func TestApplyRollsBackWhenTheConfigDoesNotComeUp(t *testing.T) {
	f := newFake()
	f.files[configPath] = []byte("# old config\n")
	f.adminBody = []byte(`{}`)
	h := managedHost()

	res, err := Apply(shortCtx(t), f, &h, plan(t, &h))
	if err == nil {
		t.Fatal("Apply should fail when the config does not come up")
	}
	if !res.RolledBack {
		t.Fatalf("not rolled back:\n%s", strings.Join(res.Log, "\n"))
	}
	if string(f.files[configPath]) != "# old config\n" {
		t.Error("the previous config is not live again")
	}
	if f.reloads < 2 {
		t.Error("the rollback did not reload the restored config")
	}
}

func TestApplyRollsBackOnReloadFailure(t *testing.T) {
	f := newFake()
	f.files[configPath] = []byte("# old config\n")
	f.reloadErr = errors.New("job failed")
	f.reloadErrOnce = true
	h := managedHost()

	res, err := Apply(context.Background(), f, &h, plan(t, &h))
	if err == nil || !res.RolledBack {
		t.Fatalf("err=%v rolledback=%v", err, res.RolledBack)
	}
	if string(f.files[configPath]) != "# old config\n" {
		t.Error("the previous config is not live again")
	}
}

// TestSnippetModeWritesAndNeverReloads: the operator owns Caddy in snippet
// mode; gvm renders the site block and stops (§17.3).
func TestSnippetModeWritesAndNeverReloads(t *testing.T) {
	f := newFake()
	h := managedHost()
	h.Spec.Edge.Mode = "caddy_snippet"
	h.Spec.Edge.Listen = "127.0.0.1:8130"

	res, err := Apply(context.Background(), f, &h, plan(t, &h))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Error("snippet not written")
	}
	if _, ok := f.files[SnippetPath]; !ok {
		t.Fatalf("no snippet at %s", SnippetPath)
	}
	for _, c := range f.calls {
		if c == "reload" || strings.HasPrefix(c, "validate:") {
			t.Errorf("snippet mode performed %q; the operator owns Caddy", c)
		}
	}
	if !strings.Contains(strings.Join(res.Log, "\n"), "import "+SnippetPath) {
		t.Error("the log should hand the operator the import line")
	}
}

// ── CA export ────────────────────────────────────────────────────────────────

func TestExportCA(t *testing.T) {
	f := newFake()
	f.adminBody = []byte(`{"id":"local","name":"Caddy Local Authority","root_certificate":"-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"}`)
	var out strings.Builder
	if err := ExportCA(context.Background(), f, "/run/caddy/admin.sock", &out); err != nil {
		t.Fatal(err)
	}
	pem := out.String()
	if !strings.HasPrefix(pem, "-----BEGIN CERTIFICATE-----") || !strings.Contains(pem, "MIIB") {
		t.Errorf("not a PEM certificate:\n%s", pem)
	}
}

// TestExportCANeverWritesAKey: key material in the response is refused, not
// filtered — a CA endpoint returning keys is a server to distrust entirely.
func TestExportCANeverWritesAKey(t *testing.T) {
	f := newFake()
	f.adminBody = []byte(`{"root_certificate":"-----BEGIN CERTIFICATE-----\nA\n-----END CERTIFICATE-----\n-----BEGIN PRIVATE KEY-----\nB\n-----END PRIVATE KEY-----\n"}`)
	var out strings.Builder
	if err := ExportCA(context.Background(), f, "/run/caddy/admin.sock", &out); err == nil {
		t.Fatal("key material was not refused")
	}
	if out.Len() != 0 {
		t.Errorf("something was written anyway: %q", out.String())
	}
}

// shortCtx bounds the verify poll so its 10s window does not stall the suite.
func shortCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	t.Cleanup(cancel)
	return ctx
}
