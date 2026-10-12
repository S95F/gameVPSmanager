// Instance-side validation: the shape checks a /etc/gvm/instances/<name>.yaml
// must pass, and check 13 of section 8.6 — the named validators.
//
// The named validators are the reason this file is separate from validate.go.
// Everything in 8.6's first twelve checks can be decided from the manifest
// alone, at `gvm game validate` time, with no instance in sight. These cannot:
// they need the operator's actual variable VALUES, so they run at apply time,
// against the values that are about to be written.
//
// They are a CLOSED registry of Go functions for the reason D-21 gives: each one
// encodes a real, otherwise-invisible boot failure whose rule cannot be
// expressed as data without inventing an expression language. Valheim silently
// refuses to boot when the join password is under five characters; the server
// prints a line and exits, and the operator sees a unit that failed with no
// explanation. That is worth thirty lines of Go and is not worth a DSL.

package spec

import (
	"fmt"
	"regexp"
	"strings"
)

// Instance egress policies (section 18.4).
const (
	EgressOpen      = "open"
	EgressAllowlist = "allowlist"
)

// Content activation policies (section 12).
const (
	ActivationPreserve = "preserve"
	ActivationDerive   = "derive"
)

// Job actions -- the actions an INSTANCE YAML may schedule. These four are
// what section 6's canonical instance YAML enumerates.
//
// It is a subset of the job kinds internal/jobs can execute, and deliberately
// so. The exclusions are in unschedulableActions below, each with its reason,
// because the generic "%q is not a job action" diagnostic is actively
// unhelpful for a word that plainly IS one.
var jobActions = map[string]bool{
	"update": true, "restart": true, "backup": true, "content_sync": true,
}

// unschedulableActions are kinds internal/jobs can run that an instance may
// not put on a timer, and why.
//
// Spelled out here rather than derived, because internal/jobs imports this
// package and not the reverse -- so the authority at RUNTIME is this list and
// the authority in TEST is internal/jobs, whose
// TestNoSchedulableActionNeedsAnOperatorToTypeAName asserts that nothing in
// JobActions() is Destructive(). Either list growing a destructive kind fails
// the build rather than producing a nightly that cannot run.
var unschedulableActions = map[string]string{
	// Host-level timers that sweep every instance. A per-instance copy of any
	// of the three would be a second schedule racing the host one.
	"prune":  "`prune` is a host-level timer that sweeps every instance; a per-instance copy would race it",
	"verify": "`verify` is a host-level timer that sweeps every instance; a per-instance copy would race it",
	"guard":  "`guard` IS the recovery sweep, and it runs host-wide every five minutes",
	// Destructive, and section 16.4's confirmation is an instance name typed
	// by a person. A timer cannot type one: gvm-job@.service's ExecStart is a
	// bare `gvm job run %i` with no flags, and jobs.Descriptor is three
	// fields with no room for consent. The engine checks the confirmation
	// before it takes the lock or opens the audit span, so a scheduled one
	// fires and fails with no job_runs row and no audit row -- the least
	// observable failure in this subsystem.
	"rebuild": "a rebuild archives the world and then DELETES it; section 16.4 requires the " +
		"instance name typed by an operator, which a timer cannot supply",
	"restore": "a restore overwrites the live world from an archive; section 16.4 requires the " +
		"instance name typed by an operator, which a timer cannot supply",
}

// JobActions returns the actions an instance may schedule, sorted.
//
// Exported so the job layer can assert it has a runner for every one of them
// without keeping a second copy of the list. A validator that accepts an
// action nothing can execute produces a timer that fires nightly and fails
// nightly, which is the least observable failure in the subsystem.
func JobActions() []string { return sortedKeys(jobActions) }

// collectionSyncModes are how often a Steam collection is re-read.
var collectionSyncModes = map[string]bool{"manual": true, "on_update": true, "off": true}

// ValidatorInput is everything a named validator may see: the resolved values
// for one instance, and nothing else.
//
// Secrets are here because two of the three v1 validators are ABOUT a secret —
// Valheim's length-and-substring rule is the whole reason the registry exists.
// Nothing in this file logs, formats or returns a secret value; the diagnostics
// describe it ("under 5 characters") rather than quoting it.
type ValidatorInput struct {
	Instance string
	Vars     VarValues
	Secrets  SecretValues
	Ports    PortValues
}

type namedValidator struct {
	doc string
	run func(m *Manifest, in ValidatorInput, ix *NodeIndex) Diags
}

