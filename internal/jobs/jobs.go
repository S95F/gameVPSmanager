// Package jobs runs third-party code where it cannot hurt the host (D-16).
//
// Job ORCHESTRATION is root: it needs systemctl, the firewall and the
// credential store. Every step that executes code gvm did not write -- SteamCMD
// self-updating itself and then running Valve-supplied binaries, an archive
// extractor unpacking a modpack somebody uploaded -- is re-launched as a
// transient systemd SERVICE in gvm-jobs.slice, as the account that owns the
// destination.
//
// A service and never a scope. `systemd-run --scope` runs the command in the
// caller's own cgroup and unit context: it accepts no hardening properties that
// mean anything, inherits root's environment and capabilities, and dies with
// the caller. Using one would give SteamCMD strictly LESS confinement than the
// game it installs, which is the regression D-16 exists to name. noscope_test.go
// asserts the token is absent from this package.
//
// Everything about building the unit is pure and lives here: Args renders the
// argv, ShowProperties names what to verify afterwards, ParseShow reads
// systemctl's answer and CheckShow delivers the verdict. The Linux CI test
// starts a real unit and runs that verdict against `systemctl show`, so the
// claim being tested is "the properties landed", not "the command was spelled
// correctly". Off Linux there is no systemd to ask and RunSandboxed refuses.
package jobs

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// The refusals. Each is a sentinel because the next move differs: an invalid
// sandbox is a gvm bug, an unsafe value is operator or manifest input that must
// be refused rather than escaped, and a unit that came back wrong is a host
// whose systemd silently ignored a directive this build depends on.
var (
	// ErrSandbox is a sandbox description gvm will not launch.
	ErrSandbox = errors.New("jobs: refusing to build this sandbox")
	// ErrUnsafeValue is a value that would not survive being written into a
	// systemd directive as itself.
	ErrUnsafeValue = errors.New("jobs: unsafe value for a systemd property")
	// ErrProperties is a started unit whose properties do not match what was
	// asked for.
	ErrProperties = errors.New("jobs: the transient unit does not carry the properties it was given")
)

// maxUnitValue bounds one directive's value.
//
// Mirrors internal/reconcile's ceiling for the same reason: systemd has its own
// limits, they differ by version, and a value long enough to be interesting is a
// value nobody wrote on purpose.
const maxUnitValue = 4096

// DefaultTimeout is the RuntimeMaxSec a sandbox gets when none is named.
//
// Not "forever". A wedged download holding the nightly job open is the failure
// this whole number exists to prevent, and a zero Timeout arriving from a
// half-filled struct would produce exactly that, so Validate refuses it rather
// than defaulting quietly -- except through Args, which is also what renders
// the documentation example, and there a stated default is better than a
// missing directive.
const DefaultTimeout = 30 * time.Minute

