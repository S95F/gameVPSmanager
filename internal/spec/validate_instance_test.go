package spec

import (
	"strings"
	"testing"
)

func valheimManifest() *Manifest {
	return &Manifest{
		APIVersion: APIVersionV1,
		Kind:       KindGame,
		Metadata:   GameMeta{ID: "valheim", Name: "Valheim"},
		Spec: GameSpec{
			Validators: []string{"valheim.password_rules"},
			Ports: []PortSpec{
				{Name: "game", Proto: "udp", Default: 2456, Group: "vh", Expose: ExposePublic},
				{Name: "query", Proto: "udp", Default: 2457, Group: "vh", Expose: ExposePublic},
				{Name: "spare", Proto: "udp", Default: 2458, Group: "vh", Expose: ExposeNone},
			},
			Vars: map[string]VarSpec{
				"public_name": {Type: VarString, Required: true, Apply: ApplyRestart},
				"world_name":  {Type: VarString, Default: "Dedicated", Apply: ApplyRebuild},
			},
		},
	}
}

func valheimInstance() *Instance {
	return &Instance{
		APIVersion: APIVersionV1,
		Kind:       KindInstance,
		Metadata:   InstanceMeta{Name: "vh-1"},
		Spec: InstanceSpec{
			Game:   "valheim",
			Vars:   map[string]string{"public_name": "The Long Dark", "world_name": "Dedicated"},
			Ports:  map[string]int{"game": 2456, "query": 2457, "spare": 2458},
			Egress: EgressPolicy{Policy: EgressOpen},
		},
	}
}

// TestValheimPasswordRules encodes a boot failure with no diagnostic: the server
// prints one line and exits, and systemd reports a failed unit with nothing in
// it that names the password.
func TestValheimPasswordRules(t *testing.T) {
	for _, tc := range []struct {
		name       string
		serverName string
		password   string
		wantErrs   int
	}{
		{name: "no password at all is an open server, not a violation", password: "", wantErrs: 0},
		{name: "five characters is the floor", password: "abcde", wantErrs: 0},
		{name: "four characters refuses to boot", password: "abcd", wantErrs: 1},
		{
			name:       "a password inside the server name refuses to boot",
			serverName: "MyZqxwvHere",
			password:   "Zqxwv",
			wantErrs:   1,
		},
		{
			name:       "the substring rule ignores case, because a false positive costs a rename and a false negative costs a boot",
			serverName: "MyZQXWVHere",
			password:   "zqxwv",
			wantErrs:   1,
		},
		{
			name:       "both rules can fire at once",
			serverName: "abcd-world",
			password:   "abcd",
			wantErrs:   2,
		},
		{name: "an unrelated password is fine", serverName: "The Long Dark", password: "correcthorse", wantErrs: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := ValidatorInput{
				Instance: "vh-1",
				Vars:     VarValues{"public_name": tc.serverName},
				Secrets:  SecretValues{"join_password": tc.password},
			}
			got := RunValidators(valheimManifest(), in, nil)
			if len(got) != tc.wantErrs {
				t.Fatalf("got %d diagnostics, want %d:\n%s", len(got), tc.wantErrs, got)
			}
			for _, d := range got {
				if strings.Contains(d.String(), tc.password) && tc.password != "" {
					t.Errorf("a diagnostic must describe the secret, never quote it: %s", d)
				}
			}
		})
	}
}

func TestPZServerNameIsWorldDir(t *testing.T) {
	m := &Manifest{
		Metadata: GameMeta{ID: "projectzomboid"},
		Spec:     GameSpec{Validators: []string{"pz.server_name_is_world_dir"}},
	}
	for _, tc := range []struct {
		value   string
		wantErr bool
	}{
		{value: "servertest"},
		{value: "pz-main"},
		{value: "PZ_Main_42"},

		// start-server.sh interpolates the name unquoted, so a space does not
		// fail loudly -- it generates a DIFFERENT world.
		{value: "my server", wantErr: true},
		{value: "", wantErr: true},
		{value: "../../etc", wantErr: true},
		{value: "main/sub", wantErr: true},
		{value: "-leading-dash", wantErr: true},
	} {
		in := ValidatorInput{Vars: VarValues{"server_name": tc.value}}
		got := RunValidators(m, in, nil)
		if (len(got) > 0) != tc.wantErr {
			t.Errorf("server_name %q: got %d diagnostics, wantErr %v\n%s", tc.value, len(got), tc.wantErr, got)
		}
	}
}

