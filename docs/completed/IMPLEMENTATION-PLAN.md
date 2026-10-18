# gvm — Implementation Plan

> **Archived 2026-10-17 — reviewed and closed.** Phases 0–19 are committed with `make check` green (see [`USABILITY-PLAN.md`](../USABILITY-PLAN.md) "Where gvm stands" and the bug sweeps in this directory). The per-phase definition-of-done checkboxes below were never ticked as the phases landed; they do not mark pending work. Foundation surface that is built but not yet wired is tracked by the usability plan (Phases 20–55), and this plan's leftover pending items — the "Definition of done for v1 as a whole" — moved to [`docs/backlog.md`](../backlog.md).

> **Target design; see the [README](../../README.md) for what works today.** This plan describes how Phases 0–19 build the foundation; not every component it lists is wired into the current build. The README "What works in this build" table is the authority on what runs now, and [`USABILITY-PLAN.md`](../USABILITY-PLAN.md) (Phases 20+) is what turns the foundation into a usable product.

Dependency-ordered build plan for the architecture in [`ARCHITECTURE.md`](../ARCHITECTURE.md). Every `D-nn` reference points at that document's decision register.

**Module:** `github.com/S95F/gameVPSmanager` · **Binary:** `gvm` · **Go 1.26, `CGO_ENABLED=0`.**

---

## How to use this plan

Phases are strictly dependency-ordered: phase *N* compiles and its tests pass before phase *N+1* starts. Each phase lists **Goal**, **Depends on**, **Files to create**, and a **Definition of done** that is a checklist of *observable* facts, not a feeling.

Four rules hold in every phase:

1. **`make check` (fmt-check + vet + test) stays green on Windows 11 after every phase.** Anything that cannot is put behind `//go:build linux` with a `//go:build !linux` stub returning `platform.ErrNotSupported`, and the pure half is tested.
2. **New Linux-only surface arrives with its stub in the same commit.** Never "add the stub later".
3. **Every phase that adds a `doctor`-checkable invariant adds the check in that phase**, not in the doctor phase. Phase 12 assembles and formats; it does not invent checks.
4. **No phase weakens a guarantee from an earlier phase.** The build gates (`noshell_test.go`, `routes_test.go`, `scope_ast_test.go`, `nosynth_test.go`, `noexec_test.go`, the `go list -deps` assertions) are added as soon as the package they guard exists, and never skipped.

**Milestones** are the four points where the thing becomes demonstrably useful:

| Milestone | After phase | What works |
|---|---|---|
| **A — a server runs** | 7 | `gvm create` + `gvm apply` + `gvm start` brings up a fresh Valheim instance with a real PTY console and a clean shutdown ladder. No Postgres, no web tier. |
| **B — a server is maintained** | 10 | A scratch PZ instance installs via SteamCMD, syncs workshop mods with the two-list model, backs up nightly, updates without stopping when nothing changed, and rebuilds with archive-before-destroy. |
| **C — the live server is adopted** | 13 | `gvm adopt discover` → `cutover` → the live Project Zomboid world runs under gvm, with rollback proven. **This is the point of the project.** |
| **D — v1** | 16 | Dashboard, RBAC, audit, API, Caddy, firewall — the full v1 scope table. |

---

## Phase 0 — Foundations and repo hygiene

**Goal:** a build that is honest about what exists, plus the credential-hygiene fixes that should not wait.

**Depends on:** nothing.

### Files to create / change

```
cmd/gvm/main.go                     role dispatch on argv[1]; `version` works, everything else
                                    returns "not implemented" with a non-zero exit
internal/cli/root.go                dispatch table (stdlib flag, no cobra)
internal/cli/usage.go
internal/platform/platform.go       ErrNotSupported + the interface surface, no impls yet
docs/ARCHITECTURE.md                ✅ exists
docs/IMPLEMENTATION-PLAN.md         ✅ this file
README.md                           what gvm is, the one rule, the v1 scope table, a link to §19
.github/workflows/ci.yml            make check on windows-latest AND ubuntu-latest
Makefile                            + `deb`, `sqlc`, `sqlc-check`, `test-linux` targets
```

Rename and un-ignore (D-46):

```
legacy/projectzomboid-bash/config.env  ->  config.env.example
.gitignore                             remove line 17: `!legacy/**/config.env`
legacy/projectzomboid-bash/README.md   note the rename
```

### Definition of done

- [ ] `make check` green on Windows and on Ubuntu CI.
- [ ] `go run ./cmd/gvm version` prints the version banner from `internal/version`.
- [ ] `go run ./cmd/gvm gvmd` exits non-zero with `not implemented`.
- [ ] `.gitignore` no longer contains a negation for `*.env`; `git ls-files | grep '\.env$'` returns nothing.
- [ ] `README.md` states the control-plane-dead rule and links to the adopt runbook.
- [ ] CI runs on both OSes and fails on a deliberate `gofmt` violation.

---

## Phase 1 — `internal/spec`: the on-disk truth

**Goal:** the manifest and instance schemas, the closed template grammar, and validation with `file:line` diagnostics. Pure Go, no build tags, no I/O beyond reading files.

**Depends on:** Phase 0.

### Files to create

```
internal/spec/manifest.go           Manifest, GameMeta, GameSpec + every sub-struct (§8.2)
internal/spec/instance.go           Instance, InstanceSpec, ManifestPin, Paths, Resources (§6.2)
internal/spec/host.go               /etc/gvm/gvm.yaml: edge + firewall + defaults (§17.1)
internal/spec/duration.go           Duration YAML codec ("3600s", "5m")
internal/spec/bytes.go              ParseBytes / FormatBytes, systemd 1024-suffixes (§6.6)
internal/spec/nodeindex.go          yaml.Node -> path -> (line, col) index
internal/spec/load.go               Catalog, Origin ranking, sha256, OverrideAuthorizer (§8.7)
internal/spec/save.go               atomic write: tmp in same dir -> fsync -> rename -> fsync(dir) -> .bak
internal/spec/pin.go                LoadPinned / WritePin (D-04, D-17)
internal/spec/diff.go               field-level ManifestDiff, classified argv/destroy/ports/shutdown
internal/spec/render.go             Renderer, Taint, RenderedFile; the CLOSED var + func list (§8.3)
internal/spec/render_guard.go       the parse-tree walker that rejects ALL control flow
internal/spec/when.go               the 7 predicates + Not; exactly-one-set validation
internal/spec/validate.go           Diag/Diags; the 13 ordered checks (§8.6)
internal/spec/validate_instance.go  spec.validators, run at apply time with var VALUES

internal/spec/manifest_test.go       instance_test.go        load_test.go
internal/spec/pin_test.go            render_test.go          render_guard_test.go
internal/spec/when_test.go           validate_test.go        bytes_test.go
internal/spec/testdata/good/*.yaml
internal/spec/testdata/bad/{unknown_hook,validate_on_update,template_if,template_range,
                            template_printf,template_env,undeclared_var,expose_bind_conflict,
                            port_key_collision,missing_sentinel,missing_verify,
                            two_when_predicates,readiness_no_deadline,bad_regex}.yaml
internal/spec/testdata/instances/{pz-main-adopted,valheim-1-new,bad/*}.yaml
```

### Definition of done

