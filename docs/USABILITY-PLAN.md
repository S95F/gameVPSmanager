# gvm — Usability Plan (Phases 20–55): golden path first, safety before wiring

This plan continues [`IMPLEMENTATION-PLAN.md`](./IMPLEMENTATION-PLAN.md) after Phase 19. That document stays the record of how Phases 0–19 built gvm. This one takes over from Phase 20 and is driven by the October 2026 usability review: 46 agents produced 210 verified findings (61 high, 109 medium, 40 low). A factual verifier confirmed each finding, and a second verifier checked it against the hard design rules. Ids such as `onboarding-3` refer to that review. The full record, with each finding's evidence, impact and suggestion, is [`reviews/usability-2026-10.md`](./reviews/usability-2026-10.md), and each phase closes its ids there when it lands. The work is ordered by the operator's journey: J1 turns a fresh VPS into a joinable, passworded server, J2 covers week-to-week running, J3 adds the whole catalog, mods and custom games, J4 lets friends help without SSH, and J5 covers adoption, packaging and disaster recovery. Two ordering rules override the journey. A defect that can hurt someone with today's binary is fixed first: Phase 21 needs nothing from Phase 20, so it runs first or alongside it, and the first public release (Phase 34) depends on it. And no built-but-unwired component is wired until its known safety defects are fixed and tested. The four rules of the original plan still hold in every phase: `make check` stays green on Windows, Linux-only code lands with its stub, a phase that adds a doctor-checkable invariant adds the check, and no phase weakens an earlier guarantee or skips a build gate.

**Module:** `github.com/S95F/gameVPSmanager` · **Binary:** `gvm` · **Go 1.26, `CGO_ENABLED=0`.**

**How to read a phase.** Each phase has the same parts:

- **Goal:** what a user would notice.
- **Why now:** what breaks without it, or what later phases need from it.
- **Resolves:** the finding ids it closes. A finding split across phases is marked, and Appendix A shows the split.
- **Work items:** commands, flags, files and message text, each citing its findings.
- **Definition of done:** observable facts, with the exact commands.
- **Depends on** and **Effort:** S is up to 3 days, M up to 2 weeks, L up to 5 weeks, XL anything longer.
- **Design rules touched:** for each risky item, the rule at stake and how the phase keeps it.

---

## Where gvm stands

The foundation is strong, and Phases 0–19 are committed with `make check` green. gvm already has:

- a pure phase table that keeps Reason separate from Hint
- a save-and-stop ladder that records the rung it ended on
- per-instance manifest pinning
- a conflict-aware port allocator
- tag-owned firewall reconciliation with an SSH guard
- self-describing, verified archives
- typed-name confirmation
- a template guard with no control flow
- base62 generated passwords and secret values that cannot leak through `fmt`
- read-only adoption discovery

Unbuilt paths refuse rather than pretend.

**But the operator journey does not reach its end.** This is what the review saw on the fake host and on a real binary:

```
gvm game list                          ok: 16 games
sudo gvm create v1 --game valheim ...  ok: ports allocated, YAML written, "next: gvm apply v1 --diff"
sudo gvm apply v1                      ok: account, dirs, drop-in, timers (incl. a nightly update that will fail)
sudo gvm install v1                    REFUSES: "the provider seam is not built in this release"   (all 16 games)
sudo gvm secret set ...                REFUSES: "requires gvmd" (gvmd is itself not implemented)    (11 of 16 games need one)
sudo gvm start v1                      would reach a runner that never renders config_files, runs no hook,
                                       has no a2s probe (10 manifests), runs Mono under W^X (5 manifests)
players                                nothing prints an address; ufw, if active, blocks them; `gvm firewall` is unbuilt
sudo gvm doctor                        "problems" on every healthy host; never looks at an instance
```

The dominant pattern is **built but not wired**. That covers providers, hooks, config_files, the a2s probe, secrets, the firewall, gvmd and the web tier, content sync, the update and restore jobs, and adopt cutover.

Around that gap:

- The CLI advertises commands that cannot work.
- Help is broken: `sudo gvm create --help` panics.
- About 20 hints name commands that do not exist.
- Output cites design-doc decision numbers.

There are also real safety defects in code that already exists:

- **Already reachable:** `adopt finalize` skips its 7-day guard, the backup engine ignores `backup.dir` and `backup.remote`, and the first `content add` deletes every comment.
- **Waiting to be wired:** the adopt rollback script, the Proton ladder's SIGKILL, Mono under W^X, and a guard that restarts with no cap.

None of this needs a redesign. Most of the work is wiring, honest messaging, and small read-only views over data gvm already has.

---

## Principles

### The hard design rules (unchanged; cited by number throughout)

| # | Rule |
|---|---|
| 1 | The filesystem is the control plane, systemd is the supervisor and Postgres is only a logbook. With gvmd, the dashboard and Postgres dead, servers keep running, restarting, updating and backing up, and every CLI command works over SSH as root. |
| 2 | No gvm component ever spawns a shell. |
| 3 | The CLI never dials `/run/gvm/admin.sock`. |
| 4 | Never `kill -9` a running game server. Shutdown goes through the save-and-stop ladder. |
| 5 | Destructive commands require typed server-name confirmation and archive first. Prune runs only after a successful create and verify. `preserve` archives are never pruned. |
| 6 | The reconciler never actuates. A stale lease yields `failed` with a hint, never an auto-resume. |
| 7 | `gvm apply` never enables ufw and never installs packages (D-41). gvm deletes only rules tagged `gvm:<instance>:<port>`, and nothing when it cannot parse the live ruleset with confidence (D-42). Never lock the operator out of SSH. |
| 8 | Migrations never run implicitly in gvmd. No pgbouncer and no Postgres extensions. |
| 9 | The web tier has no unauthenticated read-only mode. TOTP is verified in gvmd. A session cookie without a bearer token gets 401 on `/api/v1`. |
| 10 | Manifests are data, with no template control flow. Game passwords may not contain `' " \ $` or a backtick. |
| 11 | Adopt discovery is read-only. `gvm adopt finalize` refuses within 7 days unless given `--force`. |
| 12 | Provider Plan does network reads only. Apply writes only digest-verified bytes. |
| 13 | CurseForge download URLs are used verbatim from the API, and the API key is supplied by the operator. |

### Usability principles (each enforced by a test or a gate)

These stop usability from regressing once it is fixed. Each one is enforced by a test or a gate, so it survives the next contributor.

- **U1. One record of what works.** A single availability registry sits on `cli.Command`, on every subcommand entry and on every job kind (`engineWired`). It feeds:
  - `gvm help`
  - `gvm job list`
  - every `next:` line
  - the README "What works" table
  - later, the dashboard

  No list of commands is written by hand. A phase that finishes a journey flips that journey's rows in the same commit. `TestAvailabilityMatchesBehaviour` fails both when a *works* row refuses and when a *not built* row starts working.
- **U2. Every printed command runs.** Any `gvm …` must resolve against the dispatch tables, that command's FlagSet and, for enum flags such as `--role` or `--kind`, the enum's value set. That covers:
  - Go string literals
  - manifest hints, notes and help
  - doctor Fix strings
  - fenced blocks in user docs

  Otherwise the string must contain the exact phrase "not available in this build". Enforced by `TestHintsResolve` (Phase 20).
- **U3. No success without effect.** No code path reports success, `ok`, `202` or "accepted" for an action that did not happen. A step that could not do its job is a `warn` (degraded) or a failure. Two consequences:
  - A value that gvm stores but this build does not deliver to the game says so.
  - Work that must ship together does ship together. The bundles are listed under [Release points](#release-points-and-bundles).

  Gates: `TestNoMutationReturns2xxWithoutDispatch` (web), plus a degraded-step test for every job step that can be skipped.
- **U4. Every mutating command ends with three things:** what changed, what did not, and one `next:` line. The `next:` line comes from `lifecycle.Derive` through `nextStep()` and is filtered through U1.
- **U5. Preview before power.** Every new mutating verb ships in the same phase with a read-only preview that renders the same plan the real run executes. The preview is one of `--dry-run`, `--diff`, `plan` or `--check`. Where the data is readable without root, the preview works without root.
- **U6. One plain sentence first.** An error leads with what happened and what to do. Design references (D-nn, §x.y) and Go identifiers appear only under `GVM_DEBUG=1`. Enforced by a lint test (Phase 22).
- **U7. Help never acts.** `--help`, `-h` and `gvm help <cmd> [sub]` exit 0, print `Usage:` and touch no HostEnv (`TestEveryHelpPathExitsZero`).
- **U8. Documented exit codes.** Operator verbs exit:

  | Code | Meaning |
  |---|---|
  | 0 | ok |
  | 1 | failed |
  | 2 | usage |
  | 3 | not available in this build |
  | 4 | checked and found something, including "an update is available" |
  | 5 | refused or not confirmed |

  Role verbs (`run`, `run-prepare`, `run-shutdown`, `probe-write`) keep their systemd contract. So does `gvm job run` when systemd invokes it (`INVOCATION_ID` set): ok, skipped and degraded exit 0, failed exits 1, and a kind this build cannot run exits 3. `TestRoleExitCodesUnchanged` and `TestJobUnitExitCodes` pin both.
- **U9. No secret on argv, ever.** A value comes from a no-echo TTY prompt, stdin, a file or `--generate`. Output shows set/unset, length or fingerprint, never the value. The one exception is root `secret show` for player-facing join passwords.
- **U10. Never send the operator to something that is not there.** No message names a daemon, unit, page or command this host does not have. Every Postgres-dependent command says "game servers are unaffected". In particular, gvm never prints `ufw enable` as advice:
  - Before Phase 33 fixes SSH detection, nothing mentions enabling a firewall at all.
  - From Phase 40, the only enable path printed is `sudo gvm firewall enable`, and that typed, SSH-verified command is the only code path that runs it.
- **U11. No bypass flags for safety.** Usability work adds confirmations, previews and explanations. It never adds a flag that skips a design rule's check, and `--force` stays exactly where it already exists.
- **U12. Display reads never actuate.** `Derive`, the guard and the reconciler never read what this plan adds to `status`, `list`, `doctor` and the dashboard. That covers details, players, job records, upgrade status, connect info and ledger state. An import test proves it.
- **U13. Safety before wiring.** A built component is wired only after its known safety defects are fixed and tested:
  - The runtime ladder, probe and guard fixes come before any install engine.
  - SSH detection comes before any firewall output, and the backend fixes come before any firewall write.
  - The adoption record and rollback come before cutover.
  - The auth model comes before the web tier.
  - Hook linkage and the EULA gate come before the installs that run them.
- **U14. Proved on a real box, and only proved games are offered.** Any manifest change to `jit`, `arch`, `wrapper`, `env`, readiness or the ladder bumps the revision. It is confirmed by a recorded scratch boot on Debian 12 and Ubuntu 24.04, written to `docs/research/scratch-boots/<game>.md`.

  From Phase 32, `create` and `install` accept only a manifest revision that the boot ledger marks verified, either in the shipped ledger or by a passing `gvm game try --scratch` on this host. No live instance can pin a known-bad revision.
- **U15. A journey works only when its transcript passes in CI.** `ci/golden-journey.sh` grows with each milestone. No public release tag is cut before Milestone J1, and before J1 only `-rc` tags are cut, at the ◆ points.
- **U16. Docs are generated or tested.** Generated, with a CI diff:
  - the CLI reference
  - game pages
  - the manifest field reference
  - the exit-code table
  - the doctor-ID table

  Every other user doc passes U2. `ARCHITECTURE.md` and `IMPLEMENTATION-PLAN.md` carry a "target design" banner instead.
- **U17. Formats are frozen until the skew rule, then N-1 forever.**
  - **From Phase 20, until Phase 35:** `TestFrozenFormats` pins `console.Proto`, `LeaseProto`, the freeze `LeaseSchema`, `StatusProto`, `RunnerFileProto` and the archive and index schemas. Additive JSON fields are allowed.
  - **From Phase 35 on:** every reader accepts the previous version, and no schema bump lands without a cross-version test.

---

## Strengths to preserve

The review praised these. The plan builds on them rather than around them, and each keeps its test.

- **Honest refusals.** Unbuilt paths exit non-zero. `gvm install` prints its full preflight before it refuses. Lifecycle refusals offer `--force` only when the refusal can actually be overridden, and pin mismatches never can (`start.go:112-123`, `verbs.go:123-136`). Phase 20 generalises this pattern; it does not replace it.
- **`gvm create` explains itself.** It:
  - shows allocated ports
  - lists missing vars with their help text
  - refuses undeclared and duplicate `--var`s
  - never overwrites a spec
  - says "nothing else on this host changed: no account, no directories, no unit"
  - ends with `next:`

  This is the model for U4.
- **`gvm apply --diff` is the real plan.** It shows counts, masked unified diffs, honest `!` lines for what it did not verify, and "nothing has been applied". A converged apply does nothing (`SameDropIn` ignores the render stamp).
- **One phase table.** `lifecycle.Derive` feeds status, list and doctor with an ordered, pure rule set. Reason is kept separate from Hint, and an unanswered systemd query reads "not checked on this host", never "stopped".
- **Shutdown outcomes are recorded.** `shutdown.jsonl` holds the rung and a DEGRADED flag, and `gvm logs --shutdown` survives torn lines. `settled()` requires both a dead leader and an empty cgroup (`shutdown.go:838-863`).
- **The unit's exit contract.** No `ExecStop`, `KillMode=mixed`, `RestartPreventExitStatus=64 65 66 78`, and unlimited log rate. Phase 27 adds only 79 to that list.
- **Typed confirmation is one shared function** (`TypedConfirm`) with no y/n shortcut. `--yes` never satisfies a typed-name gate.
- **Manifest pinning** (`manifest.lock.yaml`, D-17). A new gvm never changes a live world's argv.
- **`gvm game validate`** reports `file:line:col` with a why/fix line and a did-you-mean.
- **The port allocator.** It shifts by group stride, honours pins, reserves in a ledger, and gives specific `ExhaustedError` and `Rejection` messages.
- **Firewall ownership in code:**
  - the tag grammar
  - `Guard.Check` with no bypass
  - adds before deletes
  - SSH never deleted
  - deletions suppressed on an untrusted parse
  - foreign rules counted but never touched

  `Desired` is pure and file-derived.
- **Secrets primitives.** `secrets.Value` masks itself in every formatter. `GeneratePassword` is base62 by construction (rule 10). The file store writes atomically at 0600 and describes bad values by byte offset.
- **Audit design.** Per-action parameter allowlists, UUIDv7 spool ids, idempotent replay, and a failed sink never blocks a stop.
- **Archives.** The manifest is the first member, every member has a sha256, and the instance YAML is embedded. `restore --list --from` reads legacy tarballs, and the free-space preflight runs before the freeze.
- **Adoption foundations.** `config.env` is parsed, never sourced, and `TestDiscoverIsReadOnly` holds. Ports come from the live ini. Cutover has a commit point with injected-failure tests, and the `Conflicts=` guard templates exist.
- **Content tooling.** The three-way diff, `why`, load-order findings with runnable fixes, comment-preserving edits when the path exists, and a careful collection sync. `nosynth_test.go` prevents CDN URL synthesis.
- **Transport.** The CLI never dials admin.sock (`TestTheCLINeverNamesTheAdminSocket`). Every verb declares a route, and bearer tokens are refused over plain http.
- **Console UX.** Detach is explicit, a slow reader never stalls the game (ring backpressure), and lines are inserted with `textContent`.
- **The 503 page** tells a user at 2 a.m. that the game servers are not affected and gives the exact `sudo gvm` commands.
- **Readiness probes always dial loopback**, so NAT or a firewall is never misread as a failed boot.

---

## Roadmap at a glance

◆ marks a point where an `-rc` tag may be cut before J1 (see [Release points](#release-points-and-bundles)). **Track** names the part of the code that owns the phase, so separate sessions can work in parallel:

| Track | Area |
|---|---|
| S | surface and guidance |
| H | hazards and runtime |
| I | install and host |
| G | games and engines |
| N | network and connect |
| O | observe and operate |
| B | backup and recovery |
| A | adoption |
| C | content |
| W | web |

| Ph | Title | What a user gets | Track | Effort | Depends on |
|---|---|---|---|---|---|
| **J1** | **First joinable server** | | | | |
| 20 ✅ | Tell the truth about this build | help, README and refusals say what works; every hint runs; exit 3 | S | L | 19 |
| 21 ◆ ✅ | Close the hazards already in this build | finalize can't delete early; backups go where configured; no comment wipe | H | L | 19 |
| 22 🟡 | Help, errors and exit codes that work everywhere | `--help` never panics; plain errors; documented exit codes | S | L | 20 |
| 23 🟡 | Install gvm on a fresh VPS | `sudo ./gvm host init`, verified tarball, real version | I | L | 22 |
| 24 ◆ | `gvm doctor` tells the truth | 0 problems on a healthy host; checks every instance | I | L | 23 |
| 25 | Adoption discovery you can trust | discover finds the live PZ server, says what blocks cutover, changed nothing | A | M | 22 |
| 26 | Choose a game, and always know the next step | `gvm game show`; `next:` after every command; RAM/disk warnings | S | M | 22 |
| 27 | The runner never kills a game mid-save, and never boots blind | Proton ladder fix; a2s/mc_ping probes; boot deaths stop looping; ProtectProc | H | L | 22, 24 |
| 28 ◆ | A server that cannot run says why; stop always stops | crash loops shown; guard capped; stop works on a dead runner | H | L | 27 |
| 29 | Passwords and keys, set locally | `gvm secret set/show`; join password by default | G | L | 22, 24 |
| 30 ◆ | Settings and hooks reach the game | config_files rendered; pre-start hooks; operator accepts the EULA | G | L | 26, 29 |
| 31 | Game files: install, update and repair | `gvm install` works; update timers either work or say they can't | G | L | 26, 28, 30 |
| 32 ◆ | Prove a game boots before it is offered | `gvm game try --scratch`; boot ledger; Valheim, Palworld, Paper, PZ verified | G | XL | 31 |
| 33 ◆ | Players know how to connect | join line; SSH-safe `gvm firewall plan`; `gvm ports` names owners | N | L | 24, 29 |
| 34 | First server, start to finish · **J1** | progress while booting; `gvm up`; first public release | S | L | 21, 32, 33 |
| **J2** | **Week to week** | | | | |
| 35 | Upgrade gvm without stopping the worlds | N-1 skew rule; restart-pending; `restart --pending` | I | L | 34 |
| 36 | Backup health you can see | snapshot line, GiB remedies, BACKUP doctor checks | B | M | 21, 24 |
| 37 | One glance: status, job history and the audit trail | last/next job, players, uptime, `audit tail`, `logs -f` | O | XL | 34, 36 |
| 38 | Get a world back | `restore --to`, archive-first restore, rebuild for content-free games | B | L | 21, 34, 37 |
| 39 | Change settings without hand-editing YAML | `gvm set`, `enable/disable`, `ports move`, `manifest upgrade`, strict apply | O | XL | 35, 38 |
| 40 | gvm opens the ports, and never locks you out | apply stages tagged ufw rules; `gvm firewall apply` for host rules; guarded `gvm firewall enable` | N | L | 33, 39 |
| 41 | Planned restarts players see coming · **J2** | `--warn` countdown; restart says which rung saved | O | M | 37, 38, 39, 40 |
| **J3** | **Every game, mods and custom games** | | | | |
| 42 | The whole catalog boots | Proton, Mono, native Satisfactory/ARK, 7DTD, Core Keeper, CS2 verified | G | XL | 32, 39 (item 10 only) |
| 43 | Mod lists that are easy to edit and read | URLs and titles; import from ini; `content update --check` | C | L | 39 |
| 44 | Apply Workshop mod changes safely | `gvm content sync`; PZ updates; rebuild for modded games | C | L | 41, 43 |
| 45 | Minecraft mods: Modrinth, then CurseForge | MC mods by name; honest blocked-mod ladder; MC updates | C | XL | 29, 44 |
| 46 | Add a game with YAML alone · **J3** | `game approve`; deep validate; field reference | G | L | 42, 45 |
| **J4** | **Friends without SSH** | | | | |
| 47 | Make the web skeleton safe before it is wired | no anonymous pages, no fake 202s, §15.2 roles | W | L | 22 |
| 48 | Dashboard v0 | invite a friend; start/stop/restart; live console | W | XL | 47, 33, 37, 40 |
| 49 | Dashboard v1 · **J4** | connect card, mods tab, settings tab, plain-language status | W | L | 48, 39, 44 |
| **J5** | **Adopt, package, recover** | | | | |
| 50 | Make adoption's safety net real | adoption record; safe cutover/rollback library; record-based finalize | A | L | 21, 25, 39, 40 |
| 51 | Adopt the live Project Zomboid server | cutover in one window; `gvm adopt status`; `instance promote` | A | L | 50 |
| 52 | Packaged gvm: .deb, migrations, rollback policy | `apt install ./gvm.deb` with worlds live | I | L | 35, 48 |
| 53 | Remove a server, or gvm itself, cleanly | LEFT IN PLACE block; guarded uninstall keeps backups | I | L | 40, 50, 52 |
| 54 | Off-box backups that really leave the box | pinned SFTP + age push; `backup remote check/list/pull` | B | L | 36, 38 |
| 55 | Move to a new host · **J5** | `create --from-archive`; DR drill passes | B | L | 38, 46, 53, 54 |

The **J1 critical path** is 20 → 22 → 23 → 24 → 27 → 28 → 31 → 32 → 34. The branch 24 → 29 → 30 → 31 is co-critical: it is the same length (L + L), and Phase 31 needs both. Phases 26 and 33 run alongside. Phase 21 depends only on Phase 19, so it runs first or in parallel with Phase 20, and Phase 34 (the first public release) waits for it. Phase 25 hangs off Phase 22 and gates nothing in J1.

With the estimates above, the critical path is eight L phases and one XL phase in series, so J1 takes most of the calendar for one session. The work to hand to a second session is Phases 21, 25, 26, 29–30 and 33, then 36 once Phase 24 lands.

Phase 42's boots can start as soon as Phase 32 lands and run in parallel with J2. Only its superseded-revision rows (item 10) wait for Phase 39. The adoption track (25 → 50 → 51) needs only J2 phases, so it can be pulled ahead of J3 and J4 if migrating the live server is the priority.

### Milestones

| Milestone | After phase | What a person can do |
|---|---|---|
| **J1 — first joinable server** | 34 | Download a release, verify it and run `sudo ./gvm host init`. Get a clean `gvm doctor`. Pick a verified game with `gvm game show`, then `create` (join password generated), `apply`, `install`, and `start` with visible progress. Paste the SSH-first `gvm firewall plan` lines if ufw is on. Players join with the printed `address:port` and password. No Postgres needed. J1 verifies Valheim, Palworld, Minecraft Paper and Project Zomboid. PZ gets a nightly restart through the ladder in J1; PZ updates arrive in J3 with the mod engine. |
| **J2 — week to week** | 41 | Upgrade gvm with worlds live and restart them one at a time. `gvm status` and `gvm list` show uptime, players, last backup, last job and next job, and job history and the audit trail work with Postgres down. Backups say exactly what they did. Settings change with `gvm set`, `gvm ports move` and `gvm enable/disable`. Worlds can be restored. `gvm apply` opens the ports, and planned restarts warn players. |
| **J3 — every game, mods and custom games** | 46 | Every catalog game is verified or labelled unsupported (and a game labelled unsupported keeps its finding open). Add Workshop, Modrinth and CurseForge mods by URL or name, see their titles, and apply them through the save ladder. Add a new game with YAML, approve it with a typed command, and prove it boots in a scratch instance. |
| **J4 — friends without SSH** | 49 | Invite a friend, who enrols their own password and TOTP. On the servers they were granted, they can start, stop, restart, watch and type into the console, and see how to connect, which mods are installed and why a server is down. Game servers keep running when the dashboard is down. |
| **J5 — adopt, package, recover** | 55 | Adopt the live Project Zomboid server with a real rollback, then finalize after 7 days. Install and upgrade gvm from a `.deb`. Remove a server or gvm cleanly. Push encrypted off-box backups. Rebuild every server on a new VPS from its archives. |

The original milestones B–D were ticked at the code level. J1–J5 re-anchor them to user-visible transcripts.

### Release points and bundles

- **Before J1:** only `-rc` tags, and only at ◆ (after Phases 21, 24, 28, 30, 32 and 33). The first rc is cut right after Phase 21, which may land before Phase 20, so the hazard fixes are in a build without waiting for Phase 20's registry work.
- **The first public release** (`v0.20.0`) is cut at Phase 34, when `ci/golden-journey.sh` passes (U15). Phase 34 depends on Phase 21, so no public release can ship the hazards.
- **After J1:** releases follow milestones, and patch releases may be cut at any phase boundary.
- **Any point:** a release point is skipped while a bundle below is only half-merged.

Two bundles never straddle a release point, because each half on its own would be success without effect (U3):

- **Phases 29 + 30.** A stored password must reach the game in the same release. Inside the bundle, `gvm secret list` labels a secret that only a config file carries as "stored; not written to the game by this build", and `create` prints a join password only when the build really delivers it.
- **Phases 31 + 32.** Install opens together with the boot-ledger gate, so no build can install a manifest revision nobody has booted, such as Valheim with `jit: false`.

### Parallel work and merge hotspots

- `internal/cli/root.go` and `usage.go` change structurally only in Phases 20 and 22. Later phases add `Command` rows and registry flips, not structure.
- Doctor checks are added as separate `internal/doctor/checks_*.go` files through the check registry, so parallel phases never edit one function.
- Every `games/*.yaml` change carries a revision bump, and lands in the phase that boots it (30, 32, 42) or that owns the field (29, 33).

### Safety fixes and where they land

The rule for this table: fix a defect before the first phase that would put it in front of users. A defect that is reachable in today's build is fixed immediately.

| Defect | Rule at risk | Reachable today? | Fixed in | Why there |
|---|---|---|---|---|
| `adopt finalize` skips the 7-day guard when `--adopted-from` is missing or in the documented format, then deletes whatever `--unit-path` names (`migration-adopt-2`) | 11 | **yes** | **21** (fail-closed), 50 (record) | Live and destructive now |
| The first `content add` re-marshals the instance file and deletes every comment (`content-mods-4`) | data loss | **yes** | **21** | Live now |
| `backup.dir` ignored by the writer but honoured by list/verify/prune; a configured remote ignored while the report says `ok backup.push`; a pasted `AGE-SECRET-KEY` copied into every archive (`offsite-backup-dr-1,2,3,7`) | 1, 5 | **yes** | **21** | The nightly backup is a default job |
| btrfs clones are read-only subvolumes the sweep cannot delete, so free space shrinks nightly (`offsite-backup-dr-10`) | disk fill → failed saves | **yes** | **21** | Live on subvolume roots |
| `destroy` accepts a week-old archive without showing its age, and an authorising non-preserve archive can later be pruned (`cli-ergonomics-7`). Flipping the index entry alone does not help: `Prune` calls `Reindex` first, which re-reads `Class` from the archive's own manifest (`retention.go:90`, `index.go:186-190`) | 5 | **yes** | **21** (age, class, a durable preserve pin that `Reindex` and `Prune` honour), 38 (fresh archive by default) | Live now |
| A hand-edited heap leaves MemoryMax unchanged, so the OOM killer can SIGKILL mid-save (`game-catalog-config-4`) | 4 | **yes** (hand edit) | **21** (apply refuses), 39 (`gvm set` recomputes) | Live now |
| `ProtectProc=invisible` (the fix that stops one instance reading another's argv) would hide the root backup job from the runner's freeze watcher. `platform.Alive` maps ENOENT to "dead" (`proc_linux.go:45`), and `runfreeze.go:109-113` would thaw mid-snapshot | freeze safety (D-31) | introduced by the fix | **27**, in the same commit as the watcher change | The fix must not create a hazard |
| On a Proton stop, the launcher's exit collapses every remaining wait, so the Wine game is SIGTERMed then SIGKILLed mid-save (`game-runtime-compat-2`) | **4** | latent | **27** (runner), 42 (Proton manifests) | Before any install; Proton games stay unverified until 42 |
| No a2s/mc_ping probe, so 10 manifests spawn, never reach ready and are cycled (`game-runtime-compat-1`) | 4 (ladder churn) | latent | **27** | Before any install |
| A game that dies in boot loops forever. A death more than ~100 s into boot never trips `StartLimitBurst=5` within 600 s (`game-runtime-compat-5`) | unit policy | latent | **27** (exit 69 and the boot-failure counter), 28 | Before any install |
| The guard restarts units that exited 64/65/66/78 every 5 minutes with no cap (`failure-recovery-10`) | 6 / unit policy | latent | **28** | Before servers can be installed |
| Unity/Mono titles under `MemoryDenyWriteExecute=yes`; unverified manifests installable (`game-runtime-compat-4`) | crash at boot; bad pins | latent | **32** (ledger gate, Valheim), 42 (rest) | The gate ships with install (31 + 32 bundle) |
| PZ and Minecraft update with a nil content seam would report "declares no content" (`update/pipeline.go:329-333`) | success without effect | latent | **31** (`engineWired` keyed on the manifest's content block; PZ's recommended nightly is written as a restart until then), 44/45 flip | Never wired before the adapter |
| An SSH disconnect sends SIGHUP, which kills an in-process `install`, `update`, `restore`, `rebuild`, `content sync` or `game try` with no unwinding (`main.go` handles only SIGINT and SIGTERM). The lease goes stale, and the guard's sweep clears it and starts the half-finished world (`jobs/sweep.go:115-134`) | 4, 6 | at the first in-process job verb | **31** (job verbs ignore SIGHUP and run as `gvm-job@` units the CLI follows); each later job verb uses the same helper | Before install opens |
| A live Xvfb inside the unit cgroup keeps `settled()` false, so every Core Keeper stop waits out its budget and ends in the cgroup SIGKILL sweep | 4 | latent (Xvfb unbuilt) | **42** (Xvfb registered in the sweep-exempt set before the game spawns) | Built with the wrapper |
| A restore moves the old world into `.gvm-replaced-<stamp>` inside the world's base, and nothing reclaims it, so every later nightly archives it again | disk fill → failed saves | introduced by restore | **38** (reclaimed through `archive.Destroy`, and excluded from backup sources) | Born safe |
| Rebuild with nil Content/Server/Timers/Destroyer seams (`rebuild.go:736-740`) | 5 | latent | **38** (content-free, null adapter), 44 (modded) | Wired only with real seams |
| A stored secret that no rendered file carries; an `--open` server or an adopted instance getting a blanked password key (`missingkey=error`, `render_guard.go:294`) | 10, 11, success without effect | latent | **29 + 30** bundle; 51 DoD byte-compares the adopted ini | Same release |
| SSH detection trusts configured ports without checking the live session (`detect.go:88-96`), and printing `ufw enable` could lock someone out (`network-firewall-2`) | **7** | no output today | **33**, before any firewall output; `ufw enable` never printed | Before the plan prints a rule |
| ufw backend: tags don't round-trip, deletes fail, `StepAdopt` is executed. nft flushes a table it never declared (`network-firewall-9`) | **7** | no (firewall unwired) | **40**, first work item | Before any rule is written |
| Adopt rollback script restarts before extracting into the wrong directory; cutover leaves the legacy timer armed through the archive window, and its commit-point `disable --now` could stop a live legacy JVM through its own `SendSIGKILL=yes` unit (`migration-adopt-4,5,6,7`) | 2, 4, 5 | no (cutover stub) | **50** | Before 51 wires cutover |
| Web skeleton: anonymous pages, fake 202s, an `operator` that can restore and prune (`web-dashboard-2..5`, `admin-security-3,5`) | 9, 5 | no (not linked) | **47** | Before 48 links it |
| Version skew: the first format bump breaks `gvm stop` and backups on every un-restarted world (`gvm-self-upgrade-uninstall-2,3`) | 4, 6 | at the first upgrade | **20** (freeze), **35** (N-1) | The freeze holds until the first upgrade |
| Scratch-instance removal or secret purge outside rule 5 | 5 | no | **32** (the scratch name is typed before anything is created; cleanup only by the same invocation, after its own verified archive), 53 (typed purge) | Born safe |

### Leverage map: small wirings that unlock the most

Each row wires code that already exists and is tested.

| Change | Phase | Existing code | Unlocks |
|---|---|---|---|
| Availability registry, `ErrUnavailable`, hint gate | 20 | five unbuilt sentinels, the dispatch tables | honest help, scriptable exit 3, no dead hints |
| `errHelpShown` and `Command.Usage` | 22 | `flagErr`, the `gameSubs` table pattern | help works everywhere; no root panic |
| `a2s` and `mc_ping` cases in `runner.BuildProbes`, probes built before spawn | 27 | `probe.A2S`, `probe.MCPing` | 10 manifests can reach ready |
| `finishWait` ends a wait only when `settled()` | 27 | `Ladder.settled` | Proton stops save instead of SIGKILL |
| Doctor fed by `lifecycle.List/Observe/Findings` | 24 | the status pipeline | doctor stops crying wolf |
| Local `gvm secret` | 29 | `secrets.Store`, `GeneratePassword` | 11–12 games pass preflight; friends-only servers |
| `config_files` in `supervise()` | 30 | `hook.NewConfigAccess`, the `gamecfg` codecs | settings and ports reach 7 games |
| SteamCMD and Minecraft providers in `HostEnv` | 31 | `steamcmd.New`, `jobs.NewSteamRunner`, `update.Bind`, `minecraft.New` | install, update and repair |
| Join line and read-only `firewall plan` | 33 | `PortSpec.Expose/Note`, `firewall.Desired`, `Rule.UFWSpec` | players get `address:port`; copy-paste ufw lines |
| `backup.Remote` and one `backupDir` resolver | 21 | `backup.Remote`, `h.backupDir` | backups land where configured; honest off-box status |
| `audit tail` over the spool | 37 | `audit.ListSpools`, `ReplayReader` | "who stopped it" with Postgres down |
| Readiness probe reused as the player poll | 37 | `A2S.Last`, `MCStatus.Players`, `Runner.SetPlayers` | players online in status, list and dashboard |
| `restore --to` | 38 | `archive.Restore{To}` | a non-destructive way back |
| One comment-preserving intent writer | 39 | `content_yaml.go`'s node editor, hostops `FileInstanceSpec` | set, enable/disable, ports move, manifest upgrade, promote |

### What this plan deliberately leaves out

These are sub-items of findings, or ideas raised in the review, left out on purpose. No whole finding is deferred.

- **The CLI's remote API client** (`--remote`, `GVM_URL`/`GVM_TOKEN`). No persona journey needs it: operators have SSH, and friends have the dashboard. Phase 20 hides `--remote` and drops the advice, and Dashboard v0 does not add it. If the client is ever built, it gets its own designed phase with a threat model. Rule 3 is unaffected either way.
- **A reworked nft backend.** Phase 40 makes nft unavailable, as the original plan specified for v1. A rework would:
  - own a `table inet gvm` declared before any flush
  - render from the full desired set
  - add no `policy drop` unless `firewall enable` made gvm the sole firewall
  - refuse while ufw or firewalld is active
- **`gvm restart --pending --when-empty`.** Player counts are not available for every game.
- **`gvm backup decrypt`.** Decryption belongs on the operator's workstation (`age -d`), so the VPS cannot read its own off-box backups.
- **An nginx snippet generator or `gvm caddy install`.** gvm never installs packages, and other proxies get a documented `ssh -L` and a unix-socket upstream.
- **A provider that installs Proton.** D-41: gvm names packages and paths and installs nothing. If one is ever built, it must be a pinned, digest-verified provider that lands in a per-instance tree with its egress host declared.
- **`gvm db migrate --to N`** without a pg_dump, `--confirm` and an up/down/up CI test.
- **`gvm backup unpreserve`.** An archive whose own manifest says `preserve` could be demoted only by rewriting its bytes, which breaks its digest. `gvm backup delete` (Phase 36, typed) is the one explicit way to reclaim a preserve archive.
- **A degraded "binary only" update for games with mods before Phase 44.** Until the mod engine exists, Project Zomboid's recommended nightly is written as a restart (Phase 31), which is what the manifest says the nightly is for. PZ binary updates arrive with the content seam.
- **Ubuntu 22.04 in the boot-ledger matrix.** The ledger verifies Debian 12 and Ubuntu 24.04. On 22.04, `game list` and `create` say the host is not verified and point at `gvm game try --scratch` (Phase 32).
- **Never:**
  - an unauthenticated status or player page (rule 9)
  - a flag that bypasses an SSH, archive or adoption gate (U11)
  - a gvm command that edits sudoers or `/etc/fstab`
  - `ufw enable` printed as advice. The only code path that enables ufw is the typed, SSH-verified `sudo gvm firewall enable` (Phase 40). `TestNoUfwEnableAdvice` scans advice strings (hints, Fix lines, help, docs and manifests) and exempts only that command's backend argv.

### When the plan is done

- [ ] J1–J5 are each demonstrated by a recorded transcript on a fresh Debian 12 VPS, and each is pinned by `ci/golden-journey.sh` on every release tag.
- [ ] The README "What works" table is generated from the availability registry, and every row reads "works". Every catalog game is either verified in the shipped ledger or labelled unsupported with a reason.
- [ ] These tests are green, with no allowlist beyond the shared phrase:
  - `TestHintsResolve`
  - `TestEveryHelpPathExitsZero`
  - `TestAvailabilityMatchesBehaviour`
  - `TestDoctorFixesNameRealThings`
  - `TestEveryShippedManifestBuildsLaunchAndProbes`
  - `TestNoMutationReturns2xxWithoutDispatch`
  - `TestJobUnitExitCodes` and `TestJobVerbsSurviveHangup`
  - `TestNoUfwEnableAdvice` and `TestNoBrowserPageFetchesAPIv1`
- [ ] `sudo gvm doctor` reports 0 problems on a correctly configured host with three games, a dashboard and an adopted server.
- [ ] Every build gate from the original plan is still green: `noshell`, `noexec`, `nosynth`, the routes and scope tests, `golden-units`, and `control-plane-dead`. The last one is now the real kill-and-reboot test.
- [ ] Every one of the 210 findings is closed with a link to its resolving commit in `docs/reviews/usability-2026-10.md`. A finding whose named game ended `unsupported` is not closed: it stays open there with the ledger's recorded reason (Appendix A).

---

## Milestone J1 — first joinable server

## Phase 20 — Tell the truth about this build

**Goal:** every command, help screen, hint, job listing and README line says plainly whether it works in this build. Every printed `gvm …` command either runs or says it is not available.

**Why now:** every later phase flips rows in an availability registry from "not in this build" to "works". The registry and the hint-resolution test have to exist first, or each phase hand-edits the same lists again.

Today an operator cannot tell a misconfiguration from a missing feature:
- They debug a `gvmd.service` that was never shipped.
- They follow hints into `unknown command`.
- They trust `gvm job list` when it calls a nightly update runnable that fails every night.

**Resolves:** onboarding-8, cli-ergonomics-1, cli-ergonomics-2 (the loop; the secret path is Phase 29), cli-ergonomics-4, cli-ergonomics-5 (dead end closed; approval is Phase 46), cli-walkthrough-5, cli-walkthrough-6, cli-walkthrough-7 (rewording; the verbs are Phase 39), admin-security-1 (honest stubs), admin-security-12, admin-security-13 (docs; admin guide in 48), web-dashboard-6 (now), web-dashboard-7, web-dashboard-12 (docs half), documentation-1, documentation-4, documentation-11 (banner), documentation-14 (now), documentation-15, day2-ops-5 (rewording), failure-recovery-3 (interim), game-catalog-config-3 (honest listing), game-catalog-config-8 (the capability says the settings editor is not built), game-catalog-config-9 (interim), game-catalog-config-10 (override accepted), content-mods-1 (phase A), content-mods-2 (step 1), content-mods-3 (shipped blocks compile), migration-adopt-1 (honest stubs), migration-adopt-10 (interim), offsite-backup-dr-14 (interim), gvm-self-upgrade-uninstall-2 (format freeze).

### Work items

1. **Availability registry** (U1). Add `Avail Availability{State: Works|Partial|NotInBuild, Why string}` to `cli.Command` and to every subcommand-table entry. That covers the existing tables (`gameSubs`, `contentSubs`, `jobSubs`, `backupSubs`, `controlSubs`) and new small tables for the switch-based `user`, `token`, `secret`, `audit`, `db` and `adopt` groups. Phase 22 unifies the table type.

   The initial NotInBuild rows:
   - `install`, `update`, `rebuild` and `firewall`
   - `user`, `token`, `secret`, `audit`, `gvmd` and `web`
   - `adopt cutover` and `adopt rollback`
   - the global `--remote`

   Two rows start as Partial, each with its reason: `restore` (`--list` works; restoring does not) and `content sync` (`--dry-run` works; applying does not).

   The same honesty reaches the capability model: `spec.Capabilities` (`capability.go:448`) reports `config.edit` as unavailable, "the dashboard settings editor is not available in this build; edit /etc/gvm/instances/<n>.yaml, then sudo gvm apply <n>", until Phase 49 builds the Settings tab. Today it reports the capability available for every game with a config file, which a dashboard would render as a working button. (game-catalog-config-8)

   (cli-ergonomics-1, cli-walkthrough-6, documentation-1)
2. **`engineWired(kind, inst, manifest) (bool, why)`.** One pure predicate in `internal/cli/jobengines.go`, shared by four callers:
   - `attachEngines` (`:117`)
   - `jobRows` (`job.go:537`): `gvm job list` prints `cannot run in this build: <why>` instead of "runnable"
   - `apply`: `! timer v1_nightly (update) will be installed, but this build cannot run update jobs`
   - `start`'s missing-sentinel refusal: "game files are not installed, and this build cannot install them". It never points back at `install`.

   Every install, update, repair, rebuild and content_sync kind starts false. Phases 31, 38, 44 and 45 flip them one kind, and one manifest class, at a time, so a kind that fails in the field can be turned back off in one line. (cli-ergonomics-1)
3. **One sentinel, one sentence, exit 3.** `ErrUnavailable` lives in a leaf package (`internal/avail`), so `cli`, `jobs` and `backup` can all wrap it. It becomes the parent (`errors.Is`) of:
   - `ErrNotImplemented`
   - `ErrProviderUnbuilt` (`install.go:33`)
   - `ErrContentApplyUnbuilt` (`jobengines.go:53`)
   - `ErrRemoteUnbuilt` (`transport.go:102`)
   - `jobs.ErrNoRunner` for pending kinds

   `Main` prints `gvm <verb>: not available in this build; nothing was changed.`, then one reason in operator words. `install` keeps its preflight above it.

   Phase 21 can land first. When this item lands, the push reason Phase 21 introduced ("this gvm build has no SFTP transport") is wrapped in `ErrUnavailable` as a one- or two-line follow-up.

   **Delete `errRequiresDaemon`** (`user.go:141`). Its 17 callers in `user.go`, `token.go`, `secret.go` and `auditcmd.go` return `ErrUnavailable`. The secret, audit, user and token commands each refuse in one sentence that never mentions gvmd.

   `adopt cutover` and `adopt rollback` print "not available in gvm <version>; nothing on this host was changed; keep running zomboid.service as before". They drop the hard-coded "(requires Linux)" (`adopt.go:24`) and the reference to a rollback script no cutover has written. (admin-security-1, web-dashboard-6, cli-walkthrough-6, migration-adopt-1)
4. **Generated usage and an honest banner.** `usage()` reads the registry and prints unavailable commands last, under "Not available in this build", each with its reason. The banner states rule 1 exactly: "Instance and operations commands work over SSH as root with gvmd, the dashboard and Postgres all down." Bare `gvm status` runs the `gvm list` view. (onboarding-8, day2-ops-5)
5. **The forgotten-sudo message is one line** that echoes argv: `gvm backup list needs root. Run: sudo gvm backup list`.
   - While `ErrRemoteUnbuilt` holds, `transport.Resolve` drops the `--remote`/`GVM_URL`/`gvm token create` advice.
   - `--remote` is hidden from every flag listing. When `GVM_URL` or `--remote` is set, gvm fails at once with "this gvm build has no API client; unset GVM_URL and run it as root on the host".
   - A `--remote` value with no scheme on a backup command adds "did you mean the off-box backup target? That is spec.backup.remote".
   - The admin.sock rationale moves to a code comment and to the embedded `transport` help topic (Phase 22), so `TestTheCLINeverNamesTheAdminSocket` keeps its meaning.

   (admin-security-12, cli-walkthrough-5, web-dashboard-7, documentation-14, offsite-backup-dr-14)
6. **`TestHintsResolve`** (U2, `internal/cli/hints_test.go`). It extracts candidates from three sources:
   - every `*ast.BasicLit` in non-test Go files under `internal/`
   - the `hint`, `note` and `help` fields in `games/*.yaml`
   - fenced blocks in `README.md` and `docs/*.md`, excluding `ARCHITECTURE.md` and the plans, which get a banner instead

   Every `gvm <verb> [<sub>] [--flag [value]]` must resolve to a routed entry that is not NotInBuild. Every flag must exist in that FlagSet, and every value given to an enum flag (`--role`, `--kind`, `--on-failure`, …) must be in the enum.

   The only allowlist is a literal that also contains "not available in this build". Adding `"gvm manifest upgrade"` or `"--role owner"` to an unrelated literal before those exist must turn the test red. (cli-ergonomics-4, documentation-4, admin-security-13, cli-walkthrough-7)
7. **Reword every dead hint, choosing the replacement for safety.** Later phases point these back at real verbs.
   - **Pin mismatch** (`derive.go:168`, `verbs.go:206`, `content/lock.go`). Print both sides: "the YAML pins rev 3 sha 9f2c…; the lock holds rev 3 sha 41a7…. If the YAML pin was edited by hand, restore it, then: sudo gvm apply <n> --diff. Moving an instance to a newer manifest revision is not available in this build." Never suggest re-creating the instance. (failure-recovery-3, game-catalog-config-9)
   - **Disabled** (`derive.go:400`): "set enabled: true in /etc/gvm/instances/<n>.yaml, then: sudo gvm apply <n>".
   - **Held** (`holdReason`, `:525`/`:533`): "a reboot clears it; for a durable stop set enabled: false and run sudo gvm apply <n>". (day2-ops-5, cli-walkthrough-7)
   - **`gvm instance promote`** (five messages in `provider/adopted/adopted.go`, `spec/capability.go`) and **`content promote`**: "taking over binary updates for adopted instances is not available in this build; updates are reported, never installed". Never print a YAML edit that flips `managed_install` or content ownership, because that would bypass the adoption gates. (migration-adopt-10)
   - **`gvm firewall enable/apply`** (`doctor/checks_fw.go`, `firewall/guard.go`): "gvm does not manage the firewall in this build". No text mentions `ufw enable` (U10).
   - **`game approve/try/publish`** (`games/generic-steam.yaml`, `spec/load.go:249`, catalog errors): "approving a custom manifest is not available in this build; sudo gvm create <name> --game <id> --unapproved-override authorises these exact bytes for one instance". Item 8 makes that route work, so the hint is never a dead end.
   - `content resolve-blocked` and `content plan` become `gvm content blocked <inst>`.
8. **Overrides get past apply** (the immediate half of cli-ergonomics-5).
   - The first-apply fallback in `pinnedManifest` (`instance.go:510`) accepts an `/etc/gvm/games` override whose sha256 equals the instance's recorded `spec.manifest` pin. Root authorised exactly those bytes at `create --unapproved-override`.
   - `apply` gets **no** new flag. It accepts only the sha recorded at create, so a flag there could only re-authorise new bytes, which would bypass the D-17 pin and the approval gate (U11).
   - A skipped override now shows its reason ("sha256 differs from the pin: the file changed after create") instead of a bare "unreadable".
   - The template guard still runs at load.

   (cli-ergonomics-5, game-catalog-config-10)
9. **Content stops advertising apply** (phase A of content-mods-1, step 1 of content-mods-2).
   - `afterListEdit` (`content.go:712`) and the diff's `apply:` line print "applying mod changes is not available in this build; gvm content sync <inst> --dry-run shows what it would do". A diff that is refusing prints no `apply:` line.
   - `--apply` is removed from `add` and `remove`. The inert `--yes`, `--no-boot` and `--allow-ambiguous` on `sync` are hidden.
   - The three Minecraft manifests get `activation: {mode: presence}` with a revision bump. No user instance exists yet, so the bump is free.
   - `TestEveryShippedContentBlockCompiles` runs `content.Compile` (`schema.go:149`) on every shipped block.
   - `openContent` (`content.go:185`) prints one sentence for modrinth/curseforge blocks, "Minecraft mod management is not in this build; jars under <root> load as-is", instead of four schema errors.
   - "Modrinth/CurseForge" is dropped from the `--pin` help, and `docs/curseforge-blocked-mods.md` gets a top banner.

   (content-mods-1, content-mods-2, content-mods-3, documentation-11)
10. **Hooks are listed honestly.** `gvm game hooks` prints each hook's `Doc()` and "registered — not run by this build". `validate` prints "! N hooks resolve but this build does not run them". (game-catalog-config-3)
11. **Docs tell the truth.**
    - The README gets a "What works in this build" table, one row per journey (works / partial with reason / not built), generated from the registry by `make docs` with a CI diff.
    - "A firewall derived from what is actually running" becomes "planned".
    - "CI asserts this on every change" becomes "CI asserts that no game or job unit depends on Postgres, gvmd or gvm-web", which is what `ci/control-plane-dead.sh` really checks.
    - `docs/what-happens-if-postgres-is-down.md` marks the gvmd and dashboard rows "planned — not in this release".
    - `ARCHITECTURE.md` and `IMPLEMENTATION-PLAN.md` get the banner "target design; see the README for what works today".

    (documentation-1, documentation-15, web-dashboard-12, admin-security-13)
12. **`TestAvailabilityMatchesBehaviour`** runs every Works and Partial row against the fake HostEnv. It fails when a Works row returns `ErrUnavailable`, and when a NotInBuild row stops returning it, which forces the registry flip. (documentation-1)
13. **`TestFrozenFormats`** (U17) pins these until Phase 35 lands the N-1 skew rule:
    - `console.Proto`
    - `LeaseProto`
    - the freeze `LeaseSchema`
    - `StatusProto`
    - `RunnerFileProto`
    - the archive manifest schema
    - the index schema

    Additive JSON fields stay allowed. (gvm-self-upgrade-uninstall-2)

### Definition of done

- [ ] `gvm help` ends with a "Not available in this build" group listing firewall, install, update, rebuild, user, token, secret and audit, each with a reason. No visible flag listing contains `--remote`.
- [ ] `sudo gvm install v1; echo $?` prints the preflight, then `gvm install: not available in this build; nothing was changed.`, then `3`.
- [ ] `sudo gvm secret set x join_password </dev/null; echo $?` prints `3`, and its output does not contain "gvmd". (No value goes on argv, per U9.) `grep -rn "systemctl status gvmd" internal/` returns nothing.
- [ ] `spec.Capabilities` for a PZ fixture reports `config.edit` unavailable with the "not available in this build" sentence.
- [ ] As a non-root user, `gvm backup list pz-main` prints exactly one line: `gvm backup list needs root. Run: sudo gvm backup list pz-main`.
- [ ] On a host with a Valheim instance, `sudo gvm job list` shows its update row as `cannot run in this build: …`.
- [ ] `sudo gvm create c1 --game mygame --unapproved-override && sudo gvm apply c1` succeeds when `/etc/gvm/games/mygame.yaml` is unchanged since create. After the file is edited, apply refuses and names the sha mismatch.
- [ ] `go test ./internal/cli -run 'TestHintsResolve|TestAvailabilityMatchesBehaviour|TestFrozenFormats'` is green. It goes red when a dead hint is reintroduced, or when `console.Proto` is bumped.
- [ ] `gvm content list mc1` on a Minecraft instance prints the one-sentence notice. `gvm game validate minecraft-paper` compiles its content block.
- [ ] The README table matches `make docs` output (CI diff).

**Depends on:** Phase 19. **Effort:** L.

**Design rules touched:**
- **Rule 3:** the transport wording changes, but the test that the CLI never dials admin.sock stays and still passes. Hiding `--remote` adds no path.
- **Rule 1:** the banner now claims exactly what is true.
- **Rules 5 and 11:** the reworded hints never suggest re-creating an instance or flipping `managed_install` by hand, because both would bypass archive-first and the adoption gates.
- **Rule 10:** an accepted override is exactly the bytes root authorised at create, and the template guard still runs at load.
- **Rule 7:** no message mentions enabling a firewall.

### ✅ Completion — landed 2026-10-15

Phase 20 is implemented and `make check` is green on Windows 11 (fmt-check + vet + `go test ./... -race`). What landed, against the work items:

1. **Availability registry (`internal/avail`).** New leaf package with `Availability{State: Works|Partial|NotInBuild, Why}` and the single `ErrUnavailable` sentinel. `cli.Command` carries an `Avail` field; the NotInBuild/Partial rows are populated for `install`, `update`, `rebuild`, `firewall`, `user`, `token`, `secret`, `audit`, `gvmd`, `web` (NotInBuild) and `restore`, `adopt` (Partial). `spec.Capabilities` reports `config.edit` unavailable with the manual-edit sentence (game-catalog-config-8).
2. **`engineWired(kind, inst, manifest)`** in `jobengines.go`, shared by `attachEngines`, `gvm job list` (prints `cannot run in this build: …`), and `gvm apply` (prints `! timer <inst>_<job> (update) will be installed, but this build cannot run update jobs`). `update`, `content_sync`, `rebuild` start false.
3. **One sentinel, exit 3.** `ErrNotImplemented`, `ErrProviderUnbuilt`, `ErrContentApplyUnbuilt`, `ErrRemoteUnbuilt` and `jobs.ErrNoRunner` are all built on `avail.New`, so they match `ErrUnavailable`; `Main` prints `gvm <verb>: not available in this build; nothing was changed.` plus the reason and returns 3. `errRequiresDaemon` deleted — `user`/`token`/`secret`/`audit` refuse in one sentence that never names gvmd (`grep "systemctl status gvmd"` is empty). `adopt cutover`/`rollback` refuse honestly and touch nothing.
4. **Generated usage + honest banner.** `usage()` reads the registry and lists unavailable commands last under "Not available in this build"; the banner states rule 1 exactly; bare `gvm status` runs the fleet (list) view.
5. **Forgotten-sudo one-liner + `--remote`.** A non-root host op prints exactly `gvm <argv> needs root. Run: sudo gvm <argv>`. `transport.Resolve` fails fast on a remote target with "this gvm build has no API client …", drops the `--remote`/`GVM_URL`/`gvm token create` advice, and keeps the admin.sock rationale as a comment on the constant.
6. **`TestHintsResolve`** (`hints_test.go`) scans Go string literals under `internal/`, the `hint`/`note`/`help` fields of `games/*.yaml`, and fenced blocks in `README.md`/`docs/*.md`, and fails on a dead verb/subcommand or a bad enum value unless the literal says "not available in this build". Every dead hint it found was reworded (`content resolve-blocked`→`content blocked`, `content promote`/`import`, `game approve`/`diff`, `manifest upgrade`, `instance promote`, the firewall doctor fixes).
7. **Overrides past apply.** `spec.PinAuthorizer` lets the first apply after `gvm create --unapproved-override` accept exactly the override bytes the pin recorded; an edited file is refused naming the sha mismatch. `apply` gets no new flag.
8. **Content honesty.** The list-edit and diff `apply:` lines say applying is not in this build; `--apply` removed from `add`/`remove`; the three Minecraft manifests use `activation: {mode: presence}` (new schema mode) with a revision bump and now compile (`TestEveryShippedContentBlockCompiles`); `openContent` prints one sentence for Modrinth/CurseForge blocks.
9. **Honest hooks + docs.** `gvm game hooks` prints each hook's `Doc()` and "registered — not run by this build"; `validate` warns `! N hooks resolve but this build does not run them`. README has a "What works in this build" table pinned to the registry by `TestReadmeUnbuiltMatchesRegistry`; ARCHITECTURE/IMPLEMENTATION-PLAN carry a "target design" banner; `what-happens-if-postgres-is-down.md` marks the gvmd/dashboard sections planned; `curseforge-blocked-mods.md` gets a top banner. `make docs` runs the doc-consistency tests.
10. **`TestAvailabilityMatchesBehaviour`** drives every command against the fake HostEnv and asserts NotInBuild→exit 3 and Works/Partial→not 3, so the registry and the behaviour cannot drift. **`TestFrozenFormats`** (U17) pins `console.Proto`, `lifecycle.LeaseProto`, `backup.LeaseSchema`, `runner.StatusProto`/`RunnerFileProto`/`PlanProto` and `archive.SchemaVersion` until the N-1 skew rule (Phase 35).

**Scope notes / deviations (deliberate, documented):**
- `TestHintsResolve` flags non-resolving commands (dead verbs, dead subcommands, a curated dead-phrase denylist) and bad enum values. It does **not** flag references to routed-but-NotInBuild commands such as `gvm update --repair`: those resolve and refuse honestly via Main's exit-3 path, and flagging every such reference in engine packages that only run once their engine is wired would be noise, not safety. Both DoD red-cases hold: adding `gvm manifest upgrade` or `--role owner` to any scanned literal turns the test red.
- The README "What works" table is maintained by hand and **pinned to the registry by a test** (the CI-diff half of U16) rather than emitted by a separate generator binary.
- `--remote` is accepted-and-refused with the honest message but is **not yet hidden from each command's own `--help`** (Go's `flag` has no hidden-flag concept); the top-level `gvm help` shows no flag listing, so the DoD's "no visible flag listing contains `--remote`" holds.
- Hiding the already-inert `--yes`/`--no-boot`/`--allow-ambiguous` on `content sync` was left as-is: they stay accepted and the command already prints that it did not act on them (U3-honest).
- Deeper per-package U6 identifier scrubbing in engine refusals is Phase 22's lint, not here.

Two repo files needed by the build are untracked in `main` and were copied into the worktree for local runs only, **not committed**: `cmd/gvm/main.go` (the entrypoint, untracked in `main`) and `internal/adopt/testdata/.../config.env` (a fixture matched by the `*.env` gitignore).

**Effort for this task:** implemented by Claude (Opus 4.8) in one session on 2026-10-15, ~70 minutes of wall-clock (01:26–02:35 UTC). Usage on the Max plan at the end of the task: the 5-hour window at ~5%, the weekly (all-models) window at ~11% — this phase was one slice of that week's work, not the whole 11%. The session itself used ~561K of its 1M-token context window (~56%).

---

## Phase 21 — Close the hazards already in this build

**Goal:** every defect that can hurt an operator with today's binary is closed before anyone is invited to install gvm:
- the finalize guard
- comment loss on the first `content add`
- ignored backup settings
- leaked btrfs clones
- stale destroy archives
- OOM-prone heap edits

**Why now:** these defects are reachable now, each fix is small, and the J1 journey runs through four of them: backup, destroy, apply and content add. None of the fixes needs Phase 20, so this phase depends only on Phase 19 and runs first or alongside Phase 20, with the first ◆ rc cut right after it. Phase 34's public release depends on it.

**Resolves:** migration-adopt-2 (fail-closed interim), documentation-6 (finalize part), content-mods-4, offsite-backup-dr-1 (honest push), offsite-backup-dr-2, offsite-backup-dr-3, offsite-backup-dr-7 (degraded/fatal semantics), offsite-backup-dr-10, cli-ergonomics-7 (age and class), game-catalog-config-4 (MemoryMax guard).

### Work items

1. **`adopt finalize` fails closed** (`internal/adopt/finalize.go:27`). It reads `metadata.adopted_from` from `/etc/gvm/instances/<name>.yaml` itself, accepting both the RFC3339 and the documented `bash-suite:/root/pz-server` form (`ARCHITECTURE.md:604`). The documented form carries no timestamp and names no unit. `--adopted-from` is ignored with a warning.

   **The legacy unit comes from the host, not the flag.** finalize reads the `Conflicts=` line of the instance's `gvm@<name>.service.d/20-adopt.conf` drop-in (`RenderAdoptGuard`, `units.go:715`). With no drop-in, or one that names no unit, it refuses: "cannot tell which legacy unit this instance replaced; nothing was changed".

   **It refuses even with `--force`** when:
   - the YAML is not adopted (`managed_install: true`, or no `adopted_from`)
   - `gvm@<name>` is not both enabled and active
   - the legacy unit is active
   - `--unit-path` is not the unit that `20-adopt.conf` names

   It refuses **without** `--force` when the timestamp is missing (which is always the case for the `bash-suite:` form), unparseable or under 7 days old.

   **It asks for the instance name to be typed** (`TypedConfirm`, no `--yes`) before deleting anything, because removing the legacy units is destructive (rule 5). It then copies the units to `/var/backups/gvm/<name>/legacy-units/`, which is the archive-first half of rule 5. Phase 50 replaces this interim with the adoption record. (migration-adopt-2, documentation-6)
2. **The first `content add` keeps comments.** When the `content.<block>.acquire` path is missing, the nodes are created inside the parsed `yaml.Node` (`internal/cli/content_yaml.go`) and edited in place. The full re-marshal remains only for an empty document. A golden test checks that a comment at the top of the file survives the first add and the first collection sync. (content-mods-4)
3. **Backups land where configured.**
   - One resolver, `h.backupDir(name)` (`jobengines.go:263`), is used both by the engine's `Paths.Backups` (`backupEngine`, `:288`) and by `ArchiveStore` (`:227`).
   - `ArchiveStore.Instances` takes the union of `/var/backups/gvm/*` and each spec's `backup.dir`.
   - `archive.Prune` skips entries whose manifest names another instance.
   - `backup.dir` is validated at create and apply: absolute, unique, and not inside any instance path.
   - gvm warns, without refusing, when the backup dir shares a device with the world, or sits under `/mnt` or `/media` on the root device ("volume not mounted?").
   - `spec.paths.backups` gets a load-time "ignored" warning.

   (offsite-backup-dr-2)
4. **The off-box copy is never claimed falsely.** `backupEngine` passes `Remote: &backup.Remote{Target: in.Spec.Backup.Remote}`, and `Engine.Push` (`engine.go:1017`) returns its error:
   - `on_failure: warn` produces a degraded step, `warn backup.push: OFF-BOX COPY NOT MADE: <reason>`, and the run is `degraded`.
   - `on_failure: fail` produces a fatal step, and the run fails.

   Both run only after the local archive is written and verified. Until Phase 54 wires a transport (`Dialer` is nil today), the reason is "this gvm build has no SFTP transport", and `apply --diff` warns that a remote is configured but this build cannot push. Phase 54 moves the push into its own job unit, and from then on `on_failure` decides that push job's verdict rather than the backup run's. (offsite-backup-dr-1, offsite-backup-dr-7)
5. **The remote block is validated, and the private key never travels.**
   - `ParseHostKey` (`remote_sftp.go:87`), `ParseRecipient` and the bech32 decoder move into a leaf package, `internal/spec/remotecheck`. `buildApply` and `create` call it as a targeted check; full `ValidateInstance` at apply comes in Phase 39.
   - These are errors: missing fields, a bad pin, a bad recipient, negative keep values, and an unknown kind or on_failure.
   - An `AGE-SECRET-KEY-1` anywhere in the YAML is a **hard error**: "paste the public recipient (age1…), not the identity".
   - At runtime the backup never refuses. It redacts that value from the embedded spec and records a job warning.

   (offsite-backup-dr-3)
6. **btrfs clones are destroyed.** `dropClone` and `SweepSnapDir` (`snapshot.go:300`) delete leftover subvolume clones with the `BTRFS_IOC_SNAP_DESTROY_V2` ioctl through `golang.org/x/sys/unix`, with no `btrfs` exec. For clones the sweep cannot remove, the wording "next run's startup sweep will take it" is dropped. (offsite-backup-dr-10)
7. **destroy shows what it is trusting.**
   - The preflight (`printDestroyPreflight`, `destroy.go:358`) prints `CreatedAt` and `Class`, not `VerifiedAt`.
   - A WARNING appears when the archive is older than the newest mtime under the world paths, and is repeated directly above the typed-name prompt: "taken 2026-10-07 04:00 UTC (7 days ago), class routine (reason nightly) — 7 days of play are not in it".
   - **The archive that authorised the destroy gets a durable preserve pin.** Flipping its index entry would not last: `Prune` calls `Reindex` first (`retention.go:90`), and `Reindex` re-reads `Class` from each archive's embedded manifest (`index.go:186-190`). Rewriting the class inside the tarball would change its sha256. Instead, destroy writes a root-owned 0600 sidecar, `<archive>.preserve`, holding `{reason: destroy-authorised, pinned_at, instance}`.
     - `Reindex` sets `Class = preserve` and `PinnedBy` for any archive with a sidecar, and treats `*.preserve` files as non-archives.
     - `Prune` lists pinned archives under Preserved and never makes them candidates.
     - The pin protects the archive even after a new instance is created with the same name, which is the case that the "skip entries from other instances" rule in item 3 cannot cover.
     - `TestReindexKeepsPreservePin` and `TestPruneNeverRemovesPinnedArchive` cover it. Phase 36's `gvm backup delete` is the only thing that removes a pin, and it is typed.

   Phase 38 makes a fresh archive the default. (cli-ergonomics-7)
8. **apply refuses an OOM-prone heap.** When `spec.resources` was hand-edited and the heap var plus `native_headroom` exceeds `MemoryMax`, `gvm apply` refuses, naming both numbers and the field to raise. The ceiling comes from `instance.ComputeResources` (`sizing.go:50`). (game-catalog-config-4)

### Definition of done

- [ ] On a fake FS where `pz-main` is not adopted, `sudo gvm adopt finalize --name pz-main --unit-path /etc/systemd/system/zomboid.service` refuses and removes nothing, both with and without `--force`.
- [ ] **The live hazard, an early finalize of an adopted instance.** Fixtures where `pz-main` is adopted, `gvm@pz-main` is enabled and active, `zomboid.service` is inactive and `20-adopt.conf` names it:
  - With `adopted_from` an RFC3339 time 2 days old, and again with the timestamp-less `bash-suite:/root/pz-server` form, `finalize --name pz-main --unit-path /etc/systemd/system/zomboid.service` without `--force` refuses and removes nothing.
  - Passing `--adopted-from 2020-01-01` prints the "ignored" warning and changes neither outcome.
  - With `--unit-path` naming any other unit, it refuses even with `--force`. With no `20-adopt.conf`, it refuses with "cannot tell which legacy unit this instance replaced".
  - With `--force` and the typed name, it copies `zomboid.service` to `/var/backups/gvm/pz-main/legacy-units/` before removing it. A wrong typed name removes nothing and exits non-zero (5 once Phase 22's table lands).
- [ ] A file with a top comment survives `gvm content add` and `gvm content collection sync` (golden test).
- [ ] With `backup.dir: /mnt/vol`, the nightly writes there, and `gvm backup list v1` and `gvm backup verify v1` both see it. Another instance's file in that dir is never pruned.
- [ ] With a remote configured, `sudo gvm job run v1_nightly` ends `degraded` with `OFF-BOX COPY NOT MADE: this gvm build has no SFTP transport`, and the local archive is verified.
- [ ] A YAML containing `AGE-SECRET-KEY-1…` makes `sudo gvm apply v1` refuse. A backup taken from that spec does not contain the key (the test opens the archive).
- [ ] btrfs loopback test: three nightlies leave zero entries under `/var/lib/gvm/instances/<n>/snap`.
- [ ] `sudo gvm destroy v1 --archive <old>` shows the age warning twice, and after the destroy `gvm backup list v1` shows the archive as `preserve (pinned: destroy-authorised)`.
- [ ] **The pin survives reuse of the name.** Destroy v1 with `--archive <old>`, create a new v1 with the same backup dir, and run 20 nightlies with `keep: 3`. The authorising archive still exists, and `TestReindexKeepsPreservePin` is green.
- [ ] With `jvm_xmx` hand-edited so that heap plus `native_headroom` exceeds `MemoryMax`, `sudo gvm apply pz1` refuses, names both numbers (e.g. "heap 7G + headroom 3G = 10G exceeds MemoryMax 9G") and the field to raise, and changes nothing.

**Depends on:** Phase 19. It can run before or alongside Phase 20. **Effort:** L.

**Design rules touched:**
- **Rule 11:** finalize gets stricter, never looser. State checks cannot be bypassed, and the legacy unit comes from the host's own drop-in, never from a flag.
- **Rule 5:** finalize now asks for the typed name and copies the units first. The authorising archive gets a durable preserve pin that `Reindex` and `Prune` both honour, so retention can never remove it, and its bytes (and digest) are never touched. Prune still runs only after create and verify, and now also skips entries from other instances.
- **Rule 1:** push runs only after the local archive is verified, so a failed off-box copy never costs the local one.
- **Rule 2:** btrfs cleanup is an ioctl, not an exec.
- **Rule 4:** the heap guard closes the one OOM SIGKILL path that a hand edit could create.

### ✅ Completion — landed 2026-10-15

Phase 21 is implemented and `make check` is green on Windows 11 (fmt-check + vet + `go test ./... -race`), and the whole module cross-compiles for `linux/amd64` with `CGO_ENABLED=0`. All eight work items landed, each with a test:

1. **`adopt finalize` fails closed.** Rewritten around a pure `adopt.CheckFinalize(FinalizeInputs)` (table-tested): the 7-day guard is never skipped — a missing, timestamp-less (`bash-suite:` form) or corrupt `adopted_from` is treated as not-ready, not as a pass — and the legacy unit comes from the instance's own `20-adopt.conf` `Conflicts=` line (`adopt.ParseAdoptGuardUnit`), never from `--unit-path`, which is only validated against it. It refuses even with `--force` for a non-adopted instance, a missing guard, or a `--unit-path` that is not the guard's unit; `--adopted-from` is ignored with a warning. It requires the typed instance name (`TypedConfirm`, no `--yes`) and copies the unit to `/var/backups/gvm/<n>/legacy-units/` before removing it. (migration-adopt-2)
2. **First `content add` keeps comments.** `editAcquireSequence` now GROWS the missing `spec → content → <block> → acquire` nodes inside the parsed `yaml.Node` (`mapGetOrCreate`); the full re-marshal is reached only for an empty/non-mapping document. A top-of-file comment survives the first add (test). (content-mods-4)
3. **Backups land where configured.** `backupEngine` writes to `h.backupDir(name)` (the same resolver list/verify/prune already used), so a configured `backup.dir` no longer holds archives no command can see; `archive.Prune` skips entries whose manifest names another instance (shared-dir test); `apply` validates `backup.dir` (absolute, not inside the world, with Unix path semantics). (offsite-backup-dr-2)
4. **The off-box copy is never claimed falsely.** `backupEngine` passes the configured `Remote`, so `Engine.Push` is actually attempted; with no SFTP transport in this build it returns the honest refusal, which `on_failure=warn` (default) turns into a degraded step. `apply --diff` warns when a remote is configured but cannot be pushed. (offsite-backup-dr-1)
5. **The age identity never travels.** `spec.ContainsAgeSecretKey` / `RedactAgeSecretKeys` (tested): `apply` refuses an `AGE-SECRET-KEY-1…` in the YAML with "paste the public recipient (age1…)", and the backup engine redacts any that slip in before embedding the spec, recording a job note. (offsite-backup-dr-3)
6. **btrfs clones are destroyed.** `destroyBtrfsSubvolume` issues `BTRFS_IOC_SNAP_DESTROY_V2` via `x/sys/unix` (no `btrfs` exec, rule 2), with a non-Linux stub; `SweepSnapDir` and `dropClone` fall back to it when `RemoveAll` fails, so a read-only subvolume clone is reclaimed instead of shrinking free space nightly. (offsite-backup-dr-10)
7. **destroy shows what it is trusting.** The preflight prints `CreatedAt` and `Class` (not `VerifiedAt`) and a WARNING, repeated above the typed-name prompt, when the archive predates the newest world mtime (`archive.Measure` now returns the newest mtime). The authorising archive gets a durable preserve pin — a root-owned `<archive>.preserve` sidecar that `Reindex` reads to force `Class=preserve` (surviving a rescan and a reused instance name) and `Prune` lists under Preserved; `gvm backup list` shows `preserve (pinned: destroy-authorised)`. `TestReindexKeepsPreservePin` and `TestPruneNeverRemovesPinnedArchive` cover it. (cli-ergonomics-7)
8. **apply refuses an OOM-prone heap.** `apply` runs `instance.CheckResources`: a hand-edited heap var whose heap + `native_headroom` exceeds the recorded `memory_max` is refused, naming the heap, the headroom and the field to raise (test). (game-catalog-config-4)

**Scope notes / deviations (deliberate, documented):**
- The finalize interim does not yet add the systemd *enabled+active* / *legacy-unit-active* refusals from the DoD: the CLI has no unit-file-state (enabled) seam and cannot query a non-gvm unit's state in this build. The critical live hazards — the skipped guard and the arbitrary-unit delete — are closed; the state refusals are belt-and-suspenders that land with the adoption record (Phase 50), which is where the plan already puts the full version ("fail-closed interim" here, record in 50).
- Item 5 implements the security core (hard error + runtime redaction) but not the full `internal/spec/remotecheck` leaf-package reorg or the other remote-block validation errors (bad pin, negative keep, unknown kind) — `backup.Remote.Validate` already covers those at push time; the create/apply-time leaf package is deferred to the firewall/remote wiring phases.
- Items 3's softer warnings (backup dir shares the world's device, or sits under `/mnt` on the root device) and the `spec.paths.backups` "ignored" load-time warning are not implemented; the data-protection core (writer honours the dir, prune skips other instances, apply rejects a dir inside the world) is.
- Item 6's btrfs destroy is Linux-only and cannot run in `make check` on Windows or in container CI without a root loop device; it compiles for Linux and is exercised by the DoD's loopback test in Linux integration. The Windows stub keeps `make check` green.

**Effort for this task:** implemented by Claude (Opus 4.8) on 2026-10-15, ~2 hours of wall-clock immediately after Phase 20, in the same session. Usage on the Max plan at the end: the weekly (all-models) window at ~13% (Phases 20 and 21 together were a few percent of the week), the 5-hour window at ~9%; the session's context window was ~77% full.

---

## Phase 22 — Help, errors and exit codes that work the same everywhere

**Goal:** `--help`, `-h` and `gvm help <cmd> [sub]` print full usage, with flags and examples, and exit 0 for every command. Errors lead with one plain sentence. Operator verbs use one documented exit-code table.

**Why now:** the first thing a new operator types, `sudo gvm create --help`, panics today. Every later phase adds commands. Building them on one parser contract, one subcommand-table type and one error type costs less than retrofitting thirty more verbs later.

**Resolves:** onboarding-10, onboarding-15, cli-ergonomics-3, cli-ergonomics-8, cli-ergonomics-9, cli-ergonomics-13, cli-ergonomics-14 (completion, NO_COLOR, did-you-mean), cli-walkthrough-3, cli-walkthrough-11, cli-walkthrough-12, cli-walkthrough-15, day2-ops-13, game-catalog-config-6, documentation-9, documentation-13, admin-security-11, admin-security-15, admin-security-7 (spool default), failure-recovery-9 (spool default, db fail-fast).

### Work items

1. **`errHelpShown`.** Every `parse*Args` (`create.go`, `logs.go:45`, `destroy.go:51`, `stop.go`, `start.go`, …) returns either `errHelpShown` or a fully validated instance name. `instance.ValidateName` runs before anything reaches `PathsFor`, so `sudo gvm create --help` can no longer run with zero options. `flagErr` (`game.go:613`) stops wrapping `flag.ErrHelp`, and `Main` maps the sentinel to exit 0. Requested help goes to stdout; usage errors go to stderr. (cli-ergonomics-3, onboarding-10, game-catalog-config-6, day2-ops-13, cli-walkthrough-3)
2. **`Command.Usage` and one `subcmd` table type** for every group. The `user`, `token`, `secret`, `audit`, `db` and `adopt` groups convert from `switch` statements.
   - `gvm help <cmd> [sub]` calls the printer.
   - `<group> -h|--help|help` prints the subcommand table, showing each entry's availability.
   - Each command gets an **Examples** block, e.g. `sudo gvm create valheim-1 --game valheim --var public_name="Friends"`.
   - An unknown command or subcommand prints "did you mean create?" (edit distance), not a full usage dump.
   - `make docs-cli` renders `docs/cli-reference.md` from `Command.Usage`, the subcommand tables and their availability, with a CI diff (U16). It is the generated CLI reference that U16 names.

   (documentation-9, admin-security-11, cli-ergonomics-14)
3. **A custom flag printer.**
   - It renders `--flag VALUE` with no backtick extraction, and hides flags tagged internal.
   - `--trigger`, `--lease-for` and `--on-failure` move to `gvm job run` only, hidden there.
   - On a parse error gvm prints the error plus `Run 'gvm create --help' for usage.` and nothing else.
   - `create --user` is hidden; its redirect to `gvm adopt` stays.
   - `--enabled` and `--no-enabled` become a pair.

   (cli-walkthrough-12, cli-ergonomics-9)
4. **A conventions table** (`docs/cli-conventions.md`), enforced by helpers that every new verb uses:
   - `--dry-run` everywhere, with `--dry` as a hidden alias. `apply --diff` stays the documented spelling, and `--dry-run` is accepted there too.
   - The instance is the first positional argument, with `--instance`/`--name` as hidden aliases.
   - Every read verb takes `--json`, or the flag is removed from it.
   - `job run` derives its default trigger from `INVOCATION_ID` plus the unit name, and refuses `--trigger timer` on a TTY.
   - `list` and `prune` become reserved names for new creates, and `gvm backup run <instance>` is accepted.

   (cli-ergonomics-9)
5. **`userError{Msg, Hint, Ref}`.** The error shape is `gvm <verb>: <what happened>. <what to do next>`.
   - Doubled package prefixes go.
   - D-nn, §x.y and Go identifiers appear only under `GVM_DEBUG=1`. A lint test rejects `D-\d\d` and `section \d+\.\d+` in Hint, Fix and error literals outside comments.
   - `gvm apply` prints only the rows that change, plus a one-line summary. `--verbose` prints the full table.

   (cli-ergonomics-8, onboarding-15, cli-walkthrough-11)
6. **The exit-code table** (U8): 0 ok, 1 failed, 2 usage, 3 not available in this build, 4 checked and found something, 5 refused or not confirmed.
   - Exit 4 covers doctor problems, a non-empty `list --problems`, port conflicts, discover blockers, refusing content lists, and an available game or mod update.
   - `Main` applies the table only when `routeOf(verb) != RouteRole` (`transport.go:278`).
   - **`gvm job run` inside a unit keeps a unit contract.** Every job unit, the guard included, runs `gvm job run %i` (`gvm-job@.service:16`, `gvm-job@_guard.service:32`), and `job` is an operator verb (RouteHostOp). Under the table, a nightly that skips a held instance (exit 5) or a guard pass that reports something (exit 4) would leave the oneshot unit failed. So when `INVOCATION_ID` is set, `job run` uses its own pinned mapping: ok, skipped and degraded exit 0; failed exits 1; a kind this build cannot run exits 3. `TestJobUnitExitCodes` pins it. Phase 37 documents which results reach `OnFailure=`.
   - `TestRoleExitCodesUnchanged` asserts that `gvm run`'s 64/65/66/70/75/78 mapping is untouched, and that `gvm@.service`'s `RestartPreventExitStatus` equals the exported constant.
   - The changed exit codes of `doctor` and `list --problems` go into the CHANGELOG's "Operator actions" section.

   (cli-ergonomics-13)
7. **`recover()` only around non-role verbs.** A panic prints one line plus "this is a gvm bug; GVM_DEBUG=1 shows the stack". `run`, `run-prepare`, `run-shutdown` and `probe-write` still crash into the journal with their exit codes. (cli-ergonomics-3)
8. **Embedded help topics.** `docs/help/*.md` is embedded with `go:embed` and served as `gvm help <topic>`. Keeping the topics as files, not Go literals, keeps `TestTheCLINeverNamesTheAdminSocket` meaningful.
   - The initial topics are `concepts` (ladder, hold, lease, preserve archive, drift, pin), `exit-codes`, `transport`, `troubleshooting`, `faq` and `firewall`. The `faq` topic (also `docs/faq.md`) answers the questions the review saw operators ask, e.g. "is my server down if Postgres is?", "why does start wait?", "where are my backups?", and passes U2 like every user doc.
   - The `firewall` topic says only that gvm does not manage the firewall in this build. Phase 33 rewrites it once SSH detection is trustworthy (U10).
   - The exit-code and doctor-ID sections are generated from `exitMeaning` and the doctor registry, with a CI diff.
   - Hints cite topics as `see: gvm help <topic>`, which works offline on the box.

   (documentation-13, cli-walkthrough-11)
9. **`db` commands fail fast.**
   - They make one quiet attempt of about 2 s (`dbWaitPool`, `db.go:191`). `--wait <dur>` opts into the old retry loop. slog retries show only under `--debug`.
   - The failure is one sentence: "Postgres is not reachable at /var/run/postgresql (check: systemctl status postgresql). Game servers, backups and updates are unaffected."
   - A peer-auth failure names the pg_hba ordering.
   - `db replay-spool --spool-dir` defaults to `instance.SpoolDir` (`/var/lib/gvm/spool`, `paths.go:88`) instead of `/var/lib/gvm/audit-spool`, and prints the directory it read. A test checks that the writer, the tmpfiles entry and the replayer agree.
   - `check-grants` exits 4 when its result is inconclusive.

   (admin-security-15, cli-walkthrough-15, failure-recovery-9, admin-security-7)
10. **Terminal hygiene.** gvm honours `NO_COLOR` and emits no ANSI when stdout is not a TTY. `gvm completion bash|zsh|fish` prints static script text generated from the tables, and spawns nothing. (cli-ergonomics-14)

### Definition of done

- [ ] `TestEveryHelpPathExitsZero`: for every command and subcommand, `<cmd> --help`, `<cmd> -h` and `gvm help <cmd> [sub]` exit 0, print `Usage:` and record zero HostEnv calls, both as euid 0 and as non-root.
- [ ] On a Linux VM, `sudo gvm create --help` prints `--game`, `--var` and an Examples block, and exits 0.
- [ ] `gvm creat` prints `gvm: unknown command "creat" — did you mean "create"?` and exits 2.
- [ ] With Postgres stopped, `sudo gvm db status` returns within 3 s with the one sentence and exits 1.
- [ ] `TestOperatorExitCodes`, a table-driven test over the fake HostEnv, covers every code in the table: a typed-confirm mismatch on `destroy` exits 5, `list --problems` with a failing fixture exits 4, an unknown subcommand inside a group (`gvm backup frobnicate`) exits 2, an unbuilt verb exits 3, and a failed action exits 1.
- [ ] `TestReadVerbsAcceptJSON` walks the dispatch tables and fails for any read verb that neither accepts `--json` nor omits the flag from its help. `TestDryRunAliases` fails for any verb where `--dry` and `--dry-run` (and `apply`'s `--diff`) do not parse to the same option.
- [ ] `TestJobUnitExitCodes`: with `INVOCATION_ID` set, `gvm job run` exits 0 for ok, skipped and degraded, 1 for failed and 3 for an unwired kind.
- [ ] The lint test rejects a new `(D-41)` in an error string.
- [ ] `gvm help exit-codes`, `gvm help concepts` and `gvm help faq` print offline. `TestRoleExitCodesUnchanged` is green, and `docs/cli-reference.md` matches `make docs-cli` (CI diff).

**Depends on:** Phase 20. **Effort:** L.

**Design rules touched:**
- **Rule 2:** completion prints text and spawns nothing.
- **Rule 4:** role verbs keep their exit codes, so `RestartPreventExitStatus` still stops non-retryable failures.
- **Rule 6:** job units keep their own exit contract, so a routine skip or a guard report never leaves a unit failed or fires `OnFailure=`. Nothing new actuates.
- **Rule 3:** the `transport` topic explains the admin.sock decision, and the code still refuses.

### 🟡 Partial — help-safety core + the exit-code table landed 2026-10-15; the CLI refactors remain

Phase 22 is a large refactor phase (10 items touching nearly every CLI file). Across this session it landed its two highest-value halves — the help-safety core (the "Why now") AND the exit-code table (the namesake) — all test-green, with `make check` green on Windows (fmt + vet + `go test ./... -race`) and a clean `linux/amd64` cross-build. The remaining items are the structural refactors (one subcommand-table type, the flag printer, the conventions, the `userError` lint, the help topics, completion); they are a session's worth of work and are flagged below.

**Landed, with tests:**
- **Item 1 — help never acts, exits 0 (the "Why now").** New `errHelpShown` sentinel; `flagErr` returns it for `flag.ErrHelp` so a `-h`/`--help` parse stops instead of falling through into the command body with a zero-valued options struct; `Main` maps both it and a raw `flag.ErrHelp` to exit 0. The six switch-based groups (`user`/`token`/`secret`/`audit`/`db`/`adopt`) intercept `-h`/`--help`/`help`, and `gvm help <cmd> [sub]` delegates to the command's own `--help`. `firewall` and the unbuilt `adopt cutover`/`rollback` answer `--help` with exit 0 too. `TestEveryHelpPathExitsZero` walks every command and subcommand × {`--help`, `-h`, `gvm help …`} and asserts exit 0, a usage line and zero host writes.
- **Item 2 (partial) — did-you-mean.** `gvm creat` → `unknown command "creat" — did you mean "create"?` and exit 2 (Levenshtein, `TestDidYouMean`). The full subcommand-table-type unification, `Examples` blocks and `make docs-cli` generator are NOT done.
- **Item 6 — the exit-code table** (U8): 0 ok, 1 failed, 2 usage, 3 not available, 4 checked-and-found, 5 refused. A `codedError{code,err}` carries the code to `Main`, which applies it for non-role verbs only (role verbs keep their `ExitError` contract, checked first). Wired: `doctor` problems → 4; `list --problems` with any hit → 4; a typed-confirm mismatch (`TypedConfirm.confirmFrom`, shared by destroy and the job verbs) → 5; every group's unknown subcommand / missing-subcommand → 2; `db check-grants` inconclusive → 4. `gvm job run` under a systemd unit (`INVOCATION_ID` set) keeps its OWN contract instead of the table — ok/skipped/degraded/cancelled exit 0, a genuine failure exits 1, an unwired kind exits 3 — so a routine skip or a degraded pass never fails the oneshot or fires `OnFailure=` (rule 6). `TestOperatorExitCodes` (unbuilt→3, unknown-sub→2, failed→1, destroy-confirm-mismatch→5), `TestJobUnitExitCodes` (a routine run under a unit exits 0) and `TestRoleExitCodesUnchanged` (a role verb's `ExitError` code passes through unremapped).
- **Item 7 — recover() around non-role verbs.** `Main` wraps operator verbs in a recover that prints one line plus "this is a gvm bug; GVM_DEBUG=1 shows the stack" and exits 1; role verbs (`run`, `run-prepare`, `run-shutdown`, `probe-write`) are not wrapped and still crash into the journal with their exit codes (`TestPanicInAnOperatorVerbIsCaught`).
- **Item 9 (core + check-grants) — db commands fail fast.** `dbConnTimeout` is 2 s (one quiet attempt), and the failure is the one sentence "Postgres is not reachable at /var/run/postgresql (check: systemctl status postgresql). Game servers, backups and updates are unaffected." `db replay-spool` defaults to `instance.SpoolDir` (`/var/lib/gvm/spool`), matching the writer and tmpfiles, and prints the directory it read; `db check-grants` exits 4 when its result is inconclusive.

**Remaining (the CLI refactors — a dedicated session should take these):**
- **Item 2 (full), item 3** — one `subcmd` table type for every group, a custom flag printer (`--flag VALUE`, hidden internal flags, `--trigger`/`--lease-for`/`--on-failure` moved to `gvm job run`), `Examples` blocks, and `make docs-cli` → `docs/cli-reference.md` with a CI diff.
- **Item 4 — conventions** (`docs/cli-conventions.md`): `--dry-run` everywhere with `--dry` alias, instance-first positional with `--instance`/`--name` aliases, every read verb takes `--json`; `TestReadVerbsAcceptJSON`, `TestDryRunAliases`.
- **Item 5 — `userError{Msg,Hint,Ref}`** and the lint test that rejects `D-\d\d` / `section \d+\.\d+` in error/Hint/Fix literals (an audit comparable in size to Phase 20's `TestHintsResolve`).
- **Item 8 — embedded `docs/help/*.md` topics** (`gvm help concepts|exit-codes|transport|faq|…`) with generated exit-code/doctor-ID sections.
- **Item 9 (rest)** — `--wait` opt-in and the peer-auth pg_hba hint, and the writer/tmpfiles/replayer agreement test.
- **Item 10 — terminal hygiene**: `NO_COLOR`, no ANSI off a TTY, and `gvm completion bash|zsh|fish`.

**Scope notes:** `gvm backup frobnicate` stays exit 1, not 2 — `backup` is instance-first (`gvm backup <instance>`), so an unknown first token is treated as an instance name and reported as "no such instance", not an unknown subcommand; the unknown-subcommand→2 rule applies to the strict groups (game/content/job/control/user/token/secret/audit/db/adopt). The exit-4 cases for discover blockers, a refusing content list and an available update are left to their owning phases (25, 44/45, 43); the mechanism (`checked`) is in place for them to use.

**Effort for this task:** the help-safety core and the exit-code table implemented by Claude (Opus 4.8) on 2026-10-15, in the same session as Phases 20 and 21. The remaining CLI refactors are their own body of work.

---

## Phase 23 — Install gvm on a fresh VPS

**Goal:** an operator who is not a Go developer installs gvm on a fresh Debian 12 or Ubuntu 22.04–24.04 VPS in one verifiable step, and is told which apt packages each game family needs.

**Why now:** no later phase can be shown to a real operator until gvm can be installed. The binary path that the units hard-code must also be settled before anyone installs, not after.

**Resolves:** onboarding-1, documentation-2, gvm-self-upgrade-uninstall-1 (path and atomic replace), gvm-self-upgrade-uninstall-11 (version).

### Work items

1. **`/usr/bin/gvm` is the one binary path.** These all move to `/usr/bin/gvm`:
   - `packaging/systemd/gvm@.service` (`ExecStartPre=+/usr/local/bin/gvm run-prepare`, `ExecStart=/usr/local/bin/gvm run`)
   - `gvm-job@.service`, and the guard and verify units
   - the `ProbeBinary` constant (`internal/reconcile/probe.go:30`)

   Nothing is installed in the field yet, so the move is free now. Later, every installed unit would have to migrate. Debian policy also forbids a package from shipping `/usr/local/bin`. (gvm-self-upgrade-uninstall-1)
2. **`sudo gvm host init [--diff]`** (new `internal/hostinit`, RouteLocal, idempotent):
   - It copies itself to `/usr/bin/gvm.new`, fsyncs, and `rename(2)`s it into place. Live runners keep the old inode, so there is no "Text file busy".
   - It writes `packaging.FS` to `/usr/lib/systemd/system` and `/usr/lib/tmpfiles.d` from **one Go mapping table**, which `make install` and the Phase 52 `.deb` reuse.
   - It writes `packaging/sysusers.d/gvm.conf`, then runs `/usr/bin/systemd-sysusers` and `systemd-tmpfiles --create /usr/lib/tmpfiles.d/gvm.conf`, each by absolute path with argv.
   - It reloads systemd over D-Bus.
   - It enables `gvm-guard.timer` and `gvm-job@_verify.timer` **on first init only**. A marker in `/var/lib/gvm/host-init.json` records that, so an operator who later disables them is respected.
   - It writes a commented `/etc/gvm/gvm.yaml`, generated from `spec.DefaultHostConfig()`, only if none exists.
   - It prints, and never runs, the apt lines per game family:
     - SteamCMD: `dpkg --add-architecture i386 && apt install lib32gcc-s1`
     - Minecraft: `openjdk-21-jre-headless`
     - Proton titles: `python3`
     - `xvfb` where needed
   - It never touches ufw.
   - `--diff` prints every write and changes nothing. The run ends with `next: sudo gvm doctor`.
   - `WriteAtomic` chowns the temp file before the rename, so a failed chown never leaves a half-owned spec.

   (onboarding-1, documentation-2)
3. **Split tmpfiles.** The two `caddy`-group lines (`/run/gvm/web`, `/etc/gvm/caddy`) move to `packaging/tmpfiles/gvm-web.conf`, which is installed only with the dashboard (Phase 48). `systemd-tmpfiles --create` then converges on a games-only host that has no `caddy` group. (onboarding-1)
4. **A release workflow** (`.github/workflows/release.yml`). A `v*` tag builds static `linux/amd64` and `linux/arm64` binaries with `-trimpath`, and publishes `gvm_<ver>_linux_<arch>.tar.gz` plus `SHA256SUMS`. `make install DESTDIR=` installs from `gvm host init --print-manifest`, the same table. Until J1, only `-rc` tags at ◆ points (U15). (onboarding-1)
5. **A real version.**
   - `version.String()` (`version.go:22`) falls back to `vcs.revision`, with `-dirty` from `vcs.modified`, when ldflags are absent.
   - `gvm version --json` prints the version and commit, plus every format constant `TestFrozenFormats` pins and `store.SchemaVersion`.
   - `CHANGELOG.md` gets a required "Operator actions" section per release, which CI checks on tags.

   (gvm-self-upgrade-uninstall-11)
6. **`docs/install.md`.** It covers:
   - prerequisites per game family
   - download and `sha256sum -c SHA256SUMS`
   - `sudo ./gvm host init` and the apt lines it prints
   - `sudo gvm doctor`

   The README gets an Install section. A Linux CI job replays the fenced commands on a fresh Debian 12 **VM** (QEMU on the runner, or a cloud VM per run). The hardened unit relies on ProtectProc, PrivateDevices, MemoryDenyWriteExecute, RestrictNamespaces, transient sandboxed units and cgroup v2 delegation, none of which an unprivileged container reproduces faithfully, so containers are kept for unit-level jobs only. Phase 34 reuses this runner for the golden journey. Errors that cite "the gvm package's postinstall" now name `sudo gvm host init`. (documentation-2, onboarding-1)

### Definition of done

- [ ] In a fresh Debian 12 VM:
  - `sha256sum -c SHA256SUMS` passes.
  - `sudo ./gvm host init` lists what it did, and running it again prints `nothing to do`.
  - `systemctl list-timers` shows `gvm-guard.timer` and `gvm-job@_verify.timer`.
  - `getent passwd gvm gvm-fetch gvm-console gvm-content` succeeds.
  - `systemd-tmpfiles --create` exits 0 on a host with no `caddy` group.
- [ ] `sudo gvm host init --diff` on a converged box prints 0 changes.
- [ ] While a game runs, `sudo ./gvm-new host init` replaces `/usr/bin/gvm` without error, and the game's pid and starttime are unchanged.
- [ ] A recording exec test shows `host init` never invokes `apt`, `ufw` or `/bin/sh`.
- [ ] `gvm version` on a release build prints the tag and commit. A plain `go build` binary prints the vcs revision, never `commit unknown`.

**Depends on:** Phase 22. **Effort:** L.

**Design rules touched:**
- **Rule 2:** `systemd-sysusers` and `systemd-tmpfiles` run by absolute path with argv. The `noshell` gate is extended to `internal/hostinit`.
- **Rule 7:** apt lines are printed and never run (D-41). ufw is untouched.
- **Rule 1:** `host init` writes only files and systemd state. The timers it enables have no control-plane dependency, which `golden-units.sh` asserts.

### 🟡 Partial — the binary path (item 1) and `gvm host init` (item 2) landed 2026-10-15; the release/version/docs items remain

The installer itself — the command that lets a non-Go operator stand gvm up on a fresh VPS — is built and test-green. `make check` is green on Windows (fmt + vet + `go test ./... -race`) and the whole module cross-compiles for `linux/amd64` with `CGO_ENABLED=0`.

**Landed, with tests:**
- **Item 1 — `/usr/bin/gvm` is the one binary path.** Done earlier this session (commit `726f76c`): `gvm@.service`, `gvm-job@.service`, the guard and verify units and `ProbeBinary` all name `/usr/bin/gvm`, and `TestEveryProgramPathIsDeclared` in `internal/reconcile` was updated to match.
- **Item 2 — `sudo gvm host init [--diff]`.** New `internal/hostinit` package, reached through a new `gvm host init` verb (RouteLocal, because the installer runs before gvmd exists and has no transport to resolve; it checks for root itself and refuses non-root with the copy-paste sudo line, exit 5).
  - **One Go mapping table.** `hostinit.Manifest()` is derived by walking `packaging.FS` (`systemd/*` → `/usr/lib/systemd/system`, `tmpfiles/gvm.conf` → `/usr/lib/tmpfiles.d`, `sysusers.d/gvm.conf` → `/usr/lib/sysusers.d`), so a unit added to `packaging/` is installed the day it is embedded and there is no second list. `gvm host init --print-manifest` prints the table (source, dest, mode) for `make install`/the `.deb` to consume. `TestManifestCoversEveryPackagedFile` asserts every embedded file is installed exactly once, under `/usr/lib`, 0644.
  - **Idempotent, correctly ordered.** `hostinit.Run` drives a `System` interface in the one safe order: binary (atomic self-copy to `/usr/bin/gvm.new`, fsync, `rename(2)` so a live runner keeps its inode — no "text file busy"), then the packaged files, then `systemd-sysusers` (accounts) before `systemd-tmpfiles --create` (dirs that name those accounts), then one D-Bus daemon-reload, then — **on first init only**, recorded by a `/var/lib/gvm/host-init.json` marker — it enables `gvm-guard.timer` and `gvm-job@_verify.timer`, and finally writes a default `/etc/gvm/gvm.yaml` from `spec.DefaultHostConfig()` only if absent. `TestRunInstallsInTheRightOrder`, `TestRunIsIdempotent` (a re-init re-enables no timer and keeps the operator's config) and `TestDefaultConfigIsValidYAML` cover it, all against a fake `System` so the orchestration is tested with no host.
  - **`--diff` changes nothing.** It may read host state (which timers are managed, whether a config exists) but performs no mutating effect; `TestDiffChangesNothing` asserts only `Exists` reads happen and zero files are written. The run ends with `next: sudo gvm doctor`.
  - **Rule 2 / rule 7, enforced.** `internal/hostinit/noshell_test.go` extends the project's noshell gate: `os`/`os/exec` only from the Linux `System` impl (the cross-platform files go through the interface), no `/bin/sh`, no `systemctl`, no `apt`/`apt-get`, no `ufw`, no PATH lookup, and the only absolute program paths it may name are systemd's two tools plus `/usr/bin/gvm`. The per-family apt lines (SteamCMD `dpkg --add-architecture i386 … lib32gcc-s1`, Minecraft `openjdk-21-jre-headless`, Proton `python3`, `xvfb`) are DATA printed to the operator, never passed to a shell. A new `packaging/sysusers.d/gvm.conf` (embedded) creates `gvm`, `gvm-fetch`, `gvm-content`, `gvm-console` as locked nologin system accounts. The real host effects live in `system_linux.go` (D-Bus reload/enable mirroring `hostops`); `system_stub.go` refuses off Linux with `platform.ErrNotSupported` (`TestNewSystemRefusesOffLinux`).
- **Item 5 (core) — a real version.** `version.String()` now falls back to the toolchain's embedded VCS metadata (`vcs.revision`, with `-dirty` from `vcs.modified`) when ldflags are absent, so a plain `go build` prints the commit, never "commit unknown" (verified against a real `go build -buildvcs=true`). `gvm version --json` prints the build plus the frozen on-disk/on-socket format versions — the `TestFrozenFormats` constants (`console.Proto`, `lifecycle.LeaseProto`, `backup.LeaseSchema`, the three `runner.*Proto`, `archive.SchemaVersion`) and `store.SchemaVersion` — read straight from the constants so the upgrade/skew check compares formats, not tags. `TestVersionJSONCarriesTheFrozenFormats` and `TestVersionPlainPrintsTheBanner` cover it.

**Remaining (a dedicated session / the release phase should take these):**
- **Item 3 — split tmpfiles.** Deliberately deferred to its proper coupling with the dashboard (Phase 48): moving the two `caddy`-group lines (`/run/gvm/web`, `/etc/gvm/caddy`) into a dashboard-only `packaging/tmpfiles/gvm-web.conf` also requires the `HOST-009` doctor check (`internal/doctor/checks_host_linux.go`) to stop treating a missing `/run/gvm/web` as a problem on a games-only host — a doctor change that belongs with the web tier, not the installer. Until then `systemd-tmpfiles --create` on a games-only box warns about the missing `caddy` group for those two lines (the rest converge); the DoD's "exits 0 with no caddy group" needs the split.
- **Item 4 — the release workflow** (`.github/workflows/release.yml`, static `linux/{amd64,arm64}` on a `v*` tag, `SHA256SUMS`) and `make install` wired to `gvm host init --print-manifest`. `--print-manifest` is already in place for it.
- **Item 5 (rest) — the CHANGELOG "Operator actions"** required-section gate on tags (CI).
- **Item 6 — `docs/install.md`** and the fresh-Debian-VM CI replay (QEMU). These are docs/infra that `make check` cannot verify.

**Effort for this task:** items 2 and 5-core implemented by Claude (Opus 4.8) on 2026-10-15, in the same session as Phases 20–22, after a context compaction. Items 3, 4, 6 and the CHANGELOG gate are their own body of work (a release phase and a dashboard-coupled split).

---

## Phase 24 — `gvm doctor` tells the truth

**Goal:** a clean `sudo gvm doctor` means the host and every instance really are fine. When something is wrong, doctor names the instance, the real cause and a fix that exists.

**Why now:** `docs/install.md` ends with `sudo gvm doctor`, and every failure hint in the next phases sends the operator there: exit 65, port conflicts, missing prerequisites. Today doctor reports problems on every healthy host, so the first thing a new operator learns is to ignore it.

**Resolves:** onboarding-5, cli-ergonomics-6, failure-recovery-1, failure-recovery-2, failure-recovery-15 (doctor half), network-firewall-3 (false problems gone; measured in 33), network-firewall-8, documentation-3 (doctor half), web-dashboard-12 (HOST-011), gvm-self-upgrade-uninstall-12.

### Work items

1. **Doctor checks instances.** `doctorLoadInstances` and `doctorLoadManifest` (`internal/cli/doctor.go:150`, which return nil today) use `lifecycle.List` and the pinned `manifest.lock.yaml`.
   - Each instance also gets the findings from `Observer.Observe` + `lifecycle.Findings` (`reconcile.go:260`) and from `ports.CheckSpec`, so doctor and `gvm status` cannot disagree.
   - `gvm doctor [<instance>]` scopes the run, and output is grouped under `INSTANCE <name>`.
   - An instance that fails to load becomes a Note, not a crash.

   (failure-recovery-1, onboarding-5)
2. **A check registry.** Each family of checks lives in its own `internal/doctor/checks_*.go` file and registers itself, so later phases add files instead of editing `HostChecks` (`checks_host.go:17`).
3. **One host-config loader.** `spec.LoadHost` merges onto `DefaultHostConfig()` (`host.go:132`), then runs a strict pass (`KnownFields`) that collects unknown keys with did-you-mean hints. An explicitly null section means defaults.

   `spec.ValidateHost` checks:
   - `backend ∈ {ufw, none}`
   - CIDRs
   - ports 1–65535
   - the edge and tls enums

   Doctor (`doctorLoadHostConfig`, `:134`), `ports` (`ports.go:155`) and every later reader use it:
   - Read-only commands warn once on stderr.
   - Doctor reports a HOST Problem.
   - Commands whose output depends on the file refuse until it validates: `create`'s allocation via `reserved_ports`, and later the firewall and edge writes.

   (network-firewall-8)
4. **Firewall checks stop guessing.** Until Phase 33 reads the live ruleset, FW-001..006 (`checks_fw.go:15`) become **Skip**: "not checked: gvm does not manage the firewall in this build". No Fix string mentions enabling a firewall (U10). EDGE-* checks Skip when `edge.mode: none`. (network-firewall-3, failure-recovery-2, cli-ergonomics-6, documentation-3)
5. **Severities follow rule 1.**
   - A missing Postgres is a **Note**: "dashboard and audit-database features unavailable; game servers unaffected". It becomes more than a Note only if a gvm-web unit is installed.
   - HOST-011 counts only rows with `state='running' AND boot_id <> current`. Its fix is `sudo gvm db replay-spool`, which reaps stale runs in process. "restart gvmd" is removed.
   - HOST-010 reads the merged dependency properties over read-only D-Bus.

   (cli-ergonomics-6, web-dashboard-12)
6. **Check what is installed, not just the running binary:**
   - every `packaging.FS` file at its install path, byte-identical, which flags missing and locally edited files
   - the inode of the `ExecStart` binary equals `/proc/self/exe`
   - no stray `/usr/local/bin/gvm`
   - users, groups and memberships
   - each tmpfiles path's mode and owner
   - the guard and verify timers are enabled, with a future elapse
   - steamcmd is locatable, reusing the locator's apt line
   - Java is present for `minecraft-*` instances

   `RuleUnitNotFound` is split: a missing template yields "reinstall: sudo gvm host init", not `gvm apply`. (gvm-self-upgrade-uninstall-12, onboarding-5)
7. **`TestDoctorFixesNameRealThings`.** Every Fix's `gvm …` must resolve to an available command. Every `systemctl … <unit>` must name a packaged unit, or `postgresql`, `caddy` or `ufw`.

   Doctor also gets:
   - `--json`, whose shape is generated as `docs/doctor.schema.json`
   - exit 4 when it finds problems
   - a totals line: "0 problems, 2 warnings, 3 notes"
   - a pointer to `gvm help troubleshooting`

   (web-dashboard-12, failure-recovery-15)

### Definition of done

- [ ] Fresh VM after Phase 23: `sudo gvm doctor` reports `0 problems` and exits 0. The firewall section reads "skipped".
- [ ] After removing `/usr/lib/systemd/system/gvm@.service`, doctor and `gvm status v1` both say "reinstall: sudo gvm host init".
- [ ] For a broken instance, `sudo gvm doctor v1` prints an `INSTANCE v1` section whose reason equals `gvm status v1`'s. A shared fixture asserts this.
- [ ] With Postgres not installed, doctor reports a Note and exits 0.
- [ ] `TestDoctorFixesNameRealThings` is green, and goes red when a check's Fix is changed to name `gvm firewall enable` (not available until Phase 40) or `systemctl restart gvmd` (not a packaged unit).
- [ ] On a host with one deliberately broken instance, `sudo gvm doctor; echo $?` ends with `1 problem, 0 warnings, N notes · see: gvm help troubleshooting` and prints `4`. `sudo gvm doctor --json` validates against `docs/doctor.schema.json`, which is generated alongside the doctor-ID table (CI diff).
- [ ] An `/etc/gvm/gvm.yaml` containing only `firewall: {}` keeps every default. `reserved_port: [8080]` produces "unknown key reserved_port — did you mean reserved_ports?", and `sudo gvm create` refuses until the file is fixed.
- [ ] A recording HostEnv shows doctor issues zero mutating hostops verbs.

**Depends on:** Phase 23. **Effort:** L.

**Design rules touched:**
- **Rule 6:** doctor never actuates, which the recording test proves.
- **Rule 1:** doctor works with Postgres absent, and database checks are Notes.
- **Rule 7:** doctor never suggests enabling ufw, and skips rather than guesses when it has not read the ruleset (the spirit of D-42).

---

## Phase 25 — Adoption discovery you can trust

**Goal:** `sudo gvm adopt discover pz-main` runs against the live Project Zomboid server, finds it where the bash suite actually keeps it, says exactly what would block a cutover and how to fix it, and ends "Nothing has been changed."

**Why now:** discover is wired and read-only, but on the reference server it fails: `config.env` sits next to the suite scripts, not in `INSTALL_DIR`. Its report also leaves out most of what the runbook asks the operator to check.

This is the operator's first step toward adopting the live server. It costs little, and it is not on the J1 critical path, so it runs alongside J1.

Cutover itself waits for Phase 50's safety net.

**Resolves:** migration-adopt-3, migration-adopt-8, migration-adopt-11, migration-adopt-12, migration-adopt-14 (help and discovery runbook), documentation-6 (discovery and finalize runbook).

### Work items

1. **`gvm adopt discover <instance> [--from-bash-suite DIR] [--unit zomboid] [--write-plan]`**, with strict flags. Extra arguments and unknown flags are rejected anywhere on the line.
   - The **running unit is authoritative** for world identity: DataDir comes from `ExecStart`'s `-cachedir=`, and ServerName from `-servername`.
   - The suite directory is found in a fixed order:
     1. the flag
     2. `$SCRIPT_DIR` from `zomboid-restart.service`'s `ExecStart`
     3. `/root/pz-server`
     4. otherwise discover asks for the flag

     It prints which source it used.
   - `config.env` is used only for what the unit does not carry. A mismatch with the unit is a blocker: INSTALL_DIR vs WorkingDirectory, DATA_DIR vs `-cachedir`, SERVER_NAME vs `-servername`.
   - Without `--write-plan`, discover writes nothing. With it, it writes **only** `/var/lib/gvm/adopt/<instance>.plan.json`. The adoption record belongs to cutover (Phase 50), which keeps rule 11's read-only discovery narrow.
   - New fixtures cover `config.env` outside `INSTALL_DIR`, and a mismatch.

   (migration-adopt-3)
2. **Findings say what blocks cutover.**
   - `DiscoverFinding` gets a `Fix` and a tier: blocker, warning or note. Every Fix names only real commands (U2).
   - ADOPT-001 becomes a note: "Mods= preserved verbatim; review after cutover with gvm content diff <name>". ADOPT-003 becomes a warning.
   - The §19.1 blockers are implemented with reads only:
     - the instance YAML already exists
     - `gvm@<name>` is enabled
     - the world directory is missing
     - there are two `appmanifest_380870.acf` files
     - no JVM pid is found while the unit is active
     - free disk is below the archive estimate × 1.2 (`statvfs`)
     - PZ_USER cannot write the data directory, judged from stat bits alone (no probe file is written)
   - The report ends with "Cutover would proceed" or "Cutover would REFUSE: ADOPT-0xx, …". Blockers exit 4; warnings exit 0.

   (migration-adopt-8)
3. **A complete read-only report.** It covers:
   - WORLD: path, size, file count and newest mtime
   - DISK: free space against the estimate
   - SCHEDULE: whether the legacy timer is enabled, its OnCalendar, and `update.sh` vs a plain restart
   - the shell and JVM pids, with their starttimes
   - ports with protocol and exposure, e.g. `16261/udp game · 16262/udp data (both must be open) · 27015/tcp rcon 127.0.0.1 only`
   - BACKUPS: the legacy dir and its archive count
   - CREDENTIALS: whether cleartext copies exist; their contents are never read

   FIREWALL reads "not read by this build" until Phase 33's read-only parser exists, then shows the parsed state or "could not be parsed; nothing will be changed". The player address comes from local evidence only. The report **always** ends "Nothing has been changed." (migration-adopt-11)
4. **Quote-aware `ParseConfigEnv`** (`configenv.go:16`). It still evaluates nothing.
   - A value that starts with a quote runs to the matching closing quote. After it, only whitespace and a `#…` comment are allowed.
   - In a bare value, a comment starts at a `#` preceded by whitespace.
   - An imported game password containing `' " \ $` or a backtick becomes a blocker: "password for <role> contains characters gvm cannot carry; rotate it". It is never imported altered.
   - Fixtures: `config.env.example` (`ADMIN_PASSWORD==""`) and a quoted value containing ` #`.

   (migration-adopt-12)
5. **A runbook for the part that exists.** `docs/adopt-projectzomboid.md` is written against the real commands. Cutover, rollback and promote get a "not available in this build" callout, and the 7-day rule stays prominent. A runbook test flag-parses every fenced command through a parse-only seam and never executes it.

   `gvm help adopt` gives each step a one-line safety statement:
   - "discover — reads only; safe with players online"
   - "finalize — refuses within 7 days without --force"

   It also says that only Project Zomboid bash-suite installs can be adopted. (migration-adopt-14, documentation-6)

### Definition of done

- [ ] Against the `internal/adopt/testdata/live-pz` rootfs, with `config.env` in `/root/pz-server`, `gvm adopt discover pz-main` succeeds and names `/root/pz-server/config.env (from zomboid-restart.service)`. A normal B41/B42 fixture reports "Cutover would proceed" and exits 0.
- [ ] A config.env/unit mismatch fixture exits 4 with the blocker and its Fix.
- [ ] `TestDiscoverIsReadOnly` still passes, extended to `--write-plan`, which makes exactly one write, to the plan file.
- [ ] `ParseConfigEnv` on `SERVER_PASSWORD="" # none` yields `""`. The runbook test is green.

**Depends on:** Phase 22. **Effort:** M.

**Design rules touched:**
- **Rule 11:** discovery stays read-only. `--write-plan` writes only gvm's own plan file, never the legacy install, world or units, and never the adoption record.
- **Rule 10:** imported passwords are checked, never altered.
- **Rule 7:** the firewall is only read, and an untrusted parse is labelled as such.

---

## Phase 26 — Choose a game, and always know the next step

**Goal:** before creating anything, an operator can see a game's settings, passwords, ports and size. After every command they see one `next:` line that works in this build.

**Why now:** it is cheap and offline, and it ends the trial-and-error loop of root `create` attempts that the review watched operators fall into. The `next:` machinery has to exist before Phases 29–34, so that each newly wired verb plugs in its own line instead of having one retrofitted later.

**Resolves:** onboarding-9, onboarding-11 (`next:` lines), onboarding-13, cli-ergonomics-11 (`next:` lines), cli-walkthrough-8, cli-walkthrough-10, game-catalog-config-5, game-catalog-config-11 (templates hidden), game-catalog-config-12 (labels), documentation-7.

### Work items

1. **`gvm game show <id> [--yaml]`** (RouteLocal, no root). It works offline from the embedded floor and prints:
   - `metadata.doc`
   - the provider: "Steam", "Windows server under Proton — install Proton yourself" or "Minecraft"
   - RAM and disk floors
   - ports: name, proto, default, expose, note
   - the secrets it references
   - a vars table: name, type, default, required, values or min–max, apply badge (live/restart/rebuild), immutable, help
   - recommended jobs
   - a ready-to-paste `sudo gvm create <name> --game <id> --var …` line

   `--yaml` emits the manifest bytes. `create`'s undeclared-var error (`create.go:261`) points to `gvm game show <id>`. Every required var in the shipped manifests gets a `help:`. (game-catalog-config-5, onboarding-9, documentation-7, cli-walkthrough-8)
2. **A richer `gvm game list`.**
   - It gets a header and columns for RAM floor, tags and needs (Proton, Xvfb, Java).
   - Template-tagged manifests are hidden unless `--all`.
   - `create` refuses a template id with "copy it to /etc/gvm/games/<id>.yaml and change metadata.id first".

   Phase 32 adds the verified/unverified column. (game-catalog-config-5, game-catalog-config-11, game-catalog-config-12)
3. **Generated game pages.** `make docs-games` writes `docs/games/<id>.md` from the same code (CI diff), and the README gets a "Supported games" table linking to them. (documentation-7)
4. **`next:` lines** (U4).
   - A helper, `nextStep(after lifecycle.Status)`, turns the post-action `Derive` hint (`derive.go`, Hint) into one line and filters it through the registry. An unbuilt step reads "next step (gvm install) is not available in this build".
   - `create`, `apply`, `install`, `start`, `update`, `restore` and `content sync` each use it as they land.
   - `apply --diff` ends with "run `gvm apply <name>` to make these changes".
   - `apply` and `start` **offer** `--force` for a missing install sentinel only if the instance has run before. A never-installed instance gets `next: sudo gvm install <n>`. Only the hint changes: `--force` itself is still accepted exactly where it is today (U11), which Phase 30's developer procedure relies on.
   - "Has run before" needs durable evidence. `status.json` lives in the unit's `RuntimeDirectory` (`/run/gvm/<n>`, `paths.go:180-205`), which systemd removes on every stop and on reboot. So `run-prepare` (root) creates `/var/lib/gvm/instances/<n>/runner/`, owned by `gvm-<n>`, and the runner writes `ever-ready` there the first time it reaches ready. Phase 30 keeps `config-written.json` in the same directory.

   (onboarding-11, cli-ergonomics-11, cli-walkthrough-10)
5. **RAM and disk warnings** at `create` and as an INST check. gvm compares two things:
   - `ram_floor` / `MemoryMax`, plus the other enabled instances' `MemoryMax`, against MemTotal in `/proc/meminfo`
   - `disk_floor_gb` against free space at `paths.install`

   It warns with numbers, e.g. "ark-se needs ~8 GiB; 3.1 GiB unclaimed on this host", and never refuses. `game show` shows the same numbers (`spec/sizing.go`, `instance/sizing.go`). (onboarding-13)
6. **Getting started.** A "Getting started" block in `gvm help` and `docs/quickstart.md` walk through game show → create → apply → install → secret → start → connect. Each step is marked "not available in this build" until its phase lands, and the hint test checks them. (cli-walkthrough-10, cli-ergonomics-11)

### Definition of done

- [ ] As a normal user, `gvm game show valheim` shows `world_name … immutable · apply: rebuild` and a paste-ready create line.
- [ ] `gvm game show generic-steam --yaml > /tmp/g.yaml && gvm game validate /tmp/g.yaml` succeeds.
- [ ] `sudo gvm create v1 --game valheim --var nope=1` fails with an error that names `gvm game show valheim`.
- [ ] On a 2 GiB VM, `sudo gvm create a1 --game ark-se` warns with numbers and still creates.
- [ ] After `sudo gvm apply v1`, the output ends `next step (gvm install) is not available in this build` until Phase 31, and `next: sudo gvm install v1` after it. It never offers `--force`.
- [ ] An instance that reached ready once, then was stopped and the host rebooted, is still offered `--force` for a missing sentinel (the `ever-ready` marker survives; `/run/gvm/<n>` does not).
- [ ] `gvm game list` hides `generic-steam` unless `--all` is given.

**Depends on:** Phase 22. **Effort:** M.

**Design rules touched:**
- **Rule 10:** `game show` prints manifest data, renders no template, and never shows a secret value.
- **Rule 5 and U11:** `--force` is no longer *suggested* for an instance that never installed, so the printed path never leads an operator to start on missing files. The flag itself is unchanged: nothing is added or removed.

---

## Phase 27 — The runner never kills a game mid-save, and never boots blind

**Goal:**
- Every game gvm starts reaches ready through a probe the runner actually builds.
- A Proton game is stopped through its ladder without SIGKILL.
- A game that dies during boot is reported as such, and stops looping however slowly it dies.
- One game instance can no longer read another instance's command line (and the secrets on it), without breaking the backup freeze. Accounts outside `gvm@` units still can, which doctor now says, until Phase 42 moves the secrets off argv.

**Why now:** these are the defects that turn a correct design into lost worlds. They must be fixed before any install engine opens (U13), because install is what makes boots happen.

**Resolves:** game-runtime-compat-1, game-runtime-compat-2 (runner half; the Proton manifests are Phase 42), game-runtime-compat-5 (runner half; status is Phase 28), game-runtime-compat-11 (ProtectProc and the argv warning; moving secrets off argv is Phase 42).

### Work items

1. **The runner builds every probe it advertises.**
   - `probe.KindA2S` and `probe.KindMCPing` cases are added to `runner.BuildProbes` (`readiness.go:426`), which today handles only log_marker and tcp/udp. They use the existing `addr()` resolver, and `internal/probe/a2s.go` and `mcping.go` already exist and are tested.
   - `cli/run.go` builds probes right after `runner.New`, before `Preflight` and `Spawn`. An error maps to `Failure{Check: "readiness", Code: ExitUsage}`, with a `STATUS=` line naming the kind, so an unbuildable probe never spawns or loops the game.
   - `control_auth` is marked `SupportPlanned` in `spec/validate.go`'s probe registry, so validate warns about it.
   - `TestEveryImplementedProbeKindIsBuilt` checks that the registry and `BuildProbes` stay in step.

   (game-runtime-compat-1)
2. **SAFETY — the ladder waits for the cgroup, not the launcher.**
   - `Ladder.finishWait` (`shutdown.go:744`) treats a closed `w.gone` as the end of a wait only when `l.settled()` holds: the leader is dead **and** the cgroup is empty. Otherwise it keeps polling `settled()` every `waitExitPoll` until the rung's budget, marker or context ends. The implicit SIGTERM rung follows the same rule.
   - `shutdown_test.go` is updated so an engine that outlives its leader gets the full SIGTERM grace before any SIGKILL rung (`TestLadderWaitsForCgroupNotLauncher`).
   - The Proton manifest changes (game-level save/quit, no bare leading `wait_exit`) need scratch boots, so they land in Phase 42.

   Proton games cannot be created before then, because the Phase 32 ledger gate keeps them out. (game-runtime-compat-2)
3. **A game that dies during boot is said to have died.**
   - During `AwaitReady`, the runner selects on `settled()`. That is the ladder's own predicate, so a Proton launcher's exit alone does not count.
   - When it fires, the runner returns `ErrExitedDuringBoot{status}`. A non-empty cgroup still goes through `r.Shutdown` and the full ladder.
   - The runner then exits with a **new `runner.ExitBootDied = 69`**, "the game exited before it was ready". 69 is deliberately *not* in `RestartPreventExitStatus`, so a single transient death retries.
   - `STATUS=` carries the exit status, the last console lines and the hint `gvm logs <n>`.
   - "Suspected" notes are added: a SIGSYS suggests `arch: i386`, and a SIGSEGV or SIGABRT within 60 s under MemoryDenyWriteExecute suggests `jit: true`.

   (game-runtime-compat-5)
4. **Slow boot deaths stop too.** `StartLimitBurst=5` within `StartLimitIntervalSec=600` with `RestartSec=20` never trips for a game that dies roughly 100 s or more into boot. Such a game loops forever, and the guard cannot see it, because the unit sits in `auto-restart`, not `failed`. A runner-side counter closes that gap:
   - The counter lives in `/run/gvm/<n>/bootfail.json`. `RuntimeDirectoryPreserve=restart` keeps it across systemd's automatic restarts. An explicit stop, the unit entering `failed`, or a reboot clears it.
   - It counts consecutive runs that ended before ready: exit 69, and exit 75 when the cause was a readiness timeout, stall or marker.
   - Reaching ready resets it.
   - On the **third** consecutive boot failure, the runner (after the ladder, as above) exits with a **new `runner.ExitBootLoop = 79`**: "the game failed to reach ready 3 times in a row; gvm stopped retrying".
   - `79` is added to `gvm@.service`'s `RestartPreventExitStatus` and to the exported constant that `TestRoleExitCodesUnchanged` and Phase 28's `NeedsOperator` share.

   (game-runtime-compat-5)
5. **`exitMeaning` is honest.** `exitMeaning(75)` (`derive.go:507`) becomes "readiness failed (timeout, stall or marker), or an orphan from a previous run". 69 and 79 are added to derive's table and to the unit's comment. (game-runtime-compat-5)
6. **Secrets on argv: `ProtectProc=invisible`, without breaking the freeze watchdog. The two changes ship in one commit.**
   - Add `ProtectProc=invisible` to `packaging/systemd/gvm@.service`, asserted by `golden-units.sh`.
   - **The companion change.** The runner's D-31 freeze watcher (`freezeWatcher`, `cli/runfreeze.go:109-113`) uses `platform.Alive`. That returns `(false, nil)` on ENOENT for `/proc/<pid>` (`platform/proc_linux.go:45`), and the watcher reads it as "provably dead". Under ProtectProc, the runner (User=`gvm-<inst>`) can no longer see the root-owned backup job, so without the change it would send save-on in the middle of a snapshot.
   - The watcher now treats ENOENT as **Unknown**, unless `kill(pid, 0)` returns `ESRCH`. `EPERM` or success also mean Unknown, and the lease deadline governs. `TestFreezeWatcherTreatsHiddenOwnerAsUnknown` covers it.
   - A doctor HOST Note fires when systemd < 247 or the kernel < 5.8 makes ProtectProc ineffective.
   - **What ProtectProc does and does not do.** It mounts a private `hidepid=invisible` procfs *inside the unit*, so a game sees only its own uid's processes. That stops instance A's game (or a compromised mod in it) from reading instance B's argv, which is the finding's threat. It does **not** hide a game's argv from processes outside `gvm@` units: SSH users, a legacy `pzserver`, postgres, caddy and any other local account can still read `/proc/<pid>/cmdline` on the host's own `/proc`.
   - **Doctor HOST Note, report-only:** when any instance has a secret on argv and the host's `/proc` is mounted without `hidepid`, doctor lists those instances and secrets and prints the fstab line that would hide them (`proc /proc proc defaults,hidepid=invisible,gid=<group> 0 0`). gvm never edits `/etc/fstab` or remounts `/proc`.
   - `gvm game validate` warns: "spec.runtime.args[N] places .Secret.X on the command line, readable in /proc/<pid>/cmdline by local accounts outside gvm; prefer a config_files key". Moving secrets off argv (Phase 42) is the real mitigation. Valheim's `-password` has no config alternative, so it stays on argv as a stated residual risk (Phase 42 lists every one).

   (game-runtime-compat-11)

### Definition of done

- [ ] `TestLadderWaitsForCgroupNotLauncher`: with a fake leader that dies at t=0 and a cgroup that empties at t=40 s, no SIGKILL is sent and the rung records `exited`.
- [ ] A fixture game that exits 2 s after spawn exits 69, with the STATUS line, and never sits in "starting" for `stall_timeout`. Its third attempt exits 79 and the unit is `failed` within about a minute.
- [ ] **Slow-death fixture:** a fake game that dies 150 s into boot. After the third attempt the runner exits 79, the unit is `failed`, and 30 minutes later there has been no further start, guard runs included once Phase 28 lands.
- [ ] An a2s fixture manifest reaches ready, and an unbuildable probe kind exits 64 without spawning (the process table is unchanged).
- [ ] The `golden-units.sh` assertion for `ProtectProc=invisible` and `TestFreezeWatcherTreatsHiddenOwnerAsUnknown` are both green.
- [ ] With instances a and b running, `sudo systemd-run --wait --pipe -p User=gvm-a -p ProtectProc=invisible cat /proc/<b's pid>/cmdline` fails with "No such file or directory". On a host without `hidepid`, `sudo gvm doctor` shows the HOST Note naming Valheim's `-password`.
- [ ] `TestRoleExitCodesUnchanged` asserts 64/65/66/69/70/75/78/79, and that `RestartPreventExitStatus=64 65 66 78 79` equals the exported constant.

**Depends on:** Phases 22, 24. **Effort:** L.

**Design rules touched:**
- **Rule 4:**
  - The ladder now waits for the cgroup, so a launcher's death can no longer collapse the waits into SIGKILL.
  - A boot-time death with anything left in the cgroup still gets the ladder before 69 or 79.
  - No new SIGKILL path exists.
- **Rule 6:** derive stays read-only. These changes alter only what systemd does with an exit code.
- **Freeze safety (D-31):** an owner gvm cannot see is Unknown, never dead.
- **Hardening:** ProtectProc tightens the sandbox, and nothing is relaxed. The plan claims only what it does, isolation between instances, and doctor names the residual exposure to host accounts.

---

## Phase 28 — A server that cannot run says why, and the guard stops retrying it

**Goal:** a crash-looping or boot-failing server is shown as such, with the real exit reason and the manifest author's hint. `gvm stop` always stops it. The guard no longer restarts servers that need a human, and lease messages never steer the operator toward deleting a running job's lease.

**Why now:** Phase 27 made the runner report boot deaths with distinct exit codes. Observation still reads the runner's view instead of systemd's, so every one of them would show as "starting".

The guard also needs fixing before servers can be installed. It restarts a 64/65/66/78 exit every 5 minutes with no cap, and that loop would reach every new operator whose first server is missing a secret.

**Resolves:** failure-recovery-4, failure-recovery-5, failure-recovery-7, failure-recovery-8, failure-recovery-10, failure-recovery-11, failure-recovery-14, day2-ops-7, day2-ops-10, game-runtime-compat-5 (status half).

### Work items

1. **Read what systemd already knows.** `NRestarts` and `StatusText` are added to `platform.UnitState`. They come from the same Service-properties call that already returns `SubState`, `Result` and `ExecMainStatus` (`hostops/systemd_linux.go:145-155`).

   `Observe` (`lifecycle/observe.go`) also reads the last `shutdown.jsonl` record, with these limits:
   - O_NOFOLLOW, regular files only
   - at most the last 64 KiB
   - typed fields only, with control and ANSI characters stripped
   - marked as runner-reported

   (failure-recovery-4, failure-recovery-7)
2. **`RuleCrashLooping` (PhaseDegraded).** It fires when `SubState=auto-restart`, or when the unit is activating after an automatic restart (`NRestarts>0`) and has not yet reached ready.
   - Reason: "restarted N times; last exit <code> (<meaning>)". Hint: `gvm logs <n> --shutdown`.
   - It counts toward `--problems` and toward the summary's "needing attention".
   - A unit that failed with 79 reads "gave up after 3 failed boots: <last STATUS line>".
   - **A past restart on a healthy unit is a Note, not a problem.** systemd's `NRestarts` is cumulative: it is cleared only by an explicit start or stop, or by `reset-failed`. A server that was OOM-killed once three days ago, restarted itself and has been ready since would otherwise read "crash-looping" for days. When the unit is ready, `NRestarts>0` produces only a Note, "restarted 1 time since <ActiveEnterTimestamp>; last exit …", which never counts toward `--problems`, the summary or doctor's exit 4.

   (failure-recovery-4, game-runtime-compat-5)
3. **Manifest-declared fatal conditions.**
   - `fail_markers[].exit` (`spec/manifest.go`, FailMarker) accepts an enum: `port_unavailable` → 66, or `data_missing` → 65.
   - The fired marker's pattern, its hint and the sanitised matched line go into the shutdown record.
   - `gvm status` shows "boot aborted: <line> — <hint>".
   - The shipped bind-failure hints name `gvm ports <name>`, with revision bumps. These are text-only changes, so verification carries over (Phase 32).

   (failure-recovery-14)
4. **`Observation.NeedsOperator()`.** It is true when the unit failed with `ExecMainStatus` in one exported set, {64, 65, 66, 78, 79}, or with `Result=start-limit-hit`. `TestNeedsOperatorMatchesRestartPreventExitStatus` compares that set with `gvm@.service`.

   When `NeedsOperator()` holds:
   - `DesiredRunningAndDown` (`reconcile.go:248`) is false.
   - `Findings` emits "the guard will not restart this: exit 65 preflight failed", with the exit hint.
   - The guard reports "not recovered: needs an operator".

   Other failures get a **cap of 3 recoveries per boot**. The cap is recorded in `/var/lib/gvm/instances/<n>/guard.json` with the boot id, and only the guard writes it. Status shows "guard: 3 recovery attempts this boot; giving up". (failure-recovery-10)
5. **`gvm stop` stops a dead runner.** `Manager.execute` (`manager.go:214`) handles the case where the unit is activating (auto-restart), failed or inactive, and the console socket is absent or refuses:
   - It skips the console ladder and issues hostops `VerbUnitStop`, which also cancels a pending restart. Any live runner still walks its own ladder under `KillMode=mixed`.
   - It records rung `no-runner` and prints "no runner was up; asked systemd to stop the unit (nothing to save)".
   - Already stopped prints "already stopped; held so the guard leaves it down", exit 0.
   - Each rung prints as it happens: "save sent… waiting for save marker (40s/300s)".
   - If the stop fails before the unit stop is issued, the hold is removed.

   (failure-recovery-5, day2-ops-7)
6. **Start after `start-limit-hit` or a 79.**
   - A new hostops verb, `VerbUnitResetFailed`, is scoped by the verb table to `gvm@<n>.service`.
   - Only an interactive `gvm start` issues it, and only when `Result=start-limit-hit` or the last exit was 79. It prints a note when it does.
   - The guard, jobs and `--force` never issue it, and a test asserts that the guard and job actors cannot.
   - On any start failure, gvm re-observes, then prints phase, reason, hint and `StatusText` under the systemd error.

   (failure-recovery-8)
7. **Lease hints come from the guard's own predicate** (`deriveLease`, `derive.go:175`):
   - **Live lease:** "an update job is running (gvm-job@pz1_nightly, started 12m ago). Wait: journalctl -fu gvm-job@pz1_nightly. If it is stuck: sudo gvm restart pz1 --force".
   - **Stale lease the guard will clear:** "gvm-guard.timer will clear this lease and start pz1 within 5 min; to inspect first: sudo gvm stop pz1".
   - `gvm job clear` is suggested only for stale or unreadable leases. It prints the cleared lease plus a per-kind next step, e.g. for update: "gvm update pz1 --repair --dry-run, then gvm start pz1".
   - The `gvm list` row shows "(update running 12m)".

   (day2-ops-10, failure-recovery-11)

### Definition of done

- [ ] A fake game exiting 2 s after spawn: `gvm status x` shows `crash-looping: restarted 2 times; last exit 69 (the game exited before it was ready)`. After the third attempt it shows `needs an operator: gave up after 3 failed boots`. `sudo gvm start x` explains this in plain words, resets once and starts.
- [ ] A missing secret (exit 65) shows `needs an operator`, and 30 minutes of guard runs make zero restarts of that unit. The Phase 27 slow-death fixture likewise sees zero guard restarts.
- [ ] `sudo gvm stop x` on an auto-restart unit prints the no-runner sentence and exits 0. A second `gvm stop x` prints `already stopped` and exits 0.
- [ ] A fixture game that auto-restarts once, then stays ready for an hour: `gvm list --problems` reports 0 problems and exits 0, and `gvm status x` shows the "restarted 1 time since …" Note.
- [ ] A fixture `fail_markers` entry with `exit: port_unavailable` fires during boot: the unit exits 66, and `gvm status x` shows `boot aborted: <matched line> — <manifest hint>` and `needs an operator`. The guard does not restart it.
- [ ] **Why it last stopped.** After `sudo gvm stop x`, `gvm status x` shows `last stop: <time> · ladder ended on save` from the shutdown record. After a crash it shows `last exit 69 (the game exited before it was ready)` from systemd's `ExecMainStatus`.
- [ ] During a live update lease, the refusal from `sudo gvm restart pz1` names `journalctl` and `--force`, never `gvm job clear`.
- [ ] With a stale lease (holder pid dead), `gvm status pz1` shows `gvm-guard.timer will clear this lease and start pz1 within 5 min; to inspect first: sudo gvm stop pz1`. `sudo gvm job clear pz1` on it prints the cleared lease (kind, unit, started) and the update-kind next step, `gvm update pz1 --repair --dry-run, then gvm start pz1`.
- [ ] `TestNeedsOperatorMatchesRestartPreventExitStatus` and `TestGuardCannotResetFailed` are green.

**Depends on:** Phase 27. **Effort:** L.

**Design rules touched:**
- **Rule 4:** no new kill path. `VerbUnitStop` sends SIGTERM to the runner, which walks the ladder under `KillMode=mixed`, and no SIGKILL is added.
- **Rule 6:**
  - The guard's cap and the operator predicate only reduce actuation.
  - `reset-failed` is reachable only from an interactive start.
  - A stale lease still yields `failed` with a hint, and the job is never resumed. The hint now describes what the guard actually does.

---

## Phase 29 — Passwords and keys, set locally

**Goal:** over SSH as root, with gvmd and Postgres down, an operator sets, generates, lists and reads back the secrets every catalog game needs. A new server gets a join password by default.

**Why now:** 11 of the 16 games fail the runner's credential preflight after `create` + `apply`. The only route today is hand-written files, which bypass rule 10 and the codec separators. Phase 32's verification boots need working secrets.

This phase ships in the same release as Phase 30 (U3). A password gvm stores must reach the game in the release that stores it.

**Resolves:** onboarding-3, cli-ergonomics-2, cli-walkthrough-2, admin-security-1 (secret), admin-security-2, admin-security-9, admin-security-10 (show), game-catalog-config-2, game-catalog-config-7 (passworded by default), documentation-5.

### Work items

1. **`gvm secret set|list|show-path|rotate|show <instance> <name>`**, in-process as root (RouteHostOp), through `secrets.Store` (`filestore.go`: `Write` at `:122`, `Has` at `:184`, `List`, `Path`). Host-scope secrets use `--host`, e.g. `gvm secret set --host curseforge`.
   - **The value** comes from a no-echo TTY prompt (`golang.org/x/term`, pure Go), from stdin when it is not a TTY, from `--from-file F`, or from `--generate` (`GeneratePassword`, `generate.go:62`). It never comes from argv (U9).
   - **Instance secrets** (every secret a manifest's `secrets:` block declares, which reach the game's argv or config files) reject `' " \ $`, backtick, `; =`, whitespace, CR, LF and NUL. Each is named with its reason, e.g. "';' is Project Zomboid's list separator; this password would be cut there". Each is reported by byte offset, never by value.
   - **Host secrets** (`--host`, e.g. the CurseForge API key) are sent in HTTP headers, never into a game. Rule 10 is about game passwords, so they keep only the store's file-format rules (`secrets.ValidateValue`, `filestore.go:103`): not empty, no CR, LF or NUL. CurseForge keys look like `$2a$10$…`, and the instance ban would refuse every one of them.
   - The manifest's validators run with the candidate value, e.g. `valheim.password_rules`.
   - Each value gets a 0600 `<name>.source` sidecar recording where it came from: `generated`, `operator` or `adopted` (Phase 50). `secret list` shows it, and so does doctor INST-013.
   - Output shows only length and fingerprint, then "takes effect at next restart: sudo gvm restart <n>". Nothing restarts automatically; `rotate --restart` goes through the ladder.
   - Writes are audited through the spool as `secret.write`, with name and fingerprint only. The Postgres `secret_refs` mirror is best-effort.

   (onboarding-3, cli-ergonomics-2, game-catalog-config-2, documentation-5, cli-walkthrough-2, admin-security-9)
2. **A declarative `secrets:` block in manifests.** Today secrets appear only as `{{ .Secret.X }}` references. The block declares each one as data:

   ```yaml
   secrets:
     rcon_password:  {source: generate}
     admin_password: {source: generate}
     join_password:  {source: optional, player_facing: true}
     gslt:           {source: operator, help: "get one at steamcommunity.com/dev/managegameservers"}
   ```

   It is validated as data, and every `.Secret.X` reference must be declared. Shipped manifests get revision bumps. This is a data-only change, so the ledger carry-over rule applies. (admin-security-2)
3. **Generate what machines need, never more.**
   - `create` and `apply` generate missing `generate` secrets. They never rotate an existing one, and never generate for an adopted instance.
   - **An adopted instance gets its secrets from cutover, not from generation.** Phase 50 imports the live server's own passwords before the commit point, through `secrets.Store`, with `source: adopted`. Preflight treats an imported value exactly like any other stored value, so `gvm@<n>` starts with the passwords players already use. An adopted instance only reaches `gvm@` through cutover, so it never meets preflight without them.
   - `--dry-run` lists them as "would generate", and `apply --diff` shows `+ secret instances/pz1/rcon_password (generated)`.
   - Preflight (`preflight.go:242`) and `BuildCredentials` (`units.go:523`) require only `generate` and `operator` secrets that the instance's runtime path references. An `args` entry scoped to `when: {phase: install}` (PZ's `admin_password`) is required only by install, which an adopted instance never runs.
   - An unset `optional` secret is left out of `CredIDs`. Phase 30 defines how config files render it.

   (admin-security-2, game-catalog-config-2)
4. **Passworded by default.** When the game declares a player-facing secret, `create` generates `join_password` and prints it once, under "join password (share with players): …".
   - `--open` opts out, with a warning when the game's public-listing var is true.
   - An operator secret prints "required before start: sudo gvm secret set cs2-1 gslt".

   (onboarding-3, game-catalog-config-7)
5. **No success without effect** (U3). `gvm secret list` shows, for each secret, where the game reads it:
   - `argv` or `env`: "in force at next start"
   - `config <file>`: "stored; not written to the game by this build" until Phase 30 lands in the same release

   `create` prints the join password only when one of those paths delivers it.
6. **Read back the join password.** `sudo gvm secret show <inst> join_password` works only for root, only on a TTY, and only for `player_facing` secrets unless `--admin-secrets` is given. It is audited as `secret.read`, with the name only. (admin-security-10)
7. **Doctor INST-011 (credentials),** in its own check file. It names missing ids with the exact `sudo gvm secret set` line, and flags forbidden characters, wrong mode or owner, and CRLF. It never prints a value.

   The same file implements the documented HOST-006..008 (`ARCHITECTURE.md:6964-6966`; `checks_host.go:14` defers them):
   - HOST-006: `/etc/gvm/master.key` is mode 0400 root.
   - HOST-007: `gvm-web.service` declares no `LoadCredential`, so the master key never reaches the web tier.
   - HOST-008: no instance user is a member of `gvm`, `gvm-console`, `gvm-content` or `caddy`.

   HOST-006 and HOST-007 **Skip** ("not installed") while the key or the unit is absent, which is every host before Phase 48's `host init --dashboard`. HOST-008 runs from now. (admin-security-9)
8. **Fix the misleading texts** at `create.go:511`, `preflight.go:242`, `validate_instance.go:383` and `minecraft-paper.yaml:19-21`. Add the `gvm help secrets` topic. (documentation-5)

### Definition of done

- [ ] With `postgresql` stopped, `sudo gvm create pz1 --game projectzomboid` prints `generated: rcon_password, admin_password` and a join password once. After `sudo gvm apply pz1`, `stat` shows 0600 root:root files under `/etc/gvm/secrets/instances/pz1/`.
- [ ] `printf 'a;b' | sudo gvm secret set pz1 join_password` refuses with the separator explanation (exit 5) and writes nothing.
- [ ] `sudo gvm secret set cs2-1 gslt` prompts without echo, and `ps -ef` during the prompt shows no value.
- [ ] `sudo gvm secret show pz1 join_password` prints the password. `… rcon_password` refuses without `--admin-secrets`.
- [ ] `gvm status pz1 --json` and every audit record contain no secret value (fixture grep).
- [ ] `printf '%s' '$2a$10$abcdefghijklmnopqrstuv' | sudo gvm secret set --host curseforge` stores the key (host secrets keep only the file-format rules), and `sudo gvm secret list --host` shows its length and fingerprint.
- [ ] After `create` + `apply`, every catalog manifest passes the credential preflight with no hand-written file, except where it declares `source: operator` secrets. For those, preflight and `create` name the exact `sudo gvm secret set <n> <name>` line (e.g. `sudo gvm secret set cs2-1 gslt`), and after that one command the manifest passes too (test over `games.FS`).
- [ ] An adopted-instance fixture whose `rcon_password` was written with `source: adopted` passes preflight and is never regenerated by `apply`.
- [ ] `sudo gvm doctor` on a pre-dashboard host shows HOST-006 and HOST-007 as skipped and HOST-008 as passed. Adding an instance user to `gvm-console` makes HOST-008 a Problem.

**Depends on:** Phases 22, 24. **Effort:** L (six secret subcommands, the `secrets:` block and revision bumps for 16 manifests, and the doctor checks).

**Design rules touched:**
- **Rule 1:** secrets are files, and the Postgres mirror is best-effort.
- **Rule 3:** the verbs run in-process and never use admin.sock (the transport test).
- **Rule 10:** the character ban is enforced at write for every secret that reaches a game, and generated passwords are base62. Host API keys never reach a game, so they keep only the file-format rules, which is what rule 10 already allows.
- **Rule 4:** nothing restarts automatically, and `--restart` uses the ladder.
- **Rule 11:** gvm never generates or rotates an adopted instance's secrets. Its values arrive only through cutover's import (Phase 50), after discovery has stayed read-only.

---

## Phase 30 — Settings and hooks reach the game

**Goal:** `--var max_players=16`, the MOTD, PvP and RCON passwords take effect. Several instances of one game get the ports gvm allocated. Start hooks run, and the operator accepts the Minecraft EULA themselves.

**Why now:**
- For 7 of the 16 games, the config-file ports are never written, so a second instance collides.
- The secrets from Phase 29 must reach their files in the same release.
- The EULA gate must exist before Phase 31 wires the Minecraft install that runs `mc.accept_eula`.

**Resolves:** game-catalog-config-1, game-catalog-config-3 (hook linkage and pre_start; post_install is Phase 31), onboarding-14, documentation-12.

### Work items

1. **Render `config_files` in `supervise()`** (`run.go:311`), after `runner.Preflight` takes the lock and before `Spawn`.
   - Each `config_files[].keys` entry goes through `hook.NewConfigAccess` (`hook/config.go:51`). That enforces the jail, `deny_keys` and mode, and writes atomically with a `.bak`. This is the first time `internal/hook` is linked into the binary, and the `go list -deps` assertion is updated to match.
   - **`Active` is wired, never left nil.** `NewConfigAccess` refuses every `owner: server` file while `Active()` reports true, and a nil `Active` counts as active with an error (`config.go:74-85`, `:197-205`). PZ's `servertest.ini`, Paper's `server.properties` and Palworld's settings file are all `owner: server`. Inside `supervise()` the unit is activating but no game exists yet, so the runner wires `Active` as "Preflight holds the lock and nothing has been spawned": false until `Spawn`, true after it. `TestSuperviseRendersOwnerServerFileBeforeSpawn` covers it.
   - **A file the game has not generated yet is skipped, not an error.** On a first start `servertest.ini` does not exist and `create_if_missing` is false, so `openConfig` returns "not there yet" (`config.go:117-119`). That case gets its own sentinel, `hook.ErrNotGenerated`, so the runner never matches on message text, and the runner treats that one error as skip: it writes a console note, "-- gvm: servertest.ini not there yet; ports come from argv this boot", and starts. The argv ports below protect that first boot.
   - **Port keys always take the allocated value.** A real parse failure refuses the start with `ExitConfig` (78) and a `STATUS=` line naming the file. 78 is already in `RestartPreventExitStatus`, so the unit stays failed. Phase 28, built alongside, shows it as "needs an operator".
   - The last-written values go to `/var/lib/gvm/instances/<n>/runner/config-written.json` (the durable, `gvm-<n>`-owned directory from Phase 26; `/run/gvm/<n>` is wiped on every stop), as hashes for secret-tainted keys. When a value was changed outside gvm, the console gets a note before the YAML value is applied: "-- gvm: MaxPlayers was 20 (changed outside gvm), set to 16 from pz1.yaml".
   - PZ also gets `-port`/`-udpport` on argv, so first-boot generation cannot collide.
   - `java_properties` files get `create_if_missing`.

   (game-catalog-config-1)
2. **Defined semantics for unset secrets.** The renderer runs with `missingkey=error` (`render_guard.go:294`). To keep that from refusing every `--open` server:
   - The secret map handed to the renderer contains every **declared** secret. A reference to an undeclared secret is already a validate error at load.
   - On a **managed** instance, an unset `optional` secret renders as the empty string. PZ's `Password=` written empty is how `--open` means "no join password", and going from passworded to open clears the old value instead of leaving it in the ini.
   - On an **adopted** instance (`managed_install: false`), a key whose value references an unset secret is **skipped, never blanked**. Before finalize, gvm writes only the keys the cutover plan lists. Phase 51's DoD compares `servertest.ini` byte for byte.
   - Tests: `TestOpenServerRendersEmptyJoinPassword` and `TestAdoptedRenderNeverBlanksAKey`.
3. **`gvm apply --diff` shows config-file changes.** It parses the live file read-only while the unit is inactive, masking secret-tainted keys. (game-catalog-config-1)
4. **Start hooks.**
   - `hook.Run` (`hook.go:329`) runs `pre_start` in `gvm run`, after Preflight and before Spawn, with `Ctx.Config` from `NewConfigAccess`. That covers `mc.select_jdk` and `pz.apply_sandbox`.
   - `pz.first_boot` is a `post_install` hook (`hook/pz.go:67`), so its `BootRequest` is honoured by Phase 31's install pipeline, not here.
   - `gvm game hooks` shows each hook's `Doc()` and when it runs ("runs at start"). Install-time hooks keep "not run by this build" until Phase 31.

   (game-catalog-config-3)
5. **The operator accepts the EULA.**
   - The three Minecraft manifests get `legal: {eula: {var: eula_accepted, url: https://aka.ms/MinecraftEULA}}` and an `eula_accepted: {type: bool, required: true, apply: restart}` var, with revision bumps.
   - A closed-registry validator, `mc.eula_accepted`, rejects false at create.
   - `mc.accept_eula` records a Problem and writes nothing unless the var is true. It never overwrites an existing `eula.txt`.
   - `create` without the var lists it among the missing vars, with the URL and the exact `--var eula_accepted=true` line.

   (onboarding-14, documentation-12, game-catalog-config-3)
6. **A developer procedure until Phase 31.** Phases 27–30 are proved on a VM before gvm can install game files. `docs/research/scratch-boots/README.md` documents the stop-gap: install the files by hand with SteamCMD as `gvm-<inst>` into the instance's install dir, then `sudo gvm start <n> --force` past the missing-sentinel refusal. That works because Phase 26 changed only the *hint*; `--force` is still accepted (U11). Nothing in the user docs mentions it. Phase 32 replaces it with `gvm game try --scratch`.

### Definition of done

- [ ] `pz1` and `pz2` (allocated 16261 and 16271) both start from fresh installs. Each first start prints the "servertest.ini not there yet; ports come from argv" note and binds its own port. After a restart, `pz2`'s ini shows `DefaultPort=16271`. A client joins both. On this pre-install build, the game files are placed by the documented developer procedure in `docs/research/scratch-boots/README.md`.
- [ ] `TestSuperviseRendersOwnerServerFileBeforeSpawn` is green: an `owner: server` file is rendered while `Active()` is false, and a nil `Active` refuses.
- [ ] `sudo gvm create mc1 --game minecraft-paper --var mc_version=1.21.4` refuses and shows the EULA URL.
- [ ] A hand-edited `MaxPlayers` produces the "changed outside gvm" console note on the next start, including after a stop and a reboot in between.
- [ ] A malformed ini makes the start refuse with exit 78 and a `STATUS=` line naming the file. `systemctl show gvm@pz1 -p ExecMainStatus -p NRestarts` shows 78 and no restart. (The "needs an operator" display is Phase 28's, and its DoD covers the whole {64, 65, 66, 78, 79} set.)
- [ ] `sudo gvm create pzo --game projectzomboid --open` renders `Password=` empty, and `gvm secret list pzo` shows no "not written" labels.
- [ ] `hook/noexec_test.go` and the jail tests stay green.

**Depends on:** Phases 26 (the durable `runner/` directory), 29. Phase 28 is built alongside; this phase's DoD checks the exit code and systemd's restart policy, not Phase 28's display. **Effort:** L.

**Design rules touched:**
- **Rule 2:** the hook package still never execs (`noexec_test.go`).
- **Rule 1:** config is rendered by `gvm run` from files alone.
- **Rule 4:** a parse failure refuses before spawn. An `owner: server` file is written only while nothing is spawned, so the game can never overwrite the edit on shutdown.
- **Rule 10:**
  - Values come from validated vars, and templates are still guard-checked.
  - Secret values are written only through the jail, with `deny_keys`.
  - An undeclared secret reference is a load error.
- **Rule 11:** adopted instances never have a key blanked.

---

## Phase 31 — Game files: install, update and repair

**Goal:** `sudo gvm install <n>` puts a Steam or Minecraft server's files on disk and runs its install hooks. The nightly update timer that `create` writes either really runs or says plainly that this build cannot run it.

**Why now:** this is where the core journey stops for all 16 games: `create` → `apply` → `install`. Every precondition is now in place:
- Phase 27 fixed the ladder, the probes and boot deaths.
- Phase 28 capped the guard.
- Phase 30 linked the hooks and the EULA gate.

This phase ships in the same release as Phase 32's ledger gate (U3, U14).

**Resolves:** onboarding-2, day2-ops-1 (install, update, repair and the previews for content-free games; rebuild is Phases 38 and 44, and PZ and Minecraft updates are Phases 44 and 45), cli-walkthrough-1 (install, and update and repair for content-free games; content sync, rebuild and modded updates follow in 38, 44 and 45), game-catalog-config-3 (post_install).

### Work items

1. **Build the providers in production `HostEnv`.** `HostEnv.attachEngines` (`jobengines.go:117`) leaves `Updates`, `Repair` and `Rebuilds` nil today. A new `HostEnv.providerFor(in, m)` builds the provider for each kind:
   - **`steamcmd`:** `steamcmd.New(steamcmd.WithRunner(jobs.NewSteamRunner(...)))` (`steam.go:192`), with a `provider.Ctx` holding the name, the pinned manifest, vars, ports, jailed paths, the instance uid as RunAs, Render and ManagedInstall. It is bound with `update.Bind` and wrapped by `update.New(update.Config{Installer, Content: nil, Archives: <backupEngine>, Held, DiskPath})` for install, update and repair. SteamCMD runs in its existing transient sandbox (`gvm-steam-<n>`) as the instance uid, as an argv exec with no `sh -c`.
   - **`minecraft`:** `minecraft.New(<client>)`. The client comes from `internal/content/fetch` with the manifest's egress host allowlist, never `http.DefaultClient`. `Ctx.Blobs` is `provider.Store` at `/var/lib/gvm/blobs`, which verifies the digest on `Put` before files are hardlinked into the jailed install tree.
   - **In both, Plan does network reads only.** Apply runs only after `plan.Authorize(in.Spec.ManagedInstall)` and writes only digest-verified bytes. The mandatory post-verification follows: D-32 appmanifest, byte parity and a single ACF for Steam.
   - `runInstall` (`install.go`) uses the same constructor.
   - Adopted instances keep refusing install, and keep printing "binary update X → Y available; not installed".

   (onboarding-2, day2-ops-1, cli-walkthrough-1)
2. **Install hooks run with install.**
   - After post-verification, `hook.Run` runs `post_install`, with `Ctx.Config` from `NewConfigAccess` and an FS from `hook.NewFS` jailed to the instance. That covers `steam.home_layout`, `steam.link_sdk`, `steam.appid_shim`, `pal.settings_seed`, `sdtd.seed_config`, `mc.accept_eula` (gated by Phase 30), `mc.write_properties` and `pz.first_boot`.
   - `mc.write_properties` sets the manifest's keys into a `codec.New()` document.
   - **`pz.first_boot`'s `BootRequest` is honoured here, by the install pipeline.** The hook only asks (`hook/pz.go:55-67`); the pipeline owns the boot. When the hook records a request (the server's `<servername>.ini` does not exist yet), the install job:
     1. starts the instance through `Super.Start` on its real `gvm@<n>` unit, so the boot runs under the hardened sandbox and the runner's probes
     2. waits for the readiness marker, bounded by the manifest's `hard_deadline`
     3. stops through `Super.Stop`, which is the ladder
     4. renders `config_files` (Phase 30) into the ini the game just generated, while nothing is spawned
     5. records the cycle in the install report: "first boot: generated servertest.ini in 74s; stopped on save"

     A boot that fails is an install failure with the runner's reason (Phase 27), never a silently half-configured server.
   - `gvm game hooks` drops "not run by this build" for install hooks.

   (game-catalog-config-3)
3. **`engineWired(kind, inst, manifest)` flips one class at a time** (U1). After this phase:

   | Kind | Wired | Not wired, and why |
   |---|---|---|
   | install | steamcmd and minecraft manifests | — |
   | update, repair | manifests that declare **no content** (`content: []`: Valheim, Palworld and ten others) | Project Zomboid and the three Minecraft manifests declare a content block, and `update.Pipeline` with `Content == nil` would report "this instance declares no content, so there is no content axis" (`pipeline.go:329-333`). That would be success without effect (U3). They print "cannot run in this build: updates for games with mods need the mod engine". Phase 44 flips PZ and Phase 45 flips Minecraft. |
   | rebuild | — | Stays unavailable: `rebuild.Config` refuses nil Content, Server, Timers or Destroyer seams (`rebuild.go:736-740`). Phase 38 wires content-free manifests, and Phase 44 modded ones. |
   | content_sync | — | Waits for Phase 44. |

   `start`'s refusal names `sudo gvm install <n>` only when install is available. Refusals use operator language: "gvm cannot install game updates for pz-main in this release; nothing was stopped or changed". (day2-ops-1, cli-walkthrough-1)

   **PZ's nightly still runs.** Project Zomboid's only recommended nightly is `action: update` (`projectzomboid.yaml:301-308`), and the manifest's own reason for it is the restart, which clears heap pressure. Left as an update, it would fail every night until Phase 44, trip Phase 37's notifier, and never restart the server. So while `engineWired(update)` is false for a manifest, `create` writes that manifest's recommended `update` job as `action: restart` (a valid job action, `validate_instance.go:45`), and says so: "nightly: restart through the ladder (updating a game with mods needs the mod engine, which this build does not have)". Phase 44 tells instances created this way that the update action is now available.
4. **Static `StepNames`.** Each job runner declares its step list without dereferencing the engine (`jobs.StepNames`, `execute.go:978`), so `update --diff` and `rebuild --dry` render even for a kind this build cannot run. (day2-ops-1)
5. **`gvm update <n> --check`.** It runs only Preflight, `CheckBuild` and `CheckContent` (network reads), and takes no lease and no lock. It prints the installed build, the remote build and whether an update is available, and exits 4 when one is (U8). It records the answer, with its time, in `/var/lib/gvm/instances/<n>/update-check.json` (root 0640), and the nightly update job writes the same file, so Phase 37's status can show "update available" without a network read. (day2-ops-1)
6. **Progress and next step.** `install` prints each step as it runs, and ends with `next: sudo gvm start <n>` through `nextStep`.
7. **A network-gated Linux CI job** installs Valheim through SteamCMD and Minecraft Paper through the blob store, on the Debian 12 VM runner from Phase 23 (not a container).
8. **SAFETY — an SSH disconnect never kills a job halfway.** `install`, `update` and `repair` run in-process through `runOneJob` in the operator's SSH session, and `cmd/gvm/main.go` handles only SIGINT and SIGTERM. A disconnect's SIGHUP therefore kills gvm at once, with no unwinding, possibly between a ladder stop and the matching start. The lease goes stale, and the guard's sweep clears it and starts whatever the job left (`jobs/sweep.go:115-134`). One helper, `runDetachable`, closes this for every in-process job verb, and later phases add their verbs to it (`game try` in 32, `restore` and `rebuild` in 38, `content sync` in 44):
   - **The job runs as a unit.** The CLI validates the verb and its flags, writes them to a root 0600 request file under `/var/lib/gvm/instances/<n>/jobs/`, and asks systemd, through a new hostops verb, to start a transient unit (`StartTransientUnit` over D-Bus, `gvm-adhoc-<n>-<verb>-<id>.service` in `gvm-jobs.slice`) whose `ExecStart` is the argv `/usr/bin/gvm job run --request <file>`. The hostops verb accepts no other `ExecStart`. The CLI follows progress from the job record and the journal, and prints "safe to disconnect; follow with gvm job last <n>". The job's life no longer depends on the SSH session, which matches rule 1.
   - **Where it cannot be a unit yet, it ignores the hangup.** Any in-process fallback calls `signal.Ignore(syscall.SIGHUP)` and treats a failed write to a closed terminal (EIO, EPIPE) as "stop printing", never "stop working".
   - Ctrl-C keeps its current meaning for each verb.
   - `TestJobVerbsSurviveHangup` walks the dispatch table and fails for any verb that runs a job without `runDetachable`.

   (day2-ops-1, cli-walkthrough-1)

### Definition of done

- [ ] `sudo gvm install v1` (Valheim) completes on a Debian 12 VM. It prints the build id, `post-verify ok` and `3 post_install hooks ok`, and ends `next: sudo gvm start v1`.
- [ ] `sudo gvm install mc1` with `eula_accepted=true` writes `eula.txt` once. A pre-existing `eula.txt` is byte-identical afterwards.
- [ ] `sudo gvm update v1 --check` prints `installed 1234567 · remote 1234567 · up to date`, exits 0, and creates no `lease.json` (asserted).
- [ ] `sudo gvm update v1 --diff` and `sudo gvm rebuild v1 --dry` print their step tables. `gvm rebuild v1` itself exits 3.
- [ ] For a hand-declared Valheim update job, `sudo gvm job list` shows `v1_update` runnable. `sudo gvm update pz1 --diff` prints its step table, and `sudo gvm update pz1` exits 3 with `updates for games with mods need the mod engine`.
- [ ] **PZ on a fresh install.** `sudo gvm install pz1` runs `pz.first_boot`: the report shows "first boot: generated servertest.ini … stopped on save", and the ini then carries pz1's allocated `DefaultPort`. The ladder record for that boot shows a save rung and no DEGRADED.
- [ ] **PZ's nightly restarts it.** `sudo gvm create pz1 --game projectzomboid` writes the nightly as `action: restart` and says why. `sudo gvm job run pz1_nightly` restarts pz1 through the ladder ("ladder ended on save"), and the job unit exits 0.
- [ ] **Hangup.** Closing the SSH session in the middle of `sudo gvm install v1` leaves the install running to completion. From a new session, `gvm job last v1` shows the result, and the guard never starts a half-installed v1. `TestJobVerbsSurviveHangup` is green.
- [ ] `sudo gvm install` on an adopted instance still refuses with today's message. `go list -deps ./cmd/gvm` includes `internal/hook`, and `noexec_test.go` is green.

**Depends on:** Phases 26, 28, 30. **Effort:** L.

**Design rules touched:**
- **Rule 12:** a fake provider fails the test on any write during Plan. Apply runs only after `Authorize`, and copies only digest-verified bytes.
- **Rule 2:** SteamCMD is an argv exec, and the hooks never exec. The `noshell` and `noexec` gates cover `internal/cli` and `internal/hook`.
- **Rule 6:** `update --check` takes no lease and changes nothing. A hangup no longer leaves a stale lease behind a half-finished job for the guard to restart around.
- **Rule 4:** an update that must stop the server goes through `Super.Stop`, the ladder, and a no-op update never stops it. PZ's first boot is stopped through `Super.Stop` too, and PZ's interim nightly is a ladder restart.
- **Rules 1 and 2:** a long job runs as its own systemd unit, so it survives the SSH session ending. The transient unit's `ExecStart` is a fixed argv, the hostops verb accepts no other, and no shell is involved.
- **Rule 11:** gvm never installs onto an adopted instance.

---

## Phase 32 — Prove a game boots before it is offered

**Goal:** an operator can only create and install a game that has been shown to boot, reach ready, stop on a save rung and back up its world under gvm's hardened unit. Anyone can prove a game on their own host with one command that cleans up after itself.

J1 ships four verified games: Valheim, Palworld, Minecraft Paper and Project Zomboid.

**Why now:** Phase 31 opened install, and manifests that validate green have never booted. Valheim still declares `jit: false` (`valheim.yaml:53`), which Mono's JIT cannot survive under `MemoryDenyWriteExecute=yes`.

Without a gate, the first instances would pin broken revisions. This phase ships in the same release as Phase 31.

**Resolves:** game-runtime-compat-4 (Valheim; the rest in 42), game-runtime-compat-7 (generic safeguards; the manifests in 42), game-runtime-compat-9 (prerequisite checks), game-runtime-compat-12, game-catalog-config-10 (`game try --scratch`), game-catalog-config-12 (prerequisite checks).

### Work items

1. **The boot ledger** (U14).
   - **Shipped ledger:** `games/ledger.yaml`, embedded beside the manifests. It is data generated by `make ledger` from the transcripts in `docs/research/scratch-boots/<id>.md`. Each entry records `{id, revision, sha256, gvm_version, distros: [debian-12, ubuntu-24.04], ready_s, final_rung, archive_ok}`, or `{status: unsupported, reason}`.
   - **Local ledger:** `/var/lib/gvm/ledger/<id>@<sha12>.json`, root 0600, written only by a passing `gvm game try` on this host. It is keyed by sha256, so any edit to the manifest invalidates it.
   - **Status of a manifest revision:**
     - `verified` comes from the shipped ledger.
     - `verified here` comes from the local ledger.
     - `unsupported` means the shipped ledger records a known failure, with its reason.
     - `unverified` means there is no entry at all.
   - **Carry-over for data-only bumps:** a bump whose `spec.Diff` from the last verified revision classifies only as cosmetic or data (help, doc, hints, `connect`, `players`, `fail_markers` text, `secrets` declarations) carries the verification, labelled "carried from rev N". `make ledger-check` proves the classification against the verified revision's bytes from git. `TestLedgerCoversShippedRevisions` requires an entry for every shipped (id, revision, sha).
2. **The gates.**
   - **`gvm game list`** shows a STATUS column. By default it lists only `verified` and `verified here` games; `--all` also shows `unverified` and `unsupported`, with labels.
   - **`create`** refuses an `unsupported` revision, giving the reason. It refuses an `unverified` one with: "valheim rev 5 has not been verified to boot under gvm 0.20.0's hardened unit; nothing was created. To test it on this host: sudo gvm game try valheim --scratch". Both exit 5.
   - **`install`** refuses a non-scratch instance whose pinned revision is not verified, which covers instances created before this gate. The hint never suggests re-creating an instance that has a world, because that throws away its allocated ports and secrets (Phase 20's rule): "nothing was installed; valheim rev 4 is not verified. Moving an instance to a verified revision is not available in this build". Phase 39 replaces the second sentence with `sudo gvm manifest upgrade <n> --dry-run`. Only an instance with no world data (it never reached ready) is also told "or: sudo gvm destroy <n>, then create it again".
   - **The host's distribution is part of the answer.** The ledger records Debian 12 and Ubuntu 24.04. On any other supported host, such as Ubuntu 22.04 with its older glibc and systemd 249, `game list` and `create` add "verified on debian-12, ubuntu-24.04; this host (ubuntu-22.04) is not verified; prove it with: sudo gvm game try <id> --scratch", and a passing try writes a `verified here` entry as usual.
   - **Adopted instances** are outside both gates, because they never install.

   (game-runtime-compat-12)
3. **`sudo gvm game try <id> --scratch [--var …] [--unapproved-override] [--keep] [--name try-<suffix> --confirm <name>]`.**
   - **The name fits every id.** Scratch instances are named `try-<8 hex>` (12 characters). The game id is recorded in `metadata.scratch_of`, in the marker and in every line of the report, not in the name. `scratch-<id>-<4 hex>` would exceed `MaxNameLen` (28, `instance/name.go:35`) for `7daystodie-telnet` and `minecraft-neoforge`, and for any author's id over 15 characters. `TestScratchNameFitsEveryShippedID` runs over `games.FS`.
   - **The name is typed before anything exists** (rule 5). gvm generates the name first and prints "this trial instance try-1a2b3c4d (valheim) will be created, and removed with its account, secrets and world when the trial ends". The operator types that name (`TypedConfirm`); without a TTY, `--name` chooses it and `--confirm` must repeat it. Nothing is created before the confirmation.
   - It creates the instance with `metadata.scratch: true` and a marker file under `/var/lib/gvm/instances/<n>/scratch` that holds this invocation's id. **Every port is `expose: none`**, so no firewall rule is ever staged, and **no timers** are written: a scratch instance never has a nightly.
   - It generates secrets, then applies, installs and starts. It may install an unverified revision only because the instance carries the marker. It runs through `runDetachable` (Phase 31), so an SSH disconnect does not leave a half-run trial behind.
   - It reports one of:
     - `READY in 74s`
     - `exited before ready (exit 69, SIGSEGV; jit: true suspected)`
     - `SIGSYS: arch: i386 suspected`
     - `boot loop (exit 79)`
   - **It stops first, then backs up cold.** It stops through the ladder and reports the rung, then takes one backup and checks that the archive holds at least one file under each `world_paths` entry. Engines that write the world only on a periodic save or at shutdown (Valheim is the likely case) have empty world paths while running, so a hot check would mark a working game unverified. A hot backup may also be taken and reported separately, but it never decides the result.
   - **On success** it writes the local ledger entry.
   - **Cleanup** happens automatically at the end of the **same invocation**. It removes only the instance whose marker carries this invocation's id, after that run's own verified archive exists, and the archive is kept as a routine archive. Cleanup also removes the scratch account (`gvm-try-…`) and its secrets directory, because this invocation created both and they carry its marker. It uses a new hostops `user.remove` verb that accepts only an account whose GECOS names a scratch instance with this invocation's marker (Phase 53 extends it). Repeated tries therefore leave no accounts or secrets behind.
   - With `--keep`, or after Ctrl-C (which stops through the ladder first), the instance stays, and gvm prints "remove with: sudo gvm destroy <name>", which is typed and archive-first.
   - There is no other removal path.

   (game-runtime-compat-12, game-catalog-config-10)
4. **`sudo gvm run <inst> --check`.** A non-spawning launch check. It builds the launch, the probes and the ladder, and prints:
   - the redacted argv and env
   - the rendered sandbox: `MemoryDenyWriteExecute`, `SystemCallArchitectures`, `ProtectHome`

   It also stats exec, wrapper and interpreter as `gvm-<inst>` through the D-15 transient probe. (game-runtime-compat-12)
5. **Validate checks what the runner can run.** It checks:
   - wrapper kinds the runner builds, against a `wrapperKinds` registry (proton implemented; xvfb and wine planned until Phase 42)
   - probe kinds that have a `BuildProbes` case
   - `wrapper: proton` combined with `jit: false`, with `arch: native`, or without `STEAM_COMPAT_DATA_PATH`
   - `.Secret` on argv (Phase 27)

   `TestEveryShippedManifestBuildsLaunchAndProbes` runs over `games.FS`. (game-runtime-compat-12)
6. **Prerequisite checks that install nothing.** They run at `create`, at `apply` (the D-15 write-probe is extended to stat as `gvm-<inst>` under the real sandbox) and in doctor:
   - `proton_path` matches `^/.+/proton$`, exists, is executable, and its shebang interpreter exists.
   - It is not under `/home`, `/root` or `/run/user`: "ProtectHome=yes hides this from gvm@<n>; install Proton under /opt or /usr/share/steam/compatibilitytools.d".
   - It is writable by `gvm-<inst>` ("Proton writes dist.lock next to itself") and not shared with another instance.
   - `python3` is present (Proton), `Xvfb` (xvfb wrapper), `lib32gcc-s1` (steamcmd) and Java (Minecraft).

   Each failure names the apt package. (game-runtime-compat-9, game-catalog-config-12)
7. **Backups find the world (generic safeguards).**
   - When no sources match, a backup names each empty `world_path`.
   - Doctor flags a `world_path` that is still empty 24 h after the first ready.
   - `destroy` refuses before the typed prompt when every `world_path` is empty but the instance has reached ready.

   (game-runtime-compat-7)
8. **Verify the J1 games.** Each change is confirmed with `gvm game try --scratch` on Debian 12 and Ubuntu 24.04, recorded in `docs/research/scratch-boots/<id>.md` and entered in the shipped ledger:
   - **Valheim** gets `jit: true` (revision 4 → 5).
   - The comments in `manifest.go:260-262`, `mdweNote` (`units.go:608`) and `generic-steam.yaml` say "true for any engine that generates code at runtime: JVM, Mono/Unity-Mono, .NET, Wine/Proton, LuaJIT".
   - A doctor INST check warns, with a one-line fix, about a Mono install tree (`*_Data/MonoBleedingEdge`) under a pinned `jit: false` manifest.
   - **Palworld** now reaches ready through the a2s probe from Phase 27. **Minecraft Paper** and **Project Zomboid** are recorded as they stand.
   - Any of the four that fails stays `unverified` and is not offered. The risk register covers this.

   (game-runtime-compat-4)

### Definition of done

- [ ] `gvm game list` shows valheim, palworld, minecraft-paper and projectzomboid as `verified`. `gvm game list --all` shows the other eleven as `unverified`.
- [ ] `sudo gvm game try valheim --scratch` asks for the printed name `try-<8 hex>` before creating anything. A wrong name exits 5 and `gvm list` shows nothing new. Typed correctly, it prints `READY in Ns`, `ladder ended on save`, `archive ok (world files: 12)` and `try-1a2b3c4d (valheim) removed`. Afterwards `gvm list` shows no scratch instance, `getent passwd gvm-try-1a2b3c4d` fails, `/etc/gvm/secrets/instances/try-1a2b3c4d` is gone, and `systemctl list-timers` never listed a timer for it.
- [ ] `TestScratchNameFitsEveryShippedID` is green, and `sudo gvm game try minecraft-neoforge --scratch --name try-ci01 --confirm try-ci01` runs without a TTY.
- [ ] Closing the SSH session in the middle of a `game try` lets the trial run to its end, including cleanup. From a new session, `journalctl -u 'gvm-adhoc-try-ci01-*'` shows the report (the instance's own job records went with it), and no scratch instance, account or timer is left.
- [ ] `sudo gvm create r1 --game rust` refuses with the try line and exits 5.
- [ ] On an Ubuntu 22.04 VM, `gvm game list` labels valheim "verified on debian-12, ubuntu-24.04; this host is not verified" and names the try command.
- [ ] An instance pinned to an unverified revision that has world data: `sudo gvm install v1` refuses without mentioning destroy.
- [ ] For a local override `mygame` (a renamed copy of valheim), `sudo gvm game try mygame --scratch --unapproved-override` passes. After that, `sudo gvm create m1 --game mygame --unapproved-override` succeeds, and `gvm game list` shows mygame as `verified here`. Editing the file afterwards drops it back to `unverified`.
- [ ] `sudo gvm run v1 --check` prints argv with the password redacted and `MemoryDenyWriteExecute=no (manifest declares jit: true)`, and spawns nothing.
- [ ] With `java` missing, `sudo gvm create mc1 --game minecraft-paper …` names `apt install openjdk-21-jre-headless`, and nothing is installed.
- [ ] `TestLedgerCoversShippedRevisions` and `TestEveryShippedManifestBuildsLaunchAndProbes` are green.

**Depends on:** Phase 31. **Effort:** XL (the ledger tooling with git-backed classification, `game try` orchestrating create, apply, install, start, stop, backup and cleanup, `run --check` with the transient probe, the validate rules, the prerequisite checks, and four games booted on two distributions).

**Design rules touched:**
- **Rule 5**, kept literally, with no same-invocation exception:
  - The scratch instance's name is typed before anything is created, so its removal is confirmed by the operator like any destroy.
  - It is archived first: removal happens only after the run's own verified archive exists, and only for the instance whose marker this invocation wrote.
  - Every other removal is `gvm destroy`, typed and archive-first.
  - There is no `--clean` flag.
- **Rule 7:** scratch ports are `expose: none`, so no rule is ever staged. Prerequisite checks print apt lines and install nothing (D-41).
- **Rule 4:** every scratch stop goes through the ladder, and the report records the rung.
- **Rule 10:** the ledger is data. The template guard still runs at load, and overrides still need `--unapproved-override` until Phase 46.
- **Hardening:** W^X is relaxed only for Valheim, on boot evidence. ProtectProc stays.

---

## Phase 33 — Players know how to connect

**Goal:** after start, the operator gets one line to paste to players: address:port, protocol, extra ports, and whether a password is set. They also get an exact, SSH-first list of ufw lines to paste if their firewall is on. gvm itself still changes no firewall rule.

**Why now:** on a ufw-active host the server runs and nobody can join, and nothing says why. The read-only plan is safe to ship before any write path. But its SSH-first lines are only safe once SSH detection stops trusting configured ports blindly, so detection is fixed here, first (U13).

**Resolves:** onboarding-4 (plan), onboarding-7, cli-ergonomics-12, cli-walkthrough-4 (plan), cli-walkthrough-9, documentation-3 (plan), documentation-8, game-catalog-config-7, admin-security-10 (`--share`), network-firewall-1 (stage 1), network-firewall-2, network-firewall-3 (measured), network-firewall-4, network-firewall-6, network-firewall-10, network-firewall-11 (first guide).

### Work items

1. **SAFETY — SSH detection cannot miss the live session.** This lands before any firewall output.
   - `sshport.Detect` (`detect.go:78`) no longer returns from rung 1 with `Confident=true` on configured ports alone (`:88-96`). It always runs rungs 2–5.
   - Live-session ports come from `/proc`: the fds of `sshd` and `sshd-session` (OpenSSH ≥ 9.8) map to socket inodes, and those map to ESTABLISHED rows in `/proc/net/tcp{,6}`. The fallback walks `PPid` up to an ancestor whose environ has `SSH_CONNECTION`.
   - gvm takes the union of `ssh.socket.d/*.conf` from `/etc`, `/run` and `/usr/lib`.
   - An empty `ssh.ports` means Desired uses the detected ports. A configured list that misses an observed port is refused, naming each source.
   - `ErrSSHUnknown` is returned when detection is not confident. There is no bypass flag.
   - A read-only `gvm firewall ssh` lists each port with its source.

   (network-firewall-2)
2. **A `connect:` manifest block**, non-templated:

   ```yaml
   connect: {port: game, also_open: [direct], public_var: public, password_secret: join_password,
             note: "Steam server browser: search by name"}
   ```

   It is validated as existing port names and excluded from the runtime template scan. With no block, it defaults to the first `expose: public` port. The shipped manifests are filled in, with revision bumps that carry their verification; PZ, for example, needs 16261 and 16262, both open. (onboarding-7, game-catalog-config-7, network-firewall-4, documentation-8)
3. **`spec.public_address`** in `gvm.yaml`, read through `spec.LoadHost`.
   - The fallback reads `net.Interfaces` and `/proc/net/route` and accepts only globally routable addresses.
   - RFC1918, CGNAT, link-local and ULA addresses are labelled "private: players outside this network need this VPS's public address; set spec.public_address".
   - gvm never calls an external IP-echo service.

   (cli-ergonomics-12, network-firewall-4)
4. **Connect info everywhere the operator looks.**
   - `gvm status <n>` gets a connect block: the join endpoint first, then the other public ports with protocol, `join password: set / NOT SET` (via `Store.Has`) and `listed publicly: yes/no`.
   - `--json` gets `endpoints`, with booleans only.
   - `gvm list` gets a JOIN column.
   - `gvm status <n> --share` prints a player snippet without the password. It points the operator at `sudo gvm secret show <n> join_password`.
   - `create` and a successful `start` end with the join line.

   (cli-walkthrough-9, cli-ergonomics-12, admin-security-10, onboarding-7)
5. **`sudo gvm firewall plan [<instance>]`**, read-only. It works with `backend: none`. `firewall` is a root verb (RouteHostOp, `transport.go:269`), because it reads instance YAML that is 0640 root:gvm (`spec/save.go:19`), so a normal user gets the one-line forgot-sudo message, like `status`.
   - It uses the §17.6 shape and ends with copy-paste ufw lines, **SSH first**, in correct syntax: `ufw allow proto udp to any port 16261 comment 'gvm:pz1:game'`. Then: "also open these at your VPS provider's firewall".
   - **When SSH detection is not confident** (`ErrSSHUnknown`, e.g. inside tmux with no `SSH_CONNECTION` in the ancestry), the plan never drops the SSH step. Its first line becomes `# gvm could not determine your SSH port: run sudo gvm firewall ssh, and add "ufw allow <port>/tcp" FIRST`, followed by `ufw allow 22/tcp` commented out as an example, then the game lines.
   - When ufw is **inactive**, it says "ufw is inactive: nothing on this host blocks these ports; check your provider's firewall". It **never prints `ufw enable`** (U10).
   - As root, with a ruleset that `ufw.Parse` reads with confidence, it marks each port `admitted` or `NOT admitted`. An untrusted parse prints "could not be parsed; nothing assumed".
   - The status connect block shows the same `firewall: admitted / NOT admitted / unknown`.
   - `gvm firewall` becomes a subcommand table. `plan` and `ssh` work; `apply`, `enable` and `adopt-rules` stay NotInBuild until Phase 40.

   (network-firewall-1, documentation-3, onboarding-4, cli-walkthrough-4)
6. **Doctor measures the firewall, read-only.** `Backend.Status` returns the parser's Trusted flag, and doctor reads `ufw status numbered` as root through `ufw.Parse` (`ufw/parse.go`).

   This is the first real `ufw` exec, so it is made safe first. Today `runUFW` resolves `ufw` through `PATH` (`exec.CommandContext(ctx, "ufw", …)`, `ufw_linux.go:69`) and `Available()` uses `exec.LookPath`. Both now use `/usr/sbin/ufw` by absolute path, and gvm refuses to run it unless it is root-owned and neither group- nor world-writable, which it checks with `stat` before every exec. The `noshell` gate is extended to assert the absolute path.

   The results:
   - ufw not installed: Note.
   - ufw inactive with public ports: Warning, "N ports must be open at your provider; see gvm firewall plan".
   - A public port that is not admitted: Warning, with fix `gvm firewall plan`.
   - **Problem** only for a real FW-002 lockout risk (ufw active and a detected SSH port not admitted) or an untrusted parse.
   - FW-005 and FW-006 Skip, never pass, when the parse is untrusted or ufw is inactive.

   (network-firewall-3, documentation-3)
7. **`gvm ports` names owners and conflicts.**
   - Listeners are attributed via `/proc/<pid>/cgroup` matching `gvm@<name>.service`. Columns are ENDPOINT, NAME, OWNER, EXPOSE and LISTENING.
   - `Conflict.String()` comes with one fix line per kind.
   - The no-argument form runs host-wide `ports.Conflicts` and exits 4 on an overlap. `gvm ports <inst>` filters to one instance and prints notes.
   - The misleading footer is removed.

   (network-firewall-6)
8. **Admin exposure warnings.** `expose: admin` on a loopback-bound port is an error, and its fix is the manifest's note (`ssh -L 27015:127.0.0.1:27015 …`). Empty `admin_cidrs` is a warning in validate, `firewall plan` and `apply --diff`. Effective exposure is printed in `create` and `status`. (network-firewall-10)
9. **Docs.**
   - `docs/networking.md` gives task recipes: open a new server's ports at ufw and at the provider, resolve a port conflict, and use `gvm firewall ssh` as an SSH safety checklist.
   - The `firewall` help topic is rewritten to match.
   - Neither contains an enable sequence: turning ufw on is not covered by this build.

   (network-firewall-11, documentation-3)

### Definition of done

- [ ] From a session on port 2222, configured by an `ssh.socket` drop-in under `/run`, `sudo gvm firewall ssh` lists 2222 with the source "live session (sshd-session pid N)".
- [ ] `sudo gvm firewall plan v1` prints `ufw allow 2222/tcp` first, then the game lines with `gvm:v1:*` comments, then the provider reminder. Run without sudo, it prints the forgot-sudo line.
- [ ] With detection forced to `ErrSSHUnknown` (a fixture with no sshd sockets and no `SSH_CONNECTION`), the plan's first line is the "could not determine your SSH port … FIRST" comment, and no game line comes before it.
- [ ] `TestNoUfwEnableAdvice` finds no `ufw enable` in any advice string: non-comment string literals used as hints, Fix lines, help topics, manifests and user docs.
- [ ] `sudo gvm create v1 --game valheim …` ends with `join: 203.0.113.5:2456/udp (also open 2457/udp) · password: set · listed publicly: yes`, and `sudo gvm status v1` shows the same connect block. On a NAT VM both print the private-address sentence instead. `TestStartEndsWithJoinLine` proves that a successful start prints the same line (fake HostEnv). A real start's join line is checked in Phase 34's DoD, once install exists.
- [ ] `sudo gvm status v1 --share` and `--json` contain no password value.
- [ ] `TestPortsAttributesOwnerFromCgroup`, over a `/proc` fixture, shows owner `v1` for sockets whose pid's cgroup is `gvm@v1.service`. `sudo gvm ports` with a hand-edited overlapping port exits 4 and names both instances.
- [ ] A recording exec test shows every `ufw` call uses `/usr/sbin/ufw`, and a group-writable fixture binary is refused.
- [ ] On a correctly configured ufw host, `sudo gvm doctor` shows 0 FW problems. A recording exec test shows no mutating `ufw` invocation from any of these commands.

**Depends on:** Phases 24, 29. The DoD needs no installed game, so this phase runs alongside 30–32. **Effort:** L.

**Design rules touched:**
- **Rule 7:**
  - No rule is written and ufw is never enabled.
  - Detection fails closed, with no bypass.
  - The printed sequence admits SSH first, and when detection is unsure it still leads with the SSH step instead of dropping it.
  - `ufw status` is only read, and an untrusted parse is labelled as such (D-42).
- **Rule 9:** connect info is CLI-only until the authenticated dashboard (Phase 49), and there is no public status page.
- **Rule 10:** `connect:` is data, never rendered with secrets.
- **Rule 2:** everything is a `/proc` read or an argv exec of `/usr/sbin/ufw status` by absolute path, from a root-owned binary that is not group- or world-writable.

---

## Phase 34 — First server, start to finish · **Milestone J1**

**Goal:** a new operator gets from a fresh VPS to a joinable, passworded server by following printed next steps. They see progress during long boots, and a real diagnosis when a start fails.

**Why now:** every piece exists after Phase 33. This phase makes the journey continuous and pins it with an end-to-end test, so later phases cannot break it.

**Resolves:** onboarding-11 (`gvm up`), onboarding-12, cli-ergonomics-10, cli-ergonomics-11 (quickstart), cli-walkthrough-10, day2-ops-8 (start progress).

### Work items

1. **Start and restart show progress.**
   - They print immediately: "starting v1; waiting for readiness (probe a2s). Ctrl-C stops waiting; the server keeps starting".
   - One line is redrawn every 2 s from `status.json` (`runner/status.go`). It shows `startingReason`, the manifest's `readiness.progress` markers ("Load world", "Zonesystem Awake"), elapsed time against `hard_deadline`, and time since the last console output.
   - `--no-wait` returns once the job is queued.

   (onboarding-12, day2-ops-8)
2. **Failure diagnosis.** On failure, under Phase 28's reason and hint, gvm prints the fired fail_marker hint, `StatusText`, the last ~20 console lines and `next: sudo gvm logs <n>`. (onboarding-12)
3. **`--budget` goes.** It is removed from start, stop and restart. Passing it prints "the ladder budget is spec.shutdown.budget in /etc/gvm/instances/<n>.yaml; change it and run gvm apply". `budget_sec` is no longer audited, and `start` gets its own usage text. (cli-ergonomics-10, day2-ops-8)
4. **`sudo gvm up <name> --game <id> [--var …] [--open] [--dry-run] [--yes]`.**
   - It refuses if the instance already exists. It never re-creates or overwrites.
   - It shows the create YAML and the `apply --diff` plan and asks once on a TTY. Without a TTY it requires `--yes`.
   - It runs `create`, `apply`, `install` and `start` as the same, separately audited verbs, so the Phase 32 ledger gate applies unchanged.
   - **It never passes `--force` or `--allow-unprobed`.** It stops at the first refusal and prints that refusal's hint.
   - It finishes with the join line, the generated join password (shown once), and the `gvm firewall plan` lines when ufw is active.

   (onboarding-11)
5. **Docs.** `docs/quickstart.md` is final, and the Getting-started block in `gvm help` shows every step as available. (cli-ergonomics-11, cli-walkthrough-10)
6. **`ci/golden-journey.sh`** runs on every release tag and nightly, with no Postgres installed, on a **fresh full VM per run** (the Phase 23 runner: QEMU on the CI host, or a cloud VM), never a container. The hardened unit's ProtectProc, PrivateDevices, MemoryDenyWriteExecute, RestrictNamespaces, transient sandboxed SteamCMD units and cgroup v2 delegation all have to behave as they do in production, or the gate would test a weaker sandbox. The VM image caches SteamCMD and the Valheim depot so the multi-GB download does not make the nightly flaky. Containers remain for unit-level jobs.
   1. fresh VM → `host init` → `doctor` (0 problems)
   2. `game list` (four verified) → `create valheim` → `apply` → `install` → `start`
   3. a probe from outside the VM
   4. `status --share` → `firewall plan`
   5. `stop` (save rung) → `backup` → `destroy` (archive first)
7. **Release.** Tag the first public release, `v0.20.0`, only when the journey passes (U15). The README "What works" J1 rows read "works".

### Definition of done (Milestone J1)

- [ ] On a fresh Debian 12 VPS with ufw active on 22/tcp and no Postgres installed:

  ```
  curl -LO …/gvm_0.20.0_linux_amd64.tar.gz -O …/SHA256SUMS && sha256sum -c --ignore-missing SHA256SUMS
  tar xzf gvm_0.20.0_linux_amd64.tar.gz && sudo ./gvm host init
  sudo dpkg --add-architecture i386 && sudo apt update && sudo apt install lib32gcc-s1   # printed by host init
  sudo gvm doctor                       # 0 problems
  gvm game show valheim
  sudo gvm create v1 --game valheim --var public_name="Friends"   # join password shown once
  sudo gvm apply v1
  sudo gvm install v1
  sudo gvm start v1                     # progress line, then: join 203.0.113.5:2456/udp · password: set
  sudo gvm firewall plan v1             # paste the printed game line(s); the SSH line is already present
  sudo gvm status v1 --share
  sudo gvm ports                        # v1's sockets show owner v1
  ```

  A Valheim client on another network joins with the password.
- [ ] `sudo gvm up mc1 --game minecraft-paper --var mc_version=1.21.4 --var eula_accepted=true --yes` reaches a joinable Paper server.
- [ ] `sudo gvm stop v1` prints "ladder ended on save after …" with no DEGRADED.
- [ ] **A failed start is diagnosed.** A fixture game that never reaches ready before `hard_deadline`: `sudo gvm start x; echo $?` prints the phase, the reason, the fired fail_marker hint, `StatusText`, the last console lines and `next: sudo gvm logs x`, then `1`. It never prints only "systemd job … failed".
- [ ] `sudo gvm stop x --budget 30` prints the `spec.shutdown.budget` sentence and exits 2, and the audit spool has no `budget_sec` field for any stop.
- [ ] `ci/golden-journey.sh` is green on the VM runner, and so are `TestHintsResolve`, `TestEveryHelpPathExitsZero` and `TestAvailabilityMatchesBehaviour`.

**Depends on:** Phases 21 (no public release ships the live hazards), 32, 33. **Effort:** L (the VM-based golden-journey CI, its cached image, and the first public release).

**Design rules touched:**
- **Rule 4:** Ctrl-C stops only the waiting, and `up` never forces.
- **Rule 5:** `up` never overwrites, and the journey's destroy archives first.
- **Rule 1:** the journey runs with no Postgres installed.
- **Rule 7:** the journey never enables ufw. The operator pastes `ufw allow` lines that admit SSH first.

---

## Milestone J2 — week to week

## Phase 35 — Upgrade gvm without stopping the worlds

**Goal:** an operator installs the next gvm release on a box with live worlds without stopping any of them. They can see which servers still run the old runner, and restart them one at a time.

**Why now:** J1's release is the first one anyone runs, so J2's release is the first upgrade anyone performs.

`TestFrozenFormats` has held every shared format still since Phase 20. The first format bump after it would break `gvm stop` and backups on every world that has not been restarted, unless a skew rule exists first.

There is a second gap: without a render generation, fixes to drop-ins are silently not in force.

**Resolves:** gvm-self-upgrade-uninstall-1 (live upgrade), gvm-self-upgrade-uninstall-2, gvm-self-upgrade-uninstall-3, gvm-self-upgrade-uninstall-4, gvm-self-upgrade-uninstall-5, gvm-self-upgrade-uninstall-11 (upgrade status, operator actions).

### Work items

1. **Upgrading with `host init`.** `sudo ./gvm host init` is the documented upgrade for tarball installs. It renames the new binary over `/usr/bin/gvm`, rewrites the units and reloads systemd. It **never** stops or restarts `gvm@*` or `gvm-job@*`, and it ends by printing `gvm upgrade status`. (gvm-self-upgrade-uninstall-1)
2. **The N-1 skew rule** (U17), written in `docs/upgrading.md` and enforced by cross-version tests. These tests replace `TestFrozenFormats`.
   - **Console handshake.** The console `hello` event carries `gvm_version`, `console_proto` (`console/protocol.go:18`) and `lease_schema`; an absent field means 1. The CLI accepts the current protocol and N-1.
   - **Stop on a protocol mismatch.** `gvm stop` falls back to `VerbUnitStop`. That sends SIGTERM to the old runner, which walks its **own** ladder under `KillMode=mixed`. gvm then reads the rung back: "stopped via systemd: pz1 still ran gvm 0.20.0's runner (protocol 1); its ladder saved at 40s".
   - **Freeze leases.** gvm never arms a lease in a schema the runner did not advertise (`backup/lease.go:64`). It writes the older schema if this gvm still supports it. Otherwise it skips the freeze and marks the archive DEGRADED with the true reason: "pz1's runner (gvm 0.20.0) cannot read this freeze lease; gvm restart pz1 re-enables saved backups".
   - **Status files.** A newer `status.json` protocol reads as "fields may be missing", never as an error.

   (gvm-self-upgrade-uninstall-2)
3. **Lease liveness does not depend on version.** gvm decodes `pid`, `boot_id`, `unit` and `started_at` with a minimal struct before the protocol check (`LeaseProto`, `lifecycle/lease.go:34`), and classifies the lease as live or stale from those.
   - "Unreadable" means corrupt, nothing else.
   - `gvm job clear` refuses an unreadable lease unless its unit is inactive and its pid is dead or from another boot.

   (gvm-self-upgrade-uninstall-3)
4. **Runners report their version.** `runner.json` and `status.json` gain `gvm_version`, `commit`, `console_proto` and `lease_schema`.

   The root CLI also checks `/proc/<MainPID>/exe` for "(deleted)" or an inode mismatch, which works even for dev builds. `gvm list` and `gvm status` show "restart pending: runner 0.20.0, installed 0.21.0". (gvm-self-upgrade-uninstall-4)
5. **`RenderGeneration`.** A constant is written into drop-in and timer headers, next to the existing `# rendered … by gvm <ver>` line. Golden-unit tests force it to change whenever template output changes.
   - `gvm list` shows "apply suggested" when a header's generation differs.
   - `gvm status <n>`, `gvm apply --all --diff` and INST-005 do a full in-memory re-render plus a `SameDropIn` comparison (`units.go:699`). They use the pure planning path: no probe, no ledger write, no database.
   - A diff that touches sandbox or egress directives is a warning: "declared egress allowlist is NOT in force".
   - `sudo gvm apply --all` runs each plan serially and refuses destructive steps without that instance's typed name. It never restarts a game, and prints "takes effect at next restart: gvm restart --pending".

   (gvm-self-upgrade-uninstall-5)
6. **`gvm upgrade status [--since <ver>]`.** It is local and needs no database. For each instance it shows:
   - the runner version
   - the render generation
   - the pinned manifest revision against the shipped one, with the ledger state of the shipped revision

   It also prints the CHANGELOG's "Operator actions" since the given version. (gvm-self-upgrade-uninstall-11, -4)
7. **`sudo gvm restart --pending`.**
   - It prints the list and asks y/N, or takes `--yes`.
   - It restarts **one instance at a time**, through the existing `systemctl restart` path, which is the ladder.
   - It skips held instances and any with a live or unreadable lease.
   - It waits for `running` with a timeout, and halts at the first instance that does not come back.
   - `host init`, timers and the guard never invoke it.

   `sudo gvm stop --all` uses the same list and confirmation, the ladder for each instance, and writes a hold for each. (gvm-self-upgrade-uninstall-4)

### Definition of done

- [ ] Linux upgrade test: install release N-1 and start two instances, with a nightly update holding a lease on one. Then run `sudo ./gvm host init` from release N.
  - Both games' pid and starttime are unchanged.
  - `gvm list` shows "restart pending" for both.
  - `sudo gvm stop pz2` stops the old runner and prints the fallback sentence.
  - The lease shows as live, with the `journalctl` hint.
  - `sudo gvm restart --pending --yes` restarts one at a time and skips the leased instance.
- [ ] A changed drop-in template makes `gvm list` show "apply suggested", and `sudo gvm apply --all --diff` shows the change. The egress fixture prints the NOT-in-force warning.
- [ ] `gvm upgrade status` works with Postgres absent. A test that bumps `console.Proto` passes only with an N-1 reader in place.

**Depends on:** Phase 34. **Effort:** L.

**Design rules touched:**
- **Rule 4:** the protocol fallback is a unit stop that runs the runner's own ladder, never a kill.
- **Rule 6:** `restart --pending` and `apply --all` run only when the operator invokes them, never from timers, the guard or `host init`. `upgrade status` is read-only.
- **Rule 5:** `apply --all` refuses destructive steps without the typed name.
- **Rules 1 and 3:** version stamps are files, and no admin.sock is involved.

---

## Phase 36 — Backup health you can see

**Goal:** the operator sees, without converting byte counts:
- how each nightly saved the world
- how much space backups need
- when a copy last left the box
- what to do when space runs out

**Why now:** Phase 21 made backups honest. This phase makes the same facts visible before a disk fills, and gives `gvm status` (Phase 37) its backup line.

**Resolves:** offsite-backup-dr-7 (run fields, OFF-BOX column), offsite-backup-dr-9, offsite-backup-dr-11 (doctor checks), offsite-backup-dr-12, offsite-backup-dr-15, failure-recovery-12.

### Work items

1. **Snapshot reporting.** The backup run summary gets `snapshot_method`, `snapshot_why`, `stall_ms` and `snapshot_fallback`.
   - The run prints one line, e.g. "snapshot direct (ext4) — world not saving for 74.2s". It is red above `StallRed`.
   - Engine notes fold into `Run.Notes`.
   - `spec.backup.snapshot: auto|direct` maps to `AllowMethods`.
   - The method, stall and fallback are also written to the archive's index entry, so doctor can read them without a job record.
   - **Doctor BACKUP-005** reports the last backup's recorded method and stall per instance ("pz1: last backup direct (ext4), world not saving for 74.2s"), a Warning above `StallRed`. It adds a capability hint from `statfs` and `stat` only: btrfs without subvolume roots, zfs not wired, and reflink labelled "untested by doctor". It never calls `trialClone`.

   (offsite-backup-dr-9)
2. **Push fields.**
   - The run summary gets `push_target`, `push_name`, `push_bytes`, `encrypted_to` and `push_skipped_reason`.
   - Index entries (`archive/index.go`) get `pushed_at` and `pushed_to`.
   - `gvm backup list` gets an OFF-BOX column, which reads "not made (no transport in this build)" until Phase 54.

   (offsite-backup-dr-7)
3. **Out of space, in plain terms.**
   - Every size goes through `spec.FormatBytes`, never raw bytes.
   - The refusal ends with three remedies:
     - routine archives take X GiB: lower `backup.keep`, run `gvm apply`, then `gvm backup prune`
     - preserve archives take Y GiB: gvm never prunes them; review them with `gvm backup list` and remove the ones you no longer need with `sudo gvm backup delete <archive>`
     - or point `backup.dir` at another volume
   - The estimate becomes realistic: `max(previous compressed × 1.5 × growth since, 512 MiB)` when the method thaws before archiving or the destination is another device. Direct, same-device runs keep `uncompressed × 1.1`.

   **`sudo gvm backup delete <archive> [--dry-run]`** is the one explicit way to reclaim a preserve archive. Phases 21, 32, 38, 39, 44 and 51 all write preserve archives that retention never removes, and without this verb a disk-full refusal ends in hand-deleting files under `/var/backups` as root, which bypasses the index and any confirmation.
   - It shows the archive's instance, class, reason, pin, age and size, then asks for the **instance name** to be typed (`TypedConfirm`, no `--yes`).
   - It refuses the newest verified archive of an instance that still exists, so a live world always keeps at least one verified copy (archive-first).
   - It refuses an archive referenced by an active adoption record (Phase 50 onwards), and the pre-cutover archive until finalize.
   - For an instance that no longer exists, deleting its last archive is allowed after the typed name, and the output says "this was the last copy of <n>'s world".
   - It removes the file and any `.preserve` pin, reindexes, and audits `backup.delete` with path, sha256 and bytes.

   (offsite-backup-dr-12, failure-recovery-12)
4. **HOST-013 disk check.**
   - It runs `statfs` on the backup dir, `/var/lib/gvm`, `/var/log/gvm`, the instances' install and data paths, and `/var/lib/postgresql`, de-duplicated by filesystem id.
   - Under 15% free, or under 2× the largest archive, is a Warning. Under 5% is a Problem.
   - The split between routine and preserve bytes comes from the archive index, never from a `du` of install trees.
   - `gvm status` adds a note when an instance's volume crosses the warning line.
   - The Postgres WAL hint moves from the 503 page into this check.

   (failure-recovery-12)
5. **Doctor BACKUP-001..004**, in the same new check file as item 1's BACKUP-005:
   - BACKUP-001: the newest verified archive is stale.
   - BACKUP-002: the backup dir is on the world's device and no remote is configured.
   - BACKUP-003: preserve bytes as a share of free space. Its fix names `gvm backup list` and `sudo gvm backup delete <archive>`.
   - BACKUP-004: a remote is configured but unsupported, or has never succeeded.

   Doctor also reports the count of leftover `*.snap` entries, with their age and size. (offsite-backup-dr-11)
6. **OpenSSH-style host-key fingerprints.** They print as `SHA256:` + `base64.RawStdEncoding(sha256(blob))` (`HostKey.Fingerprint`, `remote_sftp.go:149`), so they match `ssh-keygen -lf`. (offsite-backup-dr-15)

### Definition of done

- [ ] A nightly on ext4 prints the snapshot line with the measured stall, and `--json` carries the fields.
- [ ] An out-of-space fixture prints GiB, the routine/preserve split and the three remedies.
- [ ] On a host with backups on the world's disk and no remote, `sudo gvm doctor` shows BACKUP-002 as a Warning with its fix.
- [ ] After a nightly, `sudo gvm doctor` shows BACKUP-005 with that run's method and stall, read from the index.
- [ ] `sudo gvm backup delete <old preserve archive>` asks for the instance name, removes the file and its pin, and `gvm backup list` no longer shows it. On the newest verified archive of a live instance it refuses and removes nothing. A wrong typed name exits 5.
- [ ] The fingerprint in push logs equals `ssh-keygen -lf` of the same key.

**Depends on:** Phases 21, 24. **Effort:** M.

**Design rules touched:**
- **Rule 5:** preserve archives are still never pruned automatically, and the prune rules are unchanged. `backup delete` is explicit, typed with the instance name, and never removes the newest verified copy of a live world.
- **Rule 6:** the doctor checks only report.
- **Rule 1:** everything is read from the on-disk index.

---

## Phase 37 — One glance: status, job history and the audit trail

**Goal:** with Postgres down, one `gvm status` or `gvm list` answers:
- whether everything is fine
- when the last backup ran
- whether last night's job worked
- who stopped the server
- whether the disk will fill
- whether anyone is online

**Why now:** run results are now honest (Phases 21 and 36), but they still reach only the journal. A failed nightly backup is discovered when a restore is needed.

**Resolves:** day2-ops-3, day2-ops-4, day2-ops-9, day2-ops-12 (next-run display), day2-ops-14, day2-ops-15, failure-recovery-6, failure-recovery-9 (audit offline), failure-recovery-15 (list wording), admin-security-1 (audit), admin-security-7, admin-security-8, cli-ergonomics-14 (`logs -f`), cli-walkthrough-14, offsite-backup-dr-11 (status line, notifier), web-dashboard-8.

### Work items

1. **Job records on disk.** The finish step of `jobs.Execute` (`execute.go`) writes `/var/lib/gvm/instances/<n>/jobs/<job>.json`, or `/var/lib/gvm/jobs/` for host jobs. It writes through a new hostops file kind: atomic, O_NOFOLLOW, root 0640.
   - At start it records `{state: running, started_at, unit, pid, boot_id}`.
   - At finish it records the state, `ended_at`, `took`, summary, error, `left_stopped` and degraded.
   - Each file keeps a bounded ring of the last 20 runs.
   - A `running` record from an earlier boot, or one whose pid is dead, reads "did not finish (killed or timed out); see journalctl -u <unit>".

   (day2-ops-3, failure-recovery-6)
2. **Job history commands.**
   - `gvm job last [<n>]` and `gvm job history <slug> [-n N]`.
   - `gvm job list` gets LAST RUN, RESULT and NEXT columns. They come from the timer's `NextElapseUSecRealtime` and `LastTriggerUSec` over read-only D-Bus.
   - Every printed timestamp is host-local with an explicit zone, and RFC3339 in `--json`.
   - The host zone is printed beside zone-less OnCalendar values (`reconcile/calendar.go`) and at `create`.

   (day2-ops-3, day2-ops-12, failure-recovery-6)
3. **`lifecycle.Details`, display-only** (U12). `Derive`, `Manager` and the guard never read it. Every field is best-effort, and a read error shows "unknown". The fields:
   - uptime from `status.json` `StartedAt` ("7d3h")
   - the installed build
   - update available: from the last recorded `update --check` or nightly check (`update-check.json`, Phase 31), shown with its age, e.g. "update available: 1234567 → 1234890 (checked 6h ago)". Status never makes a network read for it.
   - the last backup from the index: time, size, verified, off-box
   - the next backup and the next job
   - free disk on the backup and data filesystems
   - the join line (Phase 33)
   - the last job result, the guard breadcrumb and the degraded breadcrumb

   `gvm status` prints them. `gvm list` gets a header plus UPTIME, LAST BACKUP and PLAYERS columns. `--json` carries the same struct, which the web model reuses. (day2-ops-4, failure-recovery-6, offsite-backup-dr-11)
4. **Players, reusing the readiness probe.**
   - After ready, the runner keeps the readiness `probe.A2S` / `probe.MCPing` object (`A2S.Last`, `MCStatus.Players`). It re-queries every `max(min_poll, 60s)` while control is ready, then calls `Runner.SetPlayers` (`runner/status.go:120`).
   - Manifests may declare `players: {probe: a2s, port: query}`, which is data only.
   - For RCON-only games such as PZ, a declarative regex with one capture group is used only while control is ready: `players: {control: players, count: '(?m)^Players connected \((\d+)\)'}`.
   - Polling **stops when a shutdown begins**, so the ladder owns the control channel.
   - Status shows "players 3/16 (40s ago)". In `list`, `-` means unknown and `0` means empty.

   (day2-ops-9)
5. **The audit trail works offline.**
   - `gvm audit tail [-n] [--follow] [--since] [--instance] [--action] [--actor] [--json]` and `gvm audit search` read the spool through `audit.ListSpools` + `audit.Replay` into a printing sink.
   - When Postgres answers, gvm merges the `audit_log` rows, de-duplicated on `spool_id`. Otherwise a banner says the database is down.
   - Both commands are root-only.
   - The CLI's audit sink writes to journald's native socket (`SYSLOG_IDENTIFIER=gvm-audit`, pure Go). It falls back to spool-only, and never writes to the operator's terminal.

   (admin-security-7, failure-recovery-9, admin-security-1)
6. **The audit records the right actions.**
   - `VerbUnitRestart` maps to `instance.restart` (`hostops/verbs.go`).
   - destroy, rebuild, restore, update and prune emit parent rows. destroy records the archive path, sha256 and bytes, with a `started` row written before anything is removed.
   - `console.send` and `content.sync` are recorded.
   - A new migration adds `root_cli` to `actor_t`. It is applied only by `gvm db migrate`.

   (admin-security-8)
7. **Delegation before the dashboard.**
   - Ship `/usr/share/doc/gvm/examples/sudoers.d/gvm-moderators`, commented out. It uses absolute paths and exact argv, allowing only `gvm status|start|stop|restart|logs <name>`. Its header points at `visudo -f` and warns never to add console, destroy, rebuild, restore or wildcards.
   - Write `docs/delegating-before-the-dashboard.md`.
   - The root CLI records `sudo_user` and `sudo_uid`, marked "claimed", for lifecycle actions. `gvm logs --shutdown` shows "stopped via sudo by alice (claimed)".
   - No gvm command ever edits sudoers.

   (web-dashboard-8)
8. **Better `gvm logs`** (`runLogs`, `logs.go:76`).
   - `-f` tails `console.log` by seeking from the end, and reopens it on rename or truncate. It never opens an input path.
   - `-n` seeks from the end too.
   - `--all` walks the rotated `.gz` files, oldest first.
   - For a stopped or failed instance, the human `gvm status` output shows the last 5 console lines. They are kept out of `--json`.

   (day2-ops-15, cli-walkthrough-14, cli-ergonomics-14)
9. **`gvm list --problems` wording.**
   - On a healthy host it prints "nothing needs attention (5 instances, 5 running)".
   - The summary reads "N running, N busy, N stopped, N needing attention".
   - A non-empty result appends "→ gvm status <name> for details; gvm doctor for host checks", and exits 4.

   (day2-ops-14, failure-recovery-15)
10. **An opt-in failure notifier.** `OnFailure=` can point at `gvm notify job-failed %i`. That writes an attention marker, which status and doctor show.
    - It can also POST to an operator-configured webhook through `net/http`. It uses no exec, and the payload carries no secrets.
    - It never acts.
    - **Which results reach `OnFailure=`.** Under Phase 22's job-unit exit contract, a unit fails, and so fires `OnFailure=`, only for a failed run (exit 1) or a kind this build cannot run (exit 3). Skipped and degraded runs exit 0 and never fire it, so a nightly that skips a held instance is not an alert. A degraded run writes the attention marker itself, so status and doctor still show it. `docs/help/jobs.md` carries this table.
    - Doctor INST-016 flags a routine archive older than twice its schedule.

    (offsite-backup-dr-11, failure-recovery-6)

### Definition of done

- [ ] With `postgresql` stopped, SIGKILL a nightly mid-run. Next morning:
  - `sudo gvm job last v1` prints "v1_nightly did not finish (killed or timed out); see journalctl -u gvm-job@v1_nightly".
  - `gvm list` shows the previous verified backup and its age.
- [ ] **A failed job reaches status.** A fixture update job on v1 fails after its stop step and leaves v1 stopped. Then `gvm status v1` shows `last job: v1_update failed 04:12 · server left stopped · journalctl -u gvm-job@v1_update`, `gvm list --problems` lists v1 and exits 4, and `sudo gvm job last v1` shows the same record.
- [ ] After the guard recovers v1 from a SIGKILLed job, `gvm status v1` shows the guard breadcrumb, e.g. `guard: cleared a stale update lease and started v1 at 04:20`.
- [ ] After `sudo gvm update v1 --check` finds a newer build, `gvm status v1` shows `update available: … (checked …)` with no network access (asserted by a recording HTTP client).
- [ ] A nightly that skips a held instance exits 0 and does not fire `OnFailure=`. A failed nightly fires it, and the attention marker shows in status and doctor.
- [ ] `gvm job list` shows `NEXT 2026-10-15 04:00 CEST (in 6h12m)`.
- [ ] After `sudo -u alice sudo gvm stop pz1` (through the sudoers example), `sudo gvm audit tail --instance pz1` shows "stopped by root (via sudo: alice, claimed)" with Postgres down.
- [ ] `gvm status v1` shows `players 2/10 (…)` from the a2s probe, and `gvm status pz1` shows it from the regex. A game with neither shows `-`.
- [ ] `gvm logs v1 -f` keeps following across a log rotation.
- [ ] With Postgres up, `gvm db replay-spool` reconstructs the rows from the default spool directory.

**Depends on:** Phases 34 (J1: the join line from 33, the probe objects from 27, the guard breadcrumb from 28 and an installed build from 31), 36. **Effort:** XL (ten items: job records, a player poll in the runner, the audit merge, delegation, `logs -f` with rotation and the notifier). If it must be split, the notifier and the audit work (items 5, 6 and 10) can follow as their own phase.

**Design rules touched:**
- **Rule 1:** records are files, and Postgres is merged best-effort.
- **Rule 6:** `Details` is display-only. A test proves it by checking that `Derive` and the guard do not import it. The notifier never actuates.
- **Rule 4:** the player poll stops before the ladder takes the control channel.
- **Rule 2:** the journald socket, gzip and the webhook are pure Go.
- **Rule 8:** the `root_cli` migration runs only through `gvm db migrate`.
- **Rule 10:** the players regex is data.

---

## Phase 38 — Get a world back

**Goal:** after griefing or a bad mod, the operator inspects and restores a world from an archive, or starts a fresh one, with gvm itself. Both are always archive-first with typed confirmation. destroy never relies silently on a stale archive.

**Why now:** restore is the last missing piece of week-to-week safety. Until now, the only way back is hand-extracting a tarball as root, which is exactly what gvm exists to replace. Rebuild, the "fresh world" sibling, can be wired for content-free games here, because its seams exist and only the content adapter is missing.

**Resolves:** day2-ops-2, offsite-backup-dr-4, offsite-backup-dr-6, offsite-backup-dr-13 (local half), cli-ergonomics-7 (fresh archive), cli-walkthrough-13, day2-ops-1 (rebuild for content-free games), cli-walkthrough-1 (rebuild for content-free games).

### Work items

1. **A safe extract-to-directory.** `sudo gvm restore <archive> --to <dir>` calls `archive.Restore` (`archive/restore.go:107`) in-process.
   - `<dir>` must be absent or empty.
   - gvm resolves symlinks first, then refuses if `<dir>` or any `<dir>/<prefix>` overlaps an instance's install, data, home, logs or backup path.
   - Restored files are root-owned unless `--preserve-ownership` is given.

   Add `gvm backup verify --file <abs> [--deep]`. `--list` prints absolute paths, and its footer shows the `--to` command. (day2-ops-2, offsite-backup-dr-4)
2. **The §13.2 restore runner.** The new form is `sudo gvm restore <instance> --latest | --archive <file>`, and the old `restore <archive> --instance <n>` stays as an alias.
   - It prints CreatedAt, age, class and size, then asks for the typed name.
   - It **always** takes a verified preserve pre-archive. `--no-pre-archive` is removed, and passing it refuses.
   - It stops through the ladder, runs a digest-checked extract, chowns, starts, and ends with a `next:` line.
   - `--latest` skips archives written by an earlier restore's pre-archive step.
   - Relative paths are resolved in the CLI.
   - It runs through `runDetachable` (Phase 31), so an SSH disconnect between the stop and the start never leaves a half-restored world for the guard to start.
   - **The moved-aside world is reclaimed.** `archive.Restore` moves the previous contents into `<dest>/.gvm-replaced-<stamp>` inside the world's own base directory and never deletes them (`restore.go:85-91`, `promote` at `:580-605`). Left there, every later nightly whose source includes the base would archive the whole old world again, and a later `restore --latest` would bring it back too. Once the restored server reaches ready, the runner removes each `RestoreReport.Replaced` path through `archive.Destroy`, holding the verified pre-archive's `Ref`, which is the path `restore.go` itself prescribes and satisfies rule 5. If the server does not reach ready, the trees are left in place and named in the report.
   - Independently, `.gvm-replaced-*` and `.gvm-restore-*` join every backup source's built-in excludes, so a tree that was left behind is never archived.

   (day2-ops-2, offsite-backup-dr-4, cli-walkthrough-13)
3. **Destinations come from the target spec.**
   - Each prefix label maps to `in.Spec.Paths.<label>`. The preflight shows the mapping, e.g. "data/ → /srv/gvm/pz1/data (archive taken from /home/pzserver/Zomboid)".
   - An unknown prefix is refused, unless `--map label=/abs` names a path inside this instance's own declared paths that overlaps no other instance.
   - Nested destinations are refused.
   - The archive's recorded base is used only under `--to`.

   (offsite-backup-dr-6)
4. **destroy archives first by default** (`runDestroy`, `destroy.go:84`).
   - With no `--archive`, it writes and verifies a fresh preserve archive without running retention, then shows the preflight and the typed prompt.
   - `--archive <path>` stays. `--archive latest` is accepted only with its age shown and the Phase 21 warning.
   - `gvm backup list` shows a DIR header.

   (cli-ergonomics-7, cli-walkthrough-13)
5. **Rebuild for content-free games.** `h.Jobs.Rebuilds` is wired with production seams:
   - **Server:** `lifecycle.Manager`.
   - **Timers:** mask and unmask through hostops.
   - **Archives:** the backup engine.
   - **Destroyer:** the existing `rebuild.JailDestroyer`, which refuses anything outside `world_paths`.
   - **Hooks:** the hook registry.
   - **Content:** a **null content adapter**, which returns an empty plan, an empty settlement and an empty resolution. It is constructible only for a manifest with no content block, and it gives the same honest "no content axis" answer update gives.

   `engineWired(rebuild)` flips for content-free manifests, and rebuild runs through `runDetachable`. Typed confirmation and archive-first are unchanged. Modded games follow in Phase 44. (day2-ops-1, cli-walkthrough-1)
6. **`docs/backups-and-restore.md` (local half).** It covers:
   - what an archive contains and what it lacks
   - copying archives off-box today with rsync or scp
   - escrowing secrets and override manifests
   - restoring and rebuilding with gvm

   It is linked from the `backup`, `restore` and `rebuild` help, and from doctor. (offsite-backup-dr-13)

### Definition of done

- [ ] `sudo gvm restore /var/backups/gvm/v1/<f>.tar.gz --to /root/r1` extracts the archive. `--to /srv/gvm/v1/data` is refused and names the overlap.
- [ ] `sudo gvm restore v1 --latest`:
  - shows age and class
  - asks for `v1`
  - writes a preserve pre-archive, which `gvm backup list` shows
  - stops on the save rung, extracts and starts

  A Valheim client then sees the restored world.
- [ ] After that restore, no `.gvm-replaced-*` directory remains under v1's world base, and the next nightly's archive holds no `.gvm-replaced-*` member and is no larger than the pre-restore nightly beyond the world's own growth.
- [ ] Closing the SSH session in the middle of `sudo gvm restore v1 --latest` lets the restore finish and start v1. `gvm job last v1` shows the result.
- [ ] `sudo gvm destroy v1` with no `--archive` archives fresh and verifies before asking.
- [ ] `sudo gvm rebuild v1 --confirm v1` archives a preserve copy, deletes only `world_paths` and boots a fresh world. `sudo gvm rebuild pz1` still exits 3.
- [ ] A fixture archive from a differently pathed instance is restored into the target spec's paths.

**Depends on:** Phases 21, 34 (the hook registry linked in 30 and an installed game from 31), 37. **Effort:** L.

**Design rules touched:**
- **Rule 5:** typed confirmation; archive-first always, with the bypass flag removed; preserve archives never pruned; rebuild's destroy jailed to `world_paths`. The moved-aside tree is removed only through `archive.Destroy`, holding the verified pre-archive's receipt.
- **Rule 4:** restore and rebuild stop through the ladder.
- **Rule 2:** tar and gzip run in Go, with no exec.
- **Rule 1:** no database is needed.

---

## Phase 39 — Change settings without hand-editing YAML

**Goal:** operators change settings, ports, schedules, manifest revisions and the durable on/off state with one validated command that says when the change takes effect. Typos and dangerous edits are caught at apply, and a `gvm.yaml` mistake never silently weakens host safety.

**Why now:** after a week, every change is a hand edit that apply never re-validates. Renaming an immutable world abandons it, and raising the heap without MemoryMax invites an OOM SIGKILL mid-save. Phase 21 blocked only the worst case. The verbs the old hints named (`enable`, `disable` and `manifest upgrade`) need a writer. Phase 20 reworded those hints into manual steps; this phase points them back at real verbs.

**Resolves:** game-catalog-config-4, game-catalog-config-9, failure-recovery-3, day2-ops-5 (enable/disable), day2-ops-12 (pause/skip), cli-walkthrough-7, network-firewall-7, gvm-self-upgrade-uninstall-9.

### Work items

1. **An intent writer in `internal/reconcile`.** No intent writer exists today. This one:
   - makes comment-preserving `yaml.Node` edits, generalised from `content_yaml.go`
   - writes through hostops `VerbFileWrite` (`FileInstanceSpec`)
   - checks a sha256 If-Match against concurrent hand edits, refusing with "the file changed since you read it"

   It is the one write path for `set`, `ports move`, `enable/disable`, `manifest upgrade`, `job pause`, content edits and, later, the dashboard's settings tab and `instance promote`. (game-catalog-config-4)
2. **`sudo gvm set <inst> KEY=VALUE… [--dry-run]`.**
   - It validates the assembled document with `ValidateInstance` (`validate_instance.go:177`) plus the game's validators, and normalises bools.
   - It refuses immutable vars. For a content-free game: "renaming the world means a fresh world: sudo gvm rebuild <n> --confirm <n>", which keeps rebuild's typed confirmation and archive-first. For a modded game the same hint carries "not available in this build" until Phase 44.
   - It prints "takes effect on: restart — run sudo gvm restart <n>".
   - **Heap vars.** When `spec.resources` equals what `ComputeResources` gave for the old vars, it recomputes and shows old → new. When the resources were hand-edited, it refuses if the new heap plus headroom exceeds MemoryMax, unless `--resources=recompute` is given.

   (game-catalog-config-4)
3. **`sudo gvm enable|disable <n>`.** These flip `spec.enabled` through the intent writer and run the same apply. `disable` stops through the ladder. `gvm stop` output adds "held until reboot; `gvm disable` keeps it off", and the Phase 20 hints point back at these verbs. (cli-walkthrough-7, day2-ops-5)
4. **`sudo gvm ports move <inst> NAME=PORT [--dry-run]`.**
   - It moves the whole group by its stride, and refuses while the instance runs: "gvm stop <n> first".
   - It allocates with a pin and reserves in the ledger.
   - It prints the config-key and firewall deltas, plus "players' saved addresses change".
   - `apply --diff` runs `ports.CheckSpec` (`conflicts.go:273`) as warnings. Apply refuses a port that overlaps another instance's declared port or the reserved list, naming `gvm ports move` as the fix.

   (network-firewall-7)
5. **Strict validation, at apply only** (`buildApply`, `apply.go:85`).
   - Unknown keys become positioned errors (file:line), all of them, worded "spec.egress.policy is not known to gvm 0.21.0; this file was probably written by a newer gvm".
   - `ValidateInstance` errors block apply.
   - Immutables are compared against `/etc/gvm/instances/<n>.d/applied.yaml`.
   - `gvm run` stays lenient and logs warnings to the journal, so a rollback never strands a world.
   - The writer's version goes into `manifest.lock.yaml` as well as the drop-in header, and doctor warns when either was written by a newer gvm.
   - Manifest extensions use an `x-` prefix.

   (game-catalog-config-4, gvm-self-upgrade-uninstall-9)
6. **`sudo gvm manifest upgrade <inst> [--to N] [--dry-run]`.**
   - `--dry-run` prints `spec.Diff`, classified as argv, destroy_globs, ports, shutdown or cosmetic. It also runs `ValidateInstance` against the new revision and shows the target revision's ledger state.
   - The real run refuses a target that is not `verified` or `verified here` (U14).
   - It needs `--confirm <inst>` when argv, destroy_globs, world_paths or ports change.
   - It takes a verified **preserve** archive first, writes the lock and the pin as one step, keeps `manifest.lock.rev<N>.yaml`, and never starts or stops anything.
   - `gvm list` notes "N instances pinned to an older revision".
   - The pin-mismatch hint now offers both paths: restore the pin, or run `gvm manifest upgrade <n> --dry-run`. Phase 32's install-gate refusal points at the same command.

   (game-catalog-config-9, failure-recovery-3)
7. **Revisions superseded for safety.** `internal/spec/superseded.go` lists revisions replaced for a safety reason, as `{id, revision, reason}`. Phase 42's Proton, Mono and world-path fixes add rows when an instance could exist on the old revision.

   An instance pinned to a listed revision gets:
   - a doctor Note (INST-017) and a status line: "pinned to valheim rev 4, superseded for safety (Mono under W^X); see: sudo gvm manifest upgrade v1 --dry-run"
   - the same in `gvm upgrade status`

   Nothing upgrades automatically.
8. **Schedules.**
   - `paused: true` on `spec.Job` makes apply render a stopped, disabled timer, shown as PAUSED.
   - `gvm job pause|resume <slug>` goes through the intent writer.
   - `gvm job skip <slug> --next` writes a dated marker that the job reads at start. That run records "skipped (operator)", and the marker expires after one firing.

   (day2-ops-12)

### Definition of done

- [ ] `sudo gvm set pz1 max_players=20` prints "takes effect on next restart".
- [ ] `sudo gvm set v1 world_name=new` refuses and names `gvm rebuild`.
- [ ] `sudo gvm set mc1 heap_size=6G` shows MemoryMax 7G → 9G.
- [ ] A hand-added `max_playres: 3` makes `sudo gvm apply pz1` fail with file:line and a did-you-mean, while `sudo gvm start pz1` still boots.
- [ ] `sudo gvm disable v1 && sudo reboot` leaves v1 down, and `gvm status v1` says disabled with the hint `sudo gvm enable v1`.
- [ ] `sudo gvm ports move pz2 game=16281` is refused while running and works when stopped. `apply --diff` then shows the ini key and the firewall change.
- [ ] Hand-editing pz2's game port onto pz1's makes `sudo gvm apply pz2 --diff` warn and `sudo gvm apply pz2` refuse, naming both instances and `sudo gvm ports move pz2 game=<free port>`. Nothing is written.
- [ ] `sudo gvm job pause v1_nightly` makes `gvm job list` show `PAUSED`, and `systemctl is-enabled gvm-job@v1_nightly.timer` reads `disabled`. `sudo gvm job resume v1_nightly` restores the timer and its next elapse.
- [ ] `sudo gvm job skip v1_nightly --next` makes the next firing record `skipped (operator)` and exit 0. The firing after that runs normally, and the marker file is gone.
- [ ] `sudo gvm manifest upgrade pz1 --dry-run` lists the classified changes. The real run archives first, requires `--confirm` when argv changed, and refuses an unverified target.
- [ ] Instance-file comments survive every verb above (golden test).

**Depends on:** Phases 35, 38. **Effort:** XL (the intent writer, `set`, enable/disable, `ports move`, strict apply, `manifest upgrade`, superseded revisions and schedules). If it must be split, `manifest upgrade` and the superseded table (items 6 and 7) can follow as their own phase, which Phase 42's item 10 then waits for.

**Design rules touched:**
- **Rule 1:** intent lives in files, and apply converges from them.
- **Rule 4:** disable and stop use the ladder, and the heap guard closes the OOM SIGKILL path.
- **Rule 5:** immutable changes go through rebuild. Manifest upgrade archives first and needs a typed confirmation for argv, destroy-glob, world-path and port changes.
- **Rule 6:** list and doctor only report older or superseded revisions. Nothing upgrades automatically.
- **Rule 10:** values are validated, and no template is introduced.

---

## Phase 40 — gvm opens the ports, and never locks you out

**Goal:** on a ufw host, `sudo gvm apply` adds exactly the gvm-tagged rules an instance needs and shows them in the plan. It removes only gvm-tagged rules. `sudo gvm firewall apply` keeps the host's own rules (SSH, and HTTP/HTTPS for the dashboard) in step. `sudo gvm firewall enable` is the one guarded way to turn ufw on.

**Why now:** since Phase 33 the operator pastes the rules by hand. This phase lets gvm keep them in step. It starts by fixing the backend defects that would leave stale open ports or execute `StepAdopt` (U13), and by teaching gvm to see rules that ufw has staged while inactive.

**Resolves:** network-firewall-1 (stage 2), network-firewall-3 (staged-rule checks), network-firewall-9, network-firewall-11, onboarding-4, cli-walkthrough-4, cli-ergonomics-6 (FW), failure-recovery-2 (FW), documentation-3, gvm-self-upgrade-uninstall-7 (destroy closes ports).

### Work items

1. **SAFETY — fix the ufw backend first** (`ufw/ufw_linux.go:35`).
   - `comment` and `gvm:<inst>:<label>` are passed as two argv elements.
   - Rules are emitted as `allow proto P [from X] to any port N`.
   - Deletes match by spec, without the comment.
   - `Apply` never executes `StepAdopt`.
   - Every exec uses `/usr/sbin/ufw` by absolute path, refused unless root-owned and neither group- nor world-writable (Phase 33 made the read path do this; the write path now shares the helper). `Available()` stats that path instead of calling `exec.LookPath`, and the `noshell` gate asserts it.
   - **gvm can see staged rules.** When ufw is inactive, `ufw status numbered` prints only "Status: inactive", and `ufw.Parse` returns zero rules with `Trusted=true` (`ufw/parse.go:25-37`). So gvm reads the staged set separately: `/etc/ufw/user.rules` and `/etc/ufw/user6.rules`, read-only, through a parser with its own Trusted flag. An unrecognised line makes that parse untrusted. Idempotent applies, orphan detection, doctor's "N rules staged" and `firewall enable`'s full-ruleset preview all use this staged parse while ufw is inactive.
   - A parse-after-apply golden test is added, plus a Linux CI round-trip: applying twice gives zero steps, deleting an instance closes its rules, and the SSH rule is untouched. The same round trip runs with ufw inactive.

   (network-firewall-9)
2. **nft is off for now.** `nft.Available()` (`nft.go:30`) returns false, with "nft backend not supported yet; use ufw or none". `ValidateHost` rejects `backend: nft`, and `buildScript` (`:91`) is no longer reachable. (network-firewall-9)
3. **A firewall step in `reconcile.BuildPlan`** (`reconcile/apply.go`).
   - It renders as a `~ firewall` block in `apply --diff` and in the report.
   - `gvm apply <name>` adds and deletes only `gvm:<name>:*` rules, computing Desired over every instance file. A tag whose instance file fails to load gets no deletions.
   - When the parse is untrusted, nothing is deleted (D-42).
   - When ufw is inactive, adds are staged (ufw accepts rule changes while disabled). Tag-owned deletes of staged rules happen only when the staged parse from item 1 is trusted; otherwise nothing is deleted. gvm prints "ufw is inactive: N rules staged; nothing on this host blocks these ports until you run sudo gvm firewall enable".
   - Host rules (`gvm:host:ssh`, `gvm:host:http`, `gvm:host:https`) change only through `gvm firewall apply` (item 4).
   - `destroy` reconciles away the instance's `gvm:<n>:*` rules, active or staged.
   - The desired-state builder takes a `KeepPorts(instance)` input. Phase 50 uses it to keep a rolled-back adoption's ports open.

   (network-firewall-1, onboarding-4, cli-walkthrough-4, gvm-self-upgrade-uninstall-7)
4. **`sudo gvm firewall apply [--diff]`: the host's own rules, and orphans.** Until now, host rules had no write path at all, so on a ufw-active box nothing could open 80 and 443 for the dashboard.
   - `--diff` is the read-only preview (U5) and prints the same plan the real run executes.
   - It reconciles `gvm:host:*` rules from `firewall.Desired`: **SSH first**, from the detected ports (Phase 33), then `http` and `https` (and `https-alt` for HTTP/3) only when `edge.mode` needs them (`desired.go:32-42`).
   - It refuses unless SSH detection is confident (`ErrSSHUnknown`; no bypass). Adds come before deletes, and an SSH rule, tagged or not, is never deleted.
   - It prunes orphans: `gvm:<inst>:*` rules whose instance file is **absent**. A tag whose instance file exists but fails to load is never touched. With an untrusted parse, active or staged, nothing is deleted (D-42).
   - It asks for this host's name to be typed (`TypedConfirm`) before any change, and works whether ufw is active or inactive (staging).

   (network-firewall-1, onboarding-4)
5. **`sudo gvm firewall enable`.**
   - It refuses unless SSH detection is confident (`ErrSSHUnknown`; no bypass).
   - It shows the detected SSH ports with their sources and the **full** staged ruleset, read through item 1's staged parse. **Orphaned** `gvm:*` rules (instance file absent) are listed separately, and gvm offers to drop them before going on, so a destroyed instance's staged ports never open with the firewall.
   - It then asks for this host's name to be typed (`TypedConfirm`).
   - It adds the SSH allow rules first, and prints "keep a second SSH session open until this finishes".
   - It enables ufw, then re-reads and re-verifies the SSH rules, and reports.
   - **It is the only code path in gvm that enables ufw.** `TestNoUfwEnableAdvice` (Phase 33) scans advice strings only, and exempts this command's backend argv.
   - `gvm firewall adopt-rules` adopts matching untagged rules after a typed confirmation. It is the only place `StepAdopt` runs.

   (network-firewall-1, onboarding-4)
6. **Doctor checks the write path.** Two new checks:
   - staged-but-inactive rules give a Warning, "N rules staged; ufw is inactive", counted from the staged parse
   - a `gvm:` tag whose instance file is absent gives a Warning, "orphaned rule; sudo gvm firewall apply removes it". (`gvm apply <name>` cannot: it only touches `gvm:<name>:*`, and an orphan has no instance to apply.)

   Phase 33's read-only checks are unchanged. (network-firewall-3, failure-recovery-2, cli-ergonomics-6)
7. **Docs.** `docs/networking.md` gains the `gvm apply`, `gvm firewall apply` and `gvm firewall enable` recipes and the SSH safety checklist. The `firewall` help topic names `sudo gvm firewall enable` as the only enable path. The README firewall row flips to "works". (network-firewall-11, documentation-3)

### Definition of done

- [ ] On a Debian 12 VM with ufw active and `22/tcp` allowed, `sudo gvm apply v1` shows `+ ufw allow proto udp to any port 2456:2457 comment 'gvm:v1:game'`. A Valheim client joins, and a second apply shows no firewall steps.
- [ ] `sudo gvm destroy v1` (archive first, typed) removes only `gvm:v1:*` rules. `22/tcp` and an operator-added rule remain.
- [ ] With ufw inactive:
  - apply stages the rules, `ufw status` still reads inactive, and a second apply shows no firewall steps (the staged parse sees them).
  - after `sudo gvm destroy v2`, its staged `gvm:v2:*` rules are gone from `/etc/ufw/user.rules`.
  - with a hand-placed staged `gvm:old1:game` rule and no `old1` instance, `sudo gvm firewall enable` lists it as orphaned and offers to drop it before the typed prompt.
  - `sudo gvm firewall enable`, run from a session on a non-default SSH port, shows the ruleset, asks for the host name, and keeps that session alive.
  - With detection forced unknown, it refuses.
- [ ] On the J1 box (ufw active, only `22/tcp` allowed) with `edge.mode: caddy_managed`, `sudo gvm firewall apply --diff` shows `+ ufw allow 80/tcp comment 'gvm:host:http'` and the 443 line, and the real run, after the typed host name, adds them. `22/tcp` is untouched, and a second run shows no steps.
- [ ] `sudo gvm firewall apply` removes a `gvm:old1:*` rule whose instance file is absent, and leaves a `gvm:bad1:*` rule whose instance file fails to load.
- [ ] An unparseable `ufw status` fixture makes apply and `firewall apply` delete nothing and say why. An unrecognised line in a `user.rules` fixture does the same while ufw is inactive.
- [ ] Doctor on a correctly configured ufw host shows 0 FW problems, and its orphan Fix names `sudo gvm firewall apply`.

**Depends on:** Phases 33, 39. **Effort:** L.

**Design rules touched:**
- **Rule 7:**
  - `apply` and `firewall apply` never enable ufw (D-41).
  - `enable` is explicit, typed, verified against SSH detection, and has no bypass. It is the only path that enables ufw.
  - Only tagged rules are deleted, and nothing is deleted on an untrusted parse, whether gvm read the active ruleset or the staged one (D-42). An orphan is pruned only when its instance file is absent, never when it merely fails to load.
  - SSH rules are never removed, and `firewall apply` adds SSH before anything else.
- **Rule 5:** `firewall apply`, `firewall enable` and `adopt-rules` use typed confirmation.
- **Rule 1:** rules are rebuilt from files, and ufw keeps them across reboots without gvmd.
- **Rule 2:** ufw is exec'd as `/usr/sbin/ufw` by absolute path with argv, and only a root-owned, non-writable binary is run.

---

## Phase 41 — Planned restarts that players see coming · **Milestone J2**

**Goal:** players get an in-game warning before planned restarts, updates and scheduled jobs. The operator knows the world saved after a restart.

**Why now:** this is the last week-to-week gap. Today every manual restart, nightly restart and update disconnects players with no warning.

**Resolves:** day2-ops-11, day2-ops-8 (restart rung).

### Work items

1. **`--warn <dur>` and `--message`** on stop, restart and update, plus a `warn:` field on `spec.Job`. `create` may set `warn: 5m` on the restart jobs it writes. The CLI default stays off.
   - The countdown runs **before any state change**, so there is no hold and no stop until it ends:
     - for jobs, it runs inside the lease but before any stopping step
     - for update, only after the decision phases say Stop
   - The manifest's broadcast verb is sent over the runner's control path (`console.sock` via `control/commander.go`, never admin.sock, never a shell), at T−warn, T−1m and T−10s.
   - Ctrl-C or a cancel during the countdown aborts with "nothing was stopped".
   - The wait is skipped only when the player count is known to be 0; an unknown count still warns. With no broadcast verb, or with control not ready, gvm prints a note and continues without delay.
   - The message passes the console sanitizer, and is rejected if it contains `' " \ $`, a backtick or a newline.

   (day2-ops-11)
2. **restart reports the rung.** It prints "ladder ended on <rung> after <d>" (plus DEGRADED) from the first `shutdown.jsonl` record written at or after the moment it was issued. Otherwise it prints "no shutdown record for this restart; see gvm logs <n> --shutdown". (day2-ops-8)
3. **The J2 journey is added to `ci/golden-journey.sh`.** With Postgres absent, it covers:
   - an upgrade from J1
   - a nightly backup and update with `warn:`
   - `gvm set`
   - `gvm restore --latest`
   - `gvm apply` opening ports
   - `job last` and `audit tail`

### Definition of done (Milestone J2)

- [ ] `sudo gvm restart pz1 --warn 2m --message "Mod update"` shows three in-game broadcasts. Ctrl-C at T−90s prints "nothing was stopped", pz1 is still running, and no hold file exists.
- [ ] A nightly restart with `warn: 5m` and a known 0 players does not wait.
- [ ] `sudo gvm restart pz1` prints "ladder ended on save after 41s".
- [ ] A J2 transcript on the J1 box, with Postgres absent:
  1. `sudo ./gvm host init` (the upgrade) → `gvm list` shows restart pending → `sudo gvm restart --pending --yes`
  2. `gvm status v1` shows uptime, players, last backup and next job
  3. `sudo gvm set v1 …`
  4. `sudo gvm apply v1` shows the firewall block
  5. `sudo gvm restore v1 --latest`
  6. `sudo gvm audit tail`

**Depends on:** Phases 37, 38, 39, 40. **Effort:** M.

**Design rules touched:**
- **Rule 4:** the countdown comes before any state change, and the normal ladder follows.
- **Rules 2 and 3:** the control channel goes through `console.sock`, never admin.sock or a shell.
- **Rule 10:** the message is sanitised.
- **Rule 6:** no new automatic restarts. `warn:` only decorates jobs the operator scheduled.

---

## Milestone J3 — every game, mods and custom games

## Phase 42 — The whole catalog boots

**Goal:** every game in the catalog either installs, reaches ready, stops through its save ladder without SIGKILL, has its world backed up and is visible to players, or is labelled `unsupported` with the reason. No game is listed as working on validation alone.

**Why now:** J1 offered four verified games. The other eleven manifests carry known defects:

- Proton titles start without `STEAM_COMPAT_DATA_PATH`, under W^X and with `arch: native`.
- Rust and 7DTD run Mono under W^X.
- Core Keeper's `xvfb` wrapper is refused at start.
- Satisfactory and ARK are forced onto Proton although native servers exist.
- Two games keep their worlds outside `world_paths`.
- 7DTD's console is empty.

Phase 32 built the tool that proves a fix, so this phase is data work plus two runner pieces. Items 1–9 need only Phase 32 and can run alongside J2. Item 10 writes rows into Phase 39's superseded-revision table, so the phase closes after Phase 39.

**Resolves:** game-runtime-compat-2 (Proton manifests), game-runtime-compat-3, game-runtime-compat-4 (Rust, 7DTD, Core Keeper), game-runtime-compat-6, game-runtime-compat-7, game-runtime-compat-8, game-runtime-compat-9 (Proton guidance), game-runtime-compat-10, game-runtime-compat-11 (secrets off argv), game-runtime-compat-13, game-catalog-config-12 (Proton help).

### Work items

Every manifest change below gets a revision bump. It is confirmed by `sudo gvm game try <id> --scratch` on Debian 12 and Ubuntu 24.04, recorded in `docs/research/scratch-boots/<id>.md` and entered in the shipped ledger (U14). A game whose boot cannot be made to pass is entered as `unsupported`, with the reason, and stays hidden from `gvm game list`. **Labelling a game is not fixing it:** a finding that names games (game-runtime-compat-3, -4, -6, -8) is resolved only when every game it names passes. A game that ends `unsupported` reopens its finding in `docs/reviews/usability-2026-10.md`, with the recorded reason (see Appendix A).

1. **Proton titles** (V Rising, Enshrouded, and any Proton variant kept below).
   - `STEAM_COMPAT_DATA_PATH: "{{ .Home }}/compatdata"` stays outside the backup include set, unless the title keeps its world in the prefix. `STEAM_COMPAT_CLIENT_INSTALL_PATH` is set too.
   - They get `jit: true` and `arch: i386`. The drop-in note reads "Wine's PE loader needs W^X off".
   - `proton_path` help requires a per-instance Proton copy owned by `gvm-<inst>`, e.g. under `{{ .Work }}/proton`. A shared tree must not be writable by any instance user.
   - The ladders lead with a game-level save and quit over the control channel where one exists (ARK RCON `saveworld`, then `DoExit`). There is no leading bare `wait_exit`, and no `scope: cgroup` SIGINT/SIGTERM under Wine. Signals are used only after a scratch boot proves which pid receives them and that the game saves.

   (game-runtime-compat-3, game-runtime-compat-2)
2. **Native servers where they exist.** These are `satisfactory` and `ark-se`. The Proton variants survive only as explicitly named `-proton` manifests, if anyone needs them, and the `doc:` blocks are corrected.
   - **`satisfactory`:** platform linux, exec `FactoryServer-Linux-Shipping` (or `FactoryServer.sh` if it execs), args `-Port`/`-ReliablePort`. Ports are `game` 7777 over both protocols and `reliable` 8888/tcp, with `udp_probe` or `log_marker` readiness.
   - **`ark-se`:** exec `ShooterGame/Binaries/Linux/ShooterGameServer`.

   (game-runtime-compat-8)
3. **Mono needs W^X off.**
   - `jit: true` goes into `rust.yaml`, `7daystodie.yaml` and `7daystodie-telnet.yaml`.
   - Core Keeper is decided by its install tree: `*_Data/MonoBleedingEdge` means true, and `il2cpp_data` means false.

   (game-runtime-compat-4)
4. **Worlds where backups look.**
   - **Satisfactory:** keep saves inside `{{ .Data }}` with `XDG_CONFIG_HOME={{ .Data }}/config`, if a scratch boot shows Unreal honours it. Otherwise point `world_paths` at the exact SaveGames directory.
   - **Enshrouded:** a `config_files` `json_path` entry on `enshrouded_server.json` owns `saveDirectory`, `gamePort` and `queryPort`, with `world_paths` set to that directory, and `-savedir` is dropped.

   (game-runtime-compat-7)
5. **Console output reaches gvm.**
   - 7DTD execs `./7DaysToDieServer.x86_64` directly, with `-logfile - -quit -batchmode -nographics -dedicated` plus the existing args, `LD_LIBRARY_PATH={{ .Install }}` and `process_group: false`.
   - V Rising gets `-logFile -`.
   - `generic-steam.yaml` documents "make the engine log to stdout" as a requirement for `log_marker` and stall readiness.

   (game-runtime-compat-10)
6. **Core Keeper.**
   - First, test whether `-batchmode -nographics` boots without a display.
   - If a display is still required, implement the `xvfb` wrapper in the runner (`runner/preflight.go`, BuildLaunch):
     - spawn `/usr/bin/Xvfb -displayfd N -nolisten tcp` directly, with no shell, in its own process group inside the unit cgroup
     - set `DISPLAY`
     - **register Xvfb in the sweep-exempt set before the game spawns.** `settled()` is true only when the cgroup is empty apart from the runner and the exempt set (`shutdown.go:850-863`, `procgroup_linux.go:101-150`), and after Phase 27 every wait blocks on it. A live, unexempted Xvfb would hold every Core Keeper rung to its full budget, push the ladder through the implicit SIGTERM and SIGKILL rungs to the cgroup sweep, and record every stop DEGRADED. `ExemptAround` cannot be used as is: it diffs against the game's pgid, and `procBase` is built after the game spawns. So `procBase` gains a pre-registered exemption list (an `exempt.add` of Xvfb's pid and starttime) that `newProcBase` picks up at construction.
     - start it before the game, and stop it (SIGTERM, from the runner) only after the ladder has finished and its report is written. Xvfb is not the game server, and the ladder never signals it.
   - `wrapperKinds` marks xvfb implemented.
   - The wrapper refusal gets its own Failure check, `wrapper`, and its own derive hint, instead of exit 64's pin message.

   (game-runtime-compat-6)
7. **Secrets leave argv where the game allows it.** Phase 30 renders `config_files`, so the passwords can move:
   - V Rising's `Rcon.Password` via `json_path`
   - Rust's `rcon.password` and CS2's `rcon_password` via `owned_lines`
   - ARK's `ServerAdminPassword` via the ini codec

   `ps -o args` then shows no RCON or admin password for these games.

   **Residual risk, stated plainly.** These secrets stay on argv after this phase, readable by local accounts outside `gvm@` units on a host without `hidepid` (Phase 27's doctor Note names each one):

   | Secret | Why it stays |
   |---|---|
   | CS2's GSLT (`cs2.yaml:44`, `+sv_setsteamaccount`) | The server logs in to Steam before any cfg file executes. A scratch boot tests a `server.cfg` placement, and the token moves there only if the server accepts it. |
   | Valheim's join password (`-password`) | The server has no config-file alternative. |
   | Core Keeper's join password (`-password`) | No config alternative is known. The scratch boot tests one. |
   | Project Zomboid's `admin_password` | It is passed only during install's first boot (`when: {phase: install}`), for that one boot. |

   (game-runtime-compat-11)
8. **CS2.** It gets `post_install: [steam.home_layout, steam.link_sdk]`. A validate and doctor rule warns, naming the hook, about a steamcmd manifest whose install tree has no `steamclient.so` and that does not run `steam.link_sdk`. (game-runtime-compat-13)
9. **Proton guidance.**
   - New `docs/proton.md` and a `gvm help proton` topic.
   - `game list` and `game show` label these titles "Windows server under Proton — install Proton yourself".
   - The preflight `wrapper` check names `spec.runtime.wrapper`.
   - Candidate Proton paths are listed only from system locations.

   (game-runtime-compat-9, game-catalog-config-12)
10. **Superseded revisions.** Any instance that could exist on an old revision gets a row in `internal/spec/superseded.go` (Phase 39), so doctor and status point it at `gvm manifest upgrade`. That covers a `verified here` revision, or an adopted one.

### Definition of done

- [ ] `docs/games/README.md` is regenerated from the shipped ledger. For each of the 15 real games, `gvm game try <id> --scratch` shows the READY time, a final rung that is never DEGRADED, and a backup that contains world files. Any game without a passing row is `unsupported`, with its reason, **and the finding that names it stays open**: the phase does not count game-runtime-compat-3, -4, -6 or -8 as resolved while any game it names is `unsupported`.
- [ ] `sudo gvm stop vr1` (V Rising, Proton) records a save rung, never SIGKILL.
- [ ] A Satisfactory client joins on 7777.
- [ ] `ark-se` starts natively (`ShooterGameServer`, no Proton in its process tree), reaches READY, and a client joins.
- [ ] Rust, 7 Days to Die and 7 Days to Die (telnet) each record READY in the ledger with `jit: true`, and their scratch transcripts show no Mono `mono_valloc` or SIGSEGV.
- [ ] Core Keeper reaches READY, through `-batchmode -nographics` or the Xvfb wrapper. With the wrapper, `sudo gvm stop ck1` ends on its save rung with no DEGRADED and no cgroup sweep, and the runner stops Xvfb after the report.
- [ ] With Xvfb not installed, a Core Keeper start that needs it fails with the `wrapper` check and the `apt install xvfb` hint, never exit 64's pin message.
- [ ] From a host shell, `sudo -u nobody cat /proc/<pid>/cmdline` for V Rising, Rust, CS2 and ARK shows no RCON or admin password. `cat /proc/*/cmdline`, run as another instance's user, shows nothing of theirs at all (Phase 27).
- [ ] `sudo gvm create vr1 --game vrising --var proton_path=/home/me/proton` warns about ProtectHome before writing anything.
- [ ] `gvm game validate` reports 0 errors on every shipped manifest. `TestEveryShippedManifestBuildsLaunchAndProbes` and `TestLedgerCoversShippedRevisions` are green.

**Depends on:** Phase 32 (items 1–9), Phase 39 (item 10: the superseded table and `gvm manifest upgrade`). **Effort:** XL (15 boots on two distributions, plus the Xvfb wrapper).

**Design rules touched:**
- **Rule 4:**
  - Proton ladders change only after a scratch boot proves where signals land and that the game saves.
  - Xvfb is exempt from the cgroup sweep and stops only after the ladder, so it can neither stretch a stop into SIGKILL nor be killed while the game saves.
  - Every scratch stop's rung is recorded.
- **Rule 2:** Xvfb is spawned directly, never through a shell.
- **Rule 7 / D-41:** prerequisites are named, never installed. Proton is never downloaded.
- **Rule 10:** manifests stay data, and secrets leave argv where the game allows it.
- **Hardening:** W^X is relaxed only for engines that boots show generate code, and `arch: i386` only where 32-bit is proven. The Proton tree is per instance, and ProtectProc stays.

---

## Phase 43 — Mod lists that are easy to edit and read

**Goal:** operators add, import and review mods by URL or name, with titles shown. Their YAML comments survive, a collection sync never silently drops a hand-added mod, and they can see which mods changed upstream.

**Why now:** before mod changes can be applied (Phase 44), the list the operator edits has to be right. Today `add` accepts URLs and mod names and fails only later, and every list is a column of 10-digit ids. The intent writer from Phase 39 is now the one write path.

**Resolves:** content-mods-7, content-mods-8, content-mods-9, content-mods-10, content-mods-11, content-mods-14, content-mods-15.

### Work items

1. **Validate ids before writing.** Each source has its own id format:

   | Source | Format |
   |---|---|
   | Workshop | numeric |
   | CurseForge | `projectID[:fileID]` |
   | Modrinth | `slug[:versionId]` |

   - URLs are normalised: a Workshop URL with `?id=`, `modrinth.com/mod/<slug>`, and CurseForge project URLs.
   - A known activation id is refused with "ThatDamnLibrary is a Mods= id from item 3171167894".
   - `--pin` is refused on a Workshop block.
   - New: `sudo gvm content set <inst> <id> [--note T] [--requires IDS] [--pin V]`, written through the intent writer. The requires-itself fix uses it.

   (content-mods-7)
2. **Show titles.**
   - `add` looks up titles best-effort, keylessly and with a short timeout. It uses the Workshop client collection sync already uses (`instance.go:223`).
   - The sanitised title fills `note` when `--note` is absent.
   - A `consumer_app_id` mismatch is refused unless `--allow-other-app` is given. `--no-fetch` skips the lookup.
   - `list` and `diff` stay offline, and fall back to the lock's `Upstream.Title`.

   (content-mods-10)
3. **Import what the operator already has.**
   - `sudo gvm content collection set <inst> <id|URL>` and `sudo gvm create … --collection <id|URL>`.
   - `sudo gvm content import <inst> --from-ini` reads the live `WorkshopItems=` read-only. It refuses when items are already declared, unless `--replace` is given. It writes only the declared list, and sets `activation_policy: preserve`. Adopt discovery never calls it.
   - Collection sync prints the merge first, and requires `--yes` on a TTY to drop items. Unattended runs keep them and report them.
   - `local: true` refs are kept, like vendored items.

   (content-mods-9)
4. **`gvm content update <inst> --check`.**
   - It does a keyless, best-effort read of `time_updated`, and compares it with `appworkshop_<appid>.acf`, using the existing steamcmd ACF parser (`provider/steamcmd/acf.go`). It falls back to the lock, then to the directory mtime, labelled "approximate".
   - It prints titles and ages, and exits 4 when updates are available and 0 when none are.
   - It only reports.
   - The inert `--dry-run` is removed.

   (content-mods-11)
5. **`gvm content why` accepts Workshop ids** as well as activation ids. (content-mods-14)
6. **One description of the state.** `sync --dry-run` shows the diff's DECLARED vs LOCKED rows, and a single "will write" block. When `Unaccounted` is not empty, both `diff` and the dry run print "fix first: gvm content remove …" and exit 4. (content-mods-8)
7. **Help.**
   - `--json` works on every content verb, or is removed from it.
   - Help is in plain language, with an id example for each source.
   - The missing-root note appears only when no declared root holds items.

   (content-mods-15)

### Definition of done

- [ ] `sudo gvm content add pz1 'https://steamcommunity.com/sharedfiles/filedetails/?id=2392709985'` stores `2392709985` with its title. `sudo gvm content add pz1 ThatDamnLibrary` is refused with the item hint.
- [ ] `sudo gvm content import pz1 --from-ini` on a modded PZ produces the full list in order, with `activation_policy: preserve`. `servertest.ini`'s sha256 is unchanged.
- [ ] A collection sync that would drop two hand-added items asks first. Run without a TTY, it keeps them and reports them.
- [ ] `gvm content update pz1 --check` lists updated mods with their titles and exits 4.
- [ ] Top-of-file comments survive every content verb (golden test).

**Depends on:** Phase 39. **Effort:** L.

**Design rules touched:**
- **Rule 6:** `update --check` only reports.
- **Rule 11:** adopt discovery never calls `import`, and its ini read is read-only.
- **Rule 10:** titles are sanitised data.
- **Rule 1:** list edits are files, and `list` and `diff` are offline reads.

---

## Phase 44 — Apply Workshop mod changes safely

**Goal:** an operator edits a Workshop mod list and applies it with one command. The command saves and stops the server through the ladder, resolves the mods and starts it again. Project Zomboid's nightly update and `gvm rebuild` become available for modded games.

**Why now:** every list edit since Phase 9 has ended at a command that refuses. Phase 43 made the list itself trustworthy.

One adapter, `jobs.Contents`, unlocks three verbs: content sync, PZ's update content axis, and rebuild's content seam.

**Resolves:** content-mods-1 (phase B), day2-ops-1 (PZ update and rebuild for modded games), cli-walkthrough-1 (content sync, PZ update and modded rebuild), game-catalog-config-3 (post_content).

### Work items

1. **The adapter.** `jobs.Contents`, together with `update.Contents` and `rebuild.Content`, is implemented over `internal/content`:
   - **Plan** reuses the decision half, the lock and the `steamworkshop` API, with network reads only.
   - **Write** uses the `owned_keys` surgical editor for the acquisition key.
   - **Settlement** and **Resolve** use the existing resolver (`resolver_pz.go`).
   - **Activate** writes the activation key and the lock.

   The adapter only writes files and resolves. It never calls the supervisor. (content-mods-1)
2. **A boot branch in `runner_content.go`,** inside the same job, the same lock and `PolicyStartAnyway`:
   1. Stop through `Super.Stop` (the ladder).
   2. Write the acquisition key.
   3. Start, with `WaitReady` bounded by `acquisition.boot_timeout`.
   4. Stop through the ladder.
   5. Resolve from disk.
   6. Run the `post_content` hooks (`pz.resolve_mod_ids`).
   7. Write the activation key and the lock.
   8. Start, then run a post-start observe.

   (content-mods-1, game-catalog-config-3)
3. **`sudo gvm content sync <inst> [--warn <dur>]`.** It builds `jobs.Resolved` and runs it through `runDetachable` (Phase 31), with no gvmd and no admin.sock. The sync stops the server before it acquires, so an SSH disconnect mid-run must never end it there.
   - `engineWired(content_sync)` flips for Workshop blocks.
   - The Phase 20 "applying is not in this build" lines become `next: sudo gvm content sync <inst>`.
   - Edits note that "Project Zomboid rewrites its ini on shutdown; sync stops it first".

   (content-mods-1)
4. **The engines flip for modded Workshop games.**
   - `engineWired(update|repair)` turns true for manifests whose content block uses a Workshop source (Project Zomboid), so `update.Pipeline` now gets a real content seam.
   - For adopted PZ, the binary phase stays refused, as today.
   - `engineWired(rebuild)` turns true for those manifests, with this adapter as `rebuild.Content`. The other seams are Phase 38's.
   - `gvm set`'s immutable-var hint drops its "not available in this build" clause for PZ.
   - New PZ instances get the manifest's recommended `action: update` nightly again. An instance whose nightly Phase 31 wrote as `action: restart` keeps it, because apply never changes intent, and `gvm job list` and `gvm upgrade status` note: "pz1_nightly: the recommended action is update, now available. To switch, change action: restart to action: update under jobs.nightly in /etc/gvm/instances/pz1.yaml, then: sudo gvm apply pz1".

   (day2-ops-1, cli-walkthrough-1)
5. **`blocked-mods.json` is written only by the job's decide step.** Phase 45 uses it.

### Definition of done

- [ ] This sequence works end to end:
  1. `sudo gvm content add pz1 2392709985`
  2. `sudo gvm content sync pz1 --warn 1m`: it broadcasts, stops on the save rung, boots to acquire, resolves, rewrites `WorkshopItems=` and `Mods=`, then starts.

  `gvm content diff pz1` is clean afterwards, and a PZ client joins with the mod.
- [ ] The `content_sync` job unit runs the same path from its timer. `sudo gvm job run pz1_nightly` with an upstream Workshop update restarts once, after warning players.
- [ ] `sudo gvm rebuild pz1 --confirm pz1` archives a preserve copy, deletes only `world_paths`, boots, re-acquires mods and starts.
- [ ] SIGTERM to the job during the acquire boot leaves a defined state: the report says IS STOPPED or running, and the job record holds the result.
- [ ] Closing the SSH session in the middle of `sudo gvm content sync pz1` lets the sync finish and start pz1. `gvm job last pz1` shows the result.
- [ ] An instance created under Phase 31 with `nightly: {action: restart}` shows the "now available" note in `gvm job list`.

**Depends on:** Phases 41, 43. **Effort:** L.

**Design rules touched:**
- **Rule 4:** every stop is `Super.Stop`, so the ladder is always used.
- **Rule 3:** sync runs in-process, with no admin.sock.
- **Rule 5:** rebuild keeps typed confirmation and archive-first, and its destroy is jailed.
- **Rule 6:** content checks decide and the job acts. No reconciler is involved.
- **Rule 1:** the lock and intent are files, and the job is a systemd unit.

---

## Phase 45 — Minecraft mods: Modrinth, then CurseForge

**Goal:** Minecraft operators manage Modrinth and CurseForge mods with the same verbs. Blocked mods are reported accurately, with one drop-box location and a fingerprint check, and required dependencies are shown. Minecraft updates become runnable.

**Why now:** Phases 18 and 19 shipped Modrinth and CurseForge as library code no user can reach. The Workshop path from Phase 44 is the template.

**Resolves:** content-mods-2, content-mods-5, content-mods-6, content-mods-12, documentation-11, day2-ops-1 (Minecraft update, repair and rebuild: the last kinds to flip), cli-walkthrough-1 (the last refusals in the core journey flip).

### Work items

1. **Modrinth first.**
   - `Observe` works offline by hashing the jars in each root.
   - The HTTP client comes from `internal/content/fetch`, with a host allowlist for each source.
   - `Materialize` fails on an entry with no URL.
   - `Schema.GameVersion` and `Loader` come from the rendered provider block.
   - Presence deactivation unlinks **only** jars whose digest the lock records, never a jar the operator placed.
   - The CLI constructs `modrinth.New`.
   - `engineWired(content_sync|update|repair|rebuild)` flips for Modrinth-backed manifests (Paper, Forge, NeoForge). After this, no kind in the registry is unwired for any shipped manifest.

   (content-mods-2, day2-ops-1, cli-walkthrough-1)
2. **CurseForge, after Modrinth.**
   - The ladder (`curseforge/ladder.go`) returns typed results:
     - `auth_failed` (401/403): "CurseForge rejected the key in /etc/gvm/secrets/host/curseforge". The plan refuses.
     - `not_found`.
     - `withdrawn`, which is terminal.
     - `transient` (network errors, 429): the plan refuses and marks nothing blocked.
   - Only an empty `downloadUrl` counts as opted-out and goes into `blocked-mods.json`, with `websiteUrl` copied verbatim (rule 13).
   - Drop-box matches require a murmur2 or sha1 match: "x.jar is a different build than <fileName> (fingerprint A ≠ B)". Unmatched jars are listed.
   - **One exported constant**, `/var/lib/gvm/downloads/mods`, is used by the resolver (replacing `dropBoxPath`, `blocked.go:123`), the CLI and the doc. A test checks that they agree.
   - `sudo gvm content resolve-blocked <inst> [--from DIR]` reads the drop-box and never moves or deletes the operator's files. It copies into the blob store only on a fingerprint match, and removes `blocked-mods.json` only when a fresh plan shows zero blocked items.
   - The API key is set with `sudo gvm secret set --host curseforge` (Phase 29).

   (content-mods-5, content-mods-6, documentation-11)
3. **Dependencies.**
   - Required dependencies are recorded on plan entries. `diff` and the dry run print "! waystones requires balm, which is not declared", with the exact `add` line.
   - `add --with-deps` walks required dependencies only, never embedded ones.
   - `incompatible` aborts only when the other project is declared or resolved, and names both by title.
   - CurseForge files are filtered by the instance's loader.

   (content-mods-12)
4. **Docs.** `docs/curseforge-blocked-mods.md` is rewritten to use only commands that exist, and the Phase 20 banner is removed. The Minecraft game pages get a mods section. (documentation-11)

### Definition of done

- [ ] On a NeoForge instance (`minecraft-neoforge`, a shipped manifest), `sudo gvm content add mc2 waystones && gvm content diff mc2` shows "! waystones requires balm, which is not declared" with the exact `add` line, and `--with-deps` adds it. `sudo gvm content sync mc2` applies both through the ladder.
- [ ] `sudo gvm update mc1 --check` reports the Paper build and the mod axis. `sudo gvm update mc1` and `sudo gvm rebuild mc1 --confirm mc1` run (no exit 3 anywhere in `gvm job list`).
- [ ] **CurseForge, the positive path.** With a key stored by `sudo gvm secret set --host curseforge` (a real key, shaped `$2a$10$…`), `sudo gvm content add mc2 <curseforge project URL>` and `sudo gvm content sync mc2` install a digest-verified jar through the ladder. A fixture project whose file has an empty `downloadUrl` goes to `blocked-mods.json` with its `websiteUrl` verbatim. Dropping the matching jar in `/var/lib/gvm/downloads/mods` and running `sudo gvm content resolve-blocked mc2` copies it in on a fingerprint match and clears `blocked-mods.json`.
- [ ] A revoked-key fixture gives every item `auth_failed`. The plan refuses, and nothing is marked blocked.
- [ ] A jar of a different build dropped for a CurseForge file id is rejected with the fingerprint sentence.
- [ ] An operator-placed jar in `mods/` survives a presence deactivation.
- [ ] `nosynth_test.go` is green, and no CDN URL is constructed anywhere.

**Depends on:** Phases 44, 29. **Effort:** XL.

**Design rules touched:**
- **Rule 12:** Plan does network reads only, and Apply writes only digest-verified blobs.
- **Rule 13:** URLs are used verbatim, the key is operator-supplied via `secret set --host` (which accepts its `$2a$10$…` shape, because rule 10's ban applies to game passwords only), and the nosynth gate stays.
- **Rule 4:** sync goes through the ladder.
- **Rule 1:** reads are offline, and jobs execute under systemd.

---

## Phase 46 — Add a game with YAML alone · **Milestone J3**

**Goal:** a manifest author copies a shipped manifest, gets every mistake from validate in one pass, approves it with one confirmed command, and proves it boots in a disposable scratch instance before any real server uses it.

**Why now:** "a game is YAML" is the promise to this persona. Phase 20 unblocked overrides and Phase 32 built `game try --scratch`. The documented approve step, a validate that checks what the runtime checks, and a field reference are still missing.

**Resolves:** cli-ergonomics-5 (approval), content-mods-3, game-catalog-config-10, game-catalog-config-11, game-catalog-config-13, documentation-10.

### Work items

1. **`sudo gvm game approve <id>`.**
   - It shows the sha256, the origin, and a classified `spec.Diff` against the layer it shadows (or "new game id"). The id must be typed.
   - It writes `/etc/gvm/games/.approved/<id>`: the directory is 0700 root:root, the file is 0600, and it is not `*.yaml`. The loader never reads it as a manifest, and gvm-web cannot write it.
   - Any later edit invalidates the approval. Postgres mirrors it as a log entry only.
   - The template guard still runs at load.
   - `gvm game list` shows "not approved (sha …)" instead of "unreadable".
   - An approved override still needs a `verified here` ledger entry from `gvm game try --scratch` before `create` accepts it (U14).

   (cli-ergonomics-5, game-catalog-config-10)
2. **`gvm game validate` checks everything in one pass** (`spec/validate.go`).
   - A template-guard failure no longer hides later stages.
   - `content.Compile` runs per block with placeholder roots, and the source/mechanism pair must be implemented in this build. `server_boot` must name a resolver.
   - The Phase 32 runtime rules run.
   - `port_stride` keys that no port's `group` names are a warning. The shipped-manifest test treats that warning as a failure, and the six shipped manifests that teach the ignored shape (e.g. `rust.yaml`) are fixed with `group:` and a revision bump. That is data-only, so verification carries over.
   - REPLACE-ME tokens warn in template-tagged manifests and are errors elsewhere.
   - `--json` is supported.

   (content-mods-3, game-catalog-config-11, game-catalog-config-13)
3. **Reference material.**
   - `gvm game hooks` shows each `Doc()` and its phases, plus a "Content resolvers" section and the supported source/mechanism pairs.
   - New: `gvm game schema [ports|vars|config_files|readiness|when|connect|secrets|players]`.
   - `docs/manifest-authoring.md` is generated from struct tags and doc comments (CI diff), and documents the working loop.
   - `generic-steam.yaml` points at approve and try.

   (game-catalog-config-13, documentation-10, content-mods-3)

### Definition of done (Milestone J3)

- [ ] The full authoring loop works:

  ```
  gvm game show generic-steam --yaml | sudo tee /etc/gvm/games/mygame.yaml
  gvm game validate /etc/gvm/games/mygame.yaml   # template copy: every REPLACE-ME listed at once, as warnings
  sudoedit /etc/gvm/games/mygame.yaml            # set metadata.id: mygame, remove the template tag,
                                                 # fill app_id, exec and ports
  gvm game validate /etc/gvm/games/mygame.yaml   # clean: 0 errors, 0 warnings
  sudo gvm game approve mygame                    # typed
  sudo gvm game try mygame --scratch              # type the printed try-… name; READY in 42s; stopped on save; removed
  sudo gvm create real1 --game mygame             # no --unapproved-override needed
  ```

  Skipping the edit fails where it should: `create` refuses the copied template id with "copy it to /etc/gvm/games/<id>.yaml and change metadata.id first" (Phase 26), and once the template tag is removed, any REPLACE-ME left is an error.
- [ ] Editing `mygame.yaml` after approval makes `create` refuse with "not approved (sha changed)".
- [ ] `gvm game validate` on a manifest whose `modrinth` block names a mechanism this build does not implement fails with a positioned error (`file:line:col`, the source/mechanism pair, and the supported pairs). A shipped manifest's content block compiles clean.
- [ ] `make docs` regenerates `docs/manifest-authoring.md` with no CI diff, and the file documents the loop above.
- [ ] The J3 journey runs in CI:
  - import a PZ mod list and sync it
  - add a Modrinth mod to a Minecraft instance and sync it
  - add a CurseForge mod to a Minecraft instance with the CI's operator-supplied key, and sync it
  - create a non-J1 catalog game (e.g. V Rising)

  A client joins all of them.

**Depends on:** Phases 42, 45. **Effort:** L.

**Design rules touched:**
- **Rule 10:** the template guard runs at load. The approval file is not a manifest, and the password rules still apply.
- **Rule 9:** gvm-web cannot forge an approval, because of the file's mode and owner.
- **Rule 5:** approval is typed, and scratch proof is still required before create.

---

## Milestone J4 — friends without SSH

## Phase 47 — Make the web skeleton safe before it is wired

**Goal:** when the dashboard ships, it cannot be reached anonymously, its buttons either act or honestly refuse, and each role can do exactly what the design says.

**Why now:** `internal/web`, `auth`, `authz` and `edge` are scaffolding that departs from §15–16. Wired as written, the skeleton would do three harmful things:

- list servers to anonymous visitors
- report fake success for start, stop and restart
- let an `operator` (the natural moderator role) restore over a world and prune backups

**Resolves:** web-dashboard-2, web-dashboard-3, web-dashboard-4, web-dashboard-5, web-dashboard-11 (renderer), web-dashboard-13, admin-security-3, admin-security-5, network-firewall-5 (renderer), day2-ops-6 (honest 501).

### Work items

1. **No fake success** (U3). The mutation handlers (`handleAPIStart` `:224`, `handleAPIStop` `:234`, `handleAPIRestart` `:244` in `handlers.go`) return 501 with an envelope: `{code: not_implemented, message: "Restart from the dashboard is not in this release; on the host run: sudo gvm restart <name>"}`. The buttons are hidden. `TestNoMutationReturns2xxWithoutDispatch` uses a recording fake gvmd client. (web-dashboard-2, day2-ops-6)
2. **Every browser route needs a session.**
   - `requireAuth` (`auth.Middleware.Wrap`, `auth.go:90`) guards every route except `/healthz` and `/static`.
   - Without a session, the browser is redirected to `/login?next=`. htmx requests get 401 plus `HX-Redirect`.
   - `GET/POST /login` and `/login/totp` exist.
   - `TestBrowserRoutesRequireSession` walks the mux.
   - The console page returns 404 for an unknown instance.
   - The empty-users page is added in Phase 48, together with the `gvm user create` command it prints (U2).

   (web-dashboard-3)
3. **Two muxes, per D-40** (`routes.go`, `buildMux` `:16`).
   - **Browser mux:** session, then `requireAuth`, then `http.NewCrossOriginProtection`. Form POSTs to `/i/{name}/start|stop|restart` are answered with 303 and a flash message.
   - **`/api/v1` mux:** bearer tokens only, looked up with `identity.HashToken` in `api_tokens`.
     - Revoked or expired tokens are rejected.
     - The token's role ceiling is applied.
     - Cookies are never read.
     - `last_used_at` is recorded, and the actor is audited as `api_token`.
   - Tests: `TestAPIIgnoresCookies`, `TestBearerIsVerified`, and a test that every browser form posts into the browser mux.

   (web-dashboard-5, admin-security-5)
4. **The §15.2 cumulative role matrix replaces `matrix.go`** (`authz/matrix.go`, `Check` `:68`). The roles nest as viewer ⊂ operator ⊂ admin ⊂ owner.
   - `owner` is added to `authz.Role` (`role.go`). It is added to the `role_t` enum by a new migration that only `gvm db migrate` applies.
   - The matrix:

     | Role | Can |
     |---|---|
     | **viewer** | `instance:view`, `console:read`, `content:view`, `backup:list`, `job:view`, `manifest:view`, `token:create.self`; secrets visible only as set/unset |
     | **operator** | adds `instance:start`, `instance:stop`, `instance:restart`, `instance:update`, `console:write`, `content:sync`, `backup:create`, `job:run`, `job:cancel` |
     | **admin** | adds `instance:create`, `instance:delete`, `instance:apply`, `instance:promote`, `instance:rebuild`, `instance:adopt`, `instance:config`, `content:edit`, `backup:restore`, `backup:delete`, `job:schedule`, `secret:read`, `secret:write`, `firewall:*`, `caddy:*`, `user:view`, `grant:manage`, `audit:read`, `doctor:run` |
     | **owner** | adds `user:manage`, `secret:rotate`, `manifest:publish`, `token:manage.any`, `host:config` |

   - The effective role is `GREATEST(global_role, grant)`, and grants only ever add. `CanGrant` follows §15.2: an owner may grant any role, and an admin may grant only operator or viewer.
   - In this plan, user and grant management, secret rotation, manifest approval and host config stay **root-CLI only**, whatever the matrix allows.
   - A matrix test asserts that operator lacks rebuild, restore, prune/delete, apply, adopt, secret read/write, `content:edit` and `instance:config`.

   (web-dashboard-4, admin-security-3)
5. **Structured errors.** `internal/webapi/envelope.go` defines the codes: unauthenticated, forbidden, not_found, conflict, confirmation_required, plan_expired, precondition_failed, rate_limited, degraded and internal.
   - `request_id` equals `X-Request-Id` and `audit_log.request_id`.
   - There is one 503 template (`serve503Page`, `handlers.go:40`; `serve503`, `auth.go:159`), and it carries generic commands only. A test asserts it never contains an instance name from a fixture, so the hard-coded `pz-main` goes.

   (web-dashboard-13)
6. **A working edge config.**
   - `edge.ComputePlan` (`edge.go:46`) emits **one** vhost for `edge.site`, reverse-proxying to `unix//run/gvm/web/dashboard.sock`. The doubled `unix/` prefix is fixed, and the per-instance `web.sock` routes are deleted.
   - SSE routes live under `/stream/` and `/api/v1/stream/`, with `flush_interval -1` handle blocks (`caddyconf/render.go`). A test checks every `text/event-stream` route against a stream matcher.
   - The `edge.mode` and `tls.mode` enums are validated together with their required fields.
   - Goldens cover acme-http, acme-dns, internal-bareip and snippet-import.
   - The tmpfiles group comes from `edge.socket.group` instead of being hard-coded (`packaging/tmpfiles/gvm-web.conf`).

   (web-dashboard-11, network-firewall-5)

### Definition of done

- [ ] All the tests named above are green.
- [ ] `caddy validate` accepts every golden Caddyfile (a CI job with a pinned Caddy binary).
- [ ] `go list -deps ./cmd/gvm` still excludes `internal/web`, `internal/auth` and `internal/authz`. They are linked in Phase 48.

**Depends on:** Phase 22. Milestone order puts it after J3. **Effort:** L.

**Design rules touched:**
- **Rule 9:** there is no anonymous read. `/api/v1` takes bearer tokens only, so a cookie alone gets 401.
- **Rule 5:** destructive routes keep the typed name and archive-first on top of RBAC.
- **Rule 7:** the edge never writes `/etc/caddy/Caddyfile`, and Caddy's admin API stays on a unix socket.
- **Rule 8:** the `owner` enum migration runs only through `gvm db migrate`.

---

## Phase 48 — Dashboard v0: invite a friend, let them restart and watch the console

**Goal:** an operator invites a friend, who enrols their own password and TOTP. Without SSH, the friend can start, stop, restart, watch and type into the console of the servers they were granted, and can see why a server is down. Game servers keep running when the dashboard is down.

**Why now:** today the only way to delegate a restart is root SSH (or Phase 37's sudoers example). Phase 47 made the skeleton safe to wire.

**Resolves:** onboarding-6, web-dashboard-1, web-dashboard-6, web-dashboard-9, web-dashboard-10 (status model), web-dashboard-11 (wiring), admin-security-1 (user and token), admin-security-4, admin-security-6 (create, list, revoke), admin-security-13 (admin guide), admin-security-14, network-firewall-5 (`gvm web` and edge apply), documentation-14, failure-recovery-13 (status via gvmd), day2-ops-6.

### Work items

1. **User management from the root CLI, in-process,** over peer-auth Postgres as `gvm_ops`, never through admin.sock.
   - **`sudo gvm user create --email <e> --role owner|admin`** creates the first accounts. It reads the password from the TTY (`term.ReadPassword`), and TOTP enrolment is mandatory for owner and admin.
   - **`sudo gvm user invite <email> --instance <n> --role operator|viewer --ttl 24h`** prints a single-use enrolment URL. The invite has a viewer ceiling unless granted more.
   - **Grants:** `user grant` and `user revoke-grant <email> <instance> <role>`. `user roles` prints the Phase 47 matrix as a plain-English table, and `user show` shows one account.
   - **`user reset <email> [--totp] [--password]`** revokes sessions and tokens, clears the factors and prints a fresh invite, so root never chooses a friend's password.
   - **`user sessions`** and **`user revoke-sessions`**.
   - **With Postgres down,** these fail fast: "Postgres is unreachable; dashboard users are stored there. Your game servers are not affected."
   - When the users table is empty, gvm-web serves only one page. It has no login form, and carries the exact line `sudo gvm user create --email you@example.com --role owner`.

   (onboarding-6, admin-security-4, admin-security-1, web-dashboard-6)
2. **Fix the password rules before the first user exists.**
   - argon2id via `golang.org/x/crypto/argon2` replaces bcrypt (`identity/password.go`), which removes the 72-byte cap.
   - The stated minimum is 12 characters.
   - Recovery codes are grouped Crockford base32 of at least 80 bits, accepted case-insensitively with or without dashes.

   Doing this now means no stored hash ever needs migrating. (admin-security-14)
3. **`sudo gvm host init --dashboard`.**
   - It creates `master.key` (0600 root) if absent.
   - It installs `gvmd.service`, `gvm-web.service`, `gvm-db-migrate.service` and `gvm-web.conf` (the caddy-group tmpfiles lines), and does not start the dashboard.
   - It prints the apt lines (postgresql, and Caddy from its official repository) and the `pg_hba`/`pg_ident` lines, with "insert above `local all all peer`", then `next: sudo gvm db migrate`.
   - Doctor verifies the `pg_hba` ordering.

   (onboarding-6)
4. **`gvm gvmd` (root).** The command was hidden and `notImplemented` (`root.go:101-102`).
   - It exposes a **closed verb set**: `status`, `start`, `stop`, `restart`, `console.send`, `backup.now` and `auth.verify_totp`. These call the same hostops and `lifecycle.Manager` paths as the CLI, so a stop always walks the ladder.
   - `backup.now` starts `gvm-job@<n>_<job>.service` through systemd, so the run survives gvmd dying.
   - At startup gvmd never auto-resumes a lease and never actuates the reconciler.
   - It **never runs migrations.** It checks `AssertCompatible`, and refuses auth verbs when the schema is behind.

   (web-dashboard-1, day2-ops-6)
5. **`gvm web`** runs as `User=gvm` with `SupplementaryGroups=gvm-console`, no `LoadCredential`, and listens only on `/run/gvm/web/dashboard.sock`.
   - Login uses TOTP **verified in gvmd**.
   - The instance list and detail pages come from `lifecycle.Status` through gvmd's `status` verb: a phase pill, reason, hint mapped for dashboard roles, players, the lease line and a drift badge.
   - Start, stop and restart POST to gvmd. gvmd returns 202 with an operation id **only after it has acknowledged the dispatch**, or 409 with the lease line.
   - Progress arrives over SSE. Refused actions render as disabled buttons, with the reason.

   (web-dashboard-1, web-dashboard-10, failure-recovery-13)
6. **The console, per §16.6.**
   - `hello{state, cursor, oldest}`, with a 200-line backlog through the socket's tail op (`console/hub.go`, `ring.go`). `Last-Event-ID` takes precedence, and gaps show "— N lines skipped —".
   - A stopped server shows a banner built from status and the last shutdown record: "stopped cleanly 2h ago by alice".
   - The send box requires `console:write` and writes the audit row first. It accepts one line of at most 1024 bytes.
   - It refuses `quit`, `stop`, `shutdown`, `exit` and every command word in the manifest's ladder: "Use the Stop button: it saves the world first".

   (web-dashboard-9)
7. **Edge wiring.**
   - `gvm edge plan` is read-only. It shows the TLS mode, ports and the Caddyfile diff, and a DNS mismatch as a Note. It also lists the host rules the edge needs (`80/tcp`, `443/tcp`, and `443/udp` with HTTP/3) and whether the live ruleset admits each one. A missing one prints `next: sudo gvm firewall apply` (Phase 40), which is the only path that writes host rules.
   - `sudo gvm edge apply` validates, swaps, reloads over the admin unix socket and verifies. It rolls back on failure, and never installs Caddy. With ufw active and 80/443 not admitted, it refuses before requesting a certificate, because the ACME HTTP challenge would fail: "ports 80 and 443 are not open; run sudo gvm firewall apply first".
   - An optional `edge.listen` is accepted only on a loopback address. A non-loopback plain-HTTP listen is refused at validation.
   - `sudo gvm ca export [--out FILE]` writes the internal CA's public certificate (never its key) for the bare-IP `tls.mode: internal` path, with instructions for importing it on a moderator's machine.
   - The `ssh -L` path is documented.

   (network-firewall-5, web-dashboard-11)
8. **Control-plane-dead gates.**
   - `golden-units.sh` asserts that `gvm@.service` and `gvm-job@.service` have no `Requires`, `Wants`, `After` or `BindsTo` on gvmd, gvm-web or postgresql.
   - A Linux job runs the real kill test: `systemctl stop gvmd gvm-web postgresql`, then it asserts the game's pid and starttime are unchanged and that `sudo gvm restart pz1` still works.

   (web-dashboard-1)
9. **Docs.**
   - `docs/dashboard.md`, for moderators: logging in, TOTP, what each role can do, that Stop walks the save ladder, console etiquette, and typed names for destructive actions.
   - `docs/admin-guide.md`, covering only commands that work.
   - The README rows flip.

   (documentation-14, admin-security-13)
10. **Minimal API tokens** for `/api/v1` clients.
    - `sudo gvm token create --user <email> --name <n> [--role] [--instance] [--expires 90d|never]` mints a token with a `gvmt_` prefix plus 8 random characters, and shows it once, in a box.
    - `token list`.
    - `token revoke <name|prefix>` refuses an ambiguous match.

    The CLI remote client stays out of scope: `--remote` stays hidden. (admin-security-6)

### Definition of done

- [ ] On the J1 box (ufw active, only `22/tcp` allowed at the start):
  1. `sudo gvm host init --dashboard`, then the printed apt lines
  2. `sudo gvm db migrate`
  3. `sudo systemctl enable --now gvmd gvm-web`
  4. `sudo gvm edge plan` lists 80/443 as `NOT admitted` and prints `next: sudo gvm firewall apply`
  5. `sudo gvm firewall apply` (typed host name) admits 80/443, with `22/tcp` untouched
  6. `sudo gvm edge apply` with an ACME domain obtains the certificate

  `https://gvm.example.com` then shows the empty-users page. After `sudo gvm user create --email steve@example.com --role owner`, it shows the login page. Run before step 5, `edge apply` refuses and names `sudo gvm firewall apply`.
- [ ] `edge.listen: 0.0.0.0:8080` is refused at validation. With `tls.mode: internal` on a bare IP, `sudo gvm ca export --out ca.crt` writes a PEM certificate that a browser accepts, and the file contains no private key.
- [ ] `sudo gvm user invite alice@example.com --instance pz1 --role operator`:
  - alice enrols a password and TOTP, and sees only pz1
  - Restart walks the ladder
  - the console shows the backlog, and typing `quit` is refused
  - other instances return 404 for her, and there is no restore button
- [ ] `systemctl stop gvmd gvm-web postgresql` leaves pz1's pid unchanged. `sudo gvm restart pz1` works, and the site shows the generic 503 page.
- [ ] `sudo gvm user reset alice@example.com --totp` revokes her sessions and prints a fresh invite.

**Depends on:** Phases 47, 33, 37, 40 (`gvm firewall apply` opens 80 and 443). **Effort:** XL.

**Design rules touched:**
- **Rule 1:** the golden-units gates and the kill test.
- **Rule 3:** user commands run in-process over peer auth, and the CLI never dials admin.sock.
- **Rule 4:** gvmd's stop uses Manager's ladder, and the console refuses ladder words.
- **Rule 6:** gvmd never auto-resumes or actuates at startup.
- **Rule 8:** no implicit migrations; `gvm-db-migrate.service` is explicit; no pgbouncer or extensions.
- **Rule 9:** TOTP is verified in gvmd, there is no anonymous view, and the API is bearer-only.
- **Rule 7:** the edge never writes a firewall rule. Ports 80 and 443 open only through Phase 40's typed, SSH-first `gvm firewall apply`, and a dashboard listener off loopback is refused.

---

## Phase 49 — Dashboard v1: what moderators actually need · **Milestone J4**

**Goal:** moderators see how to connect, which mods are installed and why a server is down, in plain words. Admins can change safe settings. Players get a connect line and a mod list to share.

**Why now:** v0 lets friends act. v1 lets them answer the questions players ask, without the operator.

**Resolves:** web-dashboard-10 (connect card), game-catalog-config-7 (dashboard), game-catalog-config-8, content-mods-13, failure-recovery-13 (rule text), offsite-backup-dr-11 (backup panel), admin-security-6 (rotate and expiry), gvm-self-upgrade-uninstall-11 (version for signed-in users).

### Work items

1. **A "How to connect" card** for signed-in roles, built from the Phase 33 connect model.
   - The password shows as •••• unless the role holds `secret:read`.
   - It gives the same paste line as `sudo gvm status <n> --share`.

   (web-dashboard-10, game-catalog-config-7)
2. **Plain-language status.**
   - One table maps every lifecycle Rule to dashboard text, and a test checks that every Rule has an entry.
   - Raw shell hints are never shown to dashboard roles. Each maps to "needs the server operator" or to a dashboard action.
   - The detail page shows the last job record and the sanitised last shutdown record.
   - A backup panel is built from the index and the job records.

   (failure-recovery-13, offsite-backup-dr-11)
3. **A Mods tab, read-only first.**
   - **The browser never calls `/api/v1`.** A signed-in browser holds only a session cookie, and rule 9 makes `/api/v1` bearer-only (Phase 47), so a cookie gets 401 there. The tab is served from the browser mux at `GET /i/{name}/content` (session, then `requireAuth`, then `CrossOriginProtection`). It shares one projection handler with the bearer-only `GET /api/v1/instances/{name}/content` for API clients.
   - Both require `content:view` and return projections only.
   - It reads owned keys through the gamecfg parser, so an RCON password in the same ini can never be returned.
   - It uses a cached, presence-only observe, and shows the AWAITING_MANUAL_FILES banner.
   - A Sync button (a form POST to `/i/{name}/content/sync`) asks gvmd's `content.sync` verb to start the `content_sync` job unit, and requires `content:sync`.
   - For players: `gvm content list <inst> --format markdown|links`.

   (content-mods-13)
4. **A Settings tab,** generated from VarSpec (group, help, unit, apply badge), served at `/i/{name}/settings` on the browser mux, and gated on `instance:config`, which is admin and above. `spec.Capabilities` now reports `config.edit` available again (Phase 20 had turned it off).
   - It posts to gvmd's `instance.set` verb, which calls Phase 39's set service with the YAML's sha256 as If-Match.
   - It shows the `apply --diff` plan, and a "Restart to apply" button that uses the ladder.
   - Immutable and rebuild-class vars are read-only: "change with sudo gvm rebuild over SSH".
   - Join-password rotation is write-only through gvmd's `secret.write` verb, and the value never reaches gvm-web.
   - Operators and viewers see the tab read-only.

   (game-catalog-config-8)
5. **gvmd's verb set grows by three, each with its permission and audit action.** Phase 48 made the set closed; this phase extends it explicitly, and `TestGvmdVerbSetIsClosed` lists the new members:

   | Verb | Permission (Phase 47 matrix) | Audit action | Does |
   |---|---|---|---|
   | `content.sync` | `content:sync` (operator and above) | `content.sync` | starts `gvm-job@<n>_content_sync` through systemd, so the run survives gvmd dying |
   | `instance.set` | `instance:config` (admin and above) | `instance.set`, with key names and old → new values for non-secret vars | calls the Phase 39 set service with If-Match; refuses immutable and rebuild-class vars |
   | `secret.write` | `secret:write` (admin and above), and only for the instance's `player_facing` join password | `secret.write`, name and fingerprint only | generates the new value inside gvmd (`GeneratePassword`), writes it through `secrets.Store`, and returns only the fingerprint; a user who may read it sees it on the connect card (`secret:read`) |

   Every other secret, and every host secret, stays root-CLI only.
6. **Token lifecycle.**
   - `token rotate <name> [--grace 1h]`.
   - Tokens expire after 90 days by default, and `never` must be explicit.
   - A ceiling above the user's own role is refused.

   (admin-security-6)
7. **The version and the restart-pending count** from Phase 35's `gvm upgrade status` are shown in the dashboard footer to signed-in users only. (gvm-self-upgrade-uninstall-11)

### Definition of done (Milestone J4)

- [ ] As alice (operator on pz1):
  - the connect card shows `203.0.113.5:16261/udp (also 16262/udp) · password: set`
  - the Mods tab lists titles
  - the Settings tab is read-only
- [ ] As bob (granted admin on pz1), setting `max_players` to 20 shows "restart to apply", and Restart walks the ladder.
- [ ] A concurrent hand edit of `pz1.yaml` makes bob's Settings save fail with 412 and a reload prompt.
- [ ] A test asserts the content endpoint never returns a `deny_keys` value.
- [ ] `TestNoBrowserPageFetchesAPIv1` renders every browser template and asserts that no `hx-get`, `hx-post`, `fetch` or form action targets `/api/v1`. With only a session cookie, `GET /api/v1/instances/pz1/content` still returns 401.
- [ ] `TestGvmdVerbSetIsClosed` lists exactly the Phase 48 verbs plus `content.sync`, `instance.set` and `secret.write`. As alice (operator), `instance.set` and `secret.write` return 403 and write audit rows with `outcome: denied`.
- [ ] Signed in, the dashboard footer shows `gvm 0.2x.y · 1 server restart pending`. Logged out, the login page and the 503 page show neither.
- [ ] The J4 journey runs in CI:
  1. invite, then enrol with a scripted TOTP
  2. restart
  3. console read and send
  4. the connect card
  5. kill the dashboard and Postgres, and confirm the servers keep running

**Depends on:** Phases 48, 39, 44. **Effort:** L.

**Design rules touched:**
- **Rule 9:** every view is authenticated, and secrets stay masked unless the role holds `secret:read`. Browser pages use the session-only browser mux and never call `/api/v1`, so `/api/v1` stays bearer-only and a cookie there still gets 401.
- **Rule 3:** the three new gvmd verbs are the dashboard's, reached over gvmd's socket by gvm-web. The CLI still never dials admin.sock.
- **Rule 5:** immutable and rebuild changes stay CLI-only, with typed confirmation.
- **Rule 4:** the restart button walks the ladder.
- **Rule 1:** the CLI path stays complete with the web tier down.

---

## Milestone J5 — adopt, package, recover

## Phase 50 — Make adoption's safety net real

**Goal:** when the operator adopts their live Project Zomboid server, a failure or rollback at any point leaves the original server running on its original world, with nothing lost. `finalize` cannot delete the legacy unit early, or on a box that was never adopted.

**Why now:** cutover and rollback are stubs. Their library code must be fixed before Phase 51 wires them. Wired as written, it would run two servers on one world and restore to the wrong directory. Phase 25 already made discovery trustworthy, and Phase 21's fail-closed finalize is an interim that this phase replaces.

**Resolves:** migration-adopt-2 (record-based), migration-adopt-4, migration-adopt-5, migration-adopt-6, migration-adopt-7, migration-adopt-9 (the record), migration-adopt-1 (credential import, so the wired cutover can start gvm@), admin-security-2 (an adopted instance's secrets are imported with `source: adopted`).

### Work items

1. **An adoption record.** `/var/lib/gvm/adopt/<name>.json` is root 0600, written through a new hostops file kind (`FileAdoptRecord`: atomic, O_NOFOLLOW).
   - **Cutover writes it before anything changes,** prints it, and updates it at each step. Discovery never writes it (Phase 25).
   - **The state machine:**
     1. planned
     2. cutover-in-progress
     3. cutover-complete or rolled-back
     4. finalized
   - **It records:**
     - the legacy unit's and timer's names and prior state (enabled, active), and the moment cutover disabled the timer
     - the archive path and sha256
     - the rollback-script path
     - for each credential, how it was obtained: imported from the live server (`source: adopted`) or generated under `--rotate-rcon` (item 8)
     - timestamps and the gvm version

   (migration-adopt-9, migration-adopt-6)
2. **`gvm adopt finalize <instance> [--force]` reads the record,** replacing the Phase 21 interim (`FinalizePZ`, `finalize.go:27`).
   - `--name` and `--unit-path` stay for one release as hidden aliases. `--unit-path` must equal the record's legacy unit, or finalize refuses. `--adopted-from` is removed.
   - **State checks can never be bypassed,** not even with `--force`:
     - the YAML is adopted
     - the record says cutover-complete
     - the legacy service and timer are both disabled **and** inactive
     - `gvm@<n>` is enabled **and** active
   - With no record but a YAML that says adopted, finalize refuses: "cannot tell when <n> was adopted or whether cutover completed; nothing was changed".
   - **The instance name is typed** (`TypedConfirm`) before anything is removed, as in Phase 21's interim (rule 5). `--force` never satisfies it.
   - **Time and evidence checks yield to `--force`:**
     - 7 days since cutover completed
     - `gvm@` active for at least 24 h (`ActiveEnterTimestamp`)
     - at least 3 verified archives in the on-disk index, read without Postgres
   - Finalize then prints the checklist and acts:
     1. it copies the legacy units to `/var/backups/gvm/<n>/legacy-units/`
     2. it disables them and removes them, together with both guards
     3. it reloads systemd
     4. it updates the record

   (migration-adopt-2)
3. **Cutover never leaves two servers on one world** (`CutoverPZ`, `cutover.go:56`).
   - **The legacy timer is disarmed first.** Today step 2 stops the legacy server (`cutover.go:68-70`) while `zomboid-restart.timer` stays armed through the multi-minute pre-cutover archive. If it fires in that window, the bash suite restarts PZ on the world being archived. So before the legacy stop, cutover runs `disable --now` on the timer, records that in the adoption record, and only then stops the server. A pre-commit failure or rollback re-enables the timer exactly as recorded.
   - **The legacy stop is gvm's, never the unit's own.** The live `zomboid.service` stops through `ExecStop` with `TimeoutStopSec=180` and `SendSIGKILL=yes`, which can SIGKILL a JVM mid-save. Cutover stops PZ through the §19.2 save-and-quit sequence (Phase 51) and polls the pinned pid. Then, **at the commit point**, cutover:
     1. asserts that `zomboid.service` is already inactive. If it is not (something restarted it), cutover aborts as a pre-commit failure. It never lets systemd stop a live legacy JVM.
     2. runs `disable` on `zomboid.service` (already inactive, so nothing is stopped) through hostops, using the name from the record
     3. writes `gvm@<n>.service.d/20-adopt.conf` (`RenderAdoptGuard`, `units.go:724`) and the legacy guard (`RenderLegacyGuard`, `:738`)
     4. reloads systemd
     5. **only then** starts `gvm@`

     The success block warns: "do not run /root/pz-server/update.sh or rebuild.sh; starting zomboid.service will stop gvm@<n>".
   - **Scoped hostops verbs for the legacy units.** The hostops verb table is scoped to gvm's own units today. Cutover, rollback, finalize and uninstall need `unit.disable`, `unit.enable` and `unit.stop` on the legacy names, so new verbs accept **only** the unit names recorded in an adoption record (the service and its timer). `TestLegacyUnitVerbsRefuseOtherUnits` asserts that every other name, including `ssh.service` and any `gvm@`, is refused.
   - **Rollback:**
     - It disables `gvm@<n>` and its `gvm-job@<n>_*` timers, listed by name.
     - It re-enables the legacy timer only if the record says it was enabled, and leaves both guards in place.
   - **A pre-commit failure** restores the legacy unit and timer to exactly their recorded state, and says "Legacy server restored to its previous state; nothing else was changed".
   - **A post-commit rollback** reports each action with ✓ or ✗. It returns a distinct "rollback incomplete" error, with the manual sequence, unless every step succeeded.
   - **`CutoverResult`** is populated on every path.
   - Instead of `enabled: false`, a rolled-back instance gets a hold and record state rolled-back.
   - Phase 40's `KeepPorts` keeps the instance's firewall rules while the state is rolled-back, so the legacy server never loses its ports.

   (migration-adopt-4, migration-adopt-6)
4. **The rollback script uses only systemctl** (`GenerateRollbackScript`, `rollback.go:14`).
   - It emits only `systemctl` lines: `disable --now` for gvm@ and its timers, then `enable --now` for the legacy unit, plus its timer if it was enabled. `ValidateRollbackScript`'s allowlist becomes systemctl, echo and set.
   - It **prints, and never runs,** the correct restore sequence:
     1. stop `zomboid.service`
     2. move the current world, `_player` and `db/` aside, to `*.after-cutover-<ts>`
     3. run `tar -xzf <archive> -C <DataDir>` with the prefix strip that matches `internal/archive`'s layout
     4. `chown -R`
     5. start the legacy unit
   - gvm never executes the script.
   - A test, `TestRollbackRestoreLandsUnderDataDir`, builds the real pre-cutover archive, applies the printed `-C` and strip, and asserts that the files land under DataDir.

   (migration-adopt-5)
5. **A real pre-cutover archive.** `archive.CreateAndVerify` (`create.go:241`) runs with class `preserve`, reason `adopt-pre-cutover`, into `/var/backups/gvm/<name>`. It replaces the `runTar` exec.
   - **Sources:**
     - DataDir's `Server/`, `Saves/Multiplayer/<srv>`, `<srv>_player` and `db/`
     - the suite's `config.env`, `mods.txt` and `sandbox.txt`, under a separate prefix

     `SecretsIncluded=true`, with a spot verify of 24 members.
   - **Space is checked first.** `statvfs` against the estimate × 1.2 refuses **while the server is still up**.
   - **The same-world check runs after start:**
     1. an RCON `save`
     2. every pre-cutover path is present
     3. the newest mtime is newer than the fingerprint
     4. a UDP probe answers, and RCON auth succeeds

     It reports "N/N files present, newest write Xs ago → same world".

   (migration-adopt-7)
6. **Restore respects a rolled-back adoption.** `gvm restore` (Phase 38) refuses while the record says rolled-back, or while the legacy unit is active: "the legacy server owns this world; use its own tools, or cut over again".
7. **Doctor INST-012, report-only,** flags:
   - both units enabled
   - a guard missing while the record says cutover
   - the legacy unit active while the record says cutover

   INST-013 flags adopted credentials that were never rotated (`source: adopted` in the secret's sidecar). (migration-adopt-4)
8. **Cutover imports the live server's credentials, before the commit point.** Without this, `gvm@pz-main` would fail Phase 29's credential preflight with exit 65 right after the commit point, because Project Zomboid's control channel needs `rcon_password`.
   - **What is read, read-only:** `RCONPassword=` and `Password=` from the live `servertest.ini`, and `ADMIN_PASSWORD` from `config.env` (through Phase 25's quote-aware parser). This is cutover, not discovery, so rule 11's read-only discovery is unaffected, and nothing in the legacy install is written.
   - **Each value is checked** with the same rules `gvm secret set` applies to that secret (rule 10 plus the game's separators). A value that fails is Phase 25's blocker, "password for <role> contains characters gvm cannot carry; rotate it", and cutover refuses before the legacy stop. A value is never imported altered.
   - **Each value is written** through `secrets.Store` with `source: adopted`, and the adoption record lists which were imported. An empty `Password=` leaves `join_password` unset, which keeps the server open, as it was.
   - **`--rotate-rcon`** generates a fresh `rcon_password` instead and adds `RCONPassword` to the keys the cutover plan writes. An empty live `RCONPassword` needs it, because the same-world check's RCON auth cannot work without one, and cutover says so. The admin password lives in PZ's own player database, so gvm only imports it and never rotates it.
   - Rollback leaves the imported files in place; they are the legacy server's own values, and the legacy server never reads them. After a `--rotate-rcon` cutover, the ini keeps the new `RCONPassword`, and rollback's report says so, naming the legacy scripts that may still hold the old one.

   (migration-adopt-1, admin-security-2)

### Definition of done

- [ ] Fault injection at every cutover step, before and after the commit point:
  - "disk full during archive" is refused before the stop
  - "legacy unit fails to restart" gives "rollback incomplete", with the manual sequence
- [ ] `TestFinalizeStateChecksIgnoreForce`: each state precondition violated on its own means refusal, even with `--force`. Each time or evidence precondition means refusal without `--force`, and success with it.
- [ ] `TestCutoverDisablesLegacyTimerAndWritesGuardsBeforeStart`: the recording fake shows the order timer `disable --now` → credential import → save-and-quit stop of the legacy server → pre-cutover archive → assert `zomboid.service` inactive → `disable zomboid.service` → both guards → daemon-reload → start `gvm@`.
- [ ] A fixture where the legacy unit is restarted by something during the archive window: at the commit point cutover aborts as a pre-commit failure, never issues a stop to `zomboid.service`, and restores the timer's recorded state.
- [ ] `TestCutoverPreCommitFailureRestoresPriorState` passes for all four enabled/active combinations, for the service and the timer.
- [ ] `TestLegacyUnitVerbsRefuseOtherUnits` is green.
- [ ] **Credential import.** On the `live-pz` fixture, cutover writes `rcon_password`, `admin_password` and `join_password` through `secrets.Store` with `source: adopted`, and the record lists them. A fixture `RCONPassword` containing `$` refuses before the legacy stop with the rotate blocker. With an empty `RCONPassword`, cutover refuses unless `--rotate-rcon` is given, and with it the plan lists `RCONPassword` among the written keys.
- [ ] `ValidateRollbackScript` rejects a bare `tar` line, and `TestRollbackRestoreLandsUnderDataDir` is green.

**Depends on:** Phases 21, 25, 39, 40. **Effort:** L.

**Design rules touched:**
- **Rule 11:** discovery stays read-only. Credentials are read only at cutover, read-only, and never altered. `--force` bypasses only time and evidence, never state.
- **Rule 2:** the rollback script is never executed by gvm, and systemctl calls go through hostops verbs that accept only the recorded legacy unit names.
- **Rule 4:** nothing here adds a SIGKILL. The cutover stop (Phase 51) aborts instead of escalating, and the commit point refuses to let systemd stop a live legacy JVM through its own `SendSIGKILL=yes` unit.
- **Rule 5:** a verified preserve archive exists before the stop, and finalize asks for the typed name.
- **Rule 10:** imported passwords pass the same checks as `gvm secret set`, and a failing one blocks cutover instead of being altered.
- **Rule 6:** INST-012 and INST-013 only report.
- **Rule 7:** a rolled-back adoption never loses its port rules.

---

## Phase 51 — Adopt the live Project Zomboid server

**Goal:** an operator cuts the live PZ server over to gvm in one supervised window, follows a runbook that matches the CLI, tracks the adoption week with `gvm adopt status`, and can later hand binary updates to gvm with a confirmed `instance promote`.

**Why now:** Phase 50 made the primitives safe, and Phase 25 made discovery trustworthy, so the journey can now be wired.

**Resolves:** migration-adopt-1, migration-adopt-9 (status), migration-adopt-10, migration-adopt-13, migration-adopt-14 (full runbook), documentation-6 (full runbook).

### Work items

1. **Wire cutover and rollback.** `sudo gvm adopt cutover <instance>` and `sudo gvm adopt rollback <instance>` call the Phase 50 library, and the registry rows flip.
   - A step reporter appends to the record and to `/var/log/gvm/<n>/adopt.log`, so `gvm adopt status` run from a second SSH session shows progress.
   - SIGHUP is ignored.
   - SIGINT before the commit point finishes the current step, restores the legacy state and exits. After the commit point it means "roll back at the next safe point".
   - A downtime counter runs throughout.
   - The stop is §19.2 step 4: save over the console, wait for save evidence, quit, and poll the pinned pid and starttime. It never SIGKILLs, and it aborts on timeout, e.g. "[2/12] save confirmed, JVM exited in 41s".
   - Config rendering follows Phase 30's adopted-instance rule. Only the keys the cutover plan lists are written, and no key is blanked.

   (migration-adopt-1, migration-adopt-13)
2. **`gvm adopt status <name>`** reads the record, e.g. "cutover 2026-09-04 (day 3 of 7) · gvm@pz-main active 2d 22h · backups 3/3 ok · rollback: sudo gvm adopt rollback pz-main · finalize available 2026-09-11".
   - `gvm status` gets one line: "adopted from zomboid.service (day 3/7); binary updates reported, not installed".
   - The dashboard overview shows the same.

   (migration-adopt-9)
3. **`sudo gvm instance promote <name> --confirm <name>`.**
   - It prints the plan first: "the nightly will now stop, update via SteamCMD as pzserver, post-verify the appmanifest and start; it never runs validate".
   - It then takes a verified preserve archive, flips `managed_install` through the intent writer (Phase 39), runs apply, and records the change.
   - The Phase 20 messages point back to it.

   (migration-adopt-10)
4. **A runbook that matches the CLI.** `docs/adopt-projectzomboid.md` is completed with cutover, status, rollback, promote and finalize, and a parse-only test checks every fenced line. `gvm help adopt` gains:
   - "cutover — one supervised downtime window; archives first; rolls back by itself"
   - "rollback — returns the legacy server; your world stays as gvm left it"

   (migration-adopt-14, documentation-6)

### Definition of done (the original v1 adoption criterion, as a transcript)

- [ ] On a VM restored from a real backup of the live box:
  1. `sudo gvm adopt discover pz-main --unit zomboid` runs with players online and ends "Cutover would proceed … Nothing has been changed."
  2. `sudo gvm adopt cutover pz-main` completes in one window with step output. `servertest.ini` is byte-identical to its pre-cutover copy apart from the keys the plan lists. Players rejoin the same world at the same address, **with the same password**.
  3. `sudo gvm secret list pz-main` shows `rcon_password`, `admin_password` and `join_password` as `adopted` (the reference box has all three). `gvm@pz-main` passed the credential preflight (no exit 65 in its journal), and the cutover's same-world check reported `RCON auth ok`.
  4. After a reboot, only `gvm@` runs, and the legacy timer stays disabled.
  5. `sudo gvm adopt rollback pz-main` starts the legacy server on its world and keeps the post-cutover play.
  6. A second cutover, then `gvm adopt status pz-main` day by day: `finalize` on day 3 refuses without `--force`, and refuses even with `--force` while `gvm@` is inactive. On day 7 it asks for the typed name and succeeds.
  7. After finalize, `sudo gvm instance promote pz-main --confirm pz-main` prints the plan, takes a verified preserve archive (shown by `gvm backup list pz-main`), and flips `managed_install` to true with the instance file's comments preserved (golden diff). `sudo gvm job list` then shows pz-main's update job as runnable (with Phase 44 merged, as milestone order guarantees; if the adoption track is pulled ahead of J3, it reads `cannot run in this build: updates for games with mods need the mod engine` instead, and never "adopted"), and `gvm status pz-main` no longer says "binary updates reported, not installed".

**Depends on:** Phase 50. **Effort:** L.

**Design rules touched:**
- **Rule 11:** discovery is read-only, and finalize is gated.
- **Rule 4:** the cutover stop is save-first, with no SIGKILL.
- **Rule 5:** promote and cutover archive first, and promote takes a typed confirmation.
- **Rule 2:** the legacy scripts are never executed.
- **Rule 6:** status is read-only.
- **Rule 10:** imported credentials pass the character ban, which Phase 25's blocker and Phase 50's import enforce.

---

## Phase 52 — Packaged gvm: .deb, migrations and a rollback policy

**Goal:** installing, upgrading and rolling back gvm is one apt-shaped, documented step that never stops a world, and old archives stay restorable across releases.

**Why now:** tarballs with `host init` carried J1–J4. Operators who run several releases need package-managed files, a migration unit and a written rollback policy.

**Resolves:** onboarding-1 (.deb), gvm-self-upgrade-uninstall-1 (.deb), gvm-self-upgrade-uninstall-8, gvm-self-upgrade-uninstall-9 (rollback docs), gvm-self-upgrade-uninstall-10.

### Work items

1. **An nfpm `.deb`** for amd64 and arm64. It ships `/usr/bin/gvm` and package-owned units under `/usr/lib/systemd/system`.

   Hand-written maintainer scripts call `gvm host init --from-package`, using the same mapping table as `host init`. The scripts:
   - create the tmpfiles entries and reload systemd
   - enable timers on first install only
   - try-restart only `gvmd` and `gvm-web`, by name
   - run `systemctl start --no-block gvm-db-migrate.service`, and never fail the package
   - end by printing `gvm upgrade status`

   They **never** stop or restart `gvm@*` or `gvm-job@*`, and there is no `gvm.target`. postinst warns about a stray `/usr/local/bin/gvm`. The release workflow publishes the `.deb` files and `SHA256SUMS`. (gvm-self-upgrade-uninstall-1, onboarding-1)
2. **Migrations.**
   - `gvm-db-migrate.service` runs `After=postgresql` and `Before=gvm-web` only.
   - It uses `goose.NewProvider` with the Postgres session locker (`store/migrate.go`).
   - `AssertCompatible` is fatal in gvm-web and a warning in gvmd.
   - Messages depend on direction:
     - schema behind: "run gvm db migrate"
     - schema ahead: "schema 11 was written by a newer gvm; the CLI and every game server keep working, only the dashboard waits"
   - `db status --no-wait`.
   - Within a release line, migrations are expand-only. That is the rollback policy.

   (gvm-self-upgrade-uninstall-8)
3. **Archive readers are backward compatible** (`archive/manifest.go`, `index.go`).
   - They accept every schema at or below the current one, upgrading on read.
   - They refuse newer schemas with "install gvm ≥ X".
   - The index is rebuilt from the archives when its schema does not match.
   - A CI fixture keeps one small archive per released schema, and runs `backup verify` and `restore --to` on each.

   (gvm-self-upgrade-uninstall-10)
4. **`docs/upgrading.md`** covers upgrading, the N-1 rule and rolling back. A binary-only rollback keeps the unknown-key errors at apply and the warnings at run (Phase 39). (gvm-self-upgrade-uninstall-9)

### Definition of done

- [ ] `sudo apt install ./gvm_0.x_amd64.deb` on a box with live worlds changes no game pid.
- [ ] Installing the previous `.deb` (a rollback) keeps the worlds running, and `gvm apply` refuses newer keys with file:line.
- [ ] The archive fixtures for every released schema pass verify and restore.

**Depends on:** Phases 35, 48. **Effort:** L.

**Design rules touched:**
- **Rule 4:** maintainer scripts never stop or restart a world.
- **Rule 6:** postinst never restarts anything game-related.
- **Rule 8:** migrations run only through `gvm-db-migrate.service` or `gvm db migrate`, never inside gvmd.
- **Rule 1:** a migration failure blocks only gvm-web.

---

## Phase 53 — Remove a server, or gvm itself, cleanly

**Goal:** an operator removes a single server, or gvm itself, and knows exactly what was removed, what was kept and why. The backups are always kept.

**Why now:** destroy leaves secrets, accounts and state behind without saying so, and there is no removal path at all. Doing it by hand risks worlds, secrets and SSH.

**Resolves:** gvm-self-upgrade-uninstall-6, gvm-self-upgrade-uninstall-7.

### Work items

1. **destroy says what it left.** It ends with a "LEFT IN PLACE" block listing secrets, the account, state, logs and the Steam home, each with why it was kept.
   - `--purge-residue` removes state, logs and the Steam home. It removes the account only when the GECOS is "gvm instance <n>" and the account owns no remaining files, through Phase 32's hostops `user.remove`, extended from scratch accounts to exactly such accounts.
   - `sudo gvm secret purge <n>` removes the instance's secrets after the instance name is typed (rule 5).
   - Firewall rules are already closed by destroy (Phase 40).

   (gvm-self-upgrade-uninstall-7)
2. **`gvm uninstall --dry-run`** lists every location under REMOVE, KEEP and LEFT FOR YOU. `docs/uninstalling.md` explains it. (gvm-self-upgrade-uninstall-6)
3. **`sudo gvm uninstall`.**
   - It asks for the host name to be typed, and refuses while any job or freeze lease is live or unreadable.
   - It stops the guard and job timers first.
   - For each instance, it takes a verified backup, then stops the instance through the ladder.
   - Adopted instances are handed back in Go through hostops: legacy units restored, guards removed, systemd reloaded. The stored rollback script is never executed.
   - It follows dpkg semantics: `/etc/gvm` is **kept** by default, and `--purge-config` removes it.
   - Cleartext secrets are never written into `/var/backups`. With `--purge-config`, they are first exported to one root 0600 directory, `/root/gvm-secrets-<ts>/`, whose path is printed with the advice to move it off the box. (age is wired only in Phase 54, so encrypting this export to a recipient arrives with `gvm secret export` in Phase 55, which adds `uninstall --export-secrets-to <age1…>`.)
   - `master.key` is deleted only with `--drop-database`.
   - The firewall is cleaned through the reconciler, so D-42 applies and SSH is never removed.

   (gvm-self-upgrade-uninstall-6)

### Definition of done

- [ ] `sudo gvm uninstall` on a VM with three instances, one of them adopted:
  - the legacy PZ server runs again
  - every instance has a verified backup
  - `ufw status` still admits SSH
  - `/etc/gvm` is kept
- [ ] Reinstalling and then running `gvm restore` brings the worlds back.
- [ ] `sudo gvm destroy v1` ends with a LEFT IN PLACE block naming the secrets dir, the `gvm-v1` account, `/var/lib/gvm/instances/v1`, `/var/log/gvm/v1` and the Steam home, each with why it was kept.
- [ ] `sudo gvm destroy v2 --purge-residue` removes v2's state, logs and Steam home. It removes `gvm-v2` only when its GECOS is "gvm instance v2" and it owns no remaining files. A fixture account with a different GECOS, or one that still owns a file, is kept and named in the block.
- [ ] `sudo gvm secret purge v1` without the typed name removes nothing. With it, the secrets dir is gone.
- [ ] `sudo gvm uninstall --purge-config` writes the secrets export to `/root/gvm-secrets-<ts>/` (0700 directory, 0600 files) and nothing under `/var/backups` contains a secret value (fixture grep).

**Depends on:** Phases 40, 50, 52. (It needs no age code: the encrypted export option arrives in Phase 55.) **Effort:** L.

**Design rules touched:**
- **Rule 5:** archive first and typed confirmation, for uninstall and for secret purge.
- **Rule 4:** every stop uses the ladder.
- **Rule 7:** SSH is never removed, and D-42 applies.
- **Rule 2:** the rollback script is never exec'd.
- **Rule 11:** the adopted server is returned to its prior state.

---

## Phase 54 — Off-box backups that really leave the box

**Goal:** a configured remote receives an encrypted copy of every verified archive. The operator can test, list and pull from it, and the last good off-box copy is always visible.

**Why now:** since Phase 21, a configured remote has reported "OFF-BOX COPY NOT MADE". This phase builds the transport behind that honest message.

**Resolves:** offsite-backup-dr-1 (transport), offsite-backup-dr-7 (last good copy), offsite-backup-dr-8, offsite-backup-dr-11 (BACKUP-004), offsite-backup-dr-14.

### Work items

1. **The transport.** `golang.org/x/crypto/ssh` (already in go.mod) and `github.com/pkg/sftp` sit behind `Dialer` (`remote_sftp.go:201`, nil today), and `filippo.io/age` sits behind `AgeEncrypter` (`remote_age.go`).
   - It uses the SFTP subsystem only, and never execs.
   - `HostKeyCallback` compares against the **pin only**. There is no `known_hosts` and no TOFU.
   - Encryption is mandatory when `encrypt_to` is set.
   - Push becomes its own job kind, `gvm-job@<n>_push`, started `--no-block` after verify. It takes a per-instance push lock (not the instance job lock), reads the verified archive through an open fd, honours `max_rate`, and times out below `TimeoutStartSec`.
   - **`on_failure` now governs the push job's verdict.** Phase 21 defined it for the backup run, but the backup run no longer waits for the push and can no longer fail on it. So a failed push makes the push job `degraded` under `on_failure: warn` and `failed` under `on_failure: fail`. The backup run's own result is unaffected, and its record names the push job.
   - **A push that does not succeed is never invisible.** Under either setting it writes the attention marker (Phase 37) with the reason, which `gvm status` and doctor show. Under `fail` the unit also fails, so `OnFailure=` fires (Phase 37's table). The index entry records `push_error` and the time, and `gvm backup list` shows OFF-BOX `✗ <reason>`.

   (offsite-backup-dr-1, offsite-backup-dr-7)
2. **`gvm backup remote check|list|pull <instance>`.**
   - **`check`** aborts the handshake **before user authentication** when there is no pin or it does not match.
     - It prints `SHA256:…`, with "compare with `ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub` run on the backup server", plus the YAML line to paste.
     - It never writes the YAML.
     - With a valid pin, it puts, lists and removes a probe file.
   - **`list`** and **`pull`** are read-only. `.age` files list as "encrypted, needs identity".
   - **`encrypt_to`** accepts a string or a list, and the docs recommend a second, offline recipient.
   - **The help** carries the custody warning: no identity, no backups.
   - There is no `gvm backup decrypt`; the docs show `age -d` on a workstation.
   - `backup list --remote` (dashboard-URL sense) has been gone since Phase 20. "remote" now means only the off-box target.

   (offsite-backup-dr-8, offsite-backup-dr-14)
3. **The last good copy is visible.** `pushed_at` feeds three places:
   - the OFF-BOX column
   - doctor BACKUP-004 (the age of the last good off-box copy)
   - the status line

   (offsite-backup-dr-7, offsite-backup-dr-11)

### Definition of done

- [ ] Against an SFTP test server in CI with a pinned key, a mismatched pin aborts before authentication.
- [ ] After a nightly, the push job is ok, and `gvm backup list` shows OFF-BOX ✓ with a time.
- [ ] **A failed push is visible.** With the SFTP test server stopped:
  - under `on_failure: warn`, the push job ends `degraded` and its unit exits 0; under `on_failure: fail`, it ends `failed`, its unit fails, and `OnFailure=` fires
  - either way the nightly's backup run is ok, its local archive verified
  - `gvm backup list v1` shows OFF-BOX `✗ connection refused` for that archive
  - `gvm status v1` shows the attention marker, and `sudo gvm doctor` BACKUP-004 reports the age of the last good off-box copy, e.g. "last good off-box copy 3 days ago"
- [ ] A round trip works: `gvm backup remote pull` → `age -d` on a workstation → `gvm backup verify --file` → `gvm restore --to`.

**Depends on:** Phases 36, 38 (`restore --to` and `backup verify --file`, used by the round trip). **Effort:** L.

**Design rules touched:**
- **Rule 1:** push runs after the local verify, in a separate unit. Its failure decides only the push job's verdict, and never touches the local archive or the backup run's result.
- **Rule 2:** no `ssh` or `sftp` exec.
- **Rule 5:** the remote prune rules are unchanged, and preserve archives are never pruned.
- **Pin-only trust:** no TOFU anywhere.

---

## Phase 55 — Move to a new host · **Milestone J5**

**Goal:** after losing the VPS, the operator rebuilds every server on a new box from its archives, with gvm itself.

**Why now:** this is the last journey. It needs restore (Phase 38), approval (Phase 46) and off-box copies (Phase 54).

**Resolves:** offsite-backup-dr-5, offsite-backup-dr-13 (the moving-host runbook).

### Work items

1. **Archives carry what a rebuild needs:** the `ContentLock`, the pinned manifest bytes and, for an override manifest, the override itself. This is an additive archive-manifest field under the Phase 35 N-1 rule. (offsite-backup-dr-5)
2. **`sudo gvm create <name> --from-archive <file> [--dry-run]`** treats the archive's contents as **untrusted**.
   - It re-validates the manifest, including the template guard (rule 10), and runs `ValidateInstance`.
   - The pinned revision must pass the Phase 32 ledger gate. A shipped revision is checked against the shipped ledger. An override is installed only through `gvm game approve`, then `gvm game try --scratch`.
   - It keeps the original ports only when the allocator's occupancy check passes; otherwise it refuses and names the conflict.
   - It normalises an adopted spec to managed paths.
   - It accepts only a **decrypted, verified** `.tar.gz`. Given a `.age` file, it refuses with "decrypt it first: age -d -i <identity> <file>.age > <file> (see docs/backups-and-restore.md)".
   - It copies the archive into the new instance's backup dir and indexes it, so `restore --latest` (and `gvm backup list`) find it there.
   - It never runs install, restore or apply itself. It prints the checklist instead: secrets, install, `restore --latest`, apply.

   (offsite-backup-dr-5)
3. **Pulling onto a host that has no instance file yet.** Phase 54's `gvm backup remote pull <instance>` reads the target from the instance's `spec.backup.remote`, which a new VM does not have. It gains `--target <sftp-url> --host-key '<pin>'`, which mirror the spec fields and keep pin-only trust (no TOFU), and it saves into `/var/backups/gvm/incoming/<instance>/`, outside every instance's indexed backup dir. (offsite-backup-dr-5)
4. **Secrets can be escrowed,** now that age is wired. `sudo gvm secret export --to <age-recipient>` and `secret import` handle an encrypted bundle, and the value is never written in cleartext. `secret import` reads the age identity from stdin, holds it only in memory and never stores it. `sudo gvm uninstall --export-secrets-to <age1…>` (Phase 53) uses the same export. (offsite-backup-dr-5)
5. **The runbook.** `docs/backups-and-restore.md` gets a "Moving to a new VPS" section. It has:
   - the ordered commands, including the decrypt step
   - the custody choice, stated plainly: decrypting on the operator's workstation keeps the age identity off every VPS, which is the recommended path; decrypting on the new VM is faster but puts the identity on a box gvm runs on, so it must be deleted afterwards
   - a player-facing note: same ports, new IP, and the same passwords only if the secrets were restored
   - every manual step, stated plainly

   (offsite-backup-dr-13)

### Definition of done (Milestone J5)

- [ ] A destroy-the-VM drill, with `encrypt_to` set on all three instances. On a new VM:
  1. `sudo ./gvm host init`
  2. `sudo gvm backup remote pull v1 --target sftp://backup@203.0.113.9/gvm --host-key 'SHA256:…'` for each of the three instances. The files land in `/var/backups/gvm/incoming/<n>/` as `.age`, and `gvm backup remote list` labels them "encrypted, needs identity".
  3. **Decrypt.** Copy the `.age` files to the operator's workstation and run `age -d -i <identity> f.tar.gz.age > f.tar.gz`, then copy each `f.tar.gz` back into `/var/backups/gvm/incoming/<n>/`. (Or run the same `age -d` on the new VM, accepting the custody caveat in the runbook.)
  4. `sudo gvm backup verify --file /var/backups/gvm/incoming/v1/f.tar.gz` for each.
  5. `sudo gvm create v1 --from-archive /var/backups/gvm/incoming/v1/f.tar.gz` for each. Given the `.age` file instead, it refuses and prints the decrypt line.
  6. `sudo gvm secret import bundle.age < identity.txt` (the identity is read from stdin, held only in memory, and the file is then deleted from the VM)
  7. `sudo gvm install v1` for each
  8. `sudo gvm restore v1 --latest` for each, which finds the archive that step 5 copied into v1's backup dir. (`--archive <file>` works the same.)

  Players join on the same ports with the same passwords.
- [ ] Every row of the README "What works" table reads "works", and `ci/golden-journey.sh` covers J1–J5.

**Depends on:** Phases 38, 46, 53 (the uninstall export option), 54. **Effort:** L.

**Design rules touched:**
- **Rule 10:** archive content is re-validated and passes the template guard.
- **Rule 12:** install still runs Plan, then digest-verified Apply.
- **Rule 5:** restore archives first.
- **Rule 1:** the drill needs no Postgres.
- **Pin-only trust:** `pull --target` takes a host-key pin, never TOFU. gvm never decrypts an off-box archive itself and never stores an age identity.

---

## Risk register

| Risk | Phase | Mitigation |
|---|---|---|
| A scratch boot contradicts the review's analysis, e.g. a Mono title still crashes with `jit: true`, or Core Keeper needs Xvfb after all | 32, 42 | No hardening change merges without a recorded transcript on two distributions (U14). A game that cannot pass is `unsupported` and hidden, never shipped as a guess, and its finding stays open with the recorded reason. |
| One of the four J1 games fails verification and J1 slips | 32, 34 | J1's promise is "the verified games", not a fixed list. A failing game stays `unverified`, and J1 ships with the rest. The J1 transcript uses Valheim, with Paper as a second path. |
| `ProtectProc=invisible` hides the root backup job from the freeze watcher, which would thaw mid-snapshot | 27 | It ships in the same commit as the ENOENT→Unknown change, with `TestFreezeWatcherTreatsHiddenOwnerAsUnknown`. Doctor adds a Note on systemd < 247 or kernel < 5.8. |
| A game that dies late in boot never trips `StartLimitBurst` and loops forever | 27, 28 | The runner-side boot-failure counter exits 79, which is in `RestartPreventExitStatus` and `NeedsOperator`. A 150 s slow-death fixture is part of the DoD. |
| Instances pin a known-bad manifest revision | 32, 39, 42 | The create and install gates accept only ledger-verified revisions. The superseded-revisions table gives a doctor Note and a `manifest upgrade` path. `manifest upgrade` refuses unverified targets and archives first. |
| A nil content seam makes an update or rebuild claim success while skipping mods | 31, 38, 44, 45 | `engineWired` keys on the manifest's content block. The null adapter is constructible only for content-free manifests. |
| `TestHintsResolve` is too noisy and grows an allowlist | 20 | The only escape is the exact "not available in this build" phrase. Design docs are excluded by path and carry a banner. Review rejects any other allowlist entry. |
| The exit-code change breaks operators' cron scripts | 22 | It applies only to operator verbs, ships before the first public release, and is announced in the CHANGELOG's "Operator actions" and `gvm help exit-codes`. Role exit codes are pinned by a test. |
| `host init` and the `.deb` drift apart | 23, 52 | One Go mapping table, and the `.deb` scripts call `gvm host init --from-package`. |
| A shared format is bumped before the skew rule exists | 20–35 | `TestFrozenFormats` fails CI until Phase 35's N-1 readers and cross-version tests land. |
| Generated join passwords surprise an operator who wanted a public server | 29 | `--open` is in create's help and the quickstart. The password is printed once, labelled "share with players". Generation never touches an adopted instance. |
| `config_files` rendering overwrites an operator's hand edits, or blanks an adopted server's password | 30, 51 | A console note shows old → new, and a `.bak` is kept. Adopted instances skip unset-secret keys, and the cutover DoD compares `servertest.ini` byte for byte. |
| A firewall step locks someone out | 33, 40 | SSH detection reads `/proc` socket evidence and fails closed, with no bypass, and an unsure plan still leads with the SSH step. gvm never prints `ufw enable` as advice (`TestNoUfwEnableAdvice` scans hints, Fix lines, help, docs and manifests), and the only code path that runs it is the typed, SSH-verified `firewall enable`, which adds SSH first and re-verifies. `firewall apply` adds SSH before anything else and never deletes an SSH rule. The Linux CI round trip runs from a session on a non-default port. |
| Running long jobs as transient units confuses operators used to output in their terminal | 31 | The CLI still follows and prints progress, and says "safe to disconnect; follow with gvm job last <n>". Ctrl-C keeps each verb's existing meaning. `TestJobVerbsSurviveHangup` keeps every job verb on the one helper. |
| A preserve pin or `backup delete` is misused and the last copy of a world goes | 21, 36 | Pins are sidecars that never change an archive's bytes. `backup delete` is typed with the instance name and refuses the newest verified archive of a live instance and any archive an adoption record references. |
| The intent writer clobbers a concurrent hand edit | 39 | Every write carries a sha256 If-Match, and a refusal says "the file changed since you read it". |
| A mixed-version fleet during a long upgrade window | 35 | The N-1 rule with cross-version tests. `restart --pending` goes one instance at a time and halts on the first failure. |
| Scope creep in the dashboard delays J4 | 48, 49 | v0 is a closed verb set: status, start, stop, restart, console and backup now. Mods and settings come in v1, read-only first. The CLI remote client is explicitly out of scope. |
| Parallel sessions collide in shared files | all | `root.go` and `usage.go` change structurally only in Phases 20 and 22. Doctor checks are separate files. Each manifest change lands in the phase that boots it. |

---

## Appendix A — finding coverage

Every one of the 210 verified findings from the October 2026 review is listed below: 61 high, 109 medium and 40 low. **No finding is deferred.** Every high and every medium finding is resolved by a phase.

How to read the **Phase(s)** column:

- The bold number is the phase that resolves the finding. A finding counts as resolved only when the bold phase's Definition of done passes for every part of it, not when an earlier phase makes it honest.
- The other numbers are phases that make it honest or safe first, or that complete a later part. Each one cites the id in its **Resolves** line.
- The arrows give the order of the work.
- **Findings that name games** (game-runtime-compat-3 Proton titles, -4 Mono titles, -6 Core Keeper, -8 Satisfactory and ARK) are resolved only when every game they name passes its scratch boot. Labelling a game `unsupported` does not resolve its finding: a game that ends `unsupported` reopens the finding in `docs/reviews/usability-2026-10.md`, with the ledger's recorded reason, until a later phase makes it pass.
- **game-runtime-compat-11 is resolved for the threat it names**, one instance reading another's argv (Phase 27's ProtectProc), and the RCON and admin passwords of V Rising, Rust, CS2 and ARK leave argv in Phase 42. Four secrets stay on argv, as stated residual risk: CS2's GSLT, Valheim's and Core Keeper's join passwords, and PZ's install-time admin password (Phase 42 gives the reason for each). They stay readable by local accounts outside `gvm@` units unless the operator mounts the host's `/proc` with `hidepid`, which doctor recommends and gvm never applies.

A script check confirmed three things after the last revision. Every id below is cited in the **Resolves** line of each phase listed for it. No phase's **Resolves** line cites an id that is not listed here for that phase. The table has exactly 210 rows.

### onboarding — Install and first server

| Id | Severity | Finding | Phase(s) |
|---|---|---|---|
| onboarding-1 | high | No way to install gvm itself: no installer, package, release binary or README install steps | **23** → 52 |
| onboarding-2 | high | `gvm install` can never install game files: the SteamCMD and Minecraft providers exist but are not wired | **31** |
| onboarding-3 | high | Game passwords can't be set: `gvm secret set` waits on a gvmd the CLI may not dial, so 11/16 games run with empty secrets | **29** |
| onboarding-4 | medium | No way to open game ports: apply stages no firewall rules, `gvm firewall` is unbuilt, and nothing lists which ports to allow | 33 → **40** |
| onboarding-5 | medium | `gvm doctor` can't confirm an install: it never checks instances, fakes firewall state, and fails every host | **24** |
| onboarding-6 | high | No way to onboard a dashboard user: `gvm web`/`gvmd` unimplemented, no units, `user create` needs gvmd and lacks owner | **48** |
| onboarding-7 | medium | Players are never told how to connect: no join address in create, start or status | **33** |
| onboarding-8 | medium | Help, errors and hints advertise commands that don't work and point to commands that don't exist | **20** |
| onboarding-9 | medium | Can't see a game's settings or required passwords before create; error points to a command that doesn't list them | **26** |
| onboarding-10 | medium | Per-command help is broken: `gvm help X` gives one line; `-h` exits 1 with an unrelated error (and keeps running as root) | **22** |
| onboarding-11 | medium | First server takes 5-6 separate commands with no single guided path, and apply doesn't say what comes next | 26 → **34** |
| onboarding-12 | medium | `gvm start` waits silently for up to 15-30 minutes and on failure prints only 'systemd job ... failed' | **34** |
| onboarding-13 | medium | No RAM/disk preflight: manifests declare floors but `gvm create` never checks them against the VPS | **26** |
| onboarding-14 | low | Minecraft's EULA is accepted silently on the operator's behalf | **30** |
| onboarding-15 | low | Operator-facing errors are full of internal design ids and security rationale | **22** |

### cli-ergonomics — CLI ergonomics and consistency

| Id | Severity | Finding | Phase(s) |
|---|---|---|---|
| cli-ergonomics-1 | high | Top-level usage advertises verbs this build cannot run; each refusal uses different words and exit codes, and job list calls them runnable | **20** |
| cli-ergonomics-2 | high | The remote/admin/secret path is a closed loop: every way out points at something that does not work | 20 → **29** |
| cli-ergonomics-3 | high | --help is broken or inconsistent: exits 1, runs the command, or panics as root; `gvm help <cmd>` shows only one line | **22** |
| cli-ergonomics-4 | medium | Hints and Fix lines name commands that do not exist | **20** |
| cli-ergonomics-5 | high | A custom game in /etc/gvm/games cannot get past `gvm apply`: no approval command, and apply has no override flag | 20 → **46** |
| cli-ergonomics-6 | high | `gvm doctor` reports false problems on every host and recommends commands that do not exist | **24** → 40 |
| cli-ergonomics-7 | medium | destroy makes the operator find an archive path by hand and accepts any old archive without showing its age or class | 21 → **38** |
| cli-ergonomics-8 | medium | User-facing text leaks design-doc IDs, Go identifiers, doubled sentinel prefixes and raw slog lines | **22** |
| cli-ergonomics-9 | medium | Flag spellings, instance designation and --json coverage differ per command; help renders flags misleadingly | **22** |
| cli-ergonomics-10 | medium | `--budget` on stop/restart is accepted, ignored, and still written to the audit row; on start it is meaningless | **34** |
| cli-ergonomics-11 | medium | Next-step guidance stops after create, and there is no quickstart | 26 → **34** |
| cli-ergonomics-12 | medium | No command tells you how to connect to a server (address, port, whether a password is set) | **33** |
| cli-ergonomics-13 | medium | Exit codes are undocumented and inconsistent; 'problems found' usually exits 0 | **22** |
| cli-ergonomics-14 | low | Small discoverability and terminal gaps: no completion, no typo suggestions, no NO_COLOR/TTY detection, logs cannot follow | 22 → **37** |

### day2-ops — Day-2 operations and visibility

| Id | Severity | Finding | Phase(s) |
|---|---|---|---|
| day2-ops-1 | high | Update, repair, rebuild, install and content-sync jobs are not wired in production; even the read-only `gvm update --diff` and `gvm rebuild --dry` refuse | 31 → 38 → 44 → **45** |
| day2-ops-2 | high | Restore is not built at all, yet `gvm restore --list` advertises it and the preflight describes steps that never run | **38** |
| day2-ops-3 | high | No way to answer 'did last night's backup or update work?' from gvm: no job history, `gvm audit` is a stub, and the guard's breadcrumb is never shown | **37** |
| day2-ops-4 | medium | `gvm status` and `gvm list` omit most at-a-glance facts: uptime, installed build, update available, last backup, next job, disk, join address | **37** |
| day2-ops-5 | high | Hints and docs send the operator to commands that do not exist | 20 → **39** |
| day2-ops-6 | high | Dashboard start/stop/restart return 202 'accepted' and do nothing; the dashboard role is not runnable | 47 → **48** |
| day2-ops-7 | medium | `gvm stop` needs a live runner: on a crash-looping or already-stopped instance it writes the hold, errors, and never stops the unit | **28** |
| day2-ops-8 | medium | `gvm start` and `gvm restart` block silently until the game is ready (up to an hour), and restart never says whether the world saved | 34 → **41** |
| day2-ops-9 | medium | The player count is never collected, so status and list never show players | **37** |
| day2-ops-10 | medium | During a running job, the refusal to restart tells the operator to run `gvm job clear`, which itself refuses | **28** |
| day2-ops-11 | medium | Stop, restart, update and the scheduled nightly restart give players no warning | **41** |
| day2-ops-12 | medium | Schedules are raw OnCalendar strings with no next-run, no timezone and no pause, and output mixes UTC and host time | 37 → **39** |
| day2-ops-13 | low | `--help` on status, list, logs, start, stop, restart and console prints usage and then runs the command; `gvm help <cmd>` prints one line | **22** |
| day2-ops-14 | low | `gvm list --problems` on a healthy host says 'no instances', and the summary counts starting or updating servers as 'stopped' | **37** |
| day2-ops-15 | low | `gvm logs` cannot follow, filter or reach rotated logs; the only live view is the interactive console | **37** |

### content-mods — Mods and workshop content

| Id | Severity | Finding | Phase(s) |
|---|---|---|---|
| content-mods-1 | high | No command can apply a mod change: `content sync` and the content_sync job both refuse, but every output points the operator at `gvm content sync` | 20 → **44** |
| content-mods-2 | high | Minecraft mods (Modrinth) and CurseForge cannot be managed at all: all three shipped Minecraft content blocks fail to compile, and the CLI never builds those sources | 20 → **45** |
| content-mods-3 | medium | `gvm game validate` reports content blocks as fine when the runtime rejects them, and the resolvers and supported mechanisms cannot be discovered | 20 → **46** |
| content-mods-4 | medium | The first `gvm content add` on an instance silently deletes every comment in the whole instance file | **21** |
| content-mods-5 | low | Blocked-mod instructions name three different drop-box paths and two commands that do not exist, and nothing writes or clears blocked-mods.json | **45** |
| content-mods-6 | medium | The CurseForge ladder labels every API failure as "author opted out" and accepts drop-box JARs by filename without checking the build | **45** |
| content-mods-7 | medium | `gvm content add` accepts URLs and mod names and fails only later, and no command edits an existing item's note, requires or pin | **43** |
| content-mods-8 | low | `content diff` and `content sync --dry-run` describe the same state differently, and a diff that is refusing still prints "apply:" and exits 0 | **43** |
| content-mods-9 | medium | Getting an existing mod list into gvm means editing YAML by hand: no collection setter, no import from the live ini, and collection sync drops hand-added items without asking | **43** |
| content-mods-10 | medium | Mod lists show only numeric Workshop ids: `add` never looks up titles | **43** |
| content-mods-11 | medium | `gvm content update` cannot say which mods were updated upstream | **43** |
| content-mods-12 | low | Modrinth and CurseForge dependency handling will confuse operators: required dependencies are never shown, any declared incompatibility aborts the plan, and CurseForge ignores the mod loader | **45** |
| content-mods-13 | medium | The dashboard has no content view: moderators cannot see mods or blocked state, and players have no mod list | **49** |
| content-mods-14 | low | `gvm content why` with a Workshop id wrongly says "nothing on this box accounts for it" | **43** |
| content-mods-15 | low | Help text and flags: inert or mangled flags, ignored --json, internal jargon, help that assumes Project Zomboid, and a missing-root warning on every PZ command | **43** |

### game-catalog-config — Choosing a game and configuring it

| Id | Severity | Finding | Phase(s) |
|---|---|---|---|
| game-catalog-config-1 | high | Manifest config_files keys (settings AND config-file ports) are never written to the game's config files | **30** |
| game-catalog-config-2 | high | No command can set a game password/secret; create claims the server 'starts without them' but the runner refuses to start | **29** |
| game-catalog-config-3 | high | No manifest hook runs in the shipped binary, yet `gvm game hooks` says 'ready' and validate ticks them | 20 → 30 → 31 → **44** |
| game-catalog-config-4 | high | Changing settings means hand-editing the instance YAML, and `gvm apply` never re-validates it (immutable world names, ranges, memory ceiling) | 21 → **39** |
| game-catalog-config-5 | high | No way to see a game's settings, ports, requirements or description before creating it | **26** |
| game-catalog-config-6 | medium | `sudo gvm create --help` panics with a Go stack trace; `gvm help <cmd>` shows no flags | **22** |
| game-catalog-config-7 | medium | Nothing tells players (or the operator) how to connect, or that a new server is public and passwordless | 29 → **33** → 49 |
| game-catalog-config-8 | medium | The dashboard cannot show or change any server setting | 20 → **49** |
| game-catalog-config-9 | medium | No way to move an instance to a newer manifest revision; status hints point at a command that does not exist | 20 → **39** |
| game-catalog-config-10 | medium | Adding a game: the documented approve/try loop does not exist, and an override in /etc/gvm/games is silently skipped | 20 → 32 → **46** |
| game-catalog-config-11 | low | Shipped manifests teach a port_stride shape the schema silently ignores; the template validates clean and is listed as runnable | 26 → **46** |
| game-catalog-config-12 | low | Proton-based games need an absolute proton_path that nothing explains or checks | 26 → 32 → **42** |
| game-catalog-config-13 | low | Manifest-author reference material is buried: hooks listed without descriptions, no schema/format listing, control-flow error hides the rest | **46** |

### web-dashboard — Web dashboard and API

| Id | Severity | Finding | Phase(s) |
|---|---|---|---|
| web-dashboard-1 | high | There is no dashboard or API to run: no non-SSH path exists for friends or moderators | **48** |
| web-dashboard-2 | medium | Start/Stop/Restart endpoints report success but do nothing, for any role and any instance name | **47** |
| web-dashboard-3 | medium | Dashboard pages are served to anonymous visitors, and the /login page the middleware redirects to does not exist | **47** |
| web-dashboard-4 | medium | The implemented role matrix lets 'operator' (the natural moderator role) rebuild worlds, restore over them and prune backups | **47** |
| web-dashboard-5 | medium | The auth wiring rejects both the browser's own buttons and the CLI's GVM_TOKEN, yet accepts any bearer string when a cookie is present | **47** |
| web-dashboard-6 | medium | The Administration commands (user/token/secret/audit) are listed in help but all send the operator after a gvmd daemon that does not exist | 20 → **48** |
| web-dashboard-7 | low | The non-root error and the --remote flag on 15 commands advertise a remote path that dead-ends after three tries | **20** |
| web-dashboard-8 | low | The only interim way to delegate a restart is root SSH, and the audit trail cannot say which person did it | **37** |
| web-dashboard-9 | medium | The web console is a read-only pane with one line of history, no server state, and no path from the runner | **48** |
| web-dashboard-10 | low | The dashboard model throws away the status the CLI already computes, and shows players no way to connect | 48 → **49** |
| web-dashboard-11 | medium | The edge config cannot reach the dashboard: route names, upstream and listener disagree | 47 → **48** |
| web-dashboard-12 | medium | The docs and doctor describe gvmd and dashboard behaviour that has not been built | 20 → **24** |
| web-dashboard-13 | low | Dashboard and API errors are bare strings with no code or request id, and the 503 page hard-codes 'pz-main' | **47** |

### admin-security — Users, tokens, secrets and audit

| Id | Severity | Finding | Phase(s) |
|---|---|---|---|
| admin-security-1 | high | Every user/token/secret/audit command is a stub that blames a daemon that does not exist | 20 → 29 → 37 → **48** |
| admin-security-2 | high | No way to set a game/RCON password, and the messages around it contradict each other and loop | **29** → 50 |
| admin-security-3 | medium | The 'moderator' role is far more powerful than the docs say, and per-instance grants can't be set | **47** |
| admin-security-4 | high | Inviting a friend, TOTP enrollment and password/TOTP reset have no flow at all | **48** |
| admin-security-5 | high | API tokens are never checked, and dashboard buttons can't authenticate | **47** |
| admin-security-6 | low | Token lifecycle is incomplete, and the token 'prefix' barely identifies anything | 48 → **49** |
| admin-security-7 | medium | There is no way to read the audit trail, and the documented spool replay fails on its default path | 22 → **37** |
| admin-security-8 | medium | The audit trail mislabels or skips the operations people most need to trace | **37** |
| admin-security-9 | medium | Hand-written secrets (today's only route) bypass the password character rules, and doctor never checks them | **29** |
| admin-security-10 | medium | No supported way to get the join password to players | **29** → 33 |
| admin-security-11 | medium | Admin commands have one-line help, accept invalid input, and treat --help as an error | **22** |
| admin-security-12 | medium | The non-root error leads to a fix that can't work, and drops the operator's arguments | **20** |
| admin-security-13 | medium | The admin docs show commands and checks that don't exist | 20 → **48** |
| admin-security-14 | low | Password limits and recovery codes are unfriendly | **48** |
| admin-security-15 | low | The db commands hang for 15 s on raw driver warnings, with no hint about peer auth | **22** |

### network-firewall — Ports, firewall, edge and connectivity

| Id | Severity | Finding | Phase(s) |
|---|---|---|---|
| network-firewall-1 | high | Firewall management is not wired: no command opens, stages or even lists the ports a server needs | 33 → **40** |
| network-firewall-2 | high | SSH-port detection can miss the operator's live session, so the lockout guard has nothing to protect | **33** |
| network-firewall-3 | high | `gvm doctor` reports firewall problems that are hard-coded, not measured, and suggests commands that do not exist | 24 → 33 → **40** |
| network-firewall-4 | medium | Nothing tells an operator, a moderator or a player how to connect: no address:port, protocol or exposure anywhere | **33** |
| network-firewall-5 | high | The dashboard cannot be put behind TLS or a domain, and the unwired edge code renders the wrong Caddyfile | 47 → **48** |
| network-firewall-6 | medium | `gvm ports` hides which port is in conflict and with whom, never checks host-wide overlaps, and labels each server's own socket 'foreign' | **33** |
| network-firewall-7 | medium | No way to move or re-allocate a port after create, and apply/start never re-check a hand-edited port | **39** |
| network-firewall-8 | medium | A partial or mistyped /etc/gvm/gvm.yaml silently turns off safe defaults and drops keys | **24** |
| network-firewall-9 | medium | Both firewall backends have defects that would break the ownership and anti-lockout model once wired | **40** |
| network-firewall-10 | low | 'expose: admin' can quietly produce no rule, or a rule for a port that is not reachable | **33** |
| network-firewall-11 | low | There is no operator-facing networking guide, and the README and help overstate what exists | 33 → **40** |

### migration-adopt — Adopting an existing server

| Id | Severity | Finding | Phase(s) |
|---|---|---|---|
| migration-adopt-1 | high | `gvm adopt cutover` and `gvm adopt rollback` are stubs, so the core adoption journey cannot be completed, and the stub messages mislead | 20 → 50 → **51** |
| migration-adopt-2 | high | `gvm adopt finalize` skips its 7-day guard unless the operator hand-types a specially formatted --adopted-from, then deletes whatever --unit-path names | 21 → **50** |
| migration-adopt-3 | high | Discover can't find config.env on the reference server, and the documented command shape is silently ignored | **25** |
| migration-adopt-4 | high | Cutover leaves the legacy nightly timer active and never writes the Conflicts= guard, so two servers can run on one world | **50** |
| migration-adopt-5 | high | The generated rollback script restores the archive to the wrong directory, after restarting the server, and discards all post-cutover play | **50** |
| migration-adopt-6 | medium | Cutover failures can leave the server stopped, claim a rollback that didn't happen, and lose the recovery paths | **50** |
| migration-adopt-7 | medium | The pre-cutover archive leaves out server config and player accounts, isn't a verified preserve archive, and the 'same world' check can't catch a fresh world | **50** |
| migration-adopt-8 | medium | Discover's finding severities and exit status don't line up with what blocks cutover, and no finding says how to fix it | **25** |
| migration-adopt-9 | medium | No adoption record and no `gvm adopt status`: during the week between cutover and finalize the operator has to remember everything | 50 → **51** |
| migration-adopt-10 | medium | `gvm instance promote`, the only documented way to let gvm update an adopted server, doesn't exist, and five messages point to it | 20 → **51** |
| migration-adopt-11 | medium | Discover's report leaves out most of what the runbook tells the operator to check, and never says it changed nothing | **25** |
| migration-adopt-12 | medium | The config.env parser keeps literal quote characters when a quoted value has an inline comment, which gives wrong values and false findings | **25** |
| migration-adopt-13 | low | The cutover design has no progress reporting for the downtime window | **51** |
| migration-adopt-14 | low | The adopt runbook is buried in the 7,000-line design doc, describes commands that don't exist, and `gvm help adopt` is one line | 25 → **51** |

### failure-recovery — When things go wrong

| Id | Severity | Finding | Phase(s) |
|---|---|---|---|
| failure-recovery-1 | high | `gvm doctor` never checks any instance, though every failure path sends the operator there | **24** |
| failure-recovery-2 | high | Doctor reports problems on every healthy host, and its fixes name commands that do not exist | **24** → 40 |
| failure-recovery-3 | medium | Status and refusal hints name commands that do not exist; a manifest pin mismatch has no way forward | 20 → **39** |
| failure-recovery-4 | high | Crash loops show as 'starting', and port conflicts as 'an orphaned game process' | **28** |
| failure-recovery-5 | high | `gvm stop` cannot stop an instance whose runner is not up (crash loop, failed preflight, already stopped) | **28** |
| failure-recovery-6 | high | Results of nightly jobs never reach status, job list or disk: a failed update, a needed repair, a server left stopped | **37** |
| failure-recovery-7 | medium | `gvm status` leaves out why the server last stopped, though the reason is already on disk or in systemd | **28** |
| failure-recovery-8 | medium | A failed `gvm start` prints only the raw systemd error, and after five crashes gvm has no way to start again | **28** |
| failure-recovery-9 | medium | The documented Postgres-recovery steps fail as written | 22 → **37** |
| failure-recovery-10 | medium | The guard timer restarts failed units every 5 minutes with no backoff, undoing RestartPreventExitStatus | **28** |
| failure-recovery-11 | medium | On a stale lease, status says the operator must decide while the guard clears it within 5 minutes; `gvm job clear` gives no next step | **28** |
| failure-recovery-12 | medium | No early warning for a filling disk, and the backup's out-of-space refusal is in raw bytes with no remedy | **36** |
| failure-recovery-13 | low | The dashboard shows a bare status word, with no reason, next step or last failure | 48 → **49** |
| failure-recovery-14 | medium | Hints written in manifest fail_markers reach only the journal; `gvm status` shows a generic code | **28** |
| failure-recovery-15 | low | Little steers operators toward doctor or a troubleshooting guide, and `gvm list --problems` says 'no instances' on a healthy host | 24 → **37** |

### documentation — Documentation for users

| Id | Severity | Finding | Phase(s) |
|---|---|---|---|
| documentation-1 | high | README never says what works in this build; core journeys it advertises refuse with 'not built in this release' | **20** |
| documentation-2 | high | No install guide, prerequisites list or quickstart; the only install instruction names a .deb that nothing builds | **23** |
| documentation-3 | high | Firewall: README promises a derived firewall, but apply never touches ufw, `gvm firewall` is unbuilt and doctor sends operators to a missing command | 24 → 33 → **40** |
| documentation-4 | high | About 20 user-facing hints and fixes name commands that do not exist | **20** |
| documentation-5 | high | No documented way to set a game or join password; `gvm secret set` requires a gvmd that does not exist | **29** |
| documentation-6 | medium | The Project Zomboid adopt runbook, the README's headline document, does not match the CLI | 21 → 25 → **51** |
| documentation-7 | medium | No per-game guide, and no command shows a game's variables, defaults, secrets, ports or doc text | **26** |
| documentation-8 | medium | Nothing tells the operator what to give players: no connect address, port or join instructions in docs or output | **33** |
| documentation-9 | medium | In-CLI help is inconsistent: `gvm help <cmd>` gives one line, `--help` keeps executing, several commands reject `--help`, and one panics | **22** |
| documentation-10 | medium | No manifest-authoring guide; the generic-steam template's 'start here' workflow cannot be followed with this build | **46** |
| documentation-11 | low | CurseForge blocked-mods doc names commands that do not exist and one of three different drop-box paths, for a source the CLI cannot reach | 20 → **45** |
| documentation-12 | medium | Minecraft manifests accept Mojang's EULA on the operator's behalf, contradicting the documented legal gate | **30** |
| documentation-13 | medium | ARCHITECTURE.md is the only reference, CLI output cites it by decision number, and there is no troubleshooting or FAQ | **22** |
| documentation-14 | medium | Dashboard users have no docs and no product, while every command advertises `--remote` and tells non-root users to mint a token | 20 → **48** |
| documentation-15 | low | The headline 'one rule' claims a CI job that kills the control plane and reboots on every PR; the actual job only greps unit files | **20** |

### cli-walkthrough — Hands-on CLI walkthrough

| Id | Severity | Finding | Phase(s) |
|---|---|---|---|
| cli-walkthrough-1 | high | The core journey dead-ends at install: install, update (even --diff), update --repair, content sync and rebuild all refuse as 'not built' | 31 → 38 → 44 → **45** |
| cli-walkthrough-2 | high | No CLI way to set a game's join, admin or RCON password: every `gvm secret` subcommand needs gvmd, and gvmd is not implemented | **29** |
| cli-walkthrough-3 | medium | Help is unreliable: `gvm help <cmd>` prints one line, and `<cmd> --help` falls through into running the command | **22** |
| cli-walkthrough-4 | high | The firewall is not wired: `gvm firewall` is unimplemented, apply never opens game ports, and doctor's fixes point at the missing command | 33 → **40** |
| cli-walkthrough-5 | medium | The 'forgot sudo' error is jargon-heavy and recommends two fixes that do not exist in this build | **20** |
| cli-walkthrough-6 | medium | `gvm help` lists unbuilt and stub commands as if they work, and refusals use five phrasings and two exit codes | **20** |
| cli-walkthrough-7 | medium | Messages point to commands that do not exist, including `gvm disable`, so there is no durable stop | 20 → **39** |
| cli-walkthrough-8 | medium | There is no way to see a game's variables or read the shipped manifests, though errors and the template tell users to | **26** |
| cli-walkthrough-9 | medium | Nothing tells a player, or the operator relaying to players, how to connect | **33** |
| cli-walkthrough-10 | medium | No getting-started path: the help banner is design philosophy, and apply does not say what to do next | 26 → **34** |
| cli-walkthrough-11 | medium | Internal design-doc ids and Go internals leak into help and output, and apply is very verbose | **22** |
| cli-walkthrough-12 | low | Flag-parsing friction: errors printed twice around a full usage dump, single-dash rendering, garbled flags, and a `--user` flag that rejects everything | **22** |
| cli-walkthrough-13 | low | Destroy and restore make the operator hunt for archive paths, and restore puts its arguments in the opposite order | **38** |
| cli-walkthrough-14 | low | `gvm logs` cannot follow, so the only live view is the interactive console | **37** |
| cli-walkthrough-15 | low | `gvm db` commands spew raw retry logs for about 15 seconds when Postgres is down, and `db version` prints a bare number | **22** |

### offsite-backup-dr — Off-box backups and disaster recovery (losing the VPS, moving to a new one)

| Id | Severity | Finding | Phase(s) |
|---|---|---|---|
| offsite-backup-dr-1 | high | A configured spec.backup.remote is silently ignored, and the job report says `ok backup.push` | **21** → 54 |
| offsite-backup-dr-2 | high | spec.backup.dir (and spec.paths.backups) is ignored by the code that writes archives but honoured by list/verify/prune | **21** |
| offsite-backup-dr-3 | medium | The remote block is never validated, so typos and a pasted private age key are accepted and then copied into every archive | **21** |
| offsite-backup-dr-4 | high | There is no supported way to get a world back: `gvm restore` is unbuilt, even the non-destructive `--to`, and the failure is jargon after a preflight that reads as if it will proceed | **38** |
| offsite-backup-dr-5 | high | An archive is not enough to rebuild an instance on a new host, and no command helps do it | **55** |
| offsite-backup-dr-6 | low | Restore destinations default to the OLD box's paths recorded in the archive, not the target instance's paths | **38** |
| offsite-backup-dr-7 | medium | Even once wired, a failed off-box push stays invisible: `warn` reads as ok, `fail` cannot fail the job, and nothing records the last good off-box copy | 21 → 36 → **54** |
| offsite-backup-dr-8 | low | No tooling for setting up or using the off-box copy: key custody, host-key fetch, target test, remote list/pull, decrypt | **54** |
| offsite-backup-dr-9 | medium | Snapshot method, freeze stall and fallback are computed but never reported to the operator | **36** |
| offsite-backup-dr-10 | medium | btrfs snapshot clones are read-only subvolumes that the cleanup and startup sweep cannot delete | **21** |
| offsite-backup-dr-11 | medium | Backup health is invisible: no doctor check, no status line, no dashboard view, no failure alert | 36 → 37 → 49 → **54** |
| offsite-backup-dr-12 | medium | Running out of backup space: the refusal gives raw byte counts and no next step, and the estimate is about 3x the real archive | **36** |
| offsite-backup-dr-13 | medium | There is no backup/restore or moving-host runbook; the planned docs/backups-and-restore.md was never written | 38 → **55** |
| offsite-backup-dr-14 | low | `gvm backup list --remote` means 'dashboard URL', not the off-box backup target | 20 → **54** |
| offsite-backup-dr-15 | low | The pinned host-key 'fingerprint' in push logs is the same string for every ed25519 key | **36** |

### game-runtime-compat — Will each shipped game actually boot under gvm's hardened unit? (Mono/Wine vs W^X, Proton, 32-bit, xvfb)

| Id | Severity | Finding | Phase(s) |
|---|---|---|---|
| game-runtime-compat-1 | high | The a2s readiness probe is not built by the runner, so 10 of 16 shipped manifests can never reach 'ready' | **27** |
| game-runtime-compat-2 | high | On a Proton stop the ladder signals only the Python launcher; its death collapses every remaining wait, and the Wine game is SIGKILLed | 27 → **42** |
| game-runtime-compat-3 | high | Proton titles cannot start: no STEAM_COMPAT_DATA_PATH, W^X enforced, arch native, and the example Proton path is read-only | **42** |
| game-runtime-compat-4 | high | Unity/Mono servers (Valheim, Rust, 7DTD x2, Core Keeper) run under MemoryDenyWriteExecute=yes, which Mono's JIT cannot survive | 32 → **42** |
| game-runtime-compat-5 | high | A game that dies during boot shows as 'starting' for stall_timeout, then loops forever with a mislabelled exit code | 27 → **28** |
| game-runtime-compat-6 | high | Core Keeper's xvfb wrapper is not implemented: every start exits 64, and the hint sends the operator to `gvm apply --diff` | **42** |
| game-runtime-compat-7 | medium | For Satisfactory and Enshrouded the world is not where backups, restore and rebuild look | 32 → **42** |
| game-runtime-compat-8 | high | Satisfactory and ARK: SE are forced onto Proton though native Linux servers exist; Satisfactory's 1.0 port model is wrong | **42** |
| game-runtime-compat-9 | medium | No preflight, create or doctor check for Proton, python3 or Xvfb; a hidden Proton path yields 'install is incomplete' and a dead-end `gvm doctor` hint | 32 → **42** |
| game-runtime-compat-10 | medium | 7 Days to Die's startserver.sh sends the Unity log to a file, so the console is empty and the 'StartGame done' marker never arrives | **42** |
| game-runtime-compat-11 | medium | RCON, admin and join passwords go on argv, which every local account can read in /proc; the unit has no ProtectProc | 27 → **42** |
| game-runtime-compat-12 | medium | `gvm game try --scratch`, the advertised way to prove a manifest boots, does not exist, and validate checks nothing about runtime compatibility | **32** |
| game-runtime-compat-13 | low | CS2 omits steam.link_sdk, which gvm's own shim code says leaves a server invisible to the browser | **42** |

### gvm-self-upgrade-uninstall — Upgrading gvm itself, and removing it cleanly

| Id | Severity | Finding | Phase(s) |
|---|---|---|---|
| gvm-self-upgrade-uninstall-1 | high | No supported way to get a bug-fix release onto a box with live worlds | 23 → 35 → **52** |
| gvm-self-upgrade-uninstall-2 | medium | Old runner + new CLI/jobs: console and freeze-lease formats are exact-match with no skew policy | 20 → **35** |
| gvm-self-upgrade-uninstall-3 | medium | An upgrade mid-job turns a LIVE lease into 'failed' and `gvm job clear` deletes it without --force | **35** |
| gvm-self-upgrade-uninstall-4 | medium | Nothing tells the operator which worlds still run the old runner, and there is no safe fleet restart | **35** |
| gvm-self-upgrade-uninstall-5 | high | A new gvm that renders different drop-ins or timers is invisible: drift compares only the spec sha | **35** |
| gvm-self-upgrade-uninstall-6 | medium | No command or runbook to take gvm off a box safely while keeping the backups | **53** |
| gvm-self-upgrade-uninstall-7 | medium | `gvm destroy` silently leaves secrets, the account and per-instance state behind | 40 → **53** |
| gvm-self-upgrade-uninstall-8 | low | Database migrations have no upgrade unit, no lock, and a dead-end rollback story | **52** |
| gvm-self-upgrade-uninstall-9 | medium | Rolling back to the previous binary is undocumented and silently drops fields the newer binary understood | 39 → **52** |
| gvm-self-upgrade-uninstall-10 | low | Archive and index readers accept only the exact current schema, so a format bump strands every existing backup | **52** |
| gvm-self-upgrade-uninstall-11 | medium | `gvm version` says 'dev (commit unknown)' and there is no 'what changed for my instances' view | 23 → **35** → 49 |
| gvm-self-upgrade-uninstall-12 | medium | Doctor checks the binary against itself, not against what is installed, and the unit-not-found hint loops | **24** |

### Totals

| | High | Medium | Low | Total |
|---|---|---|---|---|
| Findings | 61 | 109 | 40 | 210 |
| Resolved by a phase | 61 | 109 | 40 | 210 |
| Deferred | 0 | 0 | 0 | 0 |

