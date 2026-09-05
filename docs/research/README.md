# docs/research/

Primary-source research produced during the architecture phase, kept verbatim
so implementation decisions can be traced back to the facts that drove them.

| File | Covers |
| --- | --- |
| `steamcmd.md` | SteamCMD install/auth/update semantics, workshop acquisition, runtime shims, and a per-game matrix (app ids, ports, RCON support, RAM floors) |
| `minecraft.md` | Vanilla/Paper/Fabric/Forge/NeoForge acquisition APIs, launch mechanics, Java version matrix, RCON, plugin/mod sources |
| `curseforge.md` | CurseForge Eternal API, modpack `manifest.json`, server packs, the undistributable-file problem, Modrinth `.mrpack` comparison |
| `supervision-rcon.md` | systemd vs in-process supervision, console I/O, cgroup limits, and the Source RCON protocol with per-game deviations |
| `caddy.md` | Caddy v2 admin API vs Caddyfile, automatic HTTPS, the no-domain case, SSE/WebSocket passthrough, hardening |
| `datastore.md` | pgx/Postgres, migrations, argon2id, sessions/CSRF, the RBAC model, secrets at rest, audit logging |

These are **background, not instructions**. They were correct when gathered;
anything version-dependent should be re-verified before relying on it.

Behaviour that has already been encoded in Go carries the relevant fact in a
comment at the point of use, so the code stands on its own — see
`internal/rcon/dialect.go`, which exists entirely because of the per-game
terminator differences documented in `supervision-rcon.md`.
