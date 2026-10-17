package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// runGameShow is `gvm game show <id> [--yaml]`: everything an operator needs to
// decide whether and how to create an instance of a game, offline and without
// root (RouteLocal). It reads the embedded catalog, renders no template and
// never prints a secret's value (rule 10) — only the names of the secrets a
// game references, so the operator knows what they will have to set.
func runGameShow(env *Env, args []string) error {
	fs, unapproved := gameFlags(env, "show", "<game-id>")
	asYAML := fs.Bool("yaml", false, "print the manifest bytes instead of the summary")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagErr(err)
	}
	if len(pos) != 1 {
		fs.Usage()
		return usageErr(&userError{Msg: "name one game id", Hint: "`gvm game list` shows them"})
	}
	id := pos[0]

	cands, _, err := gameCatalog().Resolve(id, overrideAuth(*unapproved))
	if err != nil {
		return err
	}
	var win *spec.Manifest
	for i := range cands {
		if cands[i].Winner {
			win = cands[i].Manifest
			break
		}
	}
	if win == nil {
		return usageErr(&userError{Msg: fmt.Sprintf("no game %q", id), Hint: "`gvm game list` shows what this box can run"})
	}

	if *asYAML {
		// The verbatim bytes the loader kept, so `gvm game show <id> --yaml`
		// round-trips through `gvm game validate`.
		env.Stdout.Write(win.Raw)
		if len(win.Raw) > 0 && win.Raw[len(win.Raw)-1] != '\n' {
			fmt.Fprintln(env.Stdout)
		}
		return nil
	}

	printGameShow(env.Stdout, win)
	return nil
}

func printGameShow(w io.Writer, m *spec.Manifest) {
	fmt.Fprintf(w, "\n%s  (%s, revision %d)\n", m.Metadata.Name, m.Metadata.ID, m.Metadata.Revision)
	if doc := strings.TrimSpace(m.Metadata.Doc); doc != "" {
		fmt.Fprintf(w, "  %s\n", doc)
	}
	fmt.Fprintf(w, "\n  provider  %s\n", showProviderLine(m))
	if s := m.Spec.Sizing; s.RAMFloorMB > 0 || s.DiskFloorGB > 0 {
		fmt.Fprintf(w, "  needs     ~%d MiB RAM, ~%d GiB disk (floors)\n", s.RAMFloorMB, s.DiskFloorGB)
	}
	if m.Metadata.Deprecated != "" {
		fmt.Fprintf(w, "  DEPRECATED %s\n", m.Metadata.Deprecated)
	}

	printGamePorts(w, m.Spec.Ports)
	printGameSecrets(w, m)
	printGameVars(w, m.Spec.Vars)
	printGameJobs(w, m.Spec.Jobs)

	fmt.Fprintf(w, "\n  create it:\n    %s\n", createLine(m))
}

// protonProviderLine is what `game show` and `game list` say about a Proton
// title, in the operator's terms: gvm runs it, but the operator supplies Proton.
const protonProviderLine = "Windows server under Proton — install Proton yourself"

// isProtonTitle reports whether a manifest launches its server through Proton.
// The authoritative marker is the runtime wrapper, not the provider platform: a
// Proton title is one gvm wraps in a Proton prefix the operator must bring
// themselves (see: gvm help proton).
func isProtonTitle(m *spec.Manifest) bool {
	w := m.Spec.Runtime.Wrapper
	return w != nil && w.Kind == "proton"
}

// showProviderLine says, in an operator's terms, what runs the game and what
// they must bring themselves.
func showProviderLine(m *spec.Manifest) string {
	if isProtonTitle(m) {
		return protonProviderLine
	}
	p := m.Spec.Provider
	switch p.Kind {
	case "steamcmd":
		if p.SteamCMD != nil && p.SteamCMD.Platform == "windows" {
			return protonProviderLine
		}
		return "Steam (SteamCMD)"
	case "minecraft":
		return "Minecraft"
	case "adopted":
		return "adopted from an existing install"
	case "none", "":
		return "no installer (bring your own files)"
	default:
		return p.Kind
	}
}

func printGamePorts(w io.Writer, ports []spec.PortSpec) {
	if len(ports) == 0 {
		return
	}
	fmt.Fprint(w, "\n  ports:\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, p := range ports {
		note := p.Note
		if note != "" {
			note = "  " + note
		}
		fmt.Fprintf(tw, "    %s\t%d/%s\t%s%s\n", p.Name, p.Default, p.Proto, p.Expose, note)
	}
	_ = tw.Flush()
}

func printGameSecrets(w io.Writer, m *spec.Manifest) {
	refs, err := spec.ScanManifestRefs(m)
	if err != nil || len(refs.Secrets) == 0 {
		return
	}
	fmt.Fprintf(w, "\n  secrets:  %s\n", strings.Join(refs.Secrets, ", "))
}

func printGameVars(w io.Writer, vars map[string]spec.VarSpec) {
	if len(vars) == 0 {
		return
	}
	names := make([]string, 0, len(vars))
	for n := range vars {
		names = append(names, n)
	}
	sort.Strings(names)

	fmt.Fprint(w, "\n  vars:\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprint(tw, "    NAME\tTYPE\tDEFAULT\tAPPLY\tNOTES\n")
	for _, n := range names {
		v := vars[n]
		def := v.Default
		if v.Required && def == "" {
			def = "(required)"
		}
		var flags []string
		if v.Immutable {
			flags = append(flags, "immutable")
		}
		if len(v.Enum) > 0 {
			flags = append(flags, "one of "+strings.Join(v.Enum, "|"))
		} else if v.Min != nil || v.Max != nil {
			flags = append(flags, rangeNote(v))
		}
		if h := strings.TrimSpace(v.Help); h != "" {
			flags = append(flags, h)
		}
		apply := v.Apply
		if apply == "" {
			apply = "restart"
		}
		fmt.Fprintf(tw, "    %s\t%s\t%s\t%s\t%s\n", n, v.Type, def, apply, strings.Join(flags, "; "))
	}
	_ = tw.Flush()
}

func rangeNote(v spec.VarSpec) string {
	switch {
	case v.Min != nil && v.Max != nil:
		return fmt.Sprintf("%d–%d", *v.Min, *v.Max)
	case v.Min != nil:
		return fmt.Sprintf("≥%d", *v.Min)
	case v.Max != nil:
		return fmt.Sprintf("≤%d", *v.Max)
	}
	return ""
}

func printGameJobs(w io.Writer, jobs spec.RecommendedJobs) {
	if len(jobs) == 0 {
		return
	}
	names := make([]string, 0, len(jobs))
	for n := range jobs {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Fprint(w, "\n  recommended jobs:\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, n := range names {
		j := jobs[n]
		action := j.Action
		if action == "" {
			action = n
		}
		fmt.Fprintf(tw, "    %s\t%s at %s\t%s\n", n, action, j.At, j.Why)
	}
	_ = tw.Flush()
}

// createLine is the paste-ready command, with a --var for each required var
// that has no default (the ones an operator must supply).
func createLine(m *spec.Manifest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "sudo gvm create <name> --game %s", m.Metadata.ID)
	names := make([]string, 0, len(m.Spec.Vars))
	for n := range m.Spec.Vars {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		v := m.Spec.Vars[n]
		if v.Required && v.Default == "" {
			fmt.Fprintf(&b, " --var %s=…", n)
		}
	}
	return b.String()
}
