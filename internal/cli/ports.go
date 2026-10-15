// `gvm ports` — what this host owes, and where the answer came from.
//
// Section 6.7 names five occupancy sources and singles out one failure: a
// caller that could not read `ip_local_port_range` and treated the range as
// empty hands out a port the kernel may assign to an outbound connection, and
// the symptom is an intermittent bind failure weeks later. So every source
// gathered here reports whether it was actually READ, and the printer says so
// rather than presenting an assumption as a measurement.
//
// The command is also `gvm doctor`'s port half: Check() catches the four ways
// a materialised allocation decays after creation -- the manifest gained a
// port, the manifest dropped one, somebody hand-edited the YAML, the operator
// widened the ephemeral range -- and nothing else re-checks them, because ports
// are written literally and never recomputed.

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/S95F/gameVPSmanager/internal/lifecycle"
	"github.com/S95F/gameVPSmanager/internal/platform"
	"github.com/S95F/gameVPSmanager/internal/ports"
	"github.com/S95F/gameVPSmanager/internal/spec"

	"gopkg.in/yaml.v3"
)

// buildOccupancy gathers all five sources of section 6.7.
//
// It returns notes rather than swallowing the gaps: an unreadable listener
// table, a manifest pin that would not load, an assumed ephemeral range. Each
// of those makes the answer weaker in a way the operator has to be told about,
// and the alternative -- a silently smaller occupancy -- allocates a port
// somebody is already listening on.
func buildOccupancy(ctx context.Context, h *HostEnv, self string) (*ports.Occupancy, []string, error) {
	var notes []string

	// A range that could not be read is left ZERO rather than filled in with
	// the kernel default here: NewOccupancy performs the substitution itself
	// and RECORDS it, which is what lets the printer say "assumed" instead of
	// presenting a guess as a measurement. Doing the fallback in this function
	// would erase exactly that distinction.
	var eph platform.PortRange
	read := h.Ephemeral
	if read == nil {
		read = ports.HostEphemeral
	}
	if r, err := read(); err == nil {
		eph = r
	}
	occ := ports.NewOccupancy(eph)

	// 1. Every instance this host defines, INCLUDING the disabled ones. A
	//    disabled instance still owns its numbers: `enabled: false` closes the
	//    firewall door, it does not hand the port to somebody else.
	names, probs, err := lifecycle.List(h.files())
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", spec.EtcDir+"/instances", err)
	}
	for _, p := range probs {
		notes = append(notes, p.Detail)
	}
	for _, name := range names {
		if name == self {
			continue
		}
		in, _, err := loadInstance(h, name)
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s: %v (its ports are still claimed)", name, err))
			occ.AddInstance(name, nil, nil)
			continue
		}
		occ.AddInstance(name, in.Spec.Ports, requirementsFor(h, in, &notes))
	}

	// 2. The in-flight reservations of a concurrent `gvm create`.
	if h.Ledger != nil {
		claims, err := h.Ledger.Claims()
		switch {
		case err == nil:
			occ.AddAll(claims)
		case errors.Is(err, fs.ErrNotExist):
			// No ledger yet is the normal state of a fresh box.
		default:
			notes = append(notes, fmt.Sprintf("the port ledger could not be read: %v", err))
		}
	}

	// 3. What is actually listening.
	ls, err := h.listeners()
	switch {
	case err == nil:
		occ.AddListeners(ls)
	case errors.Is(err, platform.ErrNotSupported):
		notes = append(notes, "listening sockets were not read on this platform: "+
			"occupancy is UNKNOWN here, not empty")
	default:
		notes = append(notes, fmt.Sprintf("listening sockets could not be read: %v", err))
	}

	// 4. The operator's own don't-touch list.
	cfg, cfgNote := hostConfig(h)
	if cfgNote != "" {
		notes = append(notes, cfgNote)
	}
	occ.AddReserved(cfg.Spec.Firewall.ReservedPorts)

	_ = ctx
	return occ, notes, nil
}

// requirementsFor loads another instance's pinned port requirements.
//
// A pin that will not load is NOT a reason to skip the instance: AddInstance
// with nil requirements claims the number on both protocols, which is the
// conservative reading and the right one for an instance whose manifest is no
// longer installed.
func requirementsFor(h *HostEnv, in *spec.Instance, notes *[]string) []ports.Requirement {
	m, err := pinnedManifest(h, in)
	if err != nil {
		*notes = append(*notes, fmt.Sprintf("%s: %v; its ports are claimed on both protocols",
			in.Metadata.Name, err))
		return nil
	}
	reqs, err := m.Spec.PortRequirements()
	if err != nil {
		*notes = append(*notes, fmt.Sprintf("%s: %v", in.Metadata.Name, err))
		return nil
	}
	return reqs
}