- [ ] Every file in `testdata/bad/` produces the **exact** expected diagnostic string including line and column; golden-compared.
- [ ] `render_guard_test.go` rejects `{{if}}`, `{{range}}`, `{{with}}`, `{{template}}`, `{{call}}`, `{{printf}}`, `{{index}}`, `{{env "X"}}` — each with a `file:line`.
- [ ] `TestTaint`: any rendered field expanding `.Secret.*` returns `TaintSecret`, and `RenderedFile.TaintedKeys` names it.
- [ ] `TestWhenExactlyOne`: two predicates set is a validation error naming the line.
- [ ] `ParseBytes`/`FormatBytes` round-trip `8G`, `7168M`, `2097152`, and reject `19` where MiB was meant only by returning the literal byte count (documented).
- [ ] `SaveInstance` writes atomically: a test kills the process between tmp-write and rename (simulated) and the original file is intact.
- [ ] `TestPinIsolation` (the D-17 regression test): mutating a catalog manifest's revision on disk changes nothing about a `LoadPinned` result.
- [ ] `go doc ./internal/spec` lists the closed variable and function surface in `render.go`'s package comment.

---

## Phase 2 — `internal/gamecfg` codecs, capabilities, and the shipped manifests

**Goal:** every config format gvm writes, the computed-capability derivation, sizing, port types — and the six v1 manifests, validated by CI.

**Depends on:** Phase 1.

### Files to create

```
internal/gamecfg/codec.go           Doc + Codec interfaces, registry, KeySyntax (§8.4)
internal/gamecfg/ini.go             ✅ EXISTS — register as "pz_ini", add KeySyntax
internal/gamecfg/sandboxvars.go     ✅ EXISTS — register as "lua_sandboxvars"
internal/gamecfg/properties.go      java_properties: ISO-8859-1, escaping, comment/order preserving
internal/gamecfg/xmlprops.go        xml_property_list: attribute-surgical byte-range edit (7DTD)
internal/gamecfg/uesettings.go      ue_option_settings: quote-aware OptionSettings=( … ) (Palworld)
internal/gamecfg/jsonorder.go       json_path: order-preserving []kv, dotted paths
internal/gamecfg/tomlkv.go          toml_kv: line-oriented; inline tables are a LOAD ERROR
internal/gamecfg/ownedlines.go      owned_lines: gvm owns the whole file (Minecraft jvm args)
internal/gamecfg/*_test.go          one per codec
internal/gamecfg/testdata/{server.properties,serverconfig.xml,palworldsettings.ini,
                           enshrouded.json,servertest.ini}

internal/spec/capability.go         Capability, CapState, Live, Capabilities() (§8.5)
internal/spec/capability_test.go
internal/spec/sizing.go             Sizing.Memory()
internal/spec/sizing_test.go
internal/spec/ports.go              PortSpec flattening -> ports.Requirement

games/embed.go                      //go:embed *.yaml
games/projectzomboid.yaml           §9.1 verbatim
games/valheim.yaml                  §9.2 verbatim
games/palworld.yaml
games/7daystodie.yaml
games/corekeeper.yaml               §9.4 verbatim
games/generic-steam.yaml
internal/spec/manifests_test.go     every games/*.yaml validates clean; id == filename
internal/spec/dialect_sync_test.go  D-07: the manifest enum == rcon.Dialect constants,
                                    projectzomboid.yaml says `simple`, and
                                    `valve_empty_response` is a LOAD-TIME ERROR
internal/cli/game.go                gvm game validate|which|list|hooks
```

### Definition of done

- [ ] Every codec round-trips a real-world sample **byte-for-byte** when nothing is set, and preserves comments, ordering and unknown keys when exactly one key is set.
- [ ] `lua_sandboxvars.Set` on an unknown key returns `ErrKeyNotFound` (never invents).
- [ ] `toml_kv` returns a load-time error for a key resolving into an inline table.
- [ ] `dialect_sync_test.go` passes and **fails** if `projectzomboid.yaml` is changed to `source`.
- [ ] `manifests_test.go`: all six manifests validate with zero errors.
- [ ] `capability_test.go`: Valheim reports `players.Supported == false`; PZ reports `control.Supported && !Available` with a non-empty `Reason` while `state == activating`.
- [ ] `Sizing.Memory` for PZ (`jvm_xmx=5g`, `native_headroom=3G`, `high_margin=1G`) yields exactly `MemoryHigh=7G`, `MemoryMax=8G`.
- [ ] `gvm game validate games/projectzomboid.yaml` prints the §9.5 shape with 0 errors.

---

## Phase 3 — Platform seam, instance primitives, secrets files, audit sink

**Goal:** everything Linux-specific behind an interface, the compile-time destroy jail, secret files, and an audit sink that works with no database.

**Depends on:** Phase 1.

### Files to create

```
internal/platform/linux.go / stub.go        Statfs, Flock, EphemeralRange, UserExists, GroupList
internal/platform/proc_{linux,stub}.go      StartTicks (field-22 trap), Alive, SignalGroup, CgroupProcs
internal/platform/notify_{linux,stub}.go    hand-rolled sd_notify (§11.5)
internal/platform/peer_{linux,stub}.go      SO_PEERCRED
internal/platform/flock_{linux,stub}.go
internal/platform/*_test.go

internal/instance/name.go / name_test.go    D-45 regex, reserved words, UnitName, JobUnit, SplitJobSlug
internal/instance/sizing.go / _test.go      ComputeResources (§6.6)
internal/instance/paths.go
internal/instance/jail.go / jail_test.go    Jailed (unexported fields), Jail, Rebuild, Resolve (§10.1)
internal/instance/destroy.go                Destroy(ctx, *archive.Ref, []Jailed)  [stub until Phase 5]

internal/secrets/value.go / value_test.go   secrets.Value: String/GoString/LogValue/MarshalJSON = ••••
internal/secrets/refs.go                    Ref{Scope,Key,Name}
internal/secrets/filestore.go               /etc/gvm/secrets layout, atomic 0600 writes (D-02)
internal/secrets/filestore_{linux,stub}.go  chown/mode enforcement
internal/secrets/generate.go / _test.go     base62 rejection sampling; excludes ; = newline and
                                            shell metacharacters (§14.7)

internal/audit/audit.go                     Record, Sink, Start/Record/Denied
internal/audit/actions.go / _test.go        the per-action ALLOWLIST (§14.8)
internal/audit/spool.go / _test.go          NDJSON, O_APPEND + fsync, spool_id
internal/audit/slogsink.go                  the journald mirror; the only sink until Phase 14
```

### Definition of done

- [ ] `jail_test.go` rejects: `..` surviving Clean, a symlink escaping the root, a path at/under a protected world path, an absolute path outside every allow root, and a resolved target on a different device.
- [ ] `instance.Destroy` does not compile when passed a `string`; a compile-fail fixture documents this.
- [ ] `TestSecretValueNeverFormats`: `%v`, `%s`, `%+v`, `slog.Info`, and `json.Marshal` all yield `••••`; only `Reveal()` yields plaintext, and `grep -rn '\.Reveal()' internal/` returns ≤3 call sites (asserted by a test once those sites exist).
- [ ] `secrets.Load` refuses a group- or world-readable master key.
- [ ] `GeneratePassword(20)` never emits `;`, `=`, `\n`, `"`, `\`, `` ` ``, `$` — property-tested over 10⁵ samples.
- [ ] `StartTicks` parses a `/proc/<pid>/stat` fixture whose comm is `(Web Content)` — the parens-and-spaces trap.
- [ ] `TestEveryActionHasAnAllowlist` passes over the current action set.
- [ ] Spool replay is idempotent: replaying the same file twice yields zero duplicates (tested against an in-memory sink).
- [ ] `make check` green on Windows: every `*_linux.go` has a stub and no `x/sys/unix` import leaks into a cross-platform file (asserted by a `go list -deps` test).

---

## Phase 4 — The runner: console, socket, readiness, shutdown ladder

**Goal:** `gvm run <name>` supervises a process on a PTY, streams a ring buffer, serves the console socket, drives `Type=notify` readiness, and executes the shutdown ladder — the single most safety-critical component.

**Depends on:** Phases 1–3.

### Files to create

