package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/S95F/gameVPSmanager/internal/doctor"
	"github.com/S95F/gameVPSmanager/internal/firewall"
	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/spec"
	"github.com/S95F/gameVPSmanager/internal/sshport"
	"github.com/S95F/gameVPSmanager/internal/store"
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
	asJSON := fs.Bool("json", false, "print the findings as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Load host config. A bad file is not fatal to doctor — it reports the
	// problem below and checks the rest of the host against the merged
	// (defaults-filled) config, because an operator whose gvm.yaml has a typo
	// still needs to know whether their servers are healthy.
	hostCfg, cfgDiags := doctorLoadHostConfig()

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

	// A host config that does not validate is a HOST-CONFIG Problem (one per
	// bad key), with the did-you-mean the loader produced carried through.
	for _, d := range cfgDiags {
		if d.Sev != spec.SevError {
			continue
		}
		msg := d.Msg
		if d.Path != "" {
			msg = d.Path + ": " + d.Msg
		}
		findings = append(findings, lifecycle.CheckFinding{
			ID: "HOST-CONFIG", Severity: lifecycle.CheckProblem, Message: msg, Fix: d.Fix,
		})
	}

	// A missing Postgres is a Note, not a problem and not a scary header line:
	// the dashboard and the searchable audit history need it, game servers do
	// not (rule 1). It becomes more than a Note only once a gvm-web unit is
	// installed, which this build does not ship.
	if !ce.PostgresReachable(ctx) {
		findings = append(findings, lifecycle.CheckFinding{
			ID:       "POSTGRES",
			Severity: lifecycle.CheckNote,
			Message:  "Postgres not reachable: dashboard and audit-database features unavailable; game servers unaffected",
		})
	}

	problems, warnings, notes := countSeverities(findings)

	if *asJSON {
		if err := writeJSON(env.Stdout, doctorJSON(findings, problems, warnings, notes)); err != nil {
			return err
		}
	} else {
		for _, f := range findings {
			if *quiet && (f.Severity == lifecycle.CheckOK || f.Severity == lifecycle.CheckSkip) {
				continue
			}
			fmt.Fprintln(env.Stdout, lifecycle.FormatFinding(f))
		}
		// The totals line, always, so a clean run says "0 problems" and a dirty
		// one says exactly how dirty, and both point at where to read next.
		fmt.Fprintf(env.Stdout, "\n  %s, %s, %s · see: gvm help troubleshooting\n",
			plural(problems, "problem"), plural(warnings, "warning"), plural(notes, "note"))
	}

	if problems > 0 {
		// Exit 4, not 1: doctor ran fine and FOUND something. A script can
		// branch on "problems exist" without parsing text, and a failed doctor
		// run (which 1 would conflate with) is a different thing (U8).
		return checked(fmt.Errorf("doctor: %d problem(s)", problems))
	}
	return nil
}

// countSeverities tallies the finding severities for the totals line and the
// exit code.
func countSeverities(findings []lifecycle.CheckFinding) (problems, warnings, notes int) {
	for _, f := range findings {
		switch f.Severity {
		case lifecycle.CheckProblem:
			problems++
		case lifecycle.CheckWarning:
			warnings++
		case lifecycle.CheckNote:
			notes++
		}
	}
	return
}

// doctorJSON is the --json shape: the findings and the totals, read by the
// dashboard and by a CI check. The keys are stable (snake_case) so a consumer
// can rely on them.
func doctorJSON(findings []lifecycle.CheckFinding, problems, warnings, notes int) map[string]any {
	rows := make([]map[string]any, 0, len(findings))
	for _, f := range findings {
		row := map[string]any{"id": f.ID, "severity": string(f.Severity), "message": f.Message}
		if f.Fix != "" {
			row["fix"] = f.Fix
		}
		rows = append(rows, row)
	}
	return map[string]any{
		"findings": rows,
		"totals":   map[string]int{"problems": problems, "warnings": warnings, "notes": notes},
	}
}

// doctorCheckEnv is the production CheckEnv.
type doctorCheckEnv struct {
	checked bool
	reached bool
}

func (e *doctorCheckEnv) PostgresReachable(ctx context.Context) bool {
	if e.checked {
		return e.reached
	}
	e.checked = true

	// Only probe if the socket file exists; Postgres may be legitimately
	// absent and doctor must still run.
	const sockPath = "/var/run/postgresql/.s.PGSQL.5432"
	if _, err := os.Stat(sockPath); err != nil {
		e.reached = false
		return false
	}

	s, err := store.Open(ctx, "")
	if err != nil {
		e.reached = false
		return false
	}
	defer s.Close()

	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	select {
	case <-s.Ready():
		e.reached = true
	case <-waitCtx.Done():
		e.reached = false
	}
	return e.reached
}

// doctorLoadHostConfig reads /etc/gvm/gvm.yaml through the one loader
// (spec.LoadHost), so doctor's view of the config matches create's and ports'.
// An absent file is the normal unconfigured state: the defaults, no diagnostics.
func doctorLoadHostConfig() (*spec.HostConfig, spec.Diags) {
	const path = "/etc/gvm/gvm.yaml"
	data, err := os.ReadFile(path)
	if err != nil {
		d := spec.DefaultHostConfig()
		return &d, nil
	}
	return spec.LoadHost(path, data)
}

func doctorLoadInstances() []spec.Instance              { return nil }
func doctorLoadManifest(_ spec.Instance) *spec.Manifest { return nil }