// validators is the closed registry check 13 names. Adding one is a deliberate,
// reviewable act; there is no way for a manifest to name a function that is not
// here.
var validators = map[string]namedValidator{
	"valheim.password_rules": {
		doc: "Valheim refuses to boot when the join password is under 5 characters or appears in the server name.",
		run: valheimPasswordRules,
	},
	"pz.server_name_is_world_dir": {
		doc: "Project Zomboid's server_name names a directory under Saves/Multiplayer and an .ini beside it, so it must be a legal single path segment.",
		run: pzServerNameIsWorldDir,
	},
	"mc.port_triple_distinct": {
		doc: "Minecraft's game, query and rcon ports must not land on the same protocol and number.",
		run: mcPortTripleDistinct,
	},
}

// validatorNames is the closure check 2 consults. It is derived from the
// registry above so a validator cannot exist without being nameable, or be
// nameable without existing.
var validatorNames = registry{what: "validator", entries: func() map[string]Support {
	m := make(map[string]Support, len(validators))
	for name := range validators {
		m[name] = SupportImplemented
	}
	return m
}()}

// ValidatorDoc returns one validator's description, for `gvm game validate` and
// the manifest reference.
func ValidatorDoc(name string) (string, bool) {
	v, ok := validators[name]
	return v.doc, ok
}

// ValidatorNames returns every registered validator, sorted.
func ValidatorNames() []string { return validatorNames.names() }

// RunValidators runs the manifest's spec.validators against real values.
//
// ix positions the diagnostics in the INSTANCE file and may be nil. A finding
// about a value that lives in the secret store rather than in the YAML carries
// its path anyway (`secrets.join_password`), because the operator needs to know
// which thing to change and the address is the answer even when the file has no
// line for it.
func RunValidators(m *Manifest, in ValidatorInput, ix *NodeIndex) Diags {
	if m == nil {
		return nil
	}
	var out Diags
	for _, name := range m.Spec.Validators {
		v, ok := validators[name]
		if !ok {
			// Check 2 already reported this at load. Silence here rather than a
			// second diagnostic about the same word.
			continue
		}
		out = append(out, v.run(m, in, ix)...)
	}
	return out
}

// ValidateInstance checks one instance document against the manifest it names,
// then runs the named validators.
//
// It is the apply-time half: manifest validation proves the GAME is describable,
// this proves THIS SERVER is. m may be nil when the manifest could not be
// resolved, in which case only the manifest-independent shape checks run —
// reporting "your egress policy is misspelled" is still useful when the catalog
// is broken.
func ValidateInstance(in *Instance, m *Manifest, vals ValidatorInput, ix *NodeIndex) Diags {
	if in == nil {
		return nil
	}
	c := &checker{m: m, ix: ix, bad: map[string]bool{}}

	if in.APIVersion != APIVersionV1 {
		c.add(c.errf("apiVersion", "%q is not a known schema version (want %s)", in.APIVersion, APIVersionV1))
	}
	if in.Kind != KindInstance {
		c.add(c.errf("kind", "%q is not an instance (want %s)", in.Kind, KindInstance))
	}
	if in.Metadata.Name == "" {
		c.add(c.errf("metadata.name", "an instance needs a name; it is the systemd unit, the unix user and the directory"))
	}
	if in.Spec.Game == "" {
		c.add(c.errf("spec.game", "an instance needs a game"))
	} else if m != nil && in.Spec.Game != m.Metadata.ID {
		c.add(c.errf("spec.game", "the instance names game %q but the manifest is %q", in.Spec.Game, m.Metadata.ID))
	}

	switch in.Spec.Egress.Policy {
	case EgressOpen, "":
	case EgressAllowlist:
		if len(in.Spec.Egress.Allow) == 0 {
			c.add(c.errf("spec.egress.allow", "policy allowlist with an empty allow list blocks all outbound traffic").
				WithFix("IPAddressDeny=any with no IPAddressAllow= stops SteamCMD, workshop downloads and the game's own backend"))
		}
	default:
		c.add(c.errf("spec.egress.policy", "%q is not an egress policy (want %s|%s)",
			in.Spec.Egress.Policy, EgressOpen, EgressAllowlist))
	}

	for _, name := range sortedKeys(in.Spec.Content) {
		cl := in.Spec.Content[name]
		base := "spec.content." + name
		switch cl.ActivationPolicy {
		case ActivationPreserve, ActivationDerive, "":
		default:
			c.add(c.errf(base+".activation_policy", "%q is not an activation policy (want %s|%s)",
				cl.ActivationPolicy, ActivationPreserve, ActivationDerive).
				WithFix("`preserve` returns the live config's list untouched, which is what adoption depends on"))
		}
		if s := cl.CollectionSync; s != "" && !collectionSyncModes[s] {
			c.add(c.errf(base+".collection_sync", "%q is not a sync mode (want %s)",
				s, orList(sortedKeys(collectionSyncModes))))
		}
	}

	for _, name := range sortedKeys(in.Spec.Jobs) {
		j := in.Spec.Jobs[name]
		action := j.Action
		if action == "" {
			action = name
		}
		if !jobActions[action] {
			path := "spec.jobs." + name + ".action"
			if why, known := unschedulableActions[action]; known {
				// A known action that cannot be scheduled. The generic list
				// would be a worse message than no message: an operator told
				// that `rebuild` "is not a job action" goes looking for a
				// typo. A hard error and not a warning, because a warning
				// leaves the timer written.
				d := c.errf(path, "%q cannot be scheduled on an instance: %s", action, why)
				switch action {
				case "rebuild", "restore":
					d = d.WithFix("run it from a terminal instead: gvm %s %s --confirm %s",
						action, in.Metadata.Name, in.Metadata.Name)
				default:
					d = d.WithFix("the host timer already covers every instance; remove this " +
						"jobs entry")
				}
				c.add(d)
			} else {
				c.add(c.errf(path, "%q is not a job action (want %s)",
					action, orList(sortedKeys(jobActions))))
			}
		}
		if j.Schedule == "" {
			c.add(c.errf("spec.jobs."+name+".schedule", "a job needs a schedule (a systemd OnCalendar expression)"))
		}
	}

	if m != nil {
		c.checkInstanceAgainstManifest(in)
		c.out = append(c.out, RunValidators(m, vals, ix)...)
	}
	return c.out
}