```
internal/console/line.go  ring.go  ring_test.go
internal/console/ingest.go  ingest_test.go  ansi.go  ansi_test.go
internal/console/logfile.go  logfile_test.go          rotation + gzip, no pump stall
internal/console/protocol.go  protocol_test.go        the NDJSON op/event codec
internal/console/fake.go                              in-memory runner for tests

internal/socket/codec.go  codec_test.go
internal/socket/server.go  server_linux.go            peer-cred authorization tiers (D-09/D-10)
internal/socket/client.go

internal/runner/runner.go  config.go  preflight.go  preflight_test.go
internal/runner/process.go  process_{linux,stub}.go   the Process seam
internal/runner/spawn_linux.go  spawn_stub.go         setsid, pgid, PTY/pipe assignment
internal/runner/pty_linux.go                          /dev/ptmx + TIOCSPTLCK/TIOCGPTN/termios
internal/runner/pipe_linux.go
internal/runner/pump_{linux,stub}.go                  the EIO + close-the-slave traps
internal/runner/procgroup_linux.go
internal/runner/readiness.go  readiness_test.go       extendLoop, stall detector, hard deadline
internal/runner/shutdown.go   shutdown_test.go        the ladder + Report
internal/runner/shutdown_standalone.go                gvm run-shutdown, no ring, no socket
internal/runner/status.go  status_test.go             atomic status.json
internal/runner/report.go                             shutdown.jsonl append
internal/runner/screencompat.go                       console_compat: screen
internal/runner/noshell_test.go

internal/control/control.go  noop.go
internal/control/commander.go  commander_test.go
internal/control/args.go  args_test.go
internal/control/rconctl/rconctl.go  dialect.go  rconctl_test.go   wraps internal/rcon
internal/control/stdinctl/stdinctl.go

internal/probe/{tcp,udp,logmarker}.go  + tests
internal/cli/run.go  console.go  runprepare.go  control.go
```

### Definition of done

- [ ] `ring_test.go`: a subscriber that never reads is marked lagged and **the producer never blocks** — asserted with a 200 ms budget; a second subscriber is unaffected.
- [ ] `ingest_test.go`: CRLF normalisation, bare-`\r` progress bars, ANSI CSI+OSC stripping, partial-line flush after 250 ms with the completed line superseding, and a 40 MB line force-split rather than buffered.
- [ ] `shutdown_test.go` against `fakeProcess`, all on Windows:
  - `save` → marker → `quit` → exit ⇒ `FinalRung == RungConsole`, `Degraded == false`;
  - no save marker ⇒ the ladder still sends `quit` (`on_timeout: continue`);
  - process never exits ⇒ SIGTERM then SIGKILL, `Degraded == true`, both rungs in `Steps`;
  - a `via: control` step returning `io.EOF` on the stop command is **not** an error;
  - `wait_for_log` cannot be satisfied by a line that predates the send.
- [ ] `readiness_test.go`: `EXTEND_TIMEOUT_USEC` is emitted while a fake clock sees output; extension **stops** after `stall_timeout` of silence; `hard_deadline` is a hard wall; a `fatal` fail marker aborts within one poll interval with its `hint` in the error.
- [ ] `commander_test.go`: Valheim ⇒ `players.Supported == false`; PZ before ready ⇒ `Supported && !Available` with the manifest's reason verbatim; routing prefers control when ready, console otherwise.
- [ ] `args_test.go` rejects `"`, `\`, `` ` ``, `$`, `\n`, `\r`, `\x00`, and a 513-char string; accepts a normal player name and a UTF-8 message.
- [ ] `rconctl_test.go`: no dial while the gate is closed; a protocol error closes and redials; `ErrAuthFailed` sets a 5-minute backoff; the manifest dialect table round-trips every `rcon.Dialect`.
- [ ] `noshell_test.go` green.
- [ ] **Linux CI:** `pty_linux_test.go` (no echo doubling, `EIO` treated as EOF, full crash tail from a child that writes 200 lines then `_exit(1)`); `procgroup_linux_test.go` (a shell that forks **without exec** — `kill(-pgid)` reaches both, `kill(MainPID)` reaches only the shell); `peer_linux_test.go` (the D-09 authorization tiers).
- [ ] Manual: `gvm run` against a throwaway `sh -c 'while true; do echo tick; sleep 1; done'` on a Linux box — `gvm console` attaches, `Ctrl-D` detaches, SIGTERM runs the ladder and blocks.

---

## Phase 5 — Archive and probes

**Goal:** the single destructive chokepoint, with a self-describing tarball that makes DB-free restore real.

**Depends on:** Phases 1, 3.

### Files to create

```
internal/archive/archive.go  manifest.go              the .gvm/manifest.json schema (§13.1)
internal/archive/create.go                            pgzip, roots+prefixes, per-member sha256
internal/archive/verify.go                            shallow | spot(N) | deep
internal/archive/restore.go                           stream-verify then rename; manifest-drift check
internal/archive/destroy.go                           PANICS on a nil or unverified Ref
internal/archive/fingerprint.go                       Contains() = superset + advancing mtime (D-34)
internal/archive/index.go                             archives.index.json + Reindex
internal/archive/retention.go                         routine | preserve; prune AFTER success only
internal/archive/*_test.go
internal/archive/testdata/legacy-pz-backup.tar.gz     a bash-suite tarball with no .gvm/manifest.json

internal/probe/a2s.go  mcping.go  http.go  + tests
internal/instance/destroy.go                          completed (was stubbed in Phase 3)
internal/instance/destroy_test.go
```

### Definition of done

- [ ] `TestArchiveRoundTrip`: create → read manifest by streaming only the first 8 KB → verify deep → restore into a scratch dir → per-member sha256 matches, modes and mtimes preserved.
- [ ] `TestDestroyRequiresArchiveRef`: `Destroy(ctx, nil, …)` panics; `Destroy` with `VerifiedAt.IsZero()` panics.
- [ ] `TestPruneNeverTouchesPreserve`: `preserve` survives `keep: 1` and `--force`.
- [ ] `TestPruneOnlyAfterSuccess`: a failing create leaves the existing set untouched.
- [ ] `TestFingerprintSupersetSemantics`: a normal stop/start (files added, mtimes advanced) passes; a regenerated empty world fails and names the missing paths.
- [ ] `TestRestoreAbortsBeforeRename` on a corrupted member.
- [ ] `TestLegacyArchive`: a bash-suite tarball with no `.gvm/manifest.json` restores with reduced verification and a clear warning.
- [ ] `gvm restore --list --from <dir>` works with no database configured at all.

---

## Phase 6 — hostops, port allocation, and the reconciler

**Goal:** `gvm apply` — the single write path. Renders systemd drop-ins, timers, users and directories; write-probes the sandbox before committing.

**Depends on:** Phases 1–3, 5.

### Files to create

```
internal/ports/ports.go  occupancy.go  allocate.go  allocate_test.go
internal/ports/conflicts.go  conflicts_test.go
internal/ports/ledger.go  ledger_test.go              ports.lock.json under flock
internal/ports/procnet.go  procnet_test.go            PURE parser, takes io.Reader
internal/ports/listeners_{linux,stub}.go              inode -> pid -> comm, no `ss`
internal/ports/testdata/proc_net_{tcp,tcp6,udp,udp6}

internal/hostops/verbs.go                             the closed verb enum + verbs_test.go
internal/hostops/ops.go                               THE single implementation
internal/hostops/systemd_{linux,stub}.go              go-systemd/v22/dbus
internal/hostops/user_{linux,stub}.go                 useradd/groupadd
internal/hostops/fs.go                                atomic writes, chown, statfs

internal/reconcile/plan.go                            Action{Describe,Destructive,Do(ctx,*archive.Ref)}
internal/reconcile/apply.go                           plan -> confirm -> execute -> verify
internal/reconcile/diff.go  diff_test.go              unified diff, secret lines masked by taint
internal/reconcile/units.go  units_test.go            RenderDropIn (§7.4)
internal/reconcile/timers.go  timers_test.go          RenderTimer + the RestartUnit subtlety (§7.5)
internal/reconcile/users.go  dirs.go
internal/reconcile/probe_{linux,stub}.go              ProbeWritable (D-15)
internal/reconcile/templates/{dropin,timer,adopt-guard}.tmpl
internal/reconcile/testdata/golden/{pz-main-adopted,valheim-1-new,gmod-1-i386}.10-gvm.conf
internal/reconcile/testdata/golden/pz-main_nightly.timer
internal/cli/probewrite.go                            gvm probe-write, NDJSON results
packaging/tmpfiles/gvm.conf
packaging/systemd/{gvm@.service,gvm-job@.service,gvm.slice,gvm-jobs.slice}
```