// Sandbox is one transient unit: the confinement to build and the account to
// build it for (section 7.7).
//
// The uid is whichever account OWNS THE DESTINATION (D-16). SteamCMD writes
// into the instance install tree, so it runs as the instance user; a content
// fetch writes only into the shared blob cache, so it runs as gvm-fetch, which
// is in no group that can reach admin.sock and holds no database grant. A
// zip-slip in a modpack then lands in a uid that owns nothing but a cache.
type Sandbox struct {
	// Unit is the transient unit's name without the .service suffix:
	// gvm-steam-<instance> or gvm-fetch-<jobid>. It is a fixed name per
	// instance on purpose -- `systemctl status gvm-steam-pz-main` works while
	// a download is running -- which is also why Args passes --collect; see
	// there.
	Unit string

	// User and Group are the account the unit runs as, by NAME. Names rather
	// than numbers because that is what `systemctl show -p User` reports back,
	// so the verification in CheckShow can compare like with like; the numeric
	// ids still matter and are carried by the caller (provider.UnixIdent),
	// which refuses uid 0.
	User  string
	Group string

	// Slice defaults to gvm-jobs.slice, so a job competes for CPU and memory
	// with the other provisioning work rather than with a live world.
	Slice string

	// Home is the HOME the unit runs with: SteamCMD's per-instance steam home
	// (D-30) for an install, blobs/tmp for a fetch. SteamCMD derives
	// XDG_DATA_HOME from it, so this one variable moves both ~/.steam and
	// ~/.local/share/Steam and two instances can never share either.
	Home string
	// WorkDir becomes WorkingDirectory=.
	WorkDir string
	// Env is passed as --setenv=. HOME is set from the Home field, so a
	// duplicate HOME here is refused rather than silently losing to it.
	Env map[string]string

	// ProtectSystem and ProtectHome are emitted ALWAYS and explicitly, never
	// left to the systemd default (D-15's rule, for D-15's reason): the
	// adopted Project Zomboid server's world lives under /home, the default
	// hides it, and a unit that inherited a default nobody wrote is a unit
	// whose behaviour changes with the distro's systemd version.
	ProtectSystem string // strict | full | yes | no
	ProtectHome   string // yes | read-only | tmpfs | no

	// The on/off hardening. Pointers so that "not filled in" and "deliberately
	// off" are different values: PrivateTmp defaulting to off because a caller
	// forgot is the kind of silence this package exists to remove, and Validate
	// refuses a nil.
	PrivateTmp       *bool
	NoNewPrivileges  *bool
	RestrictSUIDSGID *bool

	// RestrictAddressFamilies, when set, is rendered as one space-separated
	// directive. Empty means the directive is omitted: a SteamCMD run needs
	// AF_NETLINK and AF_UNIX as well as inet, and a list that looked tight and
	// broke the download would be reverted in production within a day.
	RestrictAddressFamilies []string
	// SystemCallFilter is rendered as one directive PER ENTRY, in order,
	// because that is how systemd composes an allow list followed by a deny
	// list ("@system-service", then "~@privileged @resources"). Joining them
	// would produce one nonsense filter.
	SystemCallFilter []string

	MemoryMax     string // "2G"
	TasksMax      int    // 0 omits the directive
	LimitNOFILE   int    // 0 omits the directive
	MemorySwapMax string // "0" pins a download to RAM+disk rather than swap

	// ReadWritePaths is the whole of what this unit may write. With
	// ProtectSystem=strict everything else is read-only, which is what makes
	// an extractor's stray absolute path an error instead of a surprise.
	ReadWritePaths []string
	BindReadOnly   []string

	// Credentials maps a credential name to the host path holding it, rendered
	// as LoadCredential=. The secret is read by systemd as root and exposed
	// inside the unit at $CREDENTIALS_DIRECTORY/<name>, so it never passes
	// through gvm's argv, environment or logs.
	Credentials map[string]string

	// Timeout becomes RuntimeMaxSec: the kernel-enforced version of
	// update.sh's `timeout 1800`. gvm's own context deadline is a backstop one
	// rung above it, so the normal kill comes from systemd and a systemd-run
	// that itself hangs still cannot hold a job open.
	Timeout time.Duration
}

// UnitName is the full unit name, with the .service suffix.
//
// Spelled out rather than left implicit because it is what `systemctl show`
// must be asked about, and asking about "gvm-steam-pz-main" would let systemd
// pick the type -- which is the one thing about this unit that is not allowed
// to be ambiguous.
func (s Sandbox) UnitName() string { return s.Unit + ".service" }

// CredentialPath is where systemd exposes one credential inside this unit.
//
// Spelled out rather than read from $CREDENTIALS_DIRECTORY at runtime, because
// the consumer is an argv element this process composes before the unit exists:
// the environment variable is only set inside the unit. systemd derives the
// directory from the FULL unit name, so it goes through UnitName for the same
// reason `systemctl show` does -- a missing .service suffix would address a
// path that is not there, and the symptom would be a steamcmd that finds no
// commands and exits 0.
func (s Sandbox) CredentialPath(name string) string {
	return "/run/credentials/" + s.UnitName() + "/" + name
}

// SliceName is Slice or the default.
func (s Sandbox) SliceName() string {
	if strings.TrimSpace(s.Slice) == "" {
		return instance.JobSliceName
	}
	return s.Slice
}

