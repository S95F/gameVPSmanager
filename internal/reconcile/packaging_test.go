package reconcile

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/S95F/gameVPSmanager/internal/hostops"
)

// The packaged unit files and this package are two halves of one supervision
// policy, and nothing in the build makes them agree. These tests are what
// does.
//
// The failure they exist to catch is quiet in both directions. A directive
// that appears in BOTH the template and the generated drop-in has two owners,
// and the day they disagree the merged unit is whichever systemd read last. A
// directive the drop-in assumes the template sets -- ProtectSystem=full, which
// the write-probe restates -- silently stops being true if somebody edits the
// template, and the probe then tests a sandbox the game does not run in.

func readPackaged(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "packaging", rel))
	if err != nil {
		t.Fatalf("the packaged file is missing; the deb would install a unit gvm cannot drive: %v", err)
	}
	return string(b)
}

// TestTheTemplateLeavesEveryPerInstanceDirectiveToTheDropIn.
//
// Section 7.1's split: one supervision policy for the whole box in the
// template, all variance in a diffable generated file. A directive in both
// places is not redundancy, it is two owners for one value.
func TestTheTemplateLeavesEveryPerInstanceDirectiveToTheDropIn(t *testing.T) {
	tmpl := readPackaged(t, "systemd/gvm@.service")
	body, _ := renderCase(t, goldenCases()[0])

	perInstance := []string{
		"User=", "Group=", "WorkingDirectory=", "Environment=", "ReadWritePaths=",
		"ProtectHome=", "MemoryHigh=", "MemoryMax=", "MemorySwapMax=", "LimitNOFILE=",
		"SystemCallArchitectures=", "MemoryDenyWriteExecute=", "LoadCredential=", "UMask=",
	}
	for _, d := range perInstance {
		inTemplate := hasDirective(tmpl, d)
		inDropIn := hasDirective(string(body), d)
		switch {
		case d == "ProtectHome=":
			// The one deliberate exception, and it is deliberate in a
			// specific way: the template sets read-only so that a unit with
			// NO drop-in is still confined, and the drop-in ALWAYS overrides
			// it with an explicit value (D-15). Both must be present.
			if !inTemplate || !inDropIn {
				t.Errorf("ProtectHome must be in the template (as a floor) and in the drop-in "+
					"(as the decision): template=%v dropin=%v", inTemplate, inDropIn)
			}
		case inTemplate:
			t.Errorf("%s is set in the packaged template AND is a per-instance value. Two owners "+
				"for one directive is a merged unit nobody can predict.", d)
		case !inDropIn:
			t.Errorf("%s is in neither the template nor the generated drop-in", d)
		}
	}
}

// TestTheDropInDoesNotRestateTheBoxWideHardening.
//
// The other direction. NoNewPrivileges, PrivateTmp, ProtectSystem and the rest
// are policy for every instance on the box, so a gvm upgrade that tightens one
// tightens it for servers created last year. Copying them into the generated
// file would mean the upgrade only reached instances re-applied afterwards --
// which is the exact failure per-instance unit FILES would have had, and the
// reason section 7.1 chose a template plus a drop-in.
func TestTheDropInDoesNotRestateTheBoxWideHardening(t *testing.T) {
	body, _ := renderCase(t, goldenCases()[0])
	for _, d := range []string{
		"NoNewPrivileges=", "PrivateTmp=", "PrivateDevices=", "ProtectSystem=",
		"ProtectKernelTunables=", "RestrictAddressFamilies=", "KillMode=", "KillSignal=",
		"Restart=", "RestartPreventExitStatus=", "WatchdogSec=", "Type=", "Slice=",
		"ExecStart=", "ExecStartPre=",
	} {
		if hasDirective(string(body), d) {
			t.Errorf("%s is box-wide policy and belongs in the packaged template; a copy in the "+
				"generated drop-in would stop a gvm upgrade from reaching this instance", d)
		}
	}
}