### Definition of done

- [ ] `allocate_test.go`: PZ instance 1 → 16261/16262/27015, instance 2 → 16271/16272/27016 (groups shift by their own strides); a pin fixes the group at k=0; the ephemeral range is never entered; exhaustion names the blocking claim for every rejected window.
- [ ] `procnet_test.go`: TCP counts only state `0A`; UDP counts every bound entry; runs on Windows against the fixtures.
- [ ] `units_test.go` golden files match byte-for-byte, and specifically assert:
  - the adopted PZ drop-in contains `ProtectHome=no` and `ReadWritePaths=/opt/pzserver /home/pzserver/Zomboid …`;
  - `MemoryDenyWriteExecute=no` with its inline comment when `jit: true`;
  - `SystemCallArchitectures=native x86` when `arch: i386`;
  - `TimeoutStartSec` equals `readiness.hard_deadline` and `TimeoutStopSec` equals `shutdown.budget`.
- [ ] `timers_test.go`: a changed `OnCalendar` produces `RestartUnit`, an unchanged one produces `StartUnit`.
- [ ] `diff_test.go`: a rendered `.Secret.` line appears as `Password=••••••` in the diff.
- [ ] The `Action` interface does not compile when a `Destructive()` implementation ignores its `*archive.Ref`.
- [ ] `noshell_test.go` covers `internal/{ports,reconcile,hostops}`.
- [ ] **Linux CI:** `ProbeWritable` succeeds for `/home`-rooted paths with `ProtectHome=no` and **fails** with `ProtectHome=read-only`, proving the check is real.

---

## Phase 7 — Lifecycle and the root CLI · **Milestone A**

**Goal:** a fresh instance runs end to end from the command line, with no Postgres and no web tier.

**Depends on:** Phases 4–6.

### Files to create

```
internal/lifecycle/phase.go  observe.go  derive.go  derive_test.go     (the pure phase table)
internal/lifecycle/lease.go  lease_test.go
internal/lifecycle/reconcile.go  reconcile_test.go                     Sweep/Watch; NEVER actuates
internal/lifecycle/verbs.go  manager.go  status.go
internal/lifecycle/noshell_test.go

internal/cli/transport.go  transport_test.go       Resolve(): LocalRoot | Remote | Console
internal/cli/create.go  install.go  start.go  stop.go  restart.go
internal/cli/status.go  list.go  logs.go  destroy.go  apply.go  diff.go
internal/cli/ports.go  instance.go
```

### Definition of done

- [ ] `derive_test.go` is table-driven over every `Observation` shape and runs on Windows: a live lease ⇒ `updating`/`installing`; a **stale** lease ⇒ `failed` with the `gvm job clear` hint and **never** auto-resume; `!PinOK` ⇒ `failed` with the `manifest upgrade --dry` hint; `LoadState=not-found` + no `applied_sha256` ⇒ `declared`; `active` + no `status.json` ⇒ `degraded`.
- [ ] `Reconciler.Sweep` reads the instance list from `/etc/gvm/instances/*.yaml` and **never** from Postgres; a DB error logs a warning and the sweep continues.
- [ ] `TestReconcilerNeverActuates`: a fake systemd records zero start/stop calls across a full sweep with drift present.
- [ ] `gvm create valheim-1 --game valheim --var public_name=Test` writes a valid instance YAML with a UUIDv7 `metadata.uid` and a materialised port set, and touches nothing else.
- [ ] `gvm apply --diff` prints the §17.6-shaped plan; `gvm apply` converges; a second `gvm apply` reports **zero** actions (idempotence).
- [ ] **On a Debian 12 VM:** `gvm start valheim-1` → `Type=notify` READY on the log marker → `gvm status` shows `running` with the game pid and starttime → `gvm console valheim-1` attaches → `gvm stop` runs the SIGINT ladder and `shutdown.jsonl` records `FinalRung=signal, Degraded=false`.
- [ ] `systemctl restart gvmd` is a no-op for the running instance (gvmd is still a stub, but the unit relationship is already absent).
- [ ] **Milestone A demonstrated and recorded** (asciinema or a transcript in the PR).

---

## Phase 8 — Providers and the blob store

**Goal:** SteamCMD installs and updates safely, as the instance user, with mandatory post-verification.

**Depends on:** Phase 7.

### Files to create

```
internal/provider/provider.go  jailed.go                Provider, Ctx, Plan, Intent, Inspection
internal/provider/blobs.go  blobs_test.go               Get() only on digest match
internal/provider/plan_authorize_test.go                every provider refuses destructive-on-adopted
internal/provider/steamcmd/steamcmd.go  runscript.go
internal/provider/steamcmd/classify.go  classify_test.go   the hex/Error! ladder (§13.5)
internal/provider/steamcmd/acf.go  acf_test.go           Valve KeyValues parser
internal/provider/steamcmd/verify.go  verify_test.go      D-32: StateFlags, byte pairs, single ACF, sentinel
internal/provider/steamcmd/buildid.go                     appinfo.vdf purge + app_info_print
internal/provider/steamcmd/shims.go                       sdk64/sdk32 symlink, SteamAppId, home layout
internal/provider/steamcmd/inspect.go  fake.go
internal/provider/adopted/adopted.go                      Plan() always empty + non-destructive
internal/provider/none/none.go
internal/provider/steamcmd/testdata/{appmanifest_ok,appmanifest_1062,appmanifest_bytes_mismatch}.acf
internal/provider/steamcmd/testdata/steamcmd_{ok,0x602,0x202,no_subscription,2fa}.stdout

internal/jobs/sandbox_{linux,stub}.go                     RunSandboxed: transient SERVICE (D-16)
internal/hook/hook.go  registry.go  noexec_test.go  registry_test.go
internal/hook/steam.go  pal.go  sdtd.go
```

### Definition of done

- [ ] `verify_test.go`: `StateFlags != 4`, a `BytesDownloaded != BytesToDownload` mismatch, a `BytesStaged` mismatch, zero `.acf` files and two `.acf` files each fail with a distinct error.
- [ ] `classify_test.go` over the golden stdout fixtures: hard-fail vs retry vs in-progress classified correctly, including a run that exits **0** after `Error! … 0x602`.
- [ ] `plan_authorize_test.go`: a destructive plan against `managed_install:false` is refused for **every** registered provider — a new provider that forgets cannot pass.
- [ ] `blobs_test.go`: `Get` on a digest mismatch returns a terminal error and **no path**; `Put` with a mismatched digest stores nothing.
- [ ] `noexec_test.go` green: `internal/hook` contains no `os/exec`, `sh -c`, `bash -c`, `syscall.Exec`.
- [ ] `registry_test.go`: every hook named in every shipped manifest resolves and declares the phase it is used in.
- [ ] **Linux CI:** `RunSandboxed` launches a transient **service** (not a scope) with the declared properties, verified by `systemctl show`; `--uid` is the instance user; `RuntimeMaxSec` is set.
- [ ] **On a VM:** `gvm install valheim-1` performs a real SteamCMD install as `gvm-valheim-1`, post-verifies, and the sentinel is present and executable.

