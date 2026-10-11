package steamcmd

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The appinfo.vdf purge. Without it, Steam serves the cached metadata and the
// nightly reports "no update" indefinitely while the server runs an old build.
func TestPurgeAppInfoRemovesEveryCacheAndNothingElse(t *testing.T) {
	root := t.TempDir()
	caches := []string{
		filepath.Join(root, ".steam", "appcache", "appinfo.vdf"),
		filepath.Join(root, ".local", "share", "Steam", "appcache", "appinfo.vdf"),
		// Steam's own casing has varied; the match is case-insensitive.
		filepath.Join(root, "Steam", "appcache", "AppInfo.vdf"),
	}
	keep := []string{
		filepath.Join(root, ".steam", "appcache", "packageinfo.vdf"),
		filepath.Join(root, "config", "config.vdf"),
		filepath.Join(root, "ssfn1234567890"),
	}
	for _, p := range append(slices.Clone(caches), keep...) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := PurgeAppInfo(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != len(caches) {
		t.Errorf("removed %v, want %d files", removed, len(caches))
	}
	for _, p := range caches {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived", p)
		}
	}
	for _, p := range keep {
		if _, err := os.Stat(p); err != nil {
			// config.vdf holds the cached refresh token and ssfn* the sentry
			// files: deleting either would force an interactive login.
			t.Errorf("%s was deleted: %v", p, err)
		}
	}
}

// The next call after a match is os.Remove, so a relative root -- which would
// walk whatever directory the daemon happens to be in -- is a refusal.
func TestPurgeAppInfoRefusesARelativeRoot(t *testing.T) {
	for _, root := range []string{"", ".", "cache/steam"} {
		if _, err := PurgeAppInfo(root); !errors.Is(err, ErrPurgeRoot) {
			t.Errorf("PurgeAppInfo(%q) err = %v, want ErrPurgeRoot", root, err)
		}
	}
}

func TestPurgeAppInfoOnAFirstRun(t *testing.T) {
	removed, err := PurgeAppInfo(filepath.Join(t.TempDir(), "never-created"))
	if err != nil {
		t.Fatalf("a HOME that does not exist yet is the first run, not an error: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v", removed)
	}
}

func appInfoNode(t *testing.T) *KV {
	t.Helper()
	node, err := ParseAppInfo(strings.NewReader(appInfoOutput), 380870)
	if err != nil {
		t.Fatal(err)
	}
	return node
}

func TestRemoteBuildID(t *testing.T) {
	node := appInfoNode(t)
	if got, err := RemoteBuildID(node, DefaultBranch); err != nil || got != "18461233" {
		t.Errorf("public buildid = %q, %v", got, err)
	}
	if got, err := RemoteBuildID(node, "unstable"); err != nil || got != "18470001" {
		t.Errorf("unstable buildid = %q, %v", got, err)
	}
	// An empty branch is public, because Steam's name for "not a beta" is the
	// literal string public.
	if got, _ := RemoteBuildID(node, ""); got != "18461233" {
		t.Errorf("empty branch = %q", got)
	}
}

func TestRemoteBuildIDRefusals(t *testing.T) {
	node := appInfoNode(t)
	err := func() error { _, e := RemoteBuildID(node, "b42-beta"); return e }()
	if !errors.Is(err, ErrNoSuchBranch) {
		t.Fatalf("err = %v, want ErrNoSuchBranch", err)
	}
	// The available branches belong in the message: a manifest naming a branch
	// that was renamed is otherwise a dead end.
	if !strings.Contains(err.Error(), "unstable") {
		t.Errorf("the refusal does not list the branches Steam publishes: %v", err)
	}

	bare, perr := ParseAppInfo(strings.NewReader("\"380870\"\n{\n\t\"common\"\n\t{\n\t}\n}\n"), 380870)
	if perr != nil {
		t.Fatal(perr)
	}
	if _, err := RemoteBuildID(bare, DefaultBranch); !errors.Is(err, ErrNoRemoteBuild) {
		t.Errorf("err = %v, want ErrNoRemoteBuild", err)
	}
}

// The preflight figure is the worst case, and it must not include a depot for
// another platform.
func TestEstimateBytesFiltersByPlatform(t *testing.T) {
	node := appInfoNode(t)
	if got, want := EstimateBytes(node, "linux"), int64(3461823232); got != want {
		t.Errorf("linux estimate = %d, want %d", got, want)
	}
	if got, want := EstimateBytes(node, "windows"), int64(4000000000); got != want {
		t.Errorf("windows estimate = %d, want %d", got, want)
	}
	// No platform means no filter: every depot counts.
	if got, want := EstimateBytes(node, ""), int64(3461823232+4000000000); got != want {
		t.Errorf("unfiltered estimate = %d, want %d", got, want)
	}
	// "branches" lives beside the numeric depot ids and is not a depot.
	if got := EstimateBytes(&KV{Block: true}, "linux"); got != 0 {
		t.Errorf("an empty node estimated %d", got)
	}
}

func TestCheckBuild(t *testing.T) {
	cases := []struct {
		name          string
		local, remote string
		want          BuildState
		update        bool
		says          string
	}{
		{"nothing installed", "", "18461233", BuildNotInstalled, true, "nothing is installed"},
		{"current", "18461233", "18461233", BuildCurrent, false, "is current"},
		{"stale", "18452901", "18461233", BuildStale, true, "18452901 -> 18461233"},
		{"ahead of the branch", "18470001", "18461233", BuildAhead, false, "NEWER"},
		{"not numbers", "26.2.0.76", "26.3.0.1", BuildDiffers, true, "not comparable"},
		{"not numbers and equal", "26.2.0.76", "26.2.0.76", BuildCurrent, false, "is current"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := CheckBuild(tc.local, tc.remote, DefaultBranch)
			if err != nil {
				t.Fatal(err)
			}
			if b.State != tc.want {
				t.Fatalf("state = %s, want %s", b.State, tc.want)
			}
			if b.UpdateAvailable() != tc.update {
				t.Errorf("UpdateAvailable = %t, want %t", b.UpdateAvailable(), tc.update)
			}
			if !strings.Contains(b.Summary(), tc.says) {
				t.Errorf("Summary = %q, want something about %q", b.Summary(), tc.says)
			}
		})
	}
}

// A check that could not read the remote build must never be reported as a
// check that found nothing to do. That failure mode is a month of nightlies
// that say "no update" while the server falls behind.
func TestCheckBuildRefusesAnEmptyRemote(t *testing.T) {
	b, err := CheckBuild("18452901", "", DefaultBranch)
	if !errors.Is(err, ErrNoRemoteBuild) {
		t.Fatalf("err = %v, want ErrNoRemoteBuild", err)
	}
	if b.UpdateAvailable() {
		t.Error("a failed check reported an update")
	}
}

// Rolling a live world backwards on a nightly timer is not something gvm does
// without being asked.
func TestABuildAheadOfTheBranchIsNotAnUpdate(t *testing.T) {
	b, err := CheckBuild("18470001", "18461233", "unstable")
	if err != nil {
		t.Fatal(err)
	}
	if b.UpdateAvailable() {
		t.Fatal("a local build newer than the branch triggered an update")
	}
	if !strings.Contains(b.Summary(), "cached appinfo") {
		t.Errorf("the summary does not name the likely cause: %q", b.Summary())
	}
}