// Budget is Timeout or DefaultTimeout.
func (s Sandbox) Budget() time.Duration {
	if s.Timeout <= 0 {
		return DefaultTimeout
	}
	return s.Timeout
}

// Validate refuses a sandbox that is not safe to launch.
//
// It is strict about absences rather than filling them in. Every field it
// insists on is one whose default answer is "less confinement than the author
// believed", and the two that matter most are the account and the budget: a
// Sandbox is a struct of strings whose zero value names root and no deadline.
func (s Sandbox) Validate() error {
	if err := validUnitStem(s.Unit); err != nil {
		return err
	}
	switch strings.TrimSpace(s.User) {
	case "":
		return fmt.Errorf("%w: no account to run as; third-party code never runs as root "+
			"(D-16), and an empty --uid= is root", ErrSandbox)
	case "root", "0":
		return fmt.Errorf("%w: refusing to run third-party code as root (D-16)", ErrSandbox)
	}
	if strings.TrimSpace(s.Group) == "" {
		return fmt.Errorf("%w: no group to run as; an unset --gid= is the root group", ErrSandbox)
	}
	if s.Group == "root" || s.Group == "0" {
		return fmt.Errorf("%w: refusing the root group (D-16)", ErrSandbox)
	}
	if !validProtectSystem(s.ProtectSystem) {
		return fmt.Errorf("%w: ProtectSystem=%q is not one of strict, full, yes, no; it is "+
			"emitted explicitly and never inherited (D-15)", ErrSandbox, s.ProtectSystem)
	}
	if !validProtectHome(s.ProtectHome) {
		return fmt.Errorf("%w: ProtectHome=%q is not one of yes, read-only, tmpfs, no; the "+
			"adopted instance's world is under /home and the default hides it (D-15)",
			ErrSandbox, s.ProtectHome)
	}
	for _, f := range []struct {
		name string
		v    *bool
	}{
		{"PrivateTmp", s.PrivateTmp},
		{"NoNewPrivileges", s.NoNewPrivileges},
		{"RestrictSUIDSGID", s.RestrictSUIDSGID},
	} {
		if f.v == nil {
			return fmt.Errorf("%w: %s is unset; this build writes every hardening directive "+
				"explicitly, so that what confines a job is reviewable in one place rather "+
				"than being whatever the host's systemd defaults to", ErrSandbox, f.name)
		}
	}
	if s.Timeout <= 0 {
		return fmt.Errorf("%w: no RuntimeMaxSec; a wedged download must not be able to hold "+
			"the nightly job open forever", ErrSandbox)
	}
	if len(s.ReadWritePaths) == 0 {
		return fmt.Errorf("%w: no ReadWritePaths; with ProtectSystem=%s the unit could not "+
			"write anything, so either the paths are missing or this is the wrong tool",
			ErrSandbox, s.ProtectSystem)
	}
	if strings.TrimSpace(s.Home) == "" {
		return fmt.Errorf("%w: no HOME; steamcmd writes ~/.steam and ~/.local/share/Steam, "+
			"and two instances sharing either is the single best way to corrupt an install "+
			"(D-30)", ErrSandbox)
	}
	if _, dup := s.Env["HOME"]; dup {
		return fmt.Errorf("%w: HOME is in both Home and Env; one of the two would win and "+
			"nothing would say which", ErrSandbox)
	}
	for name, path := range s.Credentials {
		if err := validCredentialName(name); err != nil {
			return err
		}
		if !strings.HasPrefix(path, "/") {
			return fmt.Errorf("%w: credential %q points at %q, which is not an absolute path",
				ErrSandbox, name, path)
		}
	}
	return nil
}

// validUnitStem refuses a unit name systemd would mangle or that would address
// a different unit than the one the caller named.
func validUnitStem(u string) error {
	if strings.TrimSpace(u) == "" {
		return fmt.Errorf("%w: no unit name", ErrSandbox)
	}
	if err := checkUnitValue("unit", u); err != nil {
		return err
	}
	if strings.ContainsAny(u, " \t/\"'\\$;") || strings.Contains(u, "..") {
		return fmt.Errorf("%w: unit name %q contains a character systemd escapes or a path "+
			"element; `systemctl status %s` has to address the unit gvm started", ErrSandbox, u, u)
	}
	// A dotted suffix would make `systemctl show <unit>.service` address
	// something else entirely -- "gvm-steam-pz.timer.service" is not a typo
	// systemd corrects.
	if strings.Contains(u, ".") {
		return fmt.Errorf("%w: unit name %q contains a dot; the .service suffix is added "+
			"here so the unit type is never ambiguous", ErrSandbox, u)
	}
	return nil
}