// checkInstanceAgainstManifest is the half that needs both documents: the vars
// the manifest declares, the ports it needs, D-20's narrowing rule, and that
// every content key names a block the game actually has.
func (c *checker) checkInstanceAgainstManifest(in *Instance) {
	// The content map's KEY is how an instance's list is bound to a manifest
	// block, and it is the one spelling in the system matched by name and
	// otherwise unchecked. An unmatched key is silent in both directions: the
	// list is never read, AND the activation_policy written on it is never
	// honoured -- so an instance that says `preserve` is resolved as `derive`,
	// the lock records verbatim_activation:false, and from that point nothing
	// downstream can tell it from an instance that really does derive.
	//
	// The same class of name is already refused twice: a template reference to
	// `.Content.<name>` (checkTemplates, via this same helper) and an
	// instance/manifest SOURCE mismatch in internal/content's schema compiler.
	// Both are blind to this case, which is why it gets its own check rather
	// than a shared one.
	for _, name := range sortedKeys(in.Spec.Content) {
		if contentBlockExists(c.m, name) {
			continue
		}
		d := c.errf("spec.content."+name,
			"game %q declares no content block named %q, so this list is never read and its "+
				"activation_policy is never honoured", c.m.Metadata.ID, name)
		fix := "rename the key to the manifest's block name -- do not delete it, or the " +
			"declared list goes with it"
		if n := nearestName(name, contentBlockNames(c.m)); n != "" {
			fix = fmt.Sprintf("did you mean %q? Rename the key -- do not delete it, or the "+
				"declared list goes with it", n)
		}
		c.add(d.WithFix("%s", fix))
	}

	for _, name := range sortedKeys(in.Spec.Vars) {
		v, ok := c.m.Spec.Vars[name]
		if !ok {
			d := c.errf("spec.vars."+name, "%q is not declared by game %q", name, c.m.Metadata.ID)
			if n := nearestName(name, varNames(c.m)); n != "" {
				d = d.WithFix("did you mean %q?", n)
			}
			c.add(d)
			continue
		}
		if err := checkVarValue(v, in.Spec.Vars[name]); err != nil {
			c.add(c.errf("spec.vars."+name, "%s", err.Error()))
		}
	}
	for _, name := range varNames(c.m) {
		v := c.m.Spec.Vars[name]
		if !v.Required {
			continue
		}
		if in.Spec.Vars[name] == "" && v.Default == "" {
			c.add(c.errf("spec.vars", "var %q is required by game %q and has no value", name, c.m.Metadata.ID))
		}
	}

	for _, p := range c.m.Spec.Ports {
		if _, ok := in.Spec.Ports[p.Name]; !ok {
			c.add(c.errf("spec.ports", "port %q is declared by the game but has no allocation", p.Name).
				WithFix("ports are materialised at create time and never recomputed at runtime; run `gvm apply`"))
		}
	}

	// D-20: an instance may only NARROW its manifest's exposure. The one
	// exception is none -> admin, which is exactly what ADMIN_IP was for: a port
	// the manifest keeps off the firewall entirely, opened to the operator's own
	// CIDR and nobody else.
	for _, name := range sortedKeys(in.Spec.Firewall.Expose) {
		want := in.Spec.Firewall.Expose[name]
		p, ok := c.m.Spec.PortByName(name)
		if !ok {
			c.add(c.errf("spec.firewall.expose."+name, "there is no port named %q", name))
			continue
		}
		if !want.Valid() {
			c.add(c.errf("spec.firewall.expose."+name, "%q is not an exposure (want %s|%s|%s)",
				string(want), ExposePublic, ExposeAdmin, ExposeNone))
			continue
		}
		if want == ExposeAdmin && p.Expose == ExposeNone {
			continue
		}
		if !want.Narrows(p.Expose) {
			c.add(c.errf("spec.firewall.expose."+name,
				"the game exposes %q as %s; an instance may only narrow that, not widen it to %s",
				name, p.Expose, want).
				WithFix("narrowing is public -> admin -> none; none -> admin is the one widening allowed, for an operator-only port (D-20)"))
		}
	}
}