// TestMCPortTripleDistinct: Minecraft's game and query ports legitimately share
// a NUMBER (25565 tcp and udp), so a naive "all three differ" check would reject
// a correct manifest. What must not repeat is the protocol-and-number pair.
func TestMCPortTripleDistinct(t *testing.T) {
	m := &Manifest{
		Metadata: GameMeta{ID: "minecraft-neoforge"},
		Spec: GameSpec{
			Validators: []string{"mc.port_triple_distinct"},
			Ports: []PortSpec{
				{Name: "game", Proto: "tcp", Default: 25565},
				{Name: "query", Proto: "udp", Default: 25565},
				{Name: "rcon", Proto: "tcp", Default: 25575},
			},
		},
	}

	ok := ValidatorInput{Ports: PortValues{"game": 25565, "query": 25565, "rcon": 25575}}
	if got := RunValidators(m, ok, nil); len(got) != 0 {
		t.Fatalf("tcp/25565 and udp/25565 are different sockets:\n%s", got)
	}

	clash := ValidatorInput{Ports: PortValues{"game": 25565, "query": 25565, "rcon": 25565}}
	got := RunValidators(m, clash, nil)
	if len(got) != 1 {
		t.Fatalf("got %d diagnostics, want 1:\n%s", len(got), got)
	}
	if !strings.Contains(got[0].Msg, "tcp/25565") {
		t.Errorf("the message must name the endpoint that collides: %q", got[0].Msg)
	}
}

func TestOnlyTheValidatorsAManifestNamesAreRun(t *testing.T) {
	m := valheimManifest()
	m.Spec.Validators = nil

	in := ValidatorInput{Secrets: SecretValues{"join_password": "ab"}}
	if got := RunValidators(m, in, nil); len(got) != 0 {
		t.Errorf("a manifest that names no validators runs none:\n%s", got)
	}
}

func TestAnUnknownValidatorNameIsSilentHereBecauseCheck2AlreadySaidIt(t *testing.T) {
	m := valheimManifest()
	m.Spec.Validators = []string{"valheim.password_rulez"}

	in := ValidatorInput{Secrets: SecretValues{"join_password": "ab"}}
	if got := RunValidators(m, in, nil); len(got) != 0 {
		t.Errorf("two diagnostics about one misspelled word is one too many:\n%s", got)
	}
	// ...but the load-time check must catch it, or nothing would.
	if validatorNames.lookup("valheim.password_rulez") != SupportUnknown {
		t.Error("the registry must not know a name that has no function")
	}
}

func TestEveryRegisteredValidatorIsNameableAndDocumented(t *testing.T) {
	for _, name := range ValidatorNames() {
		doc, ok := ValidatorDoc(name)
		if !ok || doc == "" {
			t.Errorf("%s has no doc; `gvm game validate` prints it and the manifest reference lists it", name)
		}
		if validatorNames.lookup(name) == SupportUnknown {
			t.Errorf("%s is registered but check 2 would reject it", name)
		}
	}
}

func TestValidateInstanceShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Instance)
		path string
		// manifest overrides valheimManifest for the rows that need the game
		// to declare something. The content rows do: a key naming no declared
		// block is now its own diagnostic, so a row about the POLICY has to
		// use a manifest that has the block, or it would be asserting two
		// findings at once.
		manifest func() *Manifest
	}{
		{
			name: "an unknown egress policy",
			edit: func(in *Instance) { in.Spec.Egress.Policy = "deny" },
			path: "spec.egress.policy",
		},
		{
			name: "an allowlist with nothing on it blocks all outbound traffic",
			edit: func(in *Instance) { in.Spec.Egress = EgressPolicy{Policy: EgressAllowlist} },
			path: "spec.egress.allow",
		},
		{
			name: "an unknown activation policy",
			edit: func(in *Instance) {
				in.Spec.Content = map[string]ContentList{"workshop": {ActivationPolicy: "rederive"}}
			},
			path:     "spec.content.workshop.activation_policy",
			manifest: contentManifest,
		},
		{
			name: "an unknown job action",
			edit: func(in *Instance) {
				in.Spec.Jobs = map[string]Job{"nightly": {Action: "updateee", Schedule: "*-*-* 05:00:00"}}
			},
			path: "spec.jobs.nightly.action",
		},
		{
			// A destructive job cannot be scheduled, and the diagnostic has
			// to say WHY rather than listing the four acceptable words:
			// `rebuild` plainly IS a job action, so "is not a job action"
			// teaches an operator nothing. Section 16.4 wants the instance
			// name typed by a person and a timer cannot type one --
			// gvm-job@.service's ExecStart carries no flags and
			// jobs.Descriptor has no field for consent -- so such a timer
			// could only fire and fail, with no job_runs row and no audit
			// row to find it by.
			name: "a scheduled rebuild could only ever fire and fail",
			edit: func(in *Instance) {
				in.Spec.Jobs = map[string]Job{
					"wipe": {Action: "rebuild", Schedule: "Sun *-*-* 04:00:00"},
				}
			},
			path: "spec.jobs.wipe.action",
		},
		{
			name: "a scheduled restore is the same refusal",
			edit: func(in *Instance) {
				in.Spec.Jobs = map[string]Job{
					"roll": {Action: "restore", Schedule: "Sun *-*-* 04:00:00"},
				}
			},
			path: "spec.jobs.roll.action",
		},
		{
			name: "a job with no schedule would never fire",
			edit: func(in *Instance) { in.Spec.Jobs = map[string]Job{"backup": {}} },
			path: "spec.jobs.backup.schedule",
		},
		{
			name: "an instance naming a different game than the manifest resolved",
			edit: func(in *Instance) { in.Spec.Game = "projectzomboid" },
			path: "spec.game",
		},
		{
			name: "a var the game does not declare",
			edit: func(in *Instance) { in.Spec.Vars["worldname"] = "x" },
			path: "spec.vars.worldname",
		},
		{
			name: "a port the game declares but nothing allocated",
			edit: func(in *Instance) { delete(in.Spec.Ports, "spare") },
			path: "spec.ports",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := valheimInstance()
			tc.edit(in)

			mf := valheimManifest
			if tc.manifest != nil {
				mf = tc.manifest
			}
			got := ValidateInstance(in, mf(), ValidatorInput{}, nil)
			if len(got) != 1 || got[0].Path != tc.path {
				t.Fatalf("want one diagnostic at %s, got:\n%s", tc.path, got)
			}
		})
	}
}

func TestACleanInstanceValidatesQuietly(t *testing.T) {
	got := ValidateInstance(valheimInstance(), valheimManifest(),
		ValidatorInput{Vars: VarValues{"public_name": "The Long Dark"}}, nil)
	if len(got) != 0 {
		t.Errorf("want no diagnostics, got:\n%s", got)
	}
}

// TestAnInstanceMayOnlyNarrowItsExposure is D-20. The one widening allowed is
// none -> admin, which is exactly what ADMIN_IP was for: a port the game keeps
// off the firewall entirely, opened to the operator's own CIDR and nobody else.
func TestAnInstanceMayOnlyNarrowItsExposure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		port    string
		want    Expose
		wantErr bool
	}{
		{name: "public narrowed to admin", port: "game", want: ExposeAdmin},
		{name: "public narrowed to none", port: "game", want: ExposeNone},
		{name: "none opened to admin is the ADMIN_IP case", port: "spare", want: ExposeAdmin},
		{name: "none widened to public", port: "spare", want: ExposePublic, wantErr: true},
		{name: "an exposure that is not one of the three", port: "game", want: "internet", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := valheimInstance()
			in.Spec.Firewall.Expose = map[string]Expose{tc.port: tc.want}

			got := ValidateInstance(in, valheimManifest(), ValidatorInput{}, nil)
			if (len(got) > 0) != tc.wantErr {
				t.Fatalf("got %d diagnostics, wantErr %v:\n%s", len(got), tc.wantErr, got)
			}
			if tc.wantErr && got[0].Path != "spec.firewall.expose."+tc.port {
				t.Errorf("diagnostic at %s, want spec.firewall.expose.%s", got[0].Path, tc.port)
			}
		})
	}
}