func (h *HostEnv) listeners() ([]ports.Listener, error) {
	if h.Listeners != nil {
		return h.Listeners()
	}
	return ports.HostListeners()
}

// hostConfig reads /etc/gvm/gvm.yaml, falling back to the shipped defaults.
//
// An absent file is the normal state of a box nobody has configured and must
// not be an error; an unparseable one is reported and then also falls back,
// because refusing to allocate a port because the firewall stanza has a typo
// in it would be a refusal out of all proportion to the fault.
func hostConfig(h *HostEnv) (spec.HostConfig, string) {
	cfg := spec.DefaultHostConfig()
	p := spec.EtcDir + "/gvm.yaml"
	raw, err := h.files().ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cfg, ""
		}
		return cfg, fmt.Sprintf("%s could not be read (%v); shipped defaults used", p, err)
	}
	var got spec.HostConfig
	if err := yaml.Unmarshal(raw, &got); err != nil {
		return cfg, fmt.Sprintf("%s does not parse (%v); shipped defaults used", p, err)
	}
	return got, ""
}

func runPorts(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("gvm ports", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, "Usage: gvm ports [instance]\n\n"+
			"With no instance, prints every endpoint this host owes and who owes it.\n"+
			"With one, also re-checks that instance's materialised allocation.\n\n")
		fs.PrintDefaults()
	}
	remote := fs.String("remote", "", "dashboard URL to run this against instead of the local host")

	pos, err := parseWithInstance(fs, args)
	if err != nil {
		return flagErr(err)
	}
	var name string
	if len(pos) > 0 {
		if name, err = oneInstance(pos); err != nil {
			return err
		}
	}
	t, err := Resolve(env, "ports", *remote)
	if err != nil {
		return err
	}
	if err := t.RequireLocal(); err != nil {
		return err
	}
	h, err := env.host(ctx)
	if err != nil {
		return err
	}

	// self is empty so that every instance -- including the one being asked
	// about -- appears in the table. Free() ignores self-owned claims by
	// design, which is right for allocation and wrong for a report.
	occ, notes, err := buildOccupancy(ctx, h, "")
	if err != nil {
		return err
	}
	printOccupancy(env.Stdout, occ, notes)

	if name == "" {
		return nil
	}
	in, _, err := loadInstance(h, name)
	if err != nil {
		return err
	}
	m, err := pinnedManifest(h, in)
	if err != nil {
		return err
	}
	cs, err := ports.CheckSpec(name, m.Spec, ports.Allocation(in.Spec.Ports), occ)
	if err != nil {
		return err
	}
	printPortConflicts(env.Stdout, name, cs)
	return nil
}

func printOccupancy(w io.Writer, occ *ports.Occupancy, notes []string) {
	eps := occ.Endpoints()
	ports.SortEndpoints(eps)

	fmt.Fprintf(w, "\n%d endpoint(s) held on this host\n\n", len(eps))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, ep := range eps {
		var who []string
		for _, c := range occ.Claims(ep) {
			who = append(who, describeClaim(c))
		}
		sort.Strings(who)
		fmt.Fprintf(tw, "  %s/%d\t%s\n", ep.Proto, ep.Port, strings.Join(who, ", "))
	}
	_ = tw.Flush()

	r := occ.Ephemeral()
	if occ.EphemeralAssumed() {
		fmt.Fprintf(w, "\n  ephemeral  %d-%d  ASSUMED: ip_local_port_range could not be read,\n"+
			"             so this is the kernel default and not a measurement of this box\n",
			r.Low, r.High)
	} else {
		fmt.Fprintf(w, "\n  ephemeral  %d-%d  (read from ip_local_port_range)\n", r.Low, r.High)
	}
	for _, n := range notes {
		fmt.Fprintf(w, "  note       %s\n", n)
	}
}

func describeClaim(c ports.Claim) string {
	s := string(c.Source)
	if c.Owner != "" {
		s += ":" + c.Owner
	}
	if c.Desc != "" {
		s += " (" + c.Desc + ")"
	}
	return s
}

func printPortConflicts(w io.Writer, name string, cs []ports.Conflict) {
	if len(cs) == 0 {
		fmt.Fprintf(w, "\n  %s: its materialised allocation still holds.\n", name)
		return
	}
	ports.SortConflicts(cs)
	fmt.Fprintf(w, "\n  %s: %d finding(s)\n", name, len(cs))
	for _, c := range cs {
		fmt.Fprintf(w, "    ! %s  %s\n", c.Kind, c.Detail)
	}
	fmt.Fprintf(w, "\n  Ports are written literally and never recomputed, so nothing else\n"+
		"  re-checks them. `gvm apply %s --diff` shows what the file says today.\n", name)
}
