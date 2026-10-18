# gameVPSmanager (`gvm`)

Turns a Debian/Ubuntu VPS into a host for **several game servers at once** —
Steam dedicated servers first, modded Minecraft after — with a CLI, a planned
web dashboard, a planned firewall derived from what is actually running, and
backups that do not corrupt worlds. See **What works in this build** below for
what the current release does today.

It generalizes a single-game Project Zomboid installer, which lives in
[`legacy/projectzomboid-bash/`](legacy/) as the behavioural specification.

## The one rule

> Kill `gvmd`. Kill the dashboard. Kill Postgres. Reboot the box.
> **Every game server comes back up**, keeps its nightly update timer, keeps
> its firewall rules, and keeps taking backups.

The filesystem is the control plane, systemd is the supervisor, and Postgres
is the logbook — authoritative for *people* (users, roles, sessions, audit),
a mirror for machines. `sudo gvm stop pz-main` works with the database down.
CI asserts that no game or job unit depends on Postgres, gvmd or gvm-web.

## What works in this build

gvm is built bottom-up. This release delivers the command surface and honest
messaging; the engines that install game files, open the firewall and serve
the dashboard arrive in later phases of
[`docs/USABILITY-PLAN.md`](docs/USABILITY-PLAN.md). Every command below works
over SSH as root with gvmd, the dashboard and Postgres all down. Anything not
built refuses with exit code 3 and the line `not available in this build;
nothing was changed.`, so a script can never mistake an unbuilt feature for
success.

| Command | This build |
| --- | --- |
| `gvm create` · `apply` · `start` · `stop` · `restart` | **works** — define and run an instance |
| `gvm status` · `list` · `logs` · `ports` · `console` · `control` | **works** — observe and drive a running server |
| `gvm backup` · `backup verify` · `backup prune` | **works** — archive, verify, prune |
| `gvm game` · `doctor` · `db` | **works** |
| `gvm content list/diff/why/add/remove/move/lock` | **works** — edit the declared mod list |
| `gvm content sync` | **partial** — `--dry-run` decides; applying mod changes is not in this build |
| `gvm restore` | **partial** — `--list` works; restoring a world is not in this build |
| `gvm adopt discover` · `adopt finalize` | **works**; `adopt cutover` and `rollback` are not in this build |
| `gvm user` · `token` | **works** — accounts (password + TOTP), single-use invites with self-enrolment, grants, and API tokens |
| `gvm gvmd` · `gvm web` | **works** — the privileged verb gateway and the dashboard service (installed by `host init --dashboard`) |
| `gvm edge plan` · `edge apply` · `ca export` | **works** — render, validate, swap and reload the host's own Caddy config, with rollback |
| `gvm install` · `update` · `rebuild` | **not built** — installing, updating and rebuilding game files land later |
| `gvm firewall` | **not built** — gvm does not manage the firewall in this build |
| `gvm secret` · `audit` | **not built** — the secret store and audit reader land later |

**Not available in this build:** `audit`, `firewall`, `install`, `rebuild`, `secret`, `update`.

(That list is checked against the command registry by a test, so this table
cannot drift from what the binary actually refuses.)

A journey counts as done only when its whole row works end to end; see
[`docs/USABILITY-PLAN.md`](docs/USABILITY-PLAN.md) for the order — J1 (first
joinable server) through J5 (adopt, package, recover). The earlier
milestones A–D in [`docs/IMPLEMENTATION-PLAN.md`](docs/IMPLEMENTATION-PLAN.md)
are the code-level foundation those journeys build on.

## Documentation

| Document | Contents |
| --- | --- |
| [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) | The full **target design**: process topology, manifest schema, instance model, control channel, SQL schema, RBAC, security model. Describes the finished system, not this build — see the table above for what works today. |
| [`docs/ARCHITECTURE.md` §19](docs/ARCHITECTURE.md) | **The Project Zomboid adopt runbook** — importing the live server without touching the world |
| [`docs/IMPLEMENTATION-PLAN.md`](docs/IMPLEMENTATION-PLAN.md) | Phases 0–19 (**target design**): the dependency-ordered foundation, each with a definition of done |
| [`docs/USABILITY-PLAN.md`](docs/USABILITY-PLAN.md) | Phases 20–55: the operator-journey plan (J1–J5) that turns the foundation into a usable product |
| [`docs/admin-guide.md`](docs/admin-guide.md) | Standing up the dashboard: Postgres, `host init --dashboard`, accounts, tokens |
| [`docs/dashboard.md`](docs/dashboard.md) | The dashboard for moderators: signing in, roles, what the buttons do |
| [`docs/deploy-behind-servergitupdater.md`](docs/deploy-behind-servergitupdater.md) | Serving the dashboard behind a foreign proxy, e.g. a [serverGitUpdater](https://github.com/S95F/serverGitUpdater)-managed Caddy |
| [`docs/research/`](docs/research/) | Primary-source research behind the decisions |

## Building

```bash
make build      # cross-compiles linux/amd64, static (CGO_ENABLED=0)
make check      # gofmt + vet + tests with the race detector
make help       # every target
```

Development is on Windows and deployment is Linux-only, so `make check` stays
green on both. Anything that cannot is behind a build tag with a stub that
returns `platform.ErrNotSupported`, and the portable half is still tested.

## Design notes

Two behaviours the legacy scripts rely on that `gvm` deliberately does **not**
inherit, both documented in `ARCHITECTURE.md` §1.2:

- `ExecStop=screen -X stuff 'quit\015'` returns immediately instead of waiting.
  Worlds survive today only because `KillSignal=SIGCONT` neuters the terminate
  step — but that makes "saved and exited at 40s" indistinguishable from "hung
  and was SIGKILLed at 180s". `gvm` has no `ExecStop` at all; its runner is the
  main process and blocks until the game exits.
- `start-server.sh` launches the JVM in a subshell without `exec`, so the
  unit's `MainPID` is the shell and signalling it never reaches Java. `gvm`
  signals the process group, with identity pinned as `(pid, starttime)` so PID
  reuse cannot fool it.

Everything else the bash suite learned the hard way is carried over verbatim
in behaviour — see §1.1, which maps all sixteen lessons to their new homes.
