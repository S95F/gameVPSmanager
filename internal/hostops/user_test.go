package hostops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// fakeTools stands in for shadow-utils. It records every argv it was handed,
// which is the only way to assert "nothing ran" -- the assertion this whole
// file exists for.
type fakeTools struct {
	users  map[string]platform.User
	groups map[string]bool
	binary map[string]bool
	ran    [][]string

	// exit is the code the next run reports, and lookupErr replaces the
	// "absent" answer with a broken account database.
	exit      int
	lookupErr error
	// created is what a successful useradd adds to users, so the read-back
	// after creation has something to find.
	created func(name string)
}

func newFakeTools() *fakeTools {
	return &fakeTools{
		users:  map[string]platform.User{},
		groups: map[string]bool{},
		binary: map[string]bool{
			"/usr/sbin/useradd":  true,
			"/usr/sbin/groupadd": true,
			"/usr/sbin/gpasswd":  true,
			"/usr/sbin/nologin":  true,
		},
	}
}

func (f *fakeTools) users_() *systemUsers {
	return &systemUsers{
		lookupUser: func(name string) (platform.User, error) {
			if f.lookupErr != nil {
				return platform.User{}, f.lookupErr
			}
			if u, ok := f.users[name]; ok {
				return u, nil
			}
			return platform.User{}, fmt.Errorf("%w: %s", ErrNoSuchAccount, name)
		},
		lookupGroup: func(name string) error {
			if f.groups[name] {
				return nil
			}
			return fmt.Errorf("%w: %s", ErrNoSuchGroup, name)
		},
		stat: func(p string) (fs.FileInfo, error) {
			if f.binary[p] {
				return fakeInfo{name: p, e: &fakeEntry{mode: 0o755}}, nil
			}
			return nil, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
		},
		run: func(_ context.Context, argv []string) error {
			f.ran = append(f.ran, argv)
			if f.exit != 0 {
				return &toolError{Argv: argv, Code: f.exit, Stderr: "simulated"}
			}
			if strings.HasSuffix(argv[0], "groupadd") {
				f.groups[argv[len(argv)-1]] = true
			}
			if strings.HasSuffix(argv[0], "useradd") {
				name := argv[len(argv)-1]
				f.users[name] = platform.User{Name: name, UID: 991, GID: 991}
				if f.created != nil {
					f.created(name)
				}
			}
			return nil
		},
	}
}

// TestEnsureSystemUserNeverTouchesAnExistingAccount is the contract.
//
// An adopted instance's account predates gvm: pzserver owns /opt/pzserver and
// /home/pzserver/Zomboid, may be in groups gvm cannot see, and may be what a
// cron job or an sftp login uses. "Ensure" in the configuration-management
// sense -- make it match my idea of it -- would rewrite that account's home
// directory and shell on the first apply and take a working server down with
// nothing recording what it used to be.
//
// The assertion is not "it did not call usermod". It is that NO PROGRAM RAN AT
// ALL, which is the only form that survives somebody adding a new branch.
func TestEnsureSystemUserNeverTouchesAnExistingAccount(t *testing.T) {
	f := newFakeTools()
	f.users["pzserver"] = platform.User{Name: "pzserver", UID: 1001, GID: 1001, Home: "/home/pzserver"}
	su := f.users_()

	got, err := su.EnsureSystemUser(context.Background(), "pzserver", "/srv/gvm/pz-main")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.ran) != 0 {
		t.Fatalf("an existing account made gvm run %v", f.ran)
	}
	if got.Home != "/home/pzserver" || got.UID != 1001 {
		t.Errorf("the account came back as %+v; it must be returned exactly as found, "+
			"not as the caller asked for it", got)
	}
}