func validCredentialName(n string) error {
	if strings.TrimSpace(n) == "" {
		return fmt.Errorf("%w: a credential with no name", ErrSandbox)
	}
	// systemd's own rule: credential ids are filenames under
	// $CREDENTIALS_DIRECTORY, and a colon is the separator in the directive.
	if strings.ContainsAny(n, ":/ \t\"'\\") {
		return fmt.Errorf("%w: credential name %q contains a separator or a path element",
			ErrSandbox, n)
	}
	return nil
}

func validProtectSystem(v string) bool {
	switch v {
	case "strict", "full", "yes", "no":
		return true
	}
	return false
}

func validProtectHome(v string) bool {
	switch v {
	case "yes", "read-only", "tmpfs", "no":
		return true
	}
	return false
}

// checkUnitValue refuses a value that would not survive being written into a
// systemd directive.
//
// A directive is literal to end of line, so a control character in a value is
// not an escaping problem to be solved -- it is a second directive. Refusing is
// the only version of this check that cannot be argued with later.
func checkUnitValue(field, v string) error {
	if len(v) > maxUnitValue {
		return fmt.Errorf("%w: %s is %d bytes, over the %d-byte ceiling",
			ErrUnsafeValue, field, len(v), maxUnitValue)
	}
	for i, r := range v {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: %s contains %q at byte %d; a systemd directive is literal "+
				"to end of line, so this would be a second directive", ErrUnsafeValue, field, r, i)
		}
	}
	return nil
}

// joinPaths renders a space-separated directive list, refusing a path that
// contains the separator.
//
// systemd's answer to a path with a space in it is quoting. gvm's is to refuse:
// a game directory with a space in it is a thing an operator can rename, and a
// quoting rule is a thing that has to stay right forever across three systemd
// versions.
func joinPaths(field string, paths []string) (string, error) {
	for i, p := range paths {
		if err := checkUnitValue(fmt.Sprintf("%s[%d]", field, i), p); err != nil {
			return "", err
		}
		if !strings.HasPrefix(p, "/") {
			return "", fmt.Errorf("%w: %s[%d] = %q is not absolute; systemd resolves a "+
				"relative path in this directive against the unit's root directory, which is "+
				"not where the caller meant", ErrUnsafeValue, field, i, p)
		}
		if strings.ContainsAny(p, " \t\"'\\") {
			return "", fmt.Errorf("%w: %s[%d] = %q contains a space or a quote; systemd "+
				"separates this directive's paths on whitespace", ErrUnsafeValue, field, i, p)
		}
	}
	return strings.Join(paths, " "), nil
}