---

## Phase 9 — Content: the two-list model · **Milestone B (part 1)**

**Goal:** Steam Workshop content with acquisition/activation namespaces, PZ's resolver, and a lockfile that is intent.

**Depends on:** Phase 8.

### Files to create

```
internal/content/content.go  digest.go  source.go  schema.go
internal/content/lock.go  lock_test.go                 /etc/gvm/instances/<n>.lock.json (D-03)
internal/content/plan.go  plan_test.go                 Unaccounted = fail-closed
internal/content/order.go  order_test.go               CheckOrder; never reorders on its own
internal/content/resolver.go  resolver_pz.go           the CLOSED resolver registry (D-21)
internal/content/blobstore/store.go  store_test.go
internal/content/jail/jail.go  jail_test.go
internal/content/fetch/{client,policy,ratelimit,retry}.go  client_test.go
internal/content/steamworkshop/source.go  observe.go
internal/content/steamworkshop/api.go  api_test.go       keyless GetPublishedFileDetails
internal/content/steamworkshop/collection.go  collection_html.go  collection_test.go
internal/content/steamworkshop/testdata/{getpublishedfiledetails,getcollectiondetails}.json
internal/content/steamworkshop/testdata/{collection.html,collection-broken-pairing.html}
internal/content/testdata/workshop/                      fstest.MapFS item trees
internal/content/testdata/lock/pz-main.golden.json

internal/pz/tags.go  tags_test.go                        Steam tags[] as EVIDENCE only (§12.5)
internal/pz/resolve.go                                   extend: buildScore(target) parameterised
internal/hook/pz.go                                      first_boot, apply_sandbox, resolve_mod_ids
internal/cli/content.go  content_collection.go  content_blocked.go  content_diff.go
```

### Definition of done

- [ ] `internal/pz` tests still pass unchanged, including `live_test.go` against the real 33-item `mods.txt` (byte-identical round trip).
- [ ] `tags_test.go` covers the full §12.5 table, and specifically: **tags never drop an item**; an unreachable API degrades to exactly today's heuristic; a `modinfo:42` vs `tags:Build 41` conflict is recorded, not acted on.
- [ ] `plan_test.go`: a non-empty `Unaccounted` makes `Materialize` and `Activate` refuse.
- [ ] `collection_test.go`: the API path cross-checks every child, rejects a wrong `consumer_app_id`, refuses on `result==9`; the HTML fallback keeps the raw-count cross-check and **refuses to rewrite** on the mutated fixture.
- [ ] `order_test.go`: a `requires:` violation is reported with both ids and their ordinals, and nothing is reordered automatically.
- [ ] `lock_test.go`: `verbatim_activation: true` makes `Activate` return the recorded line untouched and refuse to re-derive.
- [ ] `client_test.go`: a redirect to an off-allowlist host is an **error**, not a follow.
- [ ] `gvm content diff` produces the §12.9 three-way output against a fixture instance.
- [ ] `gvm content why <activation-id>` explains an ambiguous item with its evidence.

---

## Phase 10 — Jobs, backups, updates, rebuild · **Milestone B**

**Goal:** the nightly cycle, and the invariant that a failure never leaves the server stopped.

**Depends on:** Phases 5, 7, 8, 9.

### Files to create

```
internal/jobs/jobs.go  execute.go  slug.go  slug_test.go
internal/jobs/guard.go  guard_test.go                    RunningGuard (§13.4)
internal/jobs/lock.go  lock_{linux,stub}.go              flock; holder line names pid:unit:kind:since
internal/jobs/runner_{backup,update,restart,content,guard,prune,verify}.go
internal/jobs/noshell_test.go

internal/backup/engine.go  engine_test.go
internal/backup/freeze.go  lease.go  lease_test.go       the out-of-process watchdog (D-31)
internal/backup/snapshot_{linux,stub}.go                 reflink | btrfs | direct
internal/backup/remote.go  remote_sftp.go  remote_age.go

internal/update/pipeline.go  pipeline_test.go
internal/update/decide.go                                decide BEFORE you stop
internal/update/settle.go  settle_test.go                quiescence replaces sleep 180 (D-33)
internal/update/repair.go                                the ONLY path that emits `validate`

internal/rebuild/rebuild.go  rebuild_test.go

internal/cli/{job,backup,restore,update,rebuild}.go
packaging/systemd/{gvm-guard.timer,gvm-job@_guard.service,
                   gvm-job@_verify.service,gvm-job@_verify.timer}
```

### Definition of done

- [ ] **`TestNeverLeavesServerStopped`** injects a failure at every seam of every job kind (~40 injection points, table-driven, on Windows against fakes) and asserts: if the instance was active at entry it is active at exit, or `left_stopped=true` is recorded and a breadcrumb is written.
- [ ] `lease_test.go`: kill the lease owner; a fake runner honouring the lease thaws at the deadline; `Sweep()` on runner start thaws an already-expired lease.
- [ ] `settle_test.go`: returns on the positive signal (all ids present) *or* on quiescence; names missing ids and marks the job `degraded`.
- [ ] `pipeline_test.go`: with no build diff and no content diff, the pipeline returns `skipped` and the fake systemd records **zero** stop calls (D-43).
- [ ] `pipeline_test.go`: an adopted instance with a build diff records `skipped` + `update_available` and never stops.
- [ ] `rebuild_test.go`: every early exit before the archive step leaves the world untouched and says so; a failed archive verification aborts **before** any delete.
- [ ] `slug_test.go`: `pz-main_nightly` splits unambiguously; `_guard` and `_verify` are host-level.
- [ ] **On a VM:** a scratch PZ instance installs, syncs 33 workshop items through the download-cycle, produces a `preserve` archive on rebuild, and a nightly `update` run on an unchanged server reports `skipped` in under 30 s with the world untouched.
- [ ] **Milestone B demonstrated.**

---

## Phase 11 — Firewall and the network edge

**Goal:** ufw rules derived from files with tagged ownership and an SSH guard that cannot be bypassed; Caddy rendered, validated, reloaded and rolled back.

**Depends on:** Phases 1, 6.

### Files to create

```
internal/sshport/detect.go  detect_test.go              the 6-rung ladder (§17.5)
internal/sshport/env_{linux,stub}.go
internal/sshport/testdata/{sshd_config,sshd_config.d/50-cloud-init.conf,ssh-socket-listen.txt}
internal/listeners/listeners.go  listeners_test.go  linux.go  stub.go

internal/firewall/{rule,ruleset,tag}.go  rule_test.go  tag_test.go
internal/firewall/desired.go  desired_test.go
internal/firewall/reconcile.go  reconcile_test.go
internal/firewall/guard.go  guard_test.go
internal/firewall/{plan,diff,backend,none,fake}.go  diff_test.go
internal/firewall/ufw/parse.go  parse_test.go  defaults.go
internal/firewall/ufw/ufw_{linux,stub}.go
internal/firewall/ufw/testdata/status/{active-v4v6,active-no-ipv6,inactive,
        comments-with-hash,ranges-and-cidrs,fail2ban-bans,corrupt-untrusted}.txt
internal/firewall/nft/nft.go                            v1.1 stub: Available() = false + reason

internal/edge/{edge,mode,tls,socket,snippet}.go  + tests
internal/caddyconf/render.go  render_test.go  templates/{Caddyfile,site}.tmpl
internal/caddyconf/{validate,reload,rollback,applier}.go  applier_test.go
internal/caddyconf/admin.go  modules.go  version.go  version_test.go  ca.go
internal/caddyconf/runner_{linux,stub}.go  fake.go  install_{linux,stub}.go
internal/caddyconf/testdata/golden/{acme-http,acme-dns,internal-bareip,snippet-import}.Caddyfile
internal/reconcile/{firewall,caddy}.go                  the Action adapters
internal/cli/{firewall,caddy,ca,web}.go
packaging/caddy/10-gvm.conf  packaging/fail2ban/gvm-sshd.local
```

