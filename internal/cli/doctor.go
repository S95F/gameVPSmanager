package cli

import (
	"context"
	"flag"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/S95F/gameVPSmanager/internal/doctor"
	"github.com/S95F/gameVPSmanager/internal/firewall"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/spec"
	"github.com/S95F/gameVPSmanager/internal/sshport"
	"github.com/S95F/gameVPSmanager/packaging"
)

// runDoctor implements `gvm doctor`: reads the host config and all instance
// specs, executes every registered check, and writes the results to stdout.
//
// Exit codes:
//
//	0  no problems
//	1  one or more problems
//	2  flag parse or config read error
func runDoctor(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	quiet := fs.Bool("q", false, "only show problems and warnings (suppress OK and Skip)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Load host config.
	hostCfg, err := doctorLoadHostConfig()
	if err != nil {
		fmt.Fprintf(env.Stderr, "doctor: could not load host config (%v); using defaults\n", err)
		d := spec.DefaultHostConfig()
		hostCfg = &d
	}

	// Build the check registry.
	reg := &lifecycle.CheckRegistry{}
	for _, c := range doctor.HostChecks(packaging.FS) {
		reg.Register(c)
	}
	for _, c := range doctor.EdgeChecks(hostCfg) {
		reg.Register(c)
	}

	// Detect SSH ports for FW checks — stub detection on non-Linux.
	det := sshport.Detect(sshport.OS(), hostCfg.Spec.Firewall.SSH.Ports)
	// Firewall state: stub until ufw backend CLI wiring is in place.
	var liveRules firewall.RuleSet
	liveActive := false
	liveTrusted := false
	for _, c := range doctor.FirewallChecks(hostCfg, nil, nil, liveRules, liveActive, liveTrusted, det) {
		reg.Register(c)
	}

	// Instance checks.
	for _, inst := range doctorLoadInstances() {
		m := doctorLoadManifest(inst)
		for _, c := range doctor.InstanceChecks(inst, m) {
			reg.Register(c)
		}
	}

	ce := &doctorCheckEnv{}
	findings := reg.RunAll(ctx, ce)

	// Header: if Postgres is unreachable, say so first.
	if !ce.PostgresReachable(ctx) {
		fmt.Fprintf(env.Stdout, "  - POSTGRES   not reachable; database checks skipped\n")
	}

	for _, f := range findings {
		if *quiet && (f.Severity == lifecycle.CheckOK || f.Severity == lifecycle.CheckSkip) {
			continue
		}
		fmt.Fprintln(env.Stdout, lifecycle.FormatFinding(f))
	}

	problems := lifecycle.CountCheckProblems(findings)
	if problems > 0 {
		fmt.Fprintf(env.Stdout, "\n  %d problem(s) found\n", problems)
		return fmt.Errorf("doctor: %d problem(s)", problems)
	}
	fmt.Fprintf(env.Stdout, "\n  0 problems\n")
	return nil
}

// doctorCheckEnv is the production CheckEnv.
type doctorCheckEnv struct {
	checked bool
	reached bool
}

func (e *doctorCheckEnv) PostgresReachable(_ context.Context) bool {
	if e.checked {
		return e.reached
	}
	e.checked = true
	// Without the database package (Phase 14) we cannot connect — always false.
	e.reached = false
	return false
}

func doctorLoadHostConfig() (*spec.HostConfig, error) {
	const path = "/etc/gvm/gvm.yaml"
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var h spec.HostConfig
	dec := yaml.NewDecoder(f)
	dec.KnownFields(false)
	if err := dec.Decode(&h); err != nil {
		return nil, err
	}
	return &h, nil
}

func doctorLoadInstances() []spec.Instance              { return nil }
func doctorLoadManifest(_ spec.Instance) *spec.Manifest { return nil }