// Args renders the complete systemd-run argv for cmd.
//
// Pure, and returning a []string, and both halves matter. Pure, so the exact
// command a nightly job will run is asserted on the development machine, where
// there is no systemd. A []string, so there is no shell anywhere in the path: a
// path containing a semicolon is one argv element rather than a second command,
// which is the same reason section 7.7 emits SteamCMD as argv and the reason
// nothing here ever builds a string to hand to sh.
//
// --collect, not optional. The unit name is fixed per instance, and systemd
// keeps a FAILED transient unit loaded until something runs `systemctl
// reset-failed`. Without --collect the first failed nightly would make every
// subsequent one fail with "unit already exists" -- a self-inflicted outage
// whose cause is three layers from its symptom.
func (s Sandbox) Args(cmd []string) ([]string, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if len(cmd) == 0 {
		return nil, fmt.Errorf("%w: no command to run", ErrSandbox)
	}
	if !strings.HasPrefix(cmd[0], "/") {
		// An inherited PATH deciding which binary a root-launched unit
		// executes is a decision nobody made. The caller resolved the binary;
		// this insists it did.
		return nil, fmt.Errorf("%w: %q is not an absolute path; the binary a transient unit "+
			"runs is never chosen by an inherited PATH", ErrSandbox, cmd[0])
	}
	for i, a := range cmd {
		if err := checkUnitValue(fmt.Sprintf("argv[%d]", i), a); err != nil {
			return nil, err
		}
	}

	rwp, err := joinPaths("ReadWritePaths", s.ReadWritePaths)
	if err != nil {
		return nil, err
	}
	var bro string
	if len(s.BindReadOnly) > 0 {
		if bro, err = joinPaths("BindReadOnlyPaths", s.BindReadOnly); err != nil {
			return nil, err
		}
	}
	for _, f := range []struct{ name, v string }{
		{"User", s.User}, {"Group", s.Group}, {"Slice", s.SliceName()},
		{"HOME", s.Home}, {"WorkingDirectory", s.WorkDir},
		{"MemoryMax", s.MemoryMax}, {"MemorySwapMax", s.MemorySwapMax},
	} {
		if err := checkUnitValue(f.name, f.v); err != nil {
			return nil, err
		}
	}

	args := []string{
		// --wait so the job layer has the answer before it decides anything;
		// --pipe so the child's output is the job log rather than the journal
		// only; --quiet so systemd-run's own chatter does not reach a stream
		// the SteamCMD classifier is reading.
		"--collect", "--wait", "--pipe", "--quiet",
		"--unit=" + s.Unit,
		"--slice=" + s.SliceName(),
		"--uid=" + s.User,
		"--gid=" + s.Group,
		// Type=exec, so systemd-run's start job does not complete until the
		// binary has actually been execed. With the default Type=simple a
		// missing binary is reported as a successful start followed by a
		// failure, and `--wait` would hand back an exit status for a process
		// that never ran.
		"--property=Type=exec",
		"--setenv=HOME=" + s.Home,
	}
	for _, k := range sortedKeys(s.Env) {
		if err := checkUnitValue("env "+k, s.Env[k]); err != nil {
			return nil, err
		}
		if err := validEnvName(k); err != nil {
			return nil, err
		}
		args = append(args, "--setenv="+k+"="+s.Env[k])
	}
	if s.WorkDir != "" {
		args = append(args, "--property=WorkingDirectory="+s.WorkDir)
	}
	args = append(args,
		"--property=ProtectSystem="+s.ProtectSystem,
		"--property=ProtectHome="+s.ProtectHome,
		"--property=PrivateTmp="+yesNo(s.PrivateTmp),
		"--property=NoNewPrivileges="+yesNo(s.NoNewPrivileges),
		"--property=RestrictSUIDSGID="+yesNo(s.RestrictSUIDSGID),
	)
	if len(s.RestrictAddressFamilies) > 0 {
		v := strings.Join(s.RestrictAddressFamilies, " ")
		if err := checkUnitValue("RestrictAddressFamilies", v); err != nil {
			return nil, err
		}
		args = append(args, "--property=RestrictAddressFamilies="+v)
	}
	for i, f := range s.SystemCallFilter {
		if err := checkUnitValue(fmt.Sprintf("SystemCallFilter[%d]", i), f); err != nil {
			return nil, err
		}
		args = append(args, "--property=SystemCallFilter="+f)
	}
	if s.MemoryMax != "" {
		args = append(args, "--property=MemoryMax="+s.MemoryMax)
	}
	if s.MemorySwapMax != "" {
		args = append(args, "--property=MemorySwapMax="+s.MemorySwapMax)
	}
	if s.TasksMax > 0 {
		args = append(args, "--property=TasksMax="+strconv.Itoa(s.TasksMax))
	}
	if s.LimitNOFILE > 0 {
		args = append(args, "--property=LimitNOFILE="+strconv.Itoa(s.LimitNOFILE))
	}
	args = append(args,
		fmt.Sprintf("--property=RuntimeMaxSec=%d", int(s.Budget().Round(time.Second)/time.Second)),
		"--property=ReadWritePaths="+rwp,
	)
	if bro != "" {
		args = append(args, "--property=BindReadOnlyPaths="+bro)
	}
	for _, name := range sortedKeys(s.Credentials) {
		args = append(args, "--property=LoadCredential="+name+":"+s.Credentials[name])
	}
	return append(args, cmd...), nil
}