### Definition of done

- [ ] `parse_test.go` over the whole golden corpus, including the corrupted file setting `Trusted: false`.
- [ ] `guard_test.go`: a table where **every row is a way to lock yourself out**; each returns `ErrWouldLockOut`, and there is no flag that bypasses it.
- [ ] `detect_test.go`: the Ubuntu 24.04 trap — `ssh.socket` on 2222 with `sshd_config` still saying 22 yields `Ports: [22, 2222]`; three `Port` lines all appear; no evidence at all yields `Confident: false`.
- [ ] `reconcile_test.go`: adds before deletes; SSH first; **deletion suppressed entirely** when `!LiveParseTrusted`; a gvm-tagged SSH rule is never deleted; a foreign rule in the exact slot becomes an `Adoption`, not an add or a delete; fail2ban ban shapes are excluded from shadow warnings.
- [ ] `render_test.go` golden Caddyfiles: `encode`'s matcher never includes `text/*`; `/stream/*` sets `flush_interval -1` and `stream_close_delay 5m`; the admin block is a unix socket with `enforce_origin`; `trusted_proxies` is absent when unconfigured.
- [ ] `version_test.go` refuses Caddy < 2.11.1 with the upgrade command.
- [ ] `edge.Validate` errors in `proxy_external` mode when `trusted_origins` is empty (the nginx Host-rewrite hazard).
- [ ] **Linux CI, `--cap-add NET_ADMIN`:** apply → read back → converge; apply again → **zero** steps; a lockout attempt refuses and leaves `ufw status` byte-identical; an invalid Caddyfile is caught by `validate` with the live file untouched; a valid-but-unroutable one restores `.bak`.
- [ ] `gvm apply` on a host with ufw **inactive** stages rules and prints the `gvm firewall enable` instruction, and does **not** enable it (D-41).

---

## Phase 12 — `gvm doctor` and packaging

**Goal:** every invariant asserted so far becomes an executable check, and the `.deb` installs cleanly on all three targets.

**Depends on:** Phases 1–11.

### Files to create

```
internal/lifecycle/doctor.go  doctor_test.go            the check registry: HOST-*, EDGE-*, FW-*, INST-*
internal/cli/doctor.go                                  severity split: problem / warning / note
packaging/nfpm.yaml  postinstall.sh  prerm.sh
packaging/systemd/{gvmd.service,gvm-web.service,gvm-db-migrate.service,
                   gvm-spool.service,gvm-spool.timer}
packaging/postgres/{pg_hba.gvm.conf,pg_ident.gvm.conf}
packaging/content/excludes.default.json
docs/{adopt-projectzomboid.md,control-channel.md,content-model.md,network-edge.md,
      tls-cases.md,backups-and-restore.md,what-happens-if-postgres-is-down.md,THREAT-MODEL.md}
ci/golden-units.sh
```

### Definition of done

- [ ] Every check in §20 exists, has an id, and is unit-tested against a fixture that makes it fail.
- [ ] `doctor` distinguishes problems (`✗`), warnings (`!`) and notes (`·`), and a clean host reports **0 problems** — so a permanently non-zero count never trains anyone to ignore it.
- [ ] `INST-002` prints the `config.env` OOM sentence **verbatim** when the memory check fails.
- [ ] `doctor` works with Postgres stopped and says so as its first line.
- [ ] `make deb` produces a package that installs on Debian 12, Ubuntu 22.04 and Ubuntu 24.04 in a container; `postinstall.sh` is idempotent (run it twice).
- [ ] tmpfiles creates `/run/gvm` `0755 root:root` and `/run/gvm/web` `2750 gvm:caddy`; **no unit declares `RuntimeDirectory=gvm`** (D-05), asserted by `golden-units.sh`.
- [ ] `TestNoUnitDependsOnPostgres` parses `gvm@.service` and `gvm-job@.service` and fails on any `postgresql` mention.
- [ ] `docs/THREAT-MODEL.md` exists and every adversary row maps to a doctor check id.

---

## Phase 13 — ADOPT · **Milestone C**

**Goal:** the live Project Zomboid server runs under gvm, with rollback proven before it is needed.

**Depends on:** Phases 5, 7–12.

### Files to create

```
internal/adopt/adopt.go  report.go
internal/adopt/discover.go  discover_pz.go  discover_test.go
internal/adopt/configenv.go  configenv_test.go        PARSED, never sourced
internal/adopt/proc_{linux,stub}.go                    shell-vs-JVM pid pinning
internal/adopt/cutover.go  cutover_test.go
internal/adopt/rollback.go  rollback_script.go
internal/adopt/finalize.go
internal/cli/adopt.go  adopt_pz.go
internal/adopt/testdata/live-pz/…                      the §21.4 fixture tree
internal/adopt/testdata/golden/discover-pz-main.txt
docs/adopt-projectzomboid.md                           §19.6 standalone
```

### Definition of done

- [ ] `TestAdoptConfigEnvNeverExecutes`: a `config.env` containing `$(rm -rf /)` and backticks is parsed inertly; the value is returned as a literal string.
- [ ] `discover_test.go` against the fixture reproduces `testdata/golden/discover-pz-main.txt` **exactly**, including the 32-vs-33 `Mods=` finding, the ambiguous `2866258937`, the shell-vs-JVM pid note, and the cleartext-credentials finding.
- [ ] `TestDiscoverIsReadOnly`: discovery runs against a read-only filesystem fixture and issues zero state-changing calls (asserted by a recording fake for systemd, ufw and the process API).
- [ ] `TestCutoverRollsBackOnEveryPostStep6Failure`: injecting a failure at each of steps 6–11 triggers automatic rollback; the legacy unit is re-enabled and the instance is marked degraded and held.
- [ ] The rollback script is written and its path printed **before** step 6, and it depends only on `systemctl` and `tar` (asserted by grepping the generated file).
- [ ] `TestWorldFingerprintSupersetSemantics`: a normal cutover passes; a simulated fresh-world generation fails and names missing paths.
- [ ] Ports come from the live `.ini`, never from manifest defaults (test asserts a non-default port survives).
- [ ] `finalize` refuses within 7 days without `--force`, and copies the legacy units to `/var/lib/gvm/adopt/<name>-legacy-units/` before removing them.
- [ ] **On a staging clone of the live box** (a VM restored from a real backup): full discover → cutover → verify → rollback → cutover again. Downtime measured and recorded.
- [ ] **On the live box:** the §19.6 runbook executed through step 11 (force a backup and verify it). Steps 12–16 follow on their own schedule.
- [ ] **Milestone C demonstrated.**

> **Rehearse on a clone first.** Phase 13 is the only phase where "it worked in CI" is not sufficient evidence.

---

## Phase 14 — The datastore

**Goal:** Postgres as logbook and people-registry, with three roles, column grants, and a mirror that can never block an apply.

**Depends on:** Phase 12 (packaging installs Postgres and the hba/ident snippets).

### Files to create

```
internal/store/store.go  open.go  health.go  errors.go  reap.go  hostlock.go
internal/store/migrate.go  migrate_test.go
internal/store/mirror.go  mirror_test.go
internal/store/migrations/0001_types.sql … 0010_grants.sql          (§14.4, verbatim)
internal/store/queries/ops/{authz,users,tokens,mirror,manifests,content,jobs,
                            backups,secrets,audit,status,shutdown_reports,netedge}.sql
internal/store/queries/web/{auth,instances,content,jobs,audit,plans}.sql
internal/store/dbgen/{opsq,webq}/                                   sqlc output, checked in
internal/store/grants_linux_test.go
internal/audit/pgsink.go  replay.go  replay_test.go
internal/cli/db.go  audit.go
sqlc.yaml
```

