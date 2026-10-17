package audit

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// Action is what happened, and it is a closed set.
//
// Closed because the params allowlist below is keyed by it: an action nobody
// declared has no allowlist, so every parameter it carries is dropped. That is
// the fail-closed direction, but it is still a loss of evidence, which is why
// actions_test.go turns "someone added an Action constant and forgot the
// allowlist" into a failing build rather than a quietly impoverished log line.
type Action string

// The action set, transcribed from section 14.8.
//
// The names are `<subject>.<verb>` and go into the `action` text column
// verbatim; an operator greps `action=instance.stop` in journald and in
// audit_log with the same string.
//
// These are NOT permissions. `instance.view` and `console.read` are
// authorization questions and live in internal/authz; the only overlap is
// AuthDenied, which records the answer to one of those questions.
const (
	InstanceStop    Action = "instance.stop"
	InstanceStart   Action = "instance.start"
	InstanceRebuild Action = "instance.rebuild"
	InstanceApply   Action = "instance.apply"

	ConsoleSend Action = "console.send"
	ContentSync Action = "content.sync"

	SecretWrite  Action = "secret.write"
	SecretRotate Action = "secret.rotate"

	ManifestPublish Action = "manifest.publish"
	ManifestUpgrade Action = "manifest.upgrade"

	AdoptDiscover Action = "adopt.discover"
	AdoptCutover  Action = "adopt.cutover"
	AdoptRollback Action = "adopt.rollback"

	BackupCreate  Action = "backup.create"
	BackupRestore Action = "backup.restore"
	BackupDelete  Action = "backup.delete"

	FirewallApply Action = "firewall.apply"
	JobRun        Action = "job.run"

	UserCreate Action = "user.create"
	GrantSet   Action = "grant.set"
	AuthLogin  Action = "auth.login"
	AuthDenied Action = "auth.denied"
)

// Spec is what one action is allowed to record and what it must record.
type Spec struct {
	// Params is the allowlist. A key outside it is dropped, never redacted
	// in place: redaction that has to recognise a secret is a denylist, and
	// a denylist eventually lets the CurseForge key into a table gvm
	// deliberately cannot DELETE from.
	Params []string

	// Required are the params that must be present on the terminal success
	// row. They are checked on Outcome OK only: a `started` row is written
	// before the work that produces most of these values, and an `error`
	// row legitimately has none of them because the step that computes them
	// is the step that failed. The success row is the one a later
	// investigation reads, and it is the one that must be complete.
	Required []string

	// Instance says the record is meaningless without an instance name.
	// A `backup.create` with an empty instance_name cannot be traced back to
	// a world, which defeats the reason it is audited at all.
	Instance bool

	// Target says the record needs its `target` column. For AuthDenied the
	// target is the action that was refused, which is the single most useful
	// field in the row and has nowhere else to go.
	Target bool
}

