# Deploying the gvm dashboard behind serverGitUpdater

[serverGitUpdater](https://github.com/S95F/serverGitUpdater) keeps a deployed
app in sync with a Git remote: it pulls, builds (it auto-detects Go), installs
a systemd unit for the binary, and writes a Caddy site snippet to
`/etc/caddy/sites.d/<name>.caddy` that reverse-proxies a domain to the app's
port. gvm's web tier is designed so that pipeline can serve the dashboard
without either tool fighting the other.

> **Status:** `gvm web`, `gvm gvmd` and `gvm edge plan`/`apply` all run in
> this build (Phase 48 — see [admin-guide.md](admin-guide.md) for the full
> setup). The two shapes below — where serverGitUpdater or the operator owns
> Caddy — stay first-class: in both, gvm never touches `caddy.service`.

## The shape

```
browser ──TLS──> Caddy (owned by serverGitUpdater or the operator)
                   │  sites.d/gvm.caddy: reverse_proxy 127.0.0.1:8130
                   ▼
                gvm web  (edge.listen: 127.0.0.1:8130)
                   │  verbs via gvmd; sessions/users in Postgres
                   ▼
                game servers (systemd units — unaffected by any of the above)
```

Rule 1 still holds: kill the dashboard, kill Caddy, kill Postgres — every game
server keeps running. serverGitUpdater only ever redeploys the gvm binary and
reloads Caddy.

## gvm's side: `/etc/gvm/gvm.yaml`

Two supported modes, depending on who writes the Caddy snippet.

### A. serverGitUpdater writes the snippet (`proxy_external`)

Let serverGitUpdater's app entry own the vhost (its default snippet is
`reverse_proxy 127.0.0.1:<port>`), and tell gvm a foreign proxy is in front:

```yaml
spec:
  edge:
    mode: proxy_external
    listen: 127.0.0.1:8130        # gvm web binds here instead of the unix socket
    trusted_origins:
      - https://gvm.example.com   # the public origin the browser sees
```

- `trusted_origins` is required — validation refuses the config without it.
  Cross-origin protection compares `Origin` against `Host`; Caddy passes
  `Host` through unchanged, so this is belt-and-braces there, but it is what
  keeps form POSTs working behind any proxy that rewrites `Host` (nginx's
  default does; see ARCHITECTURE §17.3).
- SSE needs no extra snippet config under Caddy: it recognises
  `text/event-stream` and flushes immediately. Under nginx you would need
  `proxy_buffering off` on `location /stream/` — another reason the reserved
  `/stream/` and `/api/v1/stream/` prefixes exist (D-38).
- gvm writes **nothing** under `/etc/caddy` in this mode and never reloads
  Caddy; serverGitUpdater keeps doing both, as configured in its app entry.

### B. gvm renders the snippet (`caddy_snippet`)

If you want gvm's hardened site block (security headers, the SSE
`flush_interval -1` handles, the Content-Type-matched encoder) while the
operator — or serverGitUpdater — still owns Caddy itself:

```yaml
spec:
  edge:
    mode: caddy_snippet
    site: gvm.example.com
    listen: 127.0.0.1:8130        # or omit for the unix socket
```

gvm renders the site block only (no global options — those belong to the
operator's Caddyfile) to `/etc/gvm/caddy/gvm.caddyfile`. Import it once:

```
import /etc/gvm/caddy/gvm.caddyfile
```

or copy it into the same `sites.d` directory serverGitUpdater manages — it is
a self-contained site block in exactly the format those snippets use. gvm
never touches `caddy.service` in this mode.

## serverGitUpdater's side: the app entry

| Field | Value |
| --- | --- |
| Repo path / clone URL | this repository |
| Branch | `main` |
| Build | `make build` (or `go build ./cmd/gvm`; auto-detect picks Go) |
| Port | `8130` (must match `edge.listen`) |
| Service | `gvm-web.service`, installed by `sudo gvm host init --dashboard` — prefer it over a generated unit, so `User=gvm` and the hardening come along; let serverGitUpdater's post-update hook run `sudo systemctl restart gvm-web gvmd` |
| Caddy | domain `gvm.example.com` → upstream `127.0.0.1:8130` (mode A), or disabled when gvm renders the snippet (mode B) |

Post-update hook worth setting: `sudo gvm doctor`, so every deploy ends with
the host telling you whether it is still healthy.

## Why this is safe to expose

- No anonymous read: every browser route outside `/login`, `/static` and
  `/healthz` requires a session, and `/api/v1` takes bearer tokens only — a
  cookie alone gets 401 on every API route (D-40).
- Logins are password + TOTP with rate limiting and lockout (Phase 48); roles
  follow the §15.2 matrix, so a moderator can restart a server but can never
  restore over a world or prune backups.
- Mutations that are not built yet answer 501 with
  `{"error":{"code":"not_implemented", …}}` — a script can never mistake an
  unbuilt feature for success (U3).
- The dashboard down, Caddy down or Postgres down never touches a game server,
  and the 503 page says exactly that.