// validEnvName refuses a variable name that is not a name.
//
// --setenv=FOO BAR=x would be one argv element systemd reads as a variable
// called "FOO BAR", which nothing can then read, and --setenv==x is a variable
// with no name at all.
func validEnvName(k string) error {
	if k == "" {
		return fmt.Errorf("%w: an environment variable with no name", ErrUnsafeValue)
	}
	for i, r := range k {
		ok := r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') ||
			(i > 0 && r >= '0' && r <= '9')
		if !ok {
			return fmt.Errorf("%w: environment variable name %q is not a name", ErrUnsafeValue, k)
		}
	}
	return nil
}

func yesNo(b *bool) string {
	if b != nil && *b {
		return "yes"
	}
	return "no"
}

// Yes and No are the addresses Sandbox's tri-state hardening fields want.
//
// They exist so a caller writes PrivateTmp: jobs.Yes rather than taking the
// address of a local, which is the shape that ends up shared between two
// sandboxes by accident.
func Yes() *bool { t := true; return &t }
func No() *bool  { f := false; return &f }

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ShowProperties names the properties CheckShow needs read back.
//
// Asked for by name rather than reading the whole of `systemctl show`, which is
// two hundred lines of which five matter, and naming them means a systemd that
// does not know a property answers with an empty value rather than omitting the
// line -- which CheckShow reports as "this host ignored the directive" instead
// of "not checked".
func (s Sandbox) ShowProperties() []string {
	props := []string{
		"Id", "Slice", "User", "Group", "RuntimeMaxUSec",
		"ProtectSystem", "ProtectHome", "PrivateTmp", "NoNewPrivileges",
		"RestrictSUIDSGID", "ReadWritePaths", "WorkingDirectory", "Environment",
	}
	if s.MemoryMax != "" {
		props = append(props, "MemoryMax")
	}
	if s.MemorySwapMax != "" {
		props = append(props, "MemorySwapMax")
	}
	if s.TasksMax > 0 {
		props = append(props, "TasksMax")
	}
	if s.LimitNOFILE > 0 {
		props = append(props, "LimitNOFILE")
	}
	if len(s.BindReadOnly) > 0 {
		props = append(props, "BindReadOnlyPaths")
	}
	return props
}

// ParseShow reads `systemctl show` output.
//
// A pure parser over an io.Reader, so every shape of answer -- a clean one, a
// property reported empty, a value containing an '=', a truncated stream, a
// blank line -- is a table-driven test on the development machine rather than
// something only a Debian box can exercise.
//
// Later lines win. systemctl prints one line per property, but a unit with a
// list property can repeat it, and for the properties CheckShow asks about the
// last value is the effective one.
func ParseShow(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(io.LimitReader(r, maxShowOutput))
	sc.Buffer(make([]byte, 0, 8<<10), 256<<10)
	for line := 1; sc.Scan(); line++ {
		t := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(t) == "" {
			continue
		}
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			return out, fmt.Errorf("%w: `systemctl show` line %d is not Key=Value: %q",
				ErrProperties, line, truncate(t, 120))
		}
		if strings.TrimSpace(k) == "" {
			return out, fmt.Errorf("%w: `systemctl show` line %d has an empty property name",
				ErrProperties, line)
		}
		out[k] = v
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("%w: reading `systemctl show`: %w", ErrProperties, err)
	}
	return out, nil
}

// maxShowOutput bounds what is read back. `systemctl show` of one unit is a few
// kilobytes; a megabyte means something else is on that pipe.
const maxShowOutput = 1 << 20

// Mismatch is one property that did not come back as it was set.
type Mismatch struct {
	Property string
	Want     string
	Got      string
	// Why says what the directive was protecting, so a failure reads as a
	// consequence rather than as a diff.
	Why string
}