### Definition of done

- [ ] `gvm db migrate` applies 0001→0010 as `gvm_migrate`; `gvm db status` and `version` work.
- [ ] `AssertCompatible` refuses to run a binary **behind** the schema, with the exact message.
- [ ] **Linux CI:** `TestMigrationsUpDownUp` — every migration reverses.
- [ ] **Linux CI:** `TestEveryTableHasExplicitGrants` against `information_schema.table_privileges`.
- [ ] **Linux CI:** `TestWebQueriesRunAsGvmWeb` — every `queries/web/*.sql` executes as `gvm_web`; every `queries/ops/*.sql` **fails** as `gvm_web`; a query selecting `users.totp_secret_ct` as `gvm_web` fails.
- [ ] `mirror_test.go`: sync **never deletes** — a vanished YAML sets `absent_since` and the `instance_grants` rows survive (D-25).
- [ ] `port_allocations` has no cross-instance unique constraint; the `port_conflicts` view reports a collision instead (D-24).
- [ ] `TestApplySucceedsWithMirrorDown`: `gvm apply` completes and reports `mirror: degraded` when Postgres is stopped.
- [ ] `TestSpoolReplayIdempotent`: replay twice ⇒ zero duplicates; `spooled = true` on replayed rows.
- [ ] `ReapStaleRuns` marks previous-boot `running` rows failed on first ready.
- [ ] `make sqlc-check` (`sqlc diff`) is a CI gate.
- [ ] `doctor` gains HOST-001..HOST-005, HOST-011, HOST-012.

---

## Phase 15 — Identity, authorization, secrets-at-rest, and gvmd

**Goal:** the privileged verb gateway, the four-role model enforced in exactly one place, and the master key that never reaches the network tier.

**Depends on:** Phase 14.

### Files to create

```
internal/authz/role.go  permission.go  matrix.go  matrix_test.go
internal/authz/scope.go  service.go
internal/authz/scope_ast_test.go                        every handle method opens with require()
internal/authz/lint_test.go                             no role comparison outside this package
internal/authz/mirror.go                                upsert authz_permissions

internal/secrets/box.go  box_test.go                    HKDF + NewGCMWithRandomNonce + AAD (§14.7)
internal/identity/manager.go  password.go  password_test.go
internal/identity/totp.go  totp_test.go                 verify happens HERE, in gvmd (D-27)
internal/identity/tokens.go  tokens_test.go
internal/identity/bootstrap.go

internal/hostops/server.go  client.go                   NDJSON on /run/gvm/admin.sock, build-id handshake
internal/hostops/attach.go  consolepool.go  shutdown_ingest.go
internal/cli/{user,token,secret,manifest}.go
docs/bootstrap.md
```

### Definition of done

- [ ] `matrix_test.go`: `viewer ⊂ operator ⊂ admin ⊂ owner`; every `Permission` constant appears in `ownerPerms`; the full role×permission table is asserted explicitly.
- [ ] `scope_ast_test.go` fails when a deliberately-added handle method omits `require()`.
- [ ] `lint_test.go` fails on a deliberately-added `== authz.RoleAdmin` outside `internal/authz`.
- [ ] `box_test.go`: seal/open round-trip; a ciphertext moved to another user's row **fails to open** (AAD binding); `NonceSize() == 0` and the nonce argument is nil.
- [ ] `totp_test.go`: RFC 6238 vectors; a code already accepted inside the skew window is **refused** (`last_totp_step`).
- [ ] `password_test.go`: PHC round-trip; a below-policy hash triggers a silent re-hash on next login.
- [ ] `gvm user create --role owner` refuses to complete without TOTP enrolment; a second owner requires `--force` and is audited.
- [ ] `gvmd` refuses a client whose build id differs.
- [ ] `gvm-web.service` declares **no** `LoadCredential=` (asserted by `golden-units.sh`).
- [ ] `verbs_test.go`: the verb enum is closed and every verb has an audit action with an allowlist.
- [ ] **CI gate:** `go list -deps ./internal/hostops` contains no `net/http` server, `html/template`, `archive/zip` or content client.
- [ ] `doctor` gains HOST-003, HOST-004, HOST-006..HOST-008.

---

## Phase 16 — The web tier and dashboard · **Milestone D**

**Goal:** the dashboard and the JSON API, sharing one service layer, one authorization path and one audit path.

**Depends on:** Phase 15.

### Files to create

```
internal/app/app.go  scope.go  instance.go  capability.go  plan.go  confirm.go
internal/app/{content,backup,secret,user,host,adopt}.go

internal/web/server.go  router.go  routes.go  routes_test.go
internal/web/middleware.go  middleware_test.go  csrf_test.go  api_cookie_test.go
internal/web/{auth,instance,console,content,config,backup,job,user,audit,settings,
              doctor,manifest,adopt,account}_handlers.go
internal/web/sse.go  sse_test.go  confirm.go  confirm_test.go
internal/web/errors.go  render.go  funcs.go  secret_render_test.go
internal/web/csp_test.go  embed.go  health.go  peercred_{linux,stub}.go
internal/web/views/*.gohtml  views/partials/*.gohtml
internal/web/static/{gvm.css,htmx.min.js,console.js,favicon.svg,THIRD_PARTY.md}

internal/console/hub.go  hub_test.go  dialer_unix.go     one socket per instance, N subscribers
internal/webapi/api.go  envelope.go  envelope_test.go  *.go
internal/auth/{session,password,tokens,throttle,verifier}.go  + tests
internal/cli/{apiclient,login}.go
docs/http-api.md
```

### Definition of done

- [ ] `routes_test.go`: every non-public route declares a **known** permission; every `Destr` route is wrapped in `RequireConfirm`.
- [ ] `api_cookie_test.go`: a valid session cookie with no bearer token ⇒ **401** on every `/api/v1` route (D-40).
- [ ] `csrf_test.go`: a cross-origin POST to every browser mutation ⇒ 403.
- [ ] `csp_test.go`: no `hx-on`, no inline `<script>`/`style=`, no `javascript:` in any `.gohtml`.
- [ ] `hub_test.go`: a subscriber that never reads gets `resync` and does **not** stall the hub reader or any other subscriber (200 ms budget); 5 subscribers on one instance ⇒ exactly **1** runner socket.
- [ ] `sse_test.go`: `Last-Event-ID` resumes and takes precedence over `?from=`; a cursor below `oldest` yields `truncated`; `event: rotate` fires at the 30-minute cap.
- [ ] `confirm_test.go`: a destructive route refuses without `plan_id` + `confirm == <name>` + `archive_ack`; an expired or reused plan is refused; a changed `spec_sha256` is refused; free-disk below `estimate × 1.2` is refused.
- [ ] `secret_render_test.go`: rendering any template with a `secrets.Value` in scope never emits plaintext.
- [ ] The console input box refuses `quit`/`stop`/`shutdown`/`exit` with the Stop-button message.
- [ ] Every page in §16.7 renders against a fixture instance, including the content two-list view with a visible `check` row and the config form with `help` tooltips and `apply` badges.
- [ ] With Postgres stopped, `/readyz` is 503 and the login page shows the degraded banner with the `sudo gvm` hint; there is **no** unauthenticated read-only mode.
- [ ] **Milestone D: the v1 scope table is satisfied.**

---

## Phase 17 — Hardening, CI gates, release

**Goal:** every architectural claim is enforced by a test that runs on every PR.

**Depends on:** Phase 16.