// valheimPasswordRules encodes a boot failure with no diagnostic: Valheim exits
// with a one-line complaint and systemd reports a failed unit. Both halves of
// the rule are real — the length floor and the substring rule — and the second
// is the one people hit, because "MyServer" / "server" looks like a sensible
// pair right up until the server will not start.
func valheimPasswordRules(m *Manifest, in ValidatorInput, ix *NodeIndex) Diags {
	const (
		secretName = "join_password"
		nameVar    = "public_name"
		minLen     = 5
	)
	pw := in.Secrets[secretName]
	if pw == "" {
		// No password is a legitimate configuration: the server is open. The
		// rules only bind once one is set.
		return nil
	}

	var out Diags
	path := "secrets." + secretName
	if len([]rune(pw)) < minLen {
		out = append(out, ix.Errorf(path,
			"the join password is %d characters; Valheim refuses to boot under %d",
			len([]rune(pw)), minLen).
			WithFix("rotate it with `gvm secret set %s %s`", in.Instance, secretName))
	}
	if server := in.Vars[nameVar]; server != "" &&
		strings.Contains(strings.ToLower(server), strings.ToLower(pw)) {
		out = append(out, ix.Errorf(path,
			"the join password appears inside the server name; Valheim refuses to boot").
			WithFix("change the password, or rename the server in spec.vars."+nameVar))
	}
	return out
}

// pzWorldDirRe is what may name a directory under Saves/Multiplayer AND the
// .ini beside it. It is deliberately narrower than the filesystem allows: PZ's
// own start-server.sh interpolates the name unquoted, so a space does not fail
// loudly, it generates a DIFFERENT world.
var pzWorldDirRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func pzServerNameIsWorldDir(m *Manifest, in ValidatorInput, ix *NodeIndex) Diags {
	const nameVar = "server_name"
	v, declared := in.Vars[nameVar]
	if !declared {
		return Diags{ix.Errorf("spec.vars."+nameVar,
			"%s is required: it names the world directory", nameVar)}
	}
	if pzWorldDirRe.MatchString(v) {
		return nil
	}
	return Diags{ix.Errorf("spec.vars."+nameVar,
		"%q cannot name a world directory", v).
		WithFix("use letters, digits, '-' and '_' only: the value becomes Saves/Multiplayer/<name> and Server/<name>.ini, and start-server.sh interpolates it unquoted, so a space generates a different world instead of failing")}
}

// mcPortTripleDistinct catches the allocation that looks fine and is not.
// Minecraft's game and query ports legitimately share a NUMBER (25565 tcp and
// udp), so a naive "all three differ" check would reject a correct manifest;
// what must not repeat is the protocol-and-number pair, because that is one
// socket and the second bind loses.
func mcPortTripleDistinct(m *Manifest, in ValidatorInput, ix *NodeIndex) Diags {
	type endpoint struct {
		proto Proto
		port  int
	}
	seen := map[endpoint]string{}
	var out Diags
	for _, p := range m.Spec.Ports {
		port, ok := in.Ports[p.Name]
		if !ok || port == 0 {
			continue
		}
		protos, err := ParseProtos(p.Proto)
		if err != nil {
			continue // check 5 already reported it
		}
		for _, pr := range protos {
			e := endpoint{pr, port}
			if other, dup := seen[e]; dup {
				out = append(out, ix.Errorf("spec.ports."+p.Name,
					"%s/%d is allocated to both %q and %q", pr, port, other, p.Name).
					WithFix("only one socket can bind %s/%d; re-run `gvm apply` to reallocate, or pin the ports explicitly", pr, port))
				continue
			}
			seen[e] = p.Name
		}
	}
	return out
}