// TestTheProbeRestatesTheTemplatesProtectSystem.
//
// The transient probe unit inherits nothing from gvm@.service, so it has to
// carry ProtectSystem itself -- and it has to carry the SAME value, or it
// proves something about a sandbox the game does not run in.
func TestTheProbeRestatesTheTemplatesProtectSystem(t *testing.T) {
	tmpl := readPackaged(t, "systemd/gvm@.service")
	_, sb := renderCase(t, goldenCases()[0])

	want := directiveValue(tmpl, "ProtectSystem")
	if want == "" {
		t.Fatal("the packaged template sets no ProtectSystem at all")
	}
	if sb.ProtectSystem != want {
		t.Errorf("the probe runs with ProtectSystem=%s and the game runs with ProtectSystem=%s",
			sb.ProtectSystem, want)
	}
}

// TestTheTemplateRefusesToRestartTheExitCodeTheProbeUses.
//
// `gvm probe-write` exits 78 (EX_CONFIG) when the sandbox blocks a write, and
// the runner uses the same code for the same condition at boot. Restarting
// cannot fix either, and a restart loop turns one legible failure into a
// journal nobody reads.
func TestTheTemplateRefusesToRestartTheExitCodeTheProbeUses(t *testing.T) {
	tmpl := readPackaged(t, "systemd/gvm@.service")
	prevent := directiveValue(tmpl, "RestartPreventExitStatus")
	for _, code := range []string{"64", "65", "66", "78"} {
		if !containsField(prevent, code) {
			t.Errorf("RestartPreventExitStatus=%q does not include %s; that failure would "+
				"restart-loop instead of being reported once", prevent, code)
		}
	}
	// 70 (panic) and 75 (orphan) must NOT be there: those are the two that a
	// restart genuinely fixes.
	for _, code := range []string{"70", "75"} {
		if containsField(prevent, code) {
			t.Errorf("RestartPreventExitStatus=%q includes %s, which a restart does fix", prevent, code)
		}
	}
}

// TestTheTemplateKeepsTheDecisionsSection7CallsOut.
//
// Each of these was chosen against an obvious alternative and each has a
// failure attached, so each gets an assertion rather than only a comment.
func TestTheTemplateKeepsTheDecisionsSection7CallsOut(t *testing.T) {
	tmpl := readPackaged(t, "systemd/gvm@.service")

	if hasDirective(tmpl, "ExecStop=") {
		t.Error("the template has an ExecStop=. systemd's manual: a command that asks a service " +
			"to terminate but does not wait is not sufficient. The runner walks the ladder and " +
			"blocks; an ExecStop would race it.")
	}
	if hasDirective(tmpl, "PartOf=") {
		t.Error("PartOf=gvm.target would stop every world when the control plane stops (D-14)")
	}
	if v := directiveValue(tmpl, "TimeoutStartSec"); v == "infinity" || v == "" {
		t.Errorf("TimeoutStartSec=%q. `infinity` leaves a wedged boot in `activating` forever "+
			"with Restart=on-failure never firing (D-12)", v)
	}
	if v := directiveValue(tmpl, "KillMode"); v != "mixed" {
		t.Errorf("KillMode=%q, want mixed: SIGTERM must reach the RUNNER only, so it can walk "+
			"the shutdown ladder rather than the game being killed under it", v)
	}
	if v := directiveValue(tmpl, "Slice"); v != "gvm.slice" {
		t.Errorf("Slice=%q. A game must never run under gvm-jobs.slice, whose MemoryMax is "+
			"sized for a downloader", v)
	}
	if !strings.Contains(directiveValue(tmpl, "RestrictAddressFamilies"), "AF_NETLINK") {
		t.Error("AF_NETLINK is missing; the JVM enumerates interfaces through it and fails at startup")
	}
	if v := directiveValue(tmpl, "LogRateLimitBurst"); v != "0" {
		t.Errorf("LogRateLimitBurst=%q; journald's default drops >10k msgs/30s and a modded "+
			"server loses its crash lines", v)
	}
}

// TestTheJobUnitCanReachAnAdoptedWorld.
//
// A backup job that could not read /home/pzserver/Zomboid would be a backup of
// nothing, discovered at restore time.
func TestTheJobUnitCanReachAnAdoptedWorld(t *testing.T) {
	job := readPackaged(t, "systemd/gvm-job@.service")
	if v := directiveValue(job, "ProtectHome"); v != "no" {
		t.Errorf("ProtectHome=%q on the job unit; a backup of an adopted world would read nothing", v)
	}
	if v := directiveValue(job, "ProtectControlGroups"); v != "no" {
		t.Errorf("ProtectControlGroups=%q; systemd-run needs the manager, and D-16 runs every "+
			"third-party program through it", v)
	}
	if v := directiveValue(job, "Slice"); v != "gvm-jobs.slice" {
		t.Errorf("Slice=%q, want gvm-jobs.slice: a 65 GB download must not starve a live world", v)
	}
	if v := directiveValue(job, "User"); v != "root" {
		t.Errorf("User=%q; a job archives, restores and reconciles, all of which are root's", v)
	}
}