// TestEnsureSystemUserCreatesSection64sAccount pins the invocation.
func TestEnsureSystemUserCreatesSection64sAccount(t *testing.T) {
	f := newFakeTools()
	su := f.users_()

	got, err := su.EnsureSystemUser(context.Background(), "gvm-pz-main", "/srv/gvm/pz-main")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.ran) != 2 {
		t.Fatalf("ran %v, want groupadd then useradd", f.ran)
	}
	// The group first: useradd --gid needs it, and a per-instance group of
	// which the instance is the only member is what keeps one compromised
	// workshop item out of every other world (D-10).
	wantGroup := []string{"/usr/sbin/groupadd", "--system", "gvm-pz-main"}
	if strings.Join(f.ran[0], " ") != strings.Join(wantGroup, " ") {
		t.Errorf("groupadd argv:\n\tgot  %v\n\twant %v", f.ran[0], wantGroup)
	}
	wantUser := []string{
		"/usr/sbin/useradd", "--system", "--gid", "gvm-pz-main",
		"--home-dir", "/srv/gvm/pz-main", "--no-create-home",
		"--shell", "/usr/sbin/nologin",
		"--comment", "gvm instance pz-main",
		"gvm-pz-main",
	}
	if strings.Join(f.ran[1], " ") != strings.Join(wantUser, " ") {
		t.Errorf("useradd argv:\n\tgot  %v\n\twant %v", f.ran[1], wantUser)
	}
	// The uid is useradd's to pick, so it has to be read back rather than
	// asserted: an apply that reported a uid it invented would be wrong in
	// exactly the field the next chown depends on.
	if got.UID != 991 {
		t.Errorf("the account was not read back after creation: %+v", got)
	}
}

// TestNoUseraddArgvEverModifies: the argv builders are pure, so the rule can
// be checked against every one of them at once.
func TestNoAccountToolIsAModifyingOne(t *testing.T) {
	argvs := [][]string{
		useraddArgv("/usr/sbin/useradd", "gvm-x", "/srv/gvm/x", "/usr/sbin/nologin"),
		groupaddArgv("/usr/sbin/groupadd", "gvm-x"),
	}
	for _, argv := range argvs {
		for _, forbidden := range []string{"usermod", "groupmod", "userdel", "groupdel", "chpasswd", "passwd"} {
			if strings.Contains(argv[0], forbidden) {
				t.Errorf("%v invokes %s", argv, forbidden)
			}
		}
		if !strings.HasPrefix(argv[0], "/") {
			t.Errorf("%v resolves its program through PATH", argv)
		}
	}
}

// TestNameAlreadyInUseIsTolerated closes the race between the lookup and the
// tool: two applies in flight for one new instance both see "absent", and one
// of them has to be allowed to lose.
func TestNameAlreadyInUseIsTolerated(t *testing.T) {
	f := newFakeTools()
	f.exit = exitAlreadyInUse
	// The tool "fails" with 9 and the account turns out to exist, which is
	// exactly what a lost race looks like.
	f.created = nil
	su := f.users_()
	su.lookupUser = func(name string) (platform.User, error) {
		if len(f.ran) == 0 {
			return platform.User{}, fmt.Errorf("%w: %s", ErrNoSuchAccount, name)
		}
		return platform.User{Name: name, UID: 992, GID: 992}, nil
	}

	got, err := su.EnsureSystemUser(context.Background(), "gvm-pz-main", "/srv/gvm/pz-main")
	if err != nil {
		t.Fatalf("exit %d should be tolerated: %v", exitAlreadyInUse, err)
	}
	if got.UID != 992 {
		t.Errorf("account came back as %+v", got)
	}

	// Any other exit code is a real failure and must not be swallowed.
	g := newFakeTools()
	g.exit = 1
	if _, err := g.users_().EnsureSystemUser(context.Background(), "gvm-x", "/srv/gvm/x"); err == nil {
		t.Fatal("a failing groupadd was reported as success")
	}
}

// TestABrokenAccountDatabaseStopsEverything: creating a user on top of a
// lookup that failed for an unknown reason is how a box ends up with two
// accounts owning one world.
func TestABrokenAccountDatabaseStopsEverything(t *testing.T) {
	f := newFakeTools()
	f.lookupErr = errors.New("/etc/passwd: input/output error")
	if _, err := f.users_().EnsureSystemUser(context.Background(), "gvm-pz-main", "/srv/gvm/pz-main"); err == nil {
		t.Fatal("a broken account database was treated as an absent account")
	}
	if len(f.ran) != 0 {
		t.Fatalf("ran %v after a lookup failure", f.ran)
	}
}