### Files to create

```
ci/control-plane-dead.sh
.github/workflows/{ci.yml,linux-integration.yml,control-plane-dead.yml}
internal/store/queries/ops/audit_export.sql
internal/cli/audit.go                                   gvm audit export --before (age-encrypted)
docs/what-happens-if-postgres-is-down.md                completed with measured results
```

### Definition of done

- [ ] **`ci/control-plane-dead.sh` passes on every PR:** bring up an instance; `systemctl stop gvmd gvm-web postgresql`; assert the game pid **and starttime** are unchanged after 30 s and the unit is active; reboot; assert every instance returns `running` with timers armed and a future elapse, ufw rules present, and the next backup scheduled; start Postgres and assert `gvm db replay-spool` reconstructs the job and audit rows.
- [ ] `ci/golden-units.sh` renders drop-ins and timers for all shipped manifests × {new, adopted} and diffs against checked-in goldens.
- [ ] All build gates green: `noshell_test.go` (10 packages), `hook/noexec_test.go`, `curseforge/nosynth_test.go` (stub until Phase 19), `authz/{scope_ast,lint,matrix}_test.go`, `web/routes_test.go`, `spec/dialect_sync_test.go`, `spec/pin_test.go`, `jobs/guard_test.go`, `go list -deps ./internal/hostops`, `sqlc diff`.
- [ ] `make check` green on Windows; `make test-linux` green in a Debian 12 container; the nested-VM reboot job green.
- [ ] A tagged release produces `gvm_<ver>_amd64.deb` and `gvm_<ver>_arm64.deb` with reproducible `-trimpath` builds.
- [ ] `docs/what-happens-if-postgres-is-down.md` records the *measured* behaviour of every row in §14.6, not the intended behaviour.

---

## Phase 18 — v1.1

**Goal:** Minecraft, Modrinth, and the second wave of control kinds.

**Depends on:** Phase 17.

### Files to create

```
internal/provider/minecraft/{vanilla,paper,purpur,fabric,neoforge,forge,java}.go  + tests
internal/content/modrinth/{source,client,deps,env,mrpack}.go  + tests
internal/control/{telnet,mcmgmt}/  + tests
internal/hook/mc.go                                     select_jdk, accept_eula, write_properties,
                                                        gamerule_fallback
games/{minecraft-neoforge,minecraft-forge,minecraft-paper,7daystodie-telnet}.yaml
internal/adopt/discover_mc.go                           sha1 every jar -> POST /v2/version_files
```

### Definition of done

- [ ] Modrinth client: filters are **JSON-encoded** and the returned `game_versions`/`loaders` are **re-verified** before any jar is written (the silent-filter trap); `dependency_type == "embedded"` is never downloaded; `incompatible` aborts the plan.
- [ ] Java selection reads `javaVersion.majorVersion` from piston-meta and pins an **absolute** interpreter path per instance; `update-alternatives` is never used.
- [ ] Paper resolution uses Fill v3 only (`api.papermc.io/v2` is gone); NeoForge/Forge version resolution parses the full `<versions>` list rather than trusting `<latest>`/`<release>`.
- [ ] `materialize_is_explicit` is honoured: the network-touching first boot is a visible install step, never a user's first Start click.
- [ ] Minecraft adoption reconstructs a manifest from jar hashes and records unresolved jars as checksummed vendored files.
- [ ] `7daystodie.yaml`'s `kind: telnet` moves from `SupportPlanned` to `SupportImplemented` and its capability reason disappears.

---

## Phase 19 — v1.2

**Goal:** CurseForge, nftables, Windows-depot games, snapshot backups, egress.

**Depends on:** Phase 18.

### Files to create

```
internal/content/curseforge/{source,client,files,ladder,blocked,murmur2,excludes,pack,loader}.go
internal/content/curseforge/nosynth_test.go             the real gate (was a stub)
internal/firewall/nft/nft.go                            the `inet gvm` table backend
internal/provider/steamcmd/platform_windows.go          +@sSteamCmdForcePlatformType windows
internal/runner/wrapper_proton.go
internal/backup/snapshot_btrfs.go  snapshot_zfs.go
internal/reconcile/egress.go                            IPAddressAllow/Deny from spec.egress
games/{ark-se,rust,satisfactory,cs2,vrising,enshrouded}.yaml
docs/curseforge-blocked-mods.md
```

### Definition of done

- [ ] `nosynth_test.go` green: no synthesized `edge.forgecdn.net` URL anywhere.
- [ ] The resolution ladder is a **typed result**; a blocked mod never aborts the run, `blocked-mods.json` is written, and the instance enters `AWAITING_MANUAL_FILES` **without stopping a running server**.
- [ ] `gvm content resolve-blocked` rescans the drop-box, verifies sha1 (and murmur2 for renamed files), and completes the install.
- [ ] `x-api-key` is sent on CDN requests and a `401` is handled as its own error class.
- [ ] itzg's exclude/include schema parses verbatim, matching by slug **or** numeric id, `forceIncludes` winning, every decision logged with its source.
- [ ] The nft backend owns exactly one table (`inet gvm`) and touches nothing else; ownership is table membership, and the `Backend` interface is unchanged.
- [ ] `spec.egress.policy: allowlist` renders `IPAddressDeny=any` + `IPAddressAllow=` and is verified by an outbound-connection test in a container.

---

## Risk register

| Risk | Phase | Mitigation |
|---|---|---|
| **Adoption damages the live world** | 13 | Rehearse on a VM restored from a real backup. Archive-then-verify before the irreversible step. Automatic rollback. The rollback script depends only on `systemctl` and `tar`. Never write into `Saves/`. |
| PZ's save-complete marker is wrong | 4, 13 | `on_timeout: continue` makes a wrong regex cost 30 s of latency, not a failed save. Capture the real line during Phase 13's discovery (Open question 2). |
| PZ does not read stdin from a PTY | 4 | `console.mode: pipe` and `console.line_ending: cr` are already schema fields. Verify on a scratch instance in Phase 10, **before** Phase 13. |
| `ProtectHome`/`ReadWritePaths` differs across systemd versions | 6 | The write-probe turns the assumption into an executed test that runs before every apply and on every doctor run. |
| SteamCMD exits 0 after a failed update | 8 | The classification ladder plus **mandatory** appmanifest post-verification; `verify` is a required manifest field. |
| The runner panics mid-save | 4 | Three layers: per-goroutine `recover()`, a `main` recover that still runs the ladder, and the standalone `gvm run-shutdown`. |
| Postgres becomes load-bearing by accident | 14, 17 | `TestNoUnitDependsOnPostgres`, the `go list -deps` gates, and `ci/control-plane-dead.sh` on every PR. |
| Authorization checks scatter | 15 | `scope_ast_test.go`, `lint_test.go`, `routes_test.go` — added with the package, never skipped. |
| A manifest change alters a live server | 1, 6 | Per-instance pin; the runner exits 64 on mismatch; `spec/pin_test.go`. |
| The CurseForge API key is revoked | 19 | Operator-supplied only; never embedded; `nosynth_test.go` prevents the circumvention that gets keys revoked. |

---

## Definition of done for v1 as a whole

- [ ] Milestones A–D demonstrated, each with a recorded transcript.
- [ ] The live Project Zomboid server has run under gvm for **7 consecutive days** with nightly updates and backups, then `gvm adopt finalize` executed.
- [ ] `gvm doctor` reports **0 problems** on that host.
- [ ] `ci/control-plane-dead.sh` green on every PR for the preceding two weeks.
- [ ] `make check` green on Windows 11 with no build tags required.
- [ ] Every decision in the architecture's register (D-01…D-46) is either implemented or has an open issue explaining why not.
- [ ] Every open question in §23 has a recorded measurement, or an issue with the measurement plan.
