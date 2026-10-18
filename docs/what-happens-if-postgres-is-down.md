# What happens if Postgres is down?

**Short answer: your game servers keep running, completely unaffected.**

This document lists every gvm subsystem and what it does when the database is unavailable.

---

## Game servers (the thing that matters most)

| Condition | Effect |
|---|---|
| Postgres goes down while a server is running | **Nothing.** The game process is supervised by `systemd`, not by gvmd. |
| Reboot while Postgres is down | The game unit starts normally. `gvm@<n>.service` is enabled; it reads the instance YAML and `LoadCredential` files — no database needed. |
| Nightly update / backup timers | Fire normally. `gvm job run` logs a warning and spools its audit row; the job continues. |

**No game unit carries `Requires=postgresql.service`, `Wants=postgresql.service`, or `After=postgresql.service`.**
The CI gate `ci/control-plane-dead.sh` enforces this.

---

## gvm CLI (all verbs)

| Verb | PG down | Mechanism |
|---|---|---|
| `sudo gvm status` | **works** | reads `/etc/gvm/instances/` + systemd |
| `sudo gvm console <name>` | **works** | unix socket to the runner |
| `sudo gvm stop <name>` | **works** | hostops in-process |
| `sudo gvm start <name>` | **works** | hostops in-process |
| `sudo gvm apply` | **works** | writes files, reconciles systemd/ufw/Caddy; reports `mirror: degraded` |
| `sudo gvm restore --list` | **works** | reads tarball manifests + `archives.index.json` |
| `sudo gvm doctor` | **works**, reports PG down first | every other check is file-derived |
| `sudo gvm backup` | **works** | files-only; audit row spooled |
| `gvm db migrate` | **needs PG** | that is its purpose |
| gvmd verbs via socket | `ErrDatastoreUnavailable` | typed error, not a crash |

The audit logger always writes to the on-disk spool first (`/var/lib/gvm/spool/`). Rows are replayed
into Postgres when it comes back, via `gvm db replay-spool`.

---

## gvmd daemon

> **In this release** (Phase 48): gvmd serves its closed verb set from `gvmd.service`; the behaviour below holds.

gvmd starts and listens on its unix socket regardless of Postgres state.
Verbs that need the database return `ErrDatastoreUnavailable` (exit 3).
Verbs that do not need the database (start, stop, console) work normally.

A background goroutine probes Postgres with exponential backoff (1 s → 60 s, ±20% jitter).
When it reconnects, `store.Open`'s `Ready()` channel closes and normal operation resumes.

---

## Web dashboard

> **In this release** (Phase 48): the dashboard runs from `gvm-web.service`; the behaviour below holds.

| State | Response |
|---|---|
| Postgres down | **HTTP 503** with the diagnostic page below |
| Postgres up, session cookie valid | normal dashboard |
| Postgres up, no session | redirect to login |

**There is no unauthenticated read-only mode.** An authorization system that fails open when
its datastore is down is worse than one that is simply unavailable.

The 503 page says:

```
gvm dashboard unavailable — the datastore is down.

Your game servers are NOT affected. They are supervised by systemd and do not
depend on this database.

  systemctl status postgresql
  journalctl -u postgresql -n 50
  df -h /var/lib/postgresql        # an unrotated WAL filling the disk is the usual cause

Everything the dashboard does is available on the host over SSH:
  sudo gvm status
  sudo gvm doctor
```

(The page carries generic commands only — never an example instance name, so
it cannot read as a lie on a box with different servers. A test holds it to
that.)

---

## Investigating a Postgres outage

```bash
systemctl status postgresql
journalctl -u postgresql -n 50
df -h /var/lib/postgresql          # WAL fill is the most common cause
pg_lsclusters                      # is the cluster actually started?
sudo -u postgres psql -c '\l'      # connectivity test
```

## Replaying the spool after recovery

```bash
sudo gvm db replay-spool           # replays /var/lib/gvm/spool/ into audit_log
sudo gvm db status                 # verify schema version is current
```

The spool replay is idempotent: rows already in `audit_log` are skipped
(`INSERT … ON CONFLICT (spool_id) DO NOTHING`), so it is safe to run multiple times.
