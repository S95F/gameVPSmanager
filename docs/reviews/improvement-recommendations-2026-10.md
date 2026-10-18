# Improvement recommendations — 2026-10-17

This review steps back from the per-finding detail of
[`usability-2026-10.md`](usability-2026-10.md) and
[`USABILITY-PLAN.md`](../USABILITY-PLAN.md). Those documents already say *what* to build.
This one is about *how* the work proceeds: ordering, verification, the build pipeline
and the docs. It was written against `main` at `c2e77a4`. Items are ordered by how much
each one would change, and the cost is noted for each.

---

## 1. Finish J1 before opening another phase (strategy, highest leverage)

**What we see.** The plan's own status review counts 2 phases complete, 32 partial and
2 not started. Work has gone breadth-first. Each phase lands its testable library
slice, while the wiring that a real operator would notice waits on a later phase. The
result is the "built but not wired" pattern the October review named. It is still the
dominant shape of the tree, because `install`, `update`, `firewall` and `secret`
refuse, and so do the PZ/Minecraft hooks.

**Recommendation.**

- Put a **WIP limit** on the plan: at most two phases open at a time, and a phase is
  closed only when its Definition of done passes on a real host.
- Take the **J1 critical path** strictly in order:
  29 (local `gvm secret`) → 30 (`config_files` and hooks in `supervise()`) →
  31 (SteamCMD and Minecraft providers in `HostEnv`) → 33 (join line, read-only
  `firewall plan`) → 34 (milestone). Hold every other phase until J1 passes, except for
  a safety fix.
- Pick **one game** for the first end-to-end run. Valheim is a good choice: it is
  SteamCMD-native, needs one password, and has an a2s probe. Do not try to cover all
  16 manifests at this stage.

**Why.** Every partial phase adds library surface that has to stay green but gives no
user anything yet. J1 is the first point where gvm replaces the legacy bash installer,
and it is the precondition for the backlog's "7 days on the live PZ server" goal.

**Cost.** None. This is a change in ordering, not new work.

## 2. Test on a real systemd host in CI

**What we see.** Every CI job runs `go test` against fake `HostEnv`s, plus grep gates.
Nothing in CI starts a real `gvm@.service`, runs a real ladder or reboots a box. Yet
the project's one rule ("kill everything, reboot, every game comes back") is a claim
about real systemd behaviour. `ci/control-plane-dead.sh` checks unit *dependencies*,
not survival.

**Recommendation.**

- Add a `journey` job on `ubuntu-latest`. GitHub's Ubuntu runners have systemd and
  passwordless `sudo`. The job builds `gvm`, runs `sudo gvm host init`, then
  `create`/`apply`/`start` on a **fixture game**: a tiny Go or shell binary that answers
  a2s and handles the save command. It asserts that `status` reaches *ready*, then
  `stop`, then that `backup` + `backup verify` succeed.
- Next, add a `systemctl kill gvmd gvm-web postgresql` step and assert the game unit
  is still active. That is the real form of `control-plane-dead`.
- A reboot test needs a VM. Run it nightly or on release tags, using a cheap VPS or a
  QEMU job, as `ci/golden-journey.sh` already plans.

**Why.** Every phase's Definition of done that says "needs a live VM" is unverified
today. The status review lists claims it could not check for exactly this reason. The
fixture game also unblocks Phase 32's boot ledger.

**Cost.** M. The fixture game is the main piece of work.

## 3. Replace the grep gates with AST checks

**What we see.** Three gates in `.github/workflows/ci.yml` are line-oriented regexes,
and each can be bypassed without anyone noticing:

- **`no-shell-in-gvm`** matches only `exec.Command(` followed by a literal `"sh"` or
  `"bash"` on the same line. All 14 production `exec.Command`/`CommandContext` call
  sites pass the program as a variable, so the gate cannot see any of them.
  `exec.CommandContext(ctx, shell, "-c", …)` would pass.
- **`no-kill-9-in-gvm`** excludes every line whose path contains `adopt` or
  `rollback`, and it misses `unix.SIGKILL`, `syscall.Kill(pid, 9)` and
  `Signal(syscall.Signal(9))`.
- **`no-unauthenticated-api`** looks only for `HandleFunc` with a literal `api/v1`.
  `mux.Handle(...)` and route tables built in a loop both pass.

**Recommendation.** Rewrite them as Go tests that walk the AST with `go/packages` and
`go/ast`, in the same style as `TestHintsResolve` and the other sentinel tests.

- **No shell:** resolve the first argument of every `os/exec` call. Fail on a constant
  that names a shell, and require a reviewed allowlist entry for any non-constant
  program.
- **No SIGKILL:** flag every reference to `SIGKILL` or the constant 9 passed to a
  signal API. Allow only an explicit allowlist of functions, not path substrings.
- **Authenticated API:** enumerate the routes actually registered on the built mux. The
  route table test that exists may already do most of this, in which case delete the
  grep.

**Cost.** S.

## 4. Dependency and supply-chain hygiene

**What we see.**

- `golang.org/x/crypto` is pinned at **v0.38.0** while `x/sys` is at v0.49.0.
  x/crypto backs the argon2id passwords and the pin-only SSH host-key callback, which
  are both security boundaries.
- No `dependabot.yml`, no `govulncheck`, no `staticcheck`.
- Actions are pinned by tag (`actions/checkout@v4`), not by commit SHA.
- `go.mod` has a stray single-line `require gopkg.in/yaml.v3` block. Tidy it.