// TestTheJobSliceIsNestedUnderTheGameSlice.
//
// systemd derives the parent from the NAME, which is what makes the job
// slice's CPUWeight a ratio against the games rather than against the box.
func TestTheJobSliceIsNestedUnderTheGameSlice(t *testing.T) {
	if !strings.HasPrefix("gvm-jobs.slice", strings.TrimSuffix("gvm.slice", ".slice")+"-") {
		t.Fatal("gvm-jobs.slice no longer nests under gvm.slice by name")
	}
	games := readPackaged(t, "systemd/gvm.slice")
	if hasDirective(games, "MemoryMax=") {
		t.Error("gvm.slice caps the SUM of every world's memory; one instance's leak would " +
			"throttle every other server on the box")
	}
	jobs := readPackaged(t, "systemd/gvm-jobs.slice")
	if !hasDirective(jobs, "MemoryMax=") {
		t.Error("gvm-jobs.slice has no MemoryMax; a modpack installer would be allowed to take " +
			"the box down")
	}
}

// TestEveryDirectoryApplyEnsuresHasAParentSomebodyCreates.
//
// hostops creates ONE directory per call and refuses a missing parent, on
// purpose: os.MkdirAll would give every intermediate directory the leaf's
// mode, so ensuring /var/log/gvm/<name> at 0750 on a host where tmpfiles had
// not run would silently create /var/log/gvm at 0750 where section 5.1 says
// 0755.
//
// That refusal is only safe if every parent is created by something. This
// walks the apply's directory list IN ORDER and checks each parent against
// tmpfiles.d plus the directories created earlier in the same list -- which is
// exactly the check nobody performs by hand and which caught
// /var/log/gvm/<name>/jobs being ensured before its parent.
func TestEveryDirectoryApplyEnsuresHasAParentSomebodyCreates(t *testing.T) {
	// Both shapes, because they plan different lists. A MANAGED instance also
	// gets managedGameDirs, whose two paths come from the instance YAML rather
	// than from its name; an ADOPTED one does not, precisely because its
	// /opt/pzserver has a parent nothing creates -- and gvm creating it would
	// be gvm inventing the tree it was told to take over.
	for _, tc := range []struct {
		name    string
		managed bool
	}{
		{"valheim-1", true},
		{"pz-main", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			created := tmpfilesDirs(t)
			in, _ := loadInstance(t, tc.name)
			if in.Spec.ManagedInstall != tc.managed {
				t.Fatalf("%s is managed=%v; this table needs one of each",
					tc.name, in.Spec.ManagedInstall)
			}

			order := append([]hostops.DirKind{}, rootOwnedDirs...)
			order = append(order, instanceOwnedDirs...)
			if tc.managed {
				order = append(order, managedGameDirs...)
			}
			order = append(order, lateRootDirs...)
			order = append(order, hostops.DirJobs)

			for i, k := range order {
				p, ok := hostops.DirPathFor(k, in)
				if !ok {
					t.Fatalf("hostops does not declare %q", k)
				}
				parent := path.Dir(p)
				if !created[parent] {
					t.Errorf("step %d ensures %s (%s), whose parent %s is created neither by "+
						"tmpfiles.d nor by an earlier step. hostops refuses a missing parent, so "+
						"this apply fails on a fresh box.", i, p, k, parent)
				}
				created[p] = true
			}
		})
	}
}