func TestNarrowingAnUndeclaredPortIsAnError(t *testing.T) {
	in := valheimInstance()
	in.Spec.Firewall.Expose = map[string]Expose{"rcon": ExposeNone}

	got := ValidateInstance(in, valheimManifest(), ValidatorInput{}, nil)
	if len(got) != 1 || got[0].Path != "spec.firewall.expose.rcon" {
		t.Fatalf("want one diagnostic naming the port, got:\n%s", got)
	}
}

func TestARequiredVarWithNoValueStopsAnApply(t *testing.T) {
	in := valheimInstance()
	delete(in.Spec.Vars, "public_name")

	got := ValidateInstance(in, valheimManifest(), ValidatorInput{}, nil)
	if len(got) != 1 || got[0].Path != "spec.vars" {
		t.Fatalf("want one diagnostic about the missing required var, got:\n%s", got)
	}
}

// TestValidateInstanceWithoutAManifest is the degraded case: the catalog is
// broken or the game id resolves to nothing. Saying "your egress policy is
// misspelled" is still useful, and inventing manifest findings is not.
func TestValidateInstanceWithoutAManifest(t *testing.T) {
	in := valheimInstance()
	in.Spec.Egress.Policy = "deny"
	in.Spec.Vars["nosuchvar"] = "x"

	got := ValidateInstance(in, nil, ValidatorInput{}, nil)
	if len(got) != 1 || got[0].Path != "spec.egress.policy" {
		t.Fatalf("want only the manifest-independent finding, got:\n%s", got)
	}
}