func (m Mismatch) String() string {
	got := m.Got
	if got == "" {
		got = "<absent>"
	}
	return fmt.Sprintf("%s: asked for %q, unit reports %s (%s)", m.Property, m.Want, got, m.Why)
}

// CheckShow is the verdict on a started unit.
//
// This is the assertion D-16 actually needs. Everything else in this package
// proves gvm BUILDS the right command; a host whose systemd is too old for a
// directive, or a property name that moved between versions, accepts the
// command and confines nothing -- and the symptom is a SteamCMD running with
// more reach than the game it installs, which looks exactly like success.
//
// It is pure, taking the map ParseShow produced, so the comparison rules --
// RuntimeMaxSec read back as microseconds, ProtectSystem's booleans, the
// service-not-scope check -- are tested here and the Linux test only has to
// gather the input.
func (s Sandbox) CheckShow(got map[string]string) []Mismatch {
	var out []Mismatch
	add := func(prop, want, why string) {
		if g := got[prop]; g != want {
			out = append(out, Mismatch{Property: prop, Want: want, Got: g, Why: why})
		}
	}

	// A scope has no Id ending in .service, no User= and no sandbox
	// properties. Checking the Id is how "never a scope" stops being a comment:
	// a scope would answer "gvm-steam-pz-main.scope" here.
	add("Id", s.UnitName(), "third-party code runs in a transient SERVICE; a scope inherits "+
		"the caller's context and carries no confinement at all (D-16)")
	add("Slice", s.SliceName(), "a job competes with the other provisioning work, not with a "+
		"live world")
	add("User", s.User, "the uid is whichever account owns the destination, and never root (D-16)")
	add("Group", s.Group, "an unset gid is the root group")
	add("ProtectSystem", s.ProtectSystem, "the rest of the filesystem is read-only to this unit")
	add("ProtectHome", s.ProtectHome, "an adopted instance's world lives under /home (D-15)")
	add("PrivateTmp", yesNo(s.PrivateTmp), "a shared /tmp is a shared writable directory "+
		"between a download and everything else on the box")
	add("NoNewPrivileges", yesNo(s.NoNewPrivileges), "SteamCMD self-updates and then runs what "+
		"it downloaded; a setuid binary inside that tree must not be able to gain anything")
	add("RestrictSUIDSGID", yesNo(s.RestrictSUIDSGID), "nothing a job unpacks has any business "+
		"creating a setuid file")

	// RuntimeMaxSec is reported as RuntimeMaxUSec, in microseconds, and as
	// "infinity" when it was never set. "infinity" is the answer this check
	// exists to catch: it is a wedged download holding the nightly open.
	want := strconv.FormatInt(int64(s.Budget().Round(time.Second)/time.Microsecond), 10)
	if g := got["RuntimeMaxUSec"]; g != want {
		out = append(out, Mismatch{
			Property: "RuntimeMaxUSec", Want: want, Got: g,
			Why: "RuntimeMaxSec is the kernel-enforced version of update.sh's `timeout 1800`; " +
				"\"infinity\" means a wedged download can hold the nightly job open forever",
		})
	}

	if rwp, err := joinPaths("ReadWritePaths", s.ReadWritePaths); err == nil {
		// systemd renders the list in its own syntax, which has carried
		// parentheses and per-path flags at various versions, so the assertion
		// is containment of every path rather than string equality of the
		// whole directive.
		for _, p := range s.ReadWritePaths {
			if !strings.Contains(got["ReadWritePaths"], p) {
				out = append(out, Mismatch{
					Property: "ReadWritePaths", Want: rwp, Got: got["ReadWritePaths"],
					Why: p + " is a path this job has to write, and the unit does not list it",
				})
			}
		}
	}
	if s.WorkDir != "" {
		// Reported with a leading '-' when the directive was marked optional,
		// which gvm never does, but trimming it keeps the check about the path.
		if g := strings.TrimPrefix(got["WorkingDirectory"], "-"); g != s.WorkDir {
			out = append(out, Mismatch{
				Property: "WorkingDirectory", Want: s.WorkDir, Got: got["WorkingDirectory"],
				Why: "steamcmd resolves its own installation relative to where it starts",
			})
		}
	}
	if s.Home != "" && !strings.Contains(got["Environment"], "HOME="+s.Home) {
		out = append(out, Mismatch{
			Property: "Environment", Want: "HOME=" + s.Home, Got: got["Environment"],
			Why: "steamcmd derives XDG_DATA_HOME from HOME, so this one variable is what keeps " +
				"two instances from sharing ~/.steam (D-30)",
		})
	}
	// Both are reported in BYTES, while the directives are written as "2G" and
	// "0". Parsed rather than compared as strings, because the two spellings of
	// the same number are both correct and a string comparison would report
	// every correctly configured unit as wrong -- which is the kind of false
	// alarm that gets a check deleted.
	for _, m := range []struct{ prop, set, why string }{
		{"MemoryMax", s.MemoryMax, "SteamCMD's depot decryption grows with chunk size rather " +
			"than with the download, so this is the ceiling that stops a leak taking the host"},
		{"MemorySwapMax", s.MemorySwapMax, "a job that swaps instead of failing takes every " +
			"live world on the box down with it"},
	} {
		if m.set == "" {
			continue
		}
		want, err := spec.ParseBytes(m.set)
		if err != nil {
			continue
		}
		if g := got[m.prop]; g != strconv.FormatInt(want, 10) {
			out = append(out, Mismatch{Property: m.prop, Want: m.set, Got: g, Why: m.why})
		}
	}
	if s.TasksMax > 0 {
		add("TasksMax", strconv.Itoa(s.TasksMax), "a fork bomb in an extractor is a host outage")
	}
	if s.LimitNOFILE > 0 {
		add("LimitNOFILE", strconv.Itoa(s.LimitNOFILE), "steamcmd opens a descriptor per depot chunk")
	}
	for _, p := range s.BindReadOnly {
		if !strings.Contains(got["BindReadOnlyPaths"], p) {
			out = append(out, Mismatch{
				Property: "BindReadOnlyPaths", Want: p, Got: got["BindReadOnlyPaths"],
				Why: p + " is an input this job reads and must not be able to modify",
			})
		}
	}
	return out
}