// TestTmpfilesCreatesTheRootsSection5Names, with the modes that make the rest
// of the layout safe.
func TestTmpfilesCreatesTheRootsSection5Names(t *testing.T) {
	modes := tmpfilesModes(t)
	want := map[string]string{
		// 0750 root:gvm so the unprivileged web tier can READ a spec and can
		// never write one.
		"/etc/gvm": "0750",
		// 0700 root:root, untraversable to that tier, which is what makes the
		// line above safe (D-02).
		"/etc/gvm/secrets": "0700",
		// 0700 root:root: a compromised mod must not be able to read every
		// world the server has ever had (D-44).
		"/var/backups/gvm": "0700",
		// 0755, NOT 0750: apply creates /var/log/gvm/<name> at 0750 with one
		// mkdir, so this parent has to be right here.
		"/var/log/gvm": "0755",
		"/run/gvm":     "0755",
	}
	for dir, mode := range want {
		got, ok := modes[dir]
		if !ok {
			t.Errorf("tmpfiles.d does not create %s", dir)
			continue
		}
		if got != mode {
			t.Errorf("%s is %s in tmpfiles.d, want %s", dir, got, mode)
		}
	}
}

// tmpfilesDirs returns every directory the packaged tmpfiles.d creates.
func tmpfilesDirs(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for dir := range tmpfilesModes(t) {
		out[dir] = true
	}
	// The roots tmpfiles.d does not own because the distribution does.
	for _, d := range []string{"/etc", "/var", "/var/lib", "/var/log", "/srv", "/run", "/var/backups"} {
		out[d] = true
	}
	return out
}

func tmpfilesModes(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, line := range strings.Split(readPackaged(t, "tmpfiles/gvm.conf"), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[0] != "d" {
			continue
		}
		out[f[1]] = f[2]
	}
	if len(out) < 15 {
		t.Fatalf("parsed only %d tmpfiles entries; the parser or the file is broken", len(out))
	}
	return out
}

// hasDirective reports whether a unit file sets a directive, ignoring
// commented-out lines: a comment explaining why a directive is absent must not
// count as setting it.
func hasDirective(unit, prefix string) bool {
	for _, line := range strings.Split(unit, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			return true
		}
	}
	return false
}

func directiveValue(unit, name string) string {
	for _, line := range strings.Split(unit, "\n") {
		t := strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(t, name+"="); ok {
			// Trailing comments are systemd-legal on a directive line.
			if i := strings.Index(v, " #"); i >= 0 {
				v = v[:i]
			}
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func containsField(list, want string) bool {
	for _, f := range strings.Fields(list) {
		if f == want {
			return true
		}
	}
	return false
}

// TestEveryPathTheProbeTestsIsCreatedByAnEarlierStep.
//
// D-15's write-probe launches a transient unit with the sandbox's own
// ReadWritePaths=, and BuildSandbox puts spec.Paths.Install and
// spec.Paths.Data at the head of that set. Systemd fails namespace setup
// (226/NAMESPACE) when a ReadWritePaths= entry does not exist, so on a fresh
// MANAGED instance -- where `gvm create` writes the YAML and touches nothing
// else, and `gvm install` has not run because the spec and the plan both put
// apply first -- the probe runs against two absent directories and the apply
// aborts before the drop-in, the reload and the enable.
//
// The parent-chain test above walks the directory LIST; this walks the probe
// SET, which is the half that had nothing checking it. The fake probe in
// internal/cli answers OK for every path it is given regardless of existence,
// so nothing else can catch this.
func TestEveryPathTheProbeTestsIsCreatedByAnEarlierStep(t *testing.T) {
	for _, tc := range []struct{ name, game string }{
		{"valheim-1", "valheim"},
		{"gmod-1", "gmod"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, _ := loadInstance(t, tc.name)
			if !in.Spec.ManagedInstall {
				t.Skip("adopted instances keep paths that pre-date gvm; nothing creates them")
			}
			sb, err := BuildSandbox(in)
			if err != nil {
				t.Fatal(err)
			}

			p, _, _ := planFor(t, tc.name, tc.game, nil)
			creates := map[string]bool{}
			for _, a := range p.Steps {
				d, ok := a.(*DirAction)
				if !ok {
					continue
				}
				creates[d.Path] = true
			}
			for _, probed := range sb.Paths {
				if !creates[probed] {
					t.Errorf("the write-probe tests %s, which no step of the plan creates. "+
						"systemd refuses namespace setup for a ReadWritePaths= entry that does "+
						"not exist, so the FIRST apply of every new managed instance fails here "+
						"-- before the drop-in, the reload and the enable.", probed)
				}
			}
		})
	}
}