// allowedParams is section 14.8's table, plus the required-field and
// instance-scope columns the Go side needs.
//
// Redaction is an allowlist, never a denylist. The keys here are the complete
// vocabulary of what gvm will write into audit_log.params, and everything else
// a call site passes is dropped and reported.
var allowedParams = map[Action]Spec{
	// rung is the shutdown-ladder step that actually ended the process. It is
	// required on success because it is the field that converts the bash
	// suite's load-bearing magic -- KillSignal=SIGCONT neutering the terminate
	// step so the screen-stuffed `quit` has time to finish -- from folklore
	// into an observable. A stop that succeeded without saying how is exactly
	// the record the legacy suite already produced.
	InstanceStop: {
		Params:   []string{"reason", "budget_sec", "rung", "degraded"},
		Required: []string{"rung"},
		Instance: true,
	},
	InstanceStart: {
		Params:   []string{"reason", "ready_after_sec"},
		Instance: true,
	},
	// archive_path/sha/bytes are required on success because "archive before
	// destroy" is the invariant this row exists to evidence (section 13.1). A
	// rebuild that reports success without naming the archive it made is
	// indistinguishable from one that destroyed a world with no archive.
	InstanceRebuild: {
		Params: []string{"archive_path", "archive_sha256", "archive_bytes",
			"mod_count", "keep_world"},
		Required: []string{"archive_path", "archive_sha256", "archive_bytes"},
		Instance: true,
	},
	InstanceApply: {
		Params: []string{"actions", "destructive_count", "requires_restart",
			"unmanaged_ufw_rules"},
		Required: []string{"actions"},
		Instance: true,
	},
	ConsoleSend: {
		Params:   []string{"line", "channel", "seq"},
		Instance: true,
	},
	// Both namespaces' before/after sets are separate fields and all four are
	// required: acquisition (WorkshopItems) and activation (Mods) are
	// genuinely different lists, and the bug class this audits is one of them
	// changing while the other did not.
	ContentSync: {
		Params: []string{"list", "added", "removed", "ambiguous", "missing",
			"acquire_before", "acquire_after",
			"activation_before", "activation_after"},
		Required: []string{"acquire_before", "acquire_after",
			"activation_before", "activation_after"},
		Instance: true,
	},
	// Never "value". The scope/name pair says WHICH secret changed, which is
	// the entire question an audit log can answer about a secret.
	SecretWrite: {
		Params:   []string{"scope", "name", "source"},
		Required: []string{"scope", "name"},
	},
	SecretRotate: {
		Params:   []string{"scope", "name", "key_id"},
		Required: []string{"scope", "name"},
	},
	ManifestPublish: {
		Params:   []string{"game_id", "revision", "sha256", "origin_path", "driver_abi"},
		Required: []string{"game_id", "sha256"},
	},
	ManifestUpgrade: {
		Params: []string{"instance", "from_sha256", "to_sha256",
			"from_revision", "to_revision", "fields_changed"},
		Required: []string{"from_sha256", "to_sha256"},
		Instance: true,
	},
	AdoptDiscover: {
		Params: []string{"source", "install", "data", "world_bytes", "ports",
			"content_count", "ambiguous"},
		Instance: true,
	},
	// The fingerprint pair is the evidence that the adopted world that came
	// back up is the same world that went down (section 19). A cutover row
	// without it records that a cutover happened and nothing about whether it
	// was safe.
	AdoptCutover: {
		Params: []string{"downtime_sec", "archive_path",
			"world_fingerprint_before", "world_fingerprint_after",
			"fingerprint_match", "rotated_credentials"},
		Required: []string{"world_fingerprint_before", "world_fingerprint_after",
			"fingerprint_match"},
		Instance: true,
	},
	AdoptRollback: {
		Params:   []string{"reason", "automatic"},
		Required: []string{"reason"},
		Instance: true,
	},
	BackupCreate: {
		Params: []string{"path", "sha256", "bytes", "retention_class",
			"freeze_ms", "snapshot_method"},
		Required: []string{"path", "sha256", "bytes"},
		Instance: true,
	},
	BackupRestore: {
		Params:   []string{"path", "sha256", "target"},
		Required: []string{"path", "sha256"},
		Instance: true,
	},
	// backup.delete is the one explicit removal of an archive, including a
	// preserve one that no retention path may touch. path/sha256/bytes identify
	// exactly which bytes were removed, so the row answers "what did I delete"
	// as precisely as backup.create answered "what did I write".
	BackupDelete: {
		Params:   []string{"path", "sha256", "bytes"},
		Required: []string{"path", "sha256", "bytes"},
		Instance: true,
	},
	// ssh_port is required because section 17.5's promise -- a computed rule
	// set is rejected if it would not admit SSH -- is only auditable if the
	// port that was preserved is written down next to the rules that were
	// applied.
	FirewallApply: {
		Params:   []string{"added", "removed", "unmanaged_kept", "ssh_port"},
		Required: []string{"ssh_port"},
	},
	JobRun: {
		Params:   []string{"kind", "unit", "run_as", "left_stopped"},
		Required: []string{"kind"},
	},
	// force is not in section 14.8's table and is added deliberately: 14.9
	// says creating a second owner requires --force and is "audited with
	// params.force=true", which the table as written would silently drop.
	UserCreate: {
		Params:   []string{"email", "role", "force"},
		Required: []string{"email", "role"},
	},
	GrantSet: {
		Params:   []string{"target_user", "instance", "role", "previous_role"},
		Required: []string{"target_user", "role"},
	},
	AuthLogin: {
		Params: []string{"method", "totp_used"},
	},
	// A denial's outcome is Failed, so Required never fires for it. The two
	// fields are guaranteed instead by Logger.Denied taking them as arguments,
	// which is a stronger guarantee than a runtime check.
	AuthDenied: {
		Params: []string{"permission", "effective_role"},
		Target: true,
	},
}