// PropertiesError renders every mismatch as one message.
//
// All of them, because an operator looking at a host whose systemd ignored one
// directive wants to know whether it ignored five.
func (s Sandbox) PropertiesError(ms []Mismatch) error {
	if len(ms) == 0 {
		return nil
	}
	lines := make([]string, 0, len(ms))
	for _, m := range ms {
		lines = append(lines, m.String())
	}
	return fmt.Errorf("%w: %s:\n  %s", ErrProperties, s.UnitName(), strings.Join(lines, "\n  "))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ExitError is a sandboxed command that ran and did not succeed.
//
// A typed error rather than a second return value, so RunSandboxed keeps
// section 7.7's signature while a caller that needs the status can still ask
// for it -- and, more importantly, so the two failures a caller must tell apart
// are distinguishable with errors.As: the child ran and exited (an ExitError),
// or the SANDBOX failed and the child may never have started (anything else).
// For SteamCMD that distinction is the difference between "classify the output"
// and "there is no output to classify".
type ExitError struct {
	Unit     string
	Code     int
	Signaled bool
	// Timeout means RuntimeMaxSec fired: the kernel killed a job that had run
	// out of budget, which is the generalised `timeout 1800`.
	Timeout bool
	// Detail is systemd-run's own complaint, trimmed. It is carried as a string
	// because it crossed a process boundary.
	Detail string
}

func (e *ExitError) Error() string {
	var what string
	switch {
	case e.Timeout:
		what = "exceeded its RuntimeMaxSec budget and was killed"
	case e.Signaled:
		what = "was killed by a signal"
	default:
		what = "exited with status " + strconv.Itoa(e.Code)
	}
	msg := fmt.Sprintf("jobs: %s %s", e.Unit, what)
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// trimDiagnostic keeps a subprocess's complaint short enough to read in a job
// log line.
func trimDiagnostic(s string) string { return truncate(strings.Join(strings.Fields(s), " "), 400) }
