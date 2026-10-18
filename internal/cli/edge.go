// `gvm edge` — plan and apply the reverse-proxy config, and `gvm ca` — export
// the internal CA (Phase 48 item 7).
//
// `edge plan` is read-only. `edge apply` is the one path that converges the
// live Caddy on the spec (render → validate as caddy → stage → swap → reload
// → verify, rollback on failure, in internal/edgeops), and it never installs
// Caddy and never writes a firewall rule (rule 7, D-41).

package cli

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/caddyconf"
	"github.com/S95F/gameVPSmanager/internal/edge"
	"github.com/S95F/gameVPSmanager/internal/edgeops"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

func edgeGroup() subGroup {
	return subGroup{Verb: "edge", Subs: []subcmd{
		{Name: "plan", Summary: "show the proxy plan: TLS mode, ports, and the Caddyfile diff (changes nothing)", Run: runEdgePlan},
		{Name: "apply", Summary: "render, validate, swap and reload the Caddy config, with rollback", Run: runEdgeApply,
			Examples: []string{"sudo gvm edge apply"}},
	}}
}

func runEdge(ctx context.Context, env *Env, args []string) error {
	return edgeGroup().dispatch(ctx, env, args)
}

// loadEdgePlan reads the host config and computes the plan both subcommands
// share.
func loadEdgePlan(configPath string) (*spec.HostConfig, *edge.Plan, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, nil, fmt.Errorf("edge: read %s: %w", configPath, err)
	}
	host, diags := spec.LoadHost(configPath, data)
	if diags.HasErrors() {
		return nil, nil, fmt.Errorf("edge: %s does not validate:\n%s", configPath, diags.String())
	}
	plan, err := edge.ComputePlan(host)
	if err != nil {
		return nil, nil, err
	}
	return host, plan, nil
}

// edgePorts is §17.4's table: what the firewall must admit for this plan.
func edgePorts(plan *edge.Plan) []string {
	if plan.Mode != "caddy_managed" {
		return nil
	}
	var ports []string
	if plan.TLS == edge.TLSAcmeHTTP || plan.HTTPRedirect {
		ports = append(ports, "80/tcp")
	}
	ports = append(ports, "443/tcp")
	if plan.HTTP3 {
		ports = append(ports, "443/udp")
	}
	return ports
}

func runEdgePlan(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("edge plan", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	configPath := fs.String("config", "/etc/gvm/gvm.yaml", "host config")
	full := fs.Bool("full", false, "print the rendered Caddyfile, not just the summary")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}
	t, err := Resolve(env, "edge", "")
	if err != nil {
		return err
	}
	if err := t.RequireLocal(); err != nil {
		return err
	}

	host, plan, err := loadEdgePlan(*configPath)
	if err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "edge plan\n")
	fmt.Fprintf(env.Stdout, "  mode      %s\n", plan.Mode)
	if plan.Mode == "none" {
		fmt.Fprintln(env.Stdout, "  no web edge: gvm-web is off or reached over an SSH tunnel")
		return nil
	}
	if plan.Mode == "proxy_external" {
		fmt.Fprintf(env.Stdout, "  upstream  %s %s (your proxy's job; gvm writes no proxy config in this mode)\n",
			plan.Upstream.Network, plan.Upstream.Address)
		return nil
	}

	fmt.Fprintf(env.Stdout, "  site      %s\n", plan.Site)
	fmt.Fprintf(env.Stdout, "  tls       %s", plan.TLS)
	if plan.DNSProvider != "" {
		fmt.Fprintf(env.Stdout, " (dns %s)", plan.DNSProvider)
	}
	fmt.Fprintln(env.Stdout)
	fmt.Fprintf(env.Stdout, "  upstream  %s %s\n", plan.Upstream.Network, plan.Upstream.Address)

	if ports := edgePorts(plan); len(ports) != 0 {
		fmt.Fprintf(env.Stdout, "  ports     %s must be admitted by the firewall\n", strings.Join(ports, ", "))
		fmt.Fprintln(env.Stdout, "            (not checked: gvm firewall is not available in this build; verify with `sudo ufw status`)")
	}

	// The diff against what is live, byte-honest.
	var desired bytes.Buffer
	if err := caddyconf.Render(&desired, host, plan); err != nil {
		return err
	}
	livePath := host.Spec.Edge.Caddy.ConfigPath
	if plan.Mode == "caddy_snippet" {
		livePath = edgeops.SnippetPath
	}
	current, readErr := os.ReadFile(livePath)
	switch {
	case readErr != nil:
		fmt.Fprintf(env.Stdout, "  config    %s does not exist yet\n", livePath)
		fmt.Fprintln(env.Stdout, "\nnext: sudo gvm edge apply")
	case bytes.Equal(current, desired.Bytes()):
		fmt.Fprintf(env.Stdout, "  config    %s is up to date\n", livePath)
	default:
		fmt.Fprintf(env.Stdout, "  config    %s DIFFERS from the plan (%d lines live, %d planned)\n",
			livePath, bytes.Count(current, []byte("\n")), bytes.Count(desired.Bytes(), []byte("\n")))
		fmt.Fprintln(env.Stdout, "\nnext: sudo gvm edge apply")
	}
	if *full {
		fmt.Fprintln(env.Stdout, "\n"+desired.String())
	}
	return nil
}