// reservedPrefix marks the keys audit itself writes into params.
//
// A caller-supplied key starting with it is dropped like any other
// unallowlisted key, which is what stops a call site -- or attacker-influenced
// data that reached a params map -- from forging an "_unallowlisted": [] entry
// that makes a dropped field look like it was never there.
const reservedPrefix = "_"

// KeyUnallowlisted lists the params that were dropped. Section 14.8: an
// unknown key is dropped and recorded, plus a slog.Error. Visible, not silent.
const KeyUnallowlisted = "_unallowlisted"

// KeyInvalid lists the record fields that failed validation. The record is
// still written: a malformed audit record is evidence, and refusing to write
// it would turn a call-site bug into a missing trail.
const KeyInvalid = "_invalid"

// Known reports whether the action has an allowlist entry.
func Known(a Action) bool { _, ok := allowedParams[a]; return ok }

// SpecFor returns the action's declaration.
func SpecFor(a Action) (Spec, bool) { s, ok := allowedParams[a]; return s, ok }

// Actions returns every declared action, sorted.
//
// It is derived from the allowlist rather than from a second hand-maintained
// slice on purpose: a list that has to be updated alongside the map is a list
// that will disagree with the map, and the disagreement would be invisible.
func Actions() []Action {
	out := make([]Action, 0, len(allowedParams))
	for a := range allowedParams {
		out = append(out, a)
	}
	slices.Sort(out)
	return out
}

// maxParamStringBytes bounds one string parameter.
//
// console.send's "line" is operator input relayed into a game whose mods can
// influence what comes back, and job params can carry a steamcmd error. The
// spool fsyncs once per record, so an unbounded string is an unbounded synchronous
// write on the path that must keep working when everything else is broken.
// Truncation is marked in the value, so a reader is never misled about having
// the whole thing.
const maxParamStringBytes = 8 << 10

// Redact applies the allowlist.
//
// It returns the params that may be recorded and, separately, the sorted keys
// it dropped. The dropped list also lands in the returned params under
// KeyUnallowlisted; the second return value exists so the caller can log the
// drop at error level without re-deriving it.
//
// An action with no allowlist at all drops everything: an undeclared action is
// exactly the case where nobody has decided which of its fields are safe.
func Redact(a Action, p Params) (Params, []string) {
	spec, known := allowedParams[a]

	out := make(Params, len(p))
	var dropped []string
	for k, v := range p {
		if !known || strings.HasPrefix(k, reservedPrefix) || !slices.Contains(spec.Params, k) {
			dropped = append(dropped, k)
			continue
		}
		out[k] = truncateParam(v)
	}

	if len(dropped) > 0 {
		slices.Sort(dropped)
		out[KeyUnallowlisted] = dropped
	}
	return out, dropped
}

// truncateParam bounds a string value. Non-string values are structured data
// gvm produced itself -- a mod-id list, a byte count -- and are bounded by
// whatever produced them.
func truncateParam(v any) any {
	s, ok := v.(string)
	if !ok || len(s) <= maxParamStringBytes {
		return v
	}
	cut := maxParamStringBytes
	// Back up to a rune boundary. Cutting a multi-byte rune in half would
	// still marshal -- encoding/json substitutes U+FFFD -- so the damage would
	// be a silently corrupted last character rather than an error.
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("%s… [truncated, %d bytes total]", s[:cut], len(s))
}

// missingRequired returns the required params absent from an otherwise
// redacted set, sorted. Only meaningful for a terminal success row.
func missingRequired(a Action, p Params) []string {
	spec, ok := allowedParams[a]
	if !ok {
		return nil
	}
	var missing []string
	for _, k := range spec.Required {
		if _, present := p[k]; !present {
			missing = append(missing, k)
		}
	}
	slices.Sort(missing)
	return missing
}
