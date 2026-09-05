# gameVPSmanager (`gvm`)

Turns a Debian/Ubuntu VPS into a host for **several game servers at once** —
Steam dedicated servers first, modded Minecraft after — with a CLI, a web
dashboard, a firewall derived from what is actually running, and backups that
do not corrupt worlds.

It generalizes a single-game Project Zomboid installer, which lives in
[`legacy/projectzomboid-bash/`](legacy/) as the behavioural specification.

## The one rule

> Kill `gvmd`. Kill the dashboard. Kill Postgres. Reboot the box.
> **Every game server comes back up**, keeps its nightly update timer, keeps
> its firewall rules, and keeps taking backups.

The filesystem is the control plane, systemd is the supervisor, and Postgres
is the logbook — authoritative for *people* (users, roles, sessions, audit),
a mirror for machines. `sudo gvm stop pz-main` works with the database down.
CI asserts this on every change.

## Status

Early. The architecture is settled and the foundation is being built
bottom-up; see [`docs/IMPLEMENTATION-PLAN.md`](docs/IMPLEMENTATION-PLAN.md)
for the phase order and what each milestone makes possible.

| Milestone | What works |
| --- | --- |
| **A** | `create` + `apply` + `start` runs a fresh server with a real console and a clean shutdown |
| **B** | SteamCMD install, workshop mods, nightly backup, update, archive-before-destroy rebuild |
| **C** | **The live Project Zomboid server is adopted**, with rollback proven |
| **D** | Dashboard, RBAC, audit, API, Caddy, firewall |

Commands that are routed but not yet built exit non-zero with
`not implemented`, so a script can never mistake an unbuilt phase for success.

## Documentation

| Document | Contents |
| --- | --- |
| [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) | The full spec: process topology, manifest schema, instance model, control channel, SQL schema, RBAC, security model |
| [`docs/ARCHITECTURE.md` §19](docs/ARCHITECTURE.md) | **The Project Zomboid adopt runbook** — importing the live server without touching the world |
| [`docs/IMPLEMENTATION-PLAN.md`](docs/IMPLEMENTATION-PLAN.md) | 20 dependency-ordered phases, each with a definition of done |
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