func runEdgeApply(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("edge apply", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	configPath := fs.String("config", "/etc/gvm/gvm.yaml", "host config")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}
	t, err := Resolve(env, "edge", "")
	if err != nil {
		return err
	}
	if err := t.RequireLocal(); err != nil {
		return err
	}

	host, plan, err := loadEdgePlan(*configPath)
	if err != nil {
		return err
	}
	switch plan.Mode {
	case "none":
		return fmt.Errorf("edge apply: edge.mode is none; there is nothing to apply")
	case "proxy_external":
		return fmt.Errorf("edge apply: edge.mode is proxy_external; your proxy owns its own config, " +
			"and gvm writes nothing outside /etc/gvm in this mode")
	}
	if plan.Mode == "caddy_managed" && plan.TLS == edge.TLSAcmeHTTP {
		// Phase 40's `gvm firewall apply` is the one path that opens 80/443;
		// until it lands, gvm cannot CHECK them either — so it says so
		// instead of letting an ACME timeout tell the story badly.
		fmt.Fprintln(env.Stdout, "note: the ACME HTTP challenge needs 80/tcp and 443/tcp open; "+
			"gvm firewall is not available in this build, so verify with `sudo ufw status` first")
	}

	res, err := edgeops.Apply(ctx, edgeops.HostSystem{}, host, plan)
	for _, line := range res.Log {
		fmt.Fprintf(env.Stdout, "  %s\n", line)
	}
	if err != nil {
		return err
	}
	if res.Changed && plan.Mode == "caddy_managed" {
		fmt.Fprintln(env.Stdout, "\nthe config is live; certificates may still be provisioning "+
			"(watch: journalctl -u caddy -f)")
	}
	return nil
}

// ── gvm ca ───────────────────────────────────────────────────────────────────

func runCA(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(env.Stdout, "Usage: sudo gvm ca export [--out FILE]")
		fmt.Fprintln(env.Stdout, "Writes the internal CA's PUBLIC certificate (never its key), for the")
		fmt.Fprintln(env.Stdout, "bare-IP `tls internal` path: import it on each moderator's machine.")
		if len(args) == 0 {
			return usageErr(fmt.Errorf("ca: subcommand required"))
		}
		return errHelpShown
	}
	if args[0] != "export" {
		return usageErr(fmt.Errorf("ca: unknown subcommand %q", args[0]))
	}

	fs := flag.NewFlagSet("ca export", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	out := fs.String("out", "", "write the certificate here instead of stdout")
	configPath := fs.String("config", "/etc/gvm/gvm.yaml", "host config")
	if err := fs.Parse(args[1:]); err != nil {
		return flagErr(err)
	}
	t, err := Resolve(env, "ca", "")
	if err != nil {
		return err
	}
	if err := t.RequireLocal(); err != nil {
		return err
	}

	host, _, err := loadEdgePlan(*configPath)
	if err != nil {
		return err
	}
	adminSock := strings.TrimPrefix(host.Spec.Edge.Caddy.Admin, "unix/")

	var buf bytes.Buffer
	if err := edgeops.ExportCA(ctx, edgeops.HostSystem{}, adminSock, &buf); err != nil {
		return err
	}
	if *out == "" {
		_, err := env.Stdout.Write(buf.Bytes())
		return err
	}
	if err := os.WriteFile(*out, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("ca export: %w", err)
	}
	fmt.Fprintf(env.Stdout, "wrote %s (%d bytes, public certificate only)\n", *out, buf.Len())
	fmt.Fprintln(env.Stdout, "import it on each moderator's machine so the bare-IP dashboard is trusted")
	return nil
}