**Recommendation.**

- Bump x/crypto, x/net and x/text to current versions, then run `go mod tidy`.
- Add `govulncheck ./...` as a CI step. It is quick, and it fails only on vulnerable
  code paths that are actually reachable.
- Add `staticcheck` (or `golangci-lint` with a short list of linters: staticcheck,
  errcheck, gosec at a low setting, bodyclose).
- Add `.github/dependabot.yml` for `gomod` and `github-actions` on a weekly schedule,
  and pin actions to SHAs.

**Cost.** S. Under an hour, apart from any findings staticcheck raises.

## 5. Cut a release, even a pre-J1 one

**What we see.** The repo has no tags. `VERSION` falls back to a short hash, and
Phase 35 (upgrade gvm without stopping worlds) and Phase 52 (.deb) both reason about
versions that do not exist yet.

**Recommendation.**

- Tag `v0.1.0` now, and add a `release.yml` that runs on `v*` tags. It should build
  linux/amd64 and linux/arm64, attach the binaries with `SHA256SUMS`, and record
  `go version -m` output.
- Keep a short `CHANGELOG.md` with one line per merged PR. It doubles as the source
  for the "What works" table history.

**Why.** Phase 35's N-1 skew rule and `TestFrozenFormats` need real released versions
to test against. A pinned binary is also what goes onto the live PZ host.

**Cost.** S.

## 6. Give the dashboard a security pass before anyone else is invited

**What we see.** Phase 48 linked the web tier into the shipped binary: login, TOTP,
invites and enrolment, the live console over SSE, and console send. The design is
careful. It uses `CrossOriginProtection` in place of per-form tokens, a 250 ms login
floor, per-IP lockout, and TOTP checked in gvmd. However, it has had only the reviews
that came with its own phases.

**Recommendation.** Before a second person gets an invite, run a dedicated review of
`internal/web`, `internal/auth`, `internal/authz`, `internal/webstore` and the gvmd
verb boundary (for example `/security-review`). Focus on:

- SSE authorisation on reconnect, and session revocation for open streams
- that the invite token is single-use under concurrent redemption
- that console send cannot inject control bytes. Phase 41 notes that C1
  (U+0080–U+009F) is not refused.
- `--dev` cookie mode being impossible to reach behind `edge.listen`

Add a small browser smoke test (Playwright on the runner, phone and desktop viewports)
for login → instance list → console. The mobile layout that just landed has no
rendered-page test.

**Cost.** S–M.

## 7. Make the plan cheaper to keep true

**What we see.** `ARCHITECTURE.md` (7,150 lines) and `USABILITY-PLAN.md` (4,412 lines)
are both hand-maintained. On its first pass, the status review found **9 of 36
phases** whose status text no longer matched the code. The README opening still calls
the dashboard "planned", although `gvm web` ships and works.

**Recommendation.**

- Fix the README intro. It is a one-line change.
- Work through the 9 "⚠ Needs review" rows as a single docs and small-code PR. Most are
  one-line fixes:
  - `create.go:278` should point at `gvm game show`
  - remove `--budget` from the job verbs (`cli/job.go:140`)
  - remove `--no-pre-archive` or refuse it (`restore.go:99`)
  - correct the `gvm help proton` topic
  - the CurseForge drop-box path in `docs/curseforge-blocked-mods.md`
  - the `mc.accept_eula` gate in `hook/mc.go:39`. This one is a real safety item, so fix
    it before Phase 30 wires the hooks.
- Apply the README's existing pattern to the plan: generate each phase's ✅/🟡/⏸️ marker
  from a small table in code (phase → required symbols or tests), and have a test
  check it. Narrative stays hand-written; status stops being prose.
- When a phase completes, move its body to `docs/completed/`, as was done for Phases
  0–19. The live plan should only ever hold open work.

**Cost.** S for the fixes, M for generated status.

## 8. Lower-priority code health

- **Large files.** `cli/content.go` (1,682 lines), `runner/shutdown.go`,
  `spec/validate.go`, `backup/engine.go` and `provider/steamcmd/steamcmd.go` are each
  over 1,200 lines. Split them only while working in them for a phase, never as a
  standalone refactor.
- **Coverage visibility.** Add `make cover` to CI as an uploaded artifact, with no
  threshold. The goal is to see which wired paths are reached only by fakes.
- **Shellcheck on legacy** runs with `|| true`. That is fine while the tree is a
  reference spec. Delete the job once Phase 51 adopts the live server and the bash
  tree is frozen.

---

## Suggested order

| # | Item | Effort | Blocks |
|---|---|---|---|
| 1 | Dependency bump + govulncheck + staticcheck + dependabot (§4) | S | — |
| 2 | README intro + the 9 status-review fixes, `mc.accept_eula` first (§7) | S | Phase 30 |
| 3 | AST gates replace grep gates (§3) | S | — |
| 4 | Tag `v0.1.0` + release workflow (§5) | S | Phases 35, 52 |
| 5 | Fixture game + `journey` CI job on a systemd runner (§2) | M | every live-VM DoD |
| 6 | J1 critical path, WIP limit of 2 (§1) | L | J2–J5, PZ adoption |
| 7 | Dashboard security pass + browser smoke test (§6) | S–M | inviting friends |

Items 1–4 are independent of each other and can run in parallel. Item 5 should land
before Phase 31 is called done.