func TestMissingToolsAreNamed(t *testing.T) {
	f := newFakeTools()
	f.binary = map[string]bool{}
	_, err := f.users_().EnsureSystemUser(context.Background(), "gvm-x", "/srv/gvm/x")
	if !errors.Is(err, ErrNoTool) {
		t.Fatalf("got %v, want ErrNoTool", err)
	}
	if !strings.Contains(err.Error(), "/usr/sbin/groupadd") {
		t.Errorf("the error does not name what is missing: %v", err)
	}
}

// TestToolResolutionPrefersTheMergedUsrPath: both spellings survive on Debian
// and derivatives, and the list is ordered rather than searched.
func TestToolResolutionUsesAbsoluteCandidatesInOrder(t *testing.T) {
	f := newFakeTools()
	f.binary = map[string]bool{"/sbin/useradd": true, "/sbin/groupadd": true}
	su := f.users_()
	if _, err := su.EnsureSystemUser(context.Background(), "gvm-x", "/srv/gvm/x"); err != nil {
		t.Fatal(err)
	}
	if f.ran[0][0] != "/sbin/groupadd" || f.ran[1][0] != "/sbin/useradd" {
		t.Errorf("resolved to %s and %s", f.ran[0][0], f.ran[1][0])
	}

	// With neither nologin present the canonical path is still recorded: an
	// account whose shell does not exist is locked too, and inventing a
	// different fallback would be a decision nobody can see in `getent passwd`.
	g := newFakeTools()
	delete(g.binary, "/usr/sbin/nologin")
	su = g.users_()
	if _, err := su.EnsureSystemUser(context.Background(), "gvm-x", "/srv/gvm/x"); err != nil {
		t.Fatal(err)
	}
	if got := g.ran[1][8]; got != "/usr/sbin/nologin" {
		t.Errorf("shell is %q", got)
	}
}

func TestEnsureGroupIsIdempotent(t *testing.T) {
	f := newFakeTools()
	f.groups["gvm-pz-main"] = true
	if err := f.users_().EnsureGroup(context.Background(), "gvm-pz-main"); err != nil {
		t.Fatal(err)
	}
	if len(f.ran) != 0 {
		t.Fatalf("an existing group made gvm run %v", f.ran)
	}
}

func TestExitCodeSurvivesWrapping(t *testing.T) {
	err := fmt.Errorf("ensure account: %w", &toolError{Argv: []string{"/usr/sbin/useradd"}, Code: 9})
	if got := exitCode(err); got != 9 {
		t.Errorf("exitCode through a wrap is %d, want 9", got)
	}
	if got := exitCode(errors.New("context canceled")); got != -1 {
		t.Errorf("a non-exit failure reports %d, want -1", got)
	}
}

// TestAddToGroupIsAvailableButIsNotAVerb.
//
// The packaging needs it -- gvm-web joins gvm-console at install time -- and
// admin.sock must never expose it: "add any account to any group" is a
// complete escalation on its own, because the sudo group is a group. The gap
// between this method existing and there being no verb for it is where the
// two-daemon split is actually drawn.
func TestAddToGroupIsAvailableButIsNotAVerb(t *testing.T) {
	f := newFakeTools()
	f.binary["/usr/bin/gpasswd"] = true
	if err := f.users_().AddToGroup(context.Background(), "gvm", "gvm-console"); err != nil {
		t.Fatal(err)
	}
	want := "/usr/bin/gpasswd --add gvm gvm-console"
	if got := strings.Join(f.ran[0], " "); got != want {
		t.Errorf("argv is %q, want %q", got, want)
	}
	for _, v := range Verbs() {
		if strings.Contains(string(v), "group") {
			t.Errorf("%q is declared; group membership is not a socket-reachable operation", v)
		}
	}
}