func TestValidateInstanceOfNothing(t *testing.T) {
	if got := ValidateInstance(nil, valheimManifest(), ValidatorInput{}, nil); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

// contentManifest is a manifest whose game declares one content block, named
// `workshop`.
func contentManifest() *Manifest {
	m := valheimManifest()
	m.Spec.Content = []ContentSpec{{
		Name:          "workshop",
		Source:        SourceSteamWorkshop,
		WorkshopAppID: 108600,
		Acquisition: ContentAcquisition{
			Mechanism: AcquireServerBoot, WriteKey: "WorkshopItems",
			WriteFile: "server_ini", Separator: ";", KeepMissing: true,
		},
		Activation: ContentActivation{
			WriteKey: "Mods", WriteFile: "server_ini", Separator: ";",
			Resolver: "pz.resolve_mod_ids",
		},
	}}
	return m
}

// TestAnInstanceContentKeyMustNameADeclaredBlock is the one spelling in the
// system that is matched by NAME and was never checked.
//
// The map key is how an instance's list is bound to the manifest's block. An
// unmatched key means the list is never read AND the activation_policy written
// on it is never honoured -- so an instance that says `preserve` resolves as
// `derive`, the lock records verbatim_activation:false, and from then on
// nothing downstream can tell it from a legitimately derive-mode instance. The
// same class of name is already refused twice elsewhere: a template reference
// to `.Content.<name>` (validate.go) and an instance/manifest SOURCE mismatch
// (content/schema.go), and both are blind to this one case.
func TestAnInstanceContentKeyMustNameADeclaredBlock(t *testing.T) {
	m := contentManifest()

	t.Run("the manifest's own block name validates", func(t *testing.T) {
		in := valheimInstance()
		in.Spec.Content = map[string]ContentList{
			"workshop": {ActivationPolicy: ActivationPreserve, Acquire: []ContentRef{{ID: "1"}}},
		}
		if d := ValidateInstance(in, m, ValidatorInput{Instance: "vh-1"}, nil); d.HasErrors() {
			t.Fatalf("the declared block was refused:\n%s", d.Errors())
		}
	})

	t.Run("a misspelled key is refused with a suggestion", func(t *testing.T) {
		in := valheimInstance()
		in.Spec.Content = map[string]ContentList{
			"workshp": {ActivationPolicy: ActivationPreserve, Acquire: []ContentRef{{ID: "1"}}},
		}
		d := ValidateInstance(in, m, ValidatorInput{Instance: "vh-1"}, nil)
		if !d.HasErrors() {
			t.Fatal("a content key no manifest block declares validated; the list it carries " +
				"is never read and its activation_policy is never honoured")
		}
		got := d.Errors().String()
		if !strings.Contains(got, "spec.content.workshp") {
			t.Errorf("the diagnostic does not name the key:\n%s", got)
		}
		if !strings.Contains(got, `"workshop"`) {
			t.Errorf("no \"did you mean\" for a one-character typo:\n%s", got)
		}
		// An operator told only that the key is unknown deletes it, and loses
		// the list they wrote.
		if !strings.Contains(strings.ToLower(got), "rename") {
			t.Errorf("the fix does not tell the operator to RENAME rather than delete:\n%s", got)
		}
	})

	// The absent-key case must stay legal: `gvm create` writes no content
	// block, so "no key at all" is the normal post-create state and
	// `gvm content add` creates the key on its first write.
	t.Run("no content key at all is legal", func(t *testing.T) {
		in := valheimInstance()
		if d := ValidateInstance(in, m, ValidatorInput{Instance: "vh-1"}, nil); d.HasErrors() {
			t.Fatalf("an instance with no content block was refused:\n%s", d.Errors())
		}
	})

	// A broken catalog must not turn into a flood of content diagnostics.
	t.Run("no manifest means no opinion", func(t *testing.T) {
		in := valheimInstance()
		in.Spec.Content = map[string]ContentList{"anything": {}}
		d := ValidateInstance(in, nil, ValidatorInput{Instance: "vh-1"}, nil)
		for _, diag := range d {
			if strings.Contains(diag.Path, "spec.content.anything") {
				t.Errorf("checked a content key with no manifest to check it against: %v", diag)
			}
		}
	})
}

// TestServerBootRefusesKeepMissingFalse: on server_boot the acquisition key is
// the ONLY way bytes ever arrive, and the server is the only thing that can
// fetch them. keep_missing:false drops an unmaterialized id from that key, so a
// fresh instance tells the server to download nothing, so nothing materializes,
// so the next sync renders the same empty line -- a permanent stall out of a
// recoverable state. The Go/YAML zero value is false, so an operator-authored
// /etc/gvm/games/<id>.yaml that simply omits the key lands here, which is why
// this is a load-time refusal and not a runtime surprise.
// TestServerBootRefusesKeepMissingFalse: on server_boot the acquisition key is
// the ONLY way bytes ever arrive, and the server is the only thing that can
// fetch them. keep_missing:false drops an unmaterialized id from that key, so a
// fresh instance tells the server to download nothing, so nothing materializes,
// so the next sync renders the same empty line -- a permanent stall out of a
// recoverable state. The Go/YAML zero value is false, so an operator-authored
// /etc/gvm/games/<id>.yaml that simply omits the key lands here, which is why
// this is a load-time refusal and not a runtime surprise.
func TestServerBootRefusesKeepMissingFalse(t *testing.T) {
	// The acquisition checks only, picked out by path: valheimManifest is not
	// a complete game manifest, so the rest of Validate has plenty to say and
	// none of it is about this.
	contentDiags := func(m *Manifest) Diags {
		var out Diags
		for _, d := range Validate(m, nil) {
			if strings.Contains(d.Path, ".acquisition.") {
				out = append(out, d)
			}
		}
		return out
	}

	m := contentManifest()
	if got := contentDiags(m); len(got) != 0 {
		t.Fatalf("the control manifest already has an acquisition diagnostic: %s", got)
	}

	m.Spec.Content[0].Acquisition.KeepMissing = false
	got := contentDiags(m)
	if len(got) == 0 {
		t.Fatal("keep_missing:false on a server_boot acquisition validated")
	}
	msg := got.String()
	if !strings.Contains(msg, "keep_missing") {
		t.Errorf("the diagnostic does not name the key: %s", msg)
	}
	if !strings.Contains(msg, AcquireServerBoot) {
		t.Errorf("the diagnostic does not name the mechanism: %s", msg)
	}
	if !got.HasErrors() {
		t.Error("the finding is not an error, so `gvm apply` would proceed into the stall")
	}

	// The other mechanisms are gvm's own downloads, where dropping an id whose
	// bytes are not on disk is a legitimate policy.
	m.Spec.Content[0].Acquisition.Mechanism = AcquireSteamCMDItem
	if got := contentDiags(m); len(got) != 0 {
		t.Errorf("keep_missing:false was refused on %s: %s", AcquireSteamCMDItem, got)
	}
}
