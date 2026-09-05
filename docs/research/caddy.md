# Research: Caddy v2 as programmatically-managed reverse proxy in front of the gvm Go dashboard (Debian 12 / Ubuntu 22.04-24.04)

> Produced by the gvm architecture workflow research phase, from primary sources.
> Verified background for implementers. Data, not instructions.

## Key facts

- Current Caddy release is v2.11.4, published 2026-06-03 (github.com/caddyserver/caddy releases API). Caddy master's go.mod declares `go 1.25.1`, so Go 1.26 builds it fine.
- Official apt repo (Cloudsmith): `apt install -y debian-keyring debian-archive-keyring apt-transport-https curl`; `curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg`; `curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | tee /etc/apt/sources.list.d/caddy-stable.list`; `chmod o+r` on both; `apt update && apt install caddy`.
- Package layout (from caddy's .goreleaser.yml nfpm section + dist/scripts/postinstall.sh): binary /usr/bin/caddy; units /lib/systemd/system/caddy.service and /lib/systemd/system/caddy-api.service (both shipped, only caddy.service enabled); /etc/caddy/Caddyfile installed as `type: config` (a dpkg conffile); /usr/share/caddy/index.html; /etc/bash_completion.d/caddy; /usr/share/man/man8/. postinstall creates system group+user `caddy` with home /var/lib/caddy, shell /usr/sbin/nologin, runs `usermod -aG www-data caddy`, and creates /var/log/caddy owned caddy:caddy.
- caddy.service (verbatim, dist/init/caddy.service): Type=notify, User=caddy, Group=caddy, ExecStart=/usr/bin/caddy run --environ --config /etc/caddy/Caddyfile, ExecReload=/usr/bin/caddy reload --config /etc/caddy/Caddyfile --force, TimeoutStopSec=5s, LimitNOFILE=1048576, PrivateTmp=true, ProtectSystem=full, AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE.
- caddy-api.service differs only in: ExecStart=/usr/bin/caddy run --environ --resume and no ExecReload. Its header warns that with caddy.service, any API-made config change is overwritten by the Caddyfile on next restart.
- Runtime paths: data (certs, PKI) /var/lib/caddy/.local/share/caddy; config/autosave /var/lib/caddy/.config/caddy/autosave.json; local CA root at /var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt.
- Admin API default listen is the const caddy.DefaultAdminListen = "localhost:2019", overridable by env CADDY_ADMIN or config (config wins). Endpoints: POST /load (replace whole config; Content-Type selects adapter, e.g. application/json or text/caddyfile), GET|POST|PUT|PATCH|DELETE /config/<path>, /id/<@id>/... , POST /adapt (adapt without loading), POST /stop (graceful shutdown + exit), GET /pki/ca/<id>, GET /pki/ca/<id>/certificates, GET /reverse_proxy/upstreams, GET /metrics.
- Optimistic concurrency: GET /config/... returns an `Etag` header (xxhash of config); send it back as `If-Match` on POST/PUT/PATCH/DELETE; HTTP 412 means retry. This is the correct primitive for gvm's concurrent config writers.
- AdminConfig JSON fields (admin.go): `disabled`, `listen`, `enforce_origin`, `origins`, `config` {`persist`, `load`, `load_delay`}, `identity`, `remote` {`listen` (default :2021), `access_control`[{`public_keys`,`permissions`[{`paths`,`methods`}]}]}. When `origins` is nil and listen is loopback, allowed origins are localhost/127.0.0.1/::1; when listen is a unix socket the same loopback set is allowed.
- Admin API hardening in code (admin.go): requests with `Origin: null` are 400; `Upgrade: websocket` to admin is rejected; `Sec-Fetch-Mode: no-cors` is rejected; and origin checking runs `if h.enforceOrigin || hasOriginHeader || hasSecHeader` — i.e. browser-originated requests are checked even when enforce_origin is false.
- CVE-2026-27589 / GHSA-879p-475x-rqh2: the local admin API's POST /load accepted cross-origin browser requests, allowing a malicious web page visited on the host to replace the whole Caddy config. Fixed in v2.11.x (advisory says v2.11.0+; the v2.11.1 notes describe blocking `no-cors`-mode cross-origin requests). Treat >= 2.11.1 as the minimum acceptable version.
- Other recent admin-relevant advisories: GHSA-x5w9-xh9r-mvfc (remote-admin authorization bypass in /config via array index normalization) and GHSA-gx7w-56w6-g48x (remote-admin authz bypass on PKI endpoints via prefix path matching), both patched 2026-05-13 — i.e. the `remote` admin permission model has had real bypasses; prefer no remote admin at all.
- Admin endpoint on a unix socket: Caddyfile global `admin unix//run/caddy/admin.sock|0660` (permission suffix after `|`, default 0200). Network-address syntax is `network/address`, supporting tcp/tcp4/tcp6/udp/unix/unixgram/unixpacket, plus `fd/`+`fdgram/` for socket activation.
- `caddy reload` is exactly a POST to /load on the admin endpoint; flags: --config, --adapter, --address (needed when admin is not on localhost:2019), --force. `caddy validate` provisions modules without starting; `caddy adapt --pretty --validate` converts Caddyfile to JSON.
- Caddy 2.11 added SIGUSR1 config reload (PR #7258). sigtrap_posix.go: on SIGUSR1 it reloads from the last-known config file; if no file was recorded it logs 'last config unknown, ignored SIGUSR1'. It only works when started as `caddy run --config <file>`, is disabled by `--resume`, and is disabled once any config change comes in through the admin API. SIGHUP and SIGUSR2 are explicitly not implemented.
- Automatic HTTPS: public CAs (Let's Encrypt primary, ZeroSSL fallback) for public DNS names; Caddy's internal CA for `localhost`, IP addresses, and .localhost/.local/.internal/.home.arpa. Default challenges are HTTP-01 (port 80) and TLS-ALPN-01 (port 443); Caddy picks one at random then learns which works. DNS-01 requires a `caddy-dns/<provider>` plugin that is NOT in the apt binary.
- Bare-IP public certs do NOT work in current Caddy: issue #7399 shows `subject '<ip>' cannot have public IP certificate from https://acme-v02.api.letsencrypt.org/directory`, even with the new `profile shortlived` ACME option (which is itself draft-spec and subject to change). IP-only hosts therefore get `tls internal`.
- Internal CA defaults (modules/caddypki/ca.go + tls directive docs): CA name 'Caddy Local Authority', root lifetime 10 years, intermediate lifetime 7 days, leaf lifetime 12h (`tls internal { lifetime }`). Root/intermediate live under pki/authorities/<ca-id>/ as root.crt/root.key. GET /pki/ca/local/certificates returns the chain as application/pem-certificate-chain; GET /pki/ca/local returns JSON with fields id, name, root_common_name, intermediate_common_name, root_certificate, intermediate_certificate.
- HTTP/3 is on by default (`servers { protocols }` default is `h1 h2 h3`), so Caddy binds UDP/443 as well as TCP/80 and TCP/443. ufw must open 443/udp or you must set `protocols h1 h2`.
- Unix-socket upstreams: Caddyfile `reverse_proxy unix//run/gvm/dash.sock` (and `unix+h2c//...` for h2c); JSON is {"handler":"reverse_proxy","upstreams":[{"dial":"unix//run/gvm/dash.sock"}]}. Module ID is `http.handlers.reverse_proxy`; handler JSON keys include upstreams, transport, load_balancing, health_checks, flush_interval, request_buffers, response_buffers, stream_timeout, stream_close_delay, headers, trusted_proxies.
- Streaming: modules/caddyhttp/reverseproxy/streaming.go picks an immediate flush (interval -1) when Content-Type is text/event-stream, when res.ContentLength == -1, or for bidirectional h2/h2c streams. WebSocket Upgrade is handled automatically with no extra config. `stream_close_delay` delays killing hijacked/upgraded streams on config reload; `stream_timeout` force-closes long-lived streams.
- `encode`'s default content-type matcher includes `text/*`, which matches text/event-stream, so SSE IS compressed by default; the encoder's FlushError() flushes the compressor, so it still streams, but the safe move is an explicit match block excluding text/event-stream. Default minimum_length is 512 bytes.
- Caddy has no built-in rate limiting and no automatic HSTS. Rate limiting means the third-party module github.com/mholt/caddy-ratelimit (Apache-2.0, NOT under the caddyserver org): Caddyfile `rate_limit { zone <name> { match {...} key <string> window <dur> events <n> ipv4_prefix <bits> ipv6_prefix <bits> } distributed {...} jitter <pct> }`, JSON handler id `rate_limit`, sliding window, emits 429 + Retry-After.
- Client IP for auditing/rate-limiting: global `servers { trusted_proxies static private_ranges | <cidrs...>; trusted_proxies_strict; client_ip_headers <hdrs...> }`. Defaults: no proxies trusted, X-Forwarded-For only, left-to-right parsing. `{client_ip}` only becomes trustworthy once trusted_proxies is set.
- Server defaults worth pinning: `servers { timeouts { read_body, read_header, write, idle } max_header_size }` — idle default 5m, max_header_size default 1MB, no read/write timeouts by default.
- Metrics are NOT on by default in current Caddy; enable with global option `metrics` and scrape http://localhost:2019/metrics (caddy_http_requests_total, caddy_http_request_duration_seconds, caddy_http_response_size_bytes, caddy_reverse_proxy_upstreams_healthy).
- Embedding: `github.com/caddyserver/caddy/v2` exposes func Run(cfg *Config) error, func Load(cfgJSON []byte, forceReload bool) error, func Stop() error, func Validate(cfg *Config) error. Config has Admin, Logging, StorageRaw, AppsRaw. Modules self-register in init(); a blank import of github.com/caddyserver/caddy/v2/modules/standard pulls in caddyhttp/standard, caddytls, caddypki, caddypki/acmeserver, filestorage, logging, metrics, caddyevents, caddyfs, and the caddyfile adapter. Caddy keeps ONE active config in package-level globals (currentCtx/currentCtxMu/rawCfgMu).
- Caddy's dependency tree is large (46 direct requires incl. quic-go, smallstep/certificates, cel-go, otel sdk+autoexport, prometheus client, chroma, goldmark, sprig) — embedding pulls all of that into the gvm binary.
- Third-party modules require a rebuilt binary: `xcaddy build --with github.com/mholt/caddy-ratelimit --with github.com/caddy-dns/cloudflare`, or `caddy add-package <module>` which replaces /usr/bin/caddy in place. xcaddy itself is installable from the Cloudsmith `caddy/xcaddy` apt repo.

## Pitfalls

- /etc/caddy/Caddyfile is a dpkg conffile (`type: config` in nfpm). If gvm rewrites that exact path, every `apt upgrade` of caddy raises a conffile conflict; in noninteractive mode dpkg keeps the local version silently, and admins get confusing prompts. Never write /etc/caddy/Caddyfile. Write /etc/gvm/caddy/Caddyfile (with a systemd drop-in) or /etc/caddy/conf.d/gvm.caddyfile plus a one-time `import /etc/caddy/conf.d/*.caddyfile` line.
- caddy.service has PrivateTmp=true, so Caddy sees a private /tmp. A dashboard unix socket in /tmp is invisible to Caddy. Put it in /run/gvm/ (systemd RuntimeDirectory=gvm) or /var/run/gvm/.
- caddy.service has ProtectSystem=full: the caddy process cannot write /etc, /usr, /boot. That's fine (gvm writes the config), but anything that expects Caddy to write next to its config will fail.
- ExecReload runs as User=caddy. Whatever config path gvm writes must be readable by the caddy user (0640 root:caddy on the file, 0750 root:caddy on the dir). And if you move the admin endpoint to a unix socket, `systemctl reload caddy` breaks unless the drop-in adds `--address unix//run/caddy/admin.sock` (known breakage: caddy issues #5056, #5694).
- Mixing caddy.service (Caddyfile) with admin-API writes is a silent data-loss trap: the unit does not pass --resume, so on restart the Caddyfile wins and every API-applied change vanishes. Pick one source of truth.
- Caddy 2.11's SIGUSR1 reload is silently disabled the moment ANY config change arrives via the admin API (it clears the last-known-config file). So you cannot mix 'reload by signal' with 'PATCH via API'.
- SIGUSR1 reload reports success/failure only to the log — the signal itself gives no result. gvm must pre-validate (POST /adapt or `caddy validate`) and then verify post-reload health, or it will happily apply a broken config.
- Admin API = full remote code execution equivalence. It can load any config, and a loaded config can point storage/logs anywhere the caddy user can write and can serve any filesystem path. Any SSRF in the gvm dashboard that can reach 127.0.0.1:2019 with a POST body is game over. CVE-2026-27589 was exactly this class via a browser.
- `origins` defaulting is subtle: nil means 'the listener address', an explicitly EMPTY list means no origins allowed at all. Setting `origins` wrong can lock out `caddy reload` itself.
- Caddy's `remote` admin (default :2021) has had two authorization-bypass advisories patched 2026-05-13 (GHSA-x5w9-xh9r-mvfc, GHSA-gx7w-56w6-g48x). Do not build gvm's multi-host story on remote admin.
- HTTP/3 means UDP/443. The inherited firewall.sh only opens TCP for what it opens; a ufw ruleset with 80/tcp+443/tcp but not 443/udp yields a working-but-mysteriously-slow-for-some-clients dashboard (Alt-Svc advertised, UDP blackholed).
- TLS-ALPN-01 needs Caddy to own TCP/443 exclusively — it cannot coexist with another 443 listener. HTTP-01 needs TCP/80, which conflicts with the reflex to close port 80 in a hardened firewall. Closing 80 without disabling the HTTP challenge leaves Caddy retrying forever.
- Let's Encrypt rate limits apply per registered domain (order/duplicate-cert limits). A gvm 'rebuild' flow that recreates a server and re-issues certs in a loop will get the operator rate-limited. Use the LE staging directory (`acme_ca https://acme-staging-v02.api.letsencrypt.org/directory`) in gvm's own test path.
- IP-only deployments: there is no public cert. `tls internal` gives a 12h leaf from a locally-generated CA that browsers do not trust; the operator must download root.crt. Also note Caddy tries to install its root into the system trust store on first use (`skip_install_trust` / `caddy trust` control this) — on a headless VPS that install is a no-op but it still logs alarming lines.
- Default `encode` compresses `text/*`, which includes text/event-stream. It works because the encoder is flushed, but any future 'optimization' that buffers, or a proxy/CDN in front, will stall the live console. Exclude text/event-stream explicitly.
- Config reload kills hijacked/upgraded connections unless `stream_close_delay` is set. If gvm rewrites Caddy config every time an instance is created, every operator's live console websocket drops at that moment.
- No idle/write timeout tuning by default beyond idle=5m: a long-lived SSE console stream can be cut by intermediate proxies or by a `write` timeout if you set one globally. Set timeouts on the site, not globally, or exempt the stream route.
- Caddy adds NO security headers and NO HSTS automatically. `header` must be configured explicitly, and HSTS is a one-way door for the domain.
- `caddy add-package` / xcaddy replace /usr/bin/caddy with a custom build; the next `apt upgrade caddy` overwrites it and silently removes your rate-limit and DNS plugins. Any gvm feature that depends on a plugin must be re-checked after package upgrades (or pinned with dpkg-divert / apt-mark hold).
- Reverse-proxying to a unix socket passes the client's Host through unchanged (Caddy only touches X-Forwarded-For/Proto/Host), but the `{http.reverse_proxy.upstream.hostport}` placeholder becomes the socket path — do not use it for logging/identity.
- Embedding Caddy in gvm means gvm needs CAP_NET_BIND_SERVICE (or root) to bind 80/443, and gvm crashes/restarts now take down HTTPS and the ACME renewal loop with them. Caddy's single-global-config model also means gvm cannot run two independent Caddy configs in-process.

## Recommendations

- Run Caddy as a SEPARATE apt-installed process. Do NOT embed caddyserver/caddy/v2 as a library in gvm. Firm recommendation.
- Pin a minimum version of Caddy 2.11.1 (current 2.11.4) at install time and refuse to configure older ones — 2.11.1 carries the admin-API cross-origin fix (CVE-2026-27589) plus five other CVEs.
- Make Caddy's config a gvm-owned file at /etc/gvm/caddy/Caddyfile (0640 root:caddy, dir 0750 root:caddy), pointed at by a systemd drop-in (`systemctl edit caddy`: ExecStart= then ExecStart=/usr/bin/caddy run --environ --config /etc/gvm/caddy/Caddyfile; same for ExecReload). Leave the dpkg conffile /etc/caddy/Caddyfile untouched.
- Generate the Caddyfile from Go templates, write it atomically (tmp file in the same dir + os.Rename + fsync of the dir), keep the previous version as .bak, validate before applying, and roll back on failure. Caddyfile is the right surface for gvm: it is diffable, reviewable by the operator, and survives a full host reboot without needing the admin API.
- Apply changes with `systemctl reload caddy` (which runs `caddy reload --config ... --force`, a POST to /load — graceful, zero-downtime). Pre-validate with `runuser -u caddy -- /usr/bin/caddy validate --config /etc/gvm/caddy/Caddyfile --adapter caddyfile` (run as caddy so provisioning doesn't create /root/.local/share/caddy), and treat a non-zero exit as 'do not reload'.
- Keep the admin endpoint on the default localhost:2019 but ALSO set `admin { enforce_origin }` in the global block, and drop a ufw/nftables rule that denies 2019 from anywhere but loopback. If you want more, move it to `admin unix//run/caddy/admin.sock|0660` (dir /run/caddy 0750 root:caddy) — but then you MUST add --address to both ExecStart-adjacent reload paths in the drop-in.
- Do NOT enable Caddy's `remote` admin (:2021) for multi-host gvm. If gvm ever manages several VPSes, run a gvm agent per host and have the agent talk to its own loopback Caddy; that keeps the blast radius local and avoids the remote-admin authz bypass class.
- Use the admin API only for read-only introspection and surgical live tweaks: GET /config/ (with Etag), GET /pki/ca/local/certificates (to hand the operator the root CA for IP-only installs), GET /reverse_proxy/upstreams (dashboard health widget), POST /adapt (server-side Caddyfile lint before writing the file). If you do write via the API, use @id tags on the gvm-owned route and PATCH /id/gvm_routes/... with If-Match, and switch the unit to caddy-api.service semantics (--resume) so restarts don't lose it.
- Never write via the API AND via the file. Pick file+reload (recommended) and record that decision, because mixing them also silently disables the 2.11 SIGUSR1 path.
- Bind the Go dashboard to a unix socket at /run/gvm/dashboard.sock (systemd RuntimeDirectory=gvm), chown root:caddy, chmod 0660, and proxy with `reverse_proxy unix//run/gvm/dashboard.sock`. This makes it impossible to reach the dashboard except through Caddy, removes the 'is 8080 firewalled?' failure mode, and needs no extra port in the ufw ruleset. Keep an optional `--listen 127.0.0.1:PORT` fallback for debugging.
- Route ALL per-instance concerns inside the Go app (path- or host-prefix routing to internal handlers). Keep the Caddy config essentially static — one site block, one upstream. Only regenerate Caddy config when the operator changes a domain, adds a TLS hostname, or opens a new public endpoint. This turns 'programmatically manage the proxy' into a rare, auditable operation rather than a hot path.
- For the live console, prefer SSE over websockets for one-way log tailing (survives reloads better, no Upgrade hijack, auto-reconnect built into EventSource); use websockets only for the interactive command input channel. Caddy auto-flushes text/event-stream, so no flush_interval is needed — but set `flush_interval -1` on the console route anyway as an explicit contract, exclude text/event-stream from `encode`, and set `stream_close_delay 5m` so config reloads don't kill live sessions.
- Set `servers { trusted_proxies static private_ranges; timeouts { read_header 10s; idle 10m } }` and use `{client_ip}` (not remote.host) for gvm's audit log and RBAC decisions.
- Emit an explicit security-header block on the dashboard site: Strict-Transport-Security (only once the domain is settled), X-Content-Type-Options nosniff, Referrer-Policy strict-origin-when-cross-origin, X-Frame-Options DENY or a CSP frame-ancestors 'none', and Cross-Origin-Opener-Policy same-origin. Caddy adds none of these by itself.
- Do the real rate limiting and brute-force lockout INSIDE the Go app (per-account and per-IP, backed by Postgres or in-memory), because that is where identity lives and because it survives a Caddy binary replacement. Treat caddy-ratelimit as an optional edge layer only, and detect at runtime whether the module is present (POST /adapt a probe config, or parse `caddy list-modules`) before generating a `rate_limit` block.
- TLS decision tree gvm should implement: (1) operator supplies a domain with public A/AAAA -> default automatic HTTPS, HTTP-01+TLS-ALPN-01, require 80/tcp, 443/tcp, 443/udp open; (2) domain behind CGNAT/closed 80 or wildcard wanted -> DNS-01, which requires a rebuilt binary (`caddy add-package github.com/caddy-dns/<provider>`) and an API token — gvm should detect the missing module and refuse with a clear message rather than half-configuring; (3) IP only -> `tls internal`, and expose a 'Download root CA' button in the CLI/dashboard that fetches GET http://localhost:2019/pki/ca/local/certificates. Do not attempt public certs for bare IPs — Caddy rejects them today (issue #7399).
- Extend the inherited firewall.sh logic: `ufw allow 80/tcp`, `ufw allow 443/tcp`, `ufw allow 443/udp`, keep `ufw limit <ssh>/tcp`, and explicitly do NOT open 2019. Keep the existing SSH-port autodetection ordering (config -> SSH_CONNECTION -> sshd_config -> 22) as-is; it is the same class of hard-won knowledge and it must run before any deny-by-default policy is applied.
- If gvm ever genuinely needs in-process Caddy behavior, ship a Caddy PLUGIN (a caddy.Module implementing an app or handler) built with xcaddy, rather than importing Caddy into gvm. That is the direction Caddy maintainers point people ("your code can be a Caddy app, which plugs into the Caddy lifecycle") and it keeps the process boundary.

## Detail

## 0. Version baseline (verify at install time)

Latest release is **v2.11.4, published 2026-06-03**. `go.mod` on master declares `go 1.25.1`, so Go 1.26 is fine for building plugins with xcaddy. **Require >= 2.11.1** — that release fixed six CVEs including the admin-API cross-origin issue below. Debian 12 / Ubuntu 22.04 archive Caddy is far older; always use the Cloudsmith repo, never `apt install caddy` from the distro.

---

## 1. Installing from the official apt repo

```bash
apt install -y debian-keyring debian-archive-keyring apt-transport-https curl
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
  | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
  | tee /etc/apt/sources.list.d/caddy-stable.list
chmod o+r /usr/share/keyrings/caddy-stable-archive-keyring.gpg
chmod o+r /etc/apt/sources.list.d/caddy-stable.list
apt update && apt install caddy
```

There is also a `testing` channel (same shape, `/public/caddy/testing/`) — do not use it for gvm.

What the package actually lays down (from `.goreleaser.yml` nfpm + `dist/scripts/postinstall.sh`):

| Path | Notes |
|---|---|
| `/usr/bin/caddy` | replaced by `caddy upgrade` / `caddy add-package` |
| `/lib/systemd/system/caddy.service` | enabled + started by postinstall |
| `/lib/systemd/system/caddy-api.service` | shipped, **not** enabled |
| `/etc/caddy/Caddyfile` | **dpkg conffile** (`type: config`) |
| `/usr/share/caddy/index.html` | welcome page |
| `/var/lib/caddy` | caddy user home; data + autosave live under it |
| `/var/log/caddy` | created `caddy:caddy` |
| `/etc/bash_completion.d/caddy`, `/usr/share/man/man8/` | |

postinstall runs: `groupadd --system caddy`; `useradd --system --gid caddy --create-home --home-dir /var/lib/caddy --shell /usr/sbin/nologin --comment "Caddy web server" caddy`; `usermod -aG www-data caddy`; then `deb-systemd-helper enable caddy.service` + `deb-systemd-invoke start caddy.service`.

Runtime dirs (XDG-derived from the caddy user's home):
- data (certs, PKI, ACME account keys): `/var/lib/caddy/.local/share/caddy`
- config/autosave: `/var/lib/caddy/.config/caddy/autosave.json`
- local CA root: `/var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt`

---

## 2. The systemd units, verbatim

`dist/init/caddy.service`:

```ini
[Unit]
Description=Caddy
Documentation=https://caddyserver.com/docs/
After=network.target network-online.target
Requires=network-online.target

[Service]
Type=notify
User=caddy
Group=caddy
ExecStart=/usr/bin/caddy run --environ --config /etc/caddy/Caddyfile
ExecReload=/usr/bin/caddy reload --config /etc/caddy/Caddyfile --force
TimeoutStopSec=5s
LimitNOFILE=1048576
PrivateTmp=true
ProtectSystem=full
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
```

`caddy-api.service` is identical except `ExecStart=/usr/bin/caddy run --environ --resume` and **no ExecReload**. The header comment on `caddy.service` is the load-bearing warning:

> "This service does not use the `--resume` flag, so if you use the API to make changes, they will be overwritten by the Caddyfile next time the service is restarted."

Three consequences for gvm:

1. **`PrivateTmp=true`** — Caddy's `/tmp` is namespaced. A dashboard socket in `/tmp` is invisible. Use `/run/gvm/`.
2. **`ProtectSystem=full`** — `/etc`, `/usr`, `/boot` are read-only to the Caddy process. Fine, gvm writes the config, Caddy only reads it.
3. **`ExecReload` runs as `User=caddy`** — the config path gvm writes must be readable by `caddy`, and a relocated admin endpoint breaks reload unless `--address` is added.

---

## 3. Caddyfile vs native JSON

Caddy's *native* config is JSON; the Caddyfile is a config adapter that produces that JSON. Verified adapt output shape (from `caddytest/integration/caddyfile_adapt/`):

```json
{
  "admin": { "listen": "localhost:2019", "origins": ["localhost:2019","[::1]:2019","127.0.0.1:2019"] },
  "logging": { "logs": { "default": { "level": "DEBUG" } } },
  "storage": { "module": "file_system", "root": "/data" },
  "apps": {
    "http": { "http_port": 8080, "https_port": 8443,
      "servers": { "srv0": { "listen": [":80"], "routes": [
        { "match": [{"path": ["/api/v1/*"]}],
          "handle": [{ "handler": "subroute", "routes": [
            { "handle": [{"handler":"rewrite","strip_path_prefix":"/api/v1"}] },
            { "handle": [{"handler":"static_response","body":"API v1"}] }
          ]}]}
      ]}}},
    "tls": { "automation": { "policies": [{ "issuers": [{"module":"internal"}], "key_type":"ed25519" }] } }
  }
}
```

For gvm, generating a **Caddyfile** beats generating JSON:

- ~10 lines vs ~120 lines of nested JSON for the same site, so the generator is trivial and template-based.
- The operator can `cat` it and understand it — matches the ethos of the bash suite it replaces.
- `caddy validate` / `caddy adapt` give you a syntax + provisioning check before you apply.
- It survives a reboot with zero extra state (no `--resume`, no autosave dependency).

JSON only wins when you need surgical, high-frequency, partial mutations of a live config — which the architecture below deliberately avoids.

Note on **which file**: `/etc/caddy/Caddyfile` is a dpkg conffile. Rewriting it makes every `apt upgrade caddy` a conffile conflict. Two clean options:

- **(preferred)** gvm owns `/etc/gvm/caddy/Caddyfile` and installs a drop-in:
  ```bash
  systemctl edit caddy   # writes /etc/systemd/system/caddy.service.d/override.conf
  ```
  ```ini
  [Service]
  ExecStart=
  ExecStart=/usr/bin/caddy run --environ --config /etc/gvm/caddy/Caddyfile
  ExecReload=
  ExecReload=/usr/bin/caddy reload --config /etc/gvm/caddy/Caddyfile --force
  ```
  (the empty-string reset lines are mandatory in systemd drop-ins)
- Or append one line `import /etc/caddy/conf.d/*.caddyfile` to the stock Caddyfile and own `/etc/caddy/conf.d/gvm.caddyfile`. `import` supports globs, an empty glob is not an error, and it works at top level or inside a site block.

Permissions: `/etc/gvm/caddy` `0750 root:caddy`, `Caddyfile` `0640 root:caddy`. Write atomically: temp file in the same directory, `fsync`, `os.Rename`, `fsync` the dir; keep `.bak`.

---

## 4. The admin API

Default listen is the constant `caddy.DefaultAdminListen = "localhost:2019"`, overridable by `CADDY_ADMIN` (config wins over env). **It is on by default even under `caddy.service`.**

Endpoints:

| Method + path | Purpose |
|---|---|
| `POST /load` | replace entire config; `Content-Type` selects the adapter (`application/json`, `text/caddyfile`). Rollback on failure, zero downtime. `Cache-Control: must-revalidate` forces reload of an identical config |
| `GET /config/[path]` | export config subtree as JSON; response carries an `Etag` |
| `POST /config/[path]` | append to array / create-or-replace object; `/...` expands multiple array elements |
| `PUT /config/[path]` | insert into array / strictly create |
| `PATCH /config/[path]` | strictly replace an existing value |
| `DELETE /config/[path]` | delete; `DELETE /config/` unloads everything without exiting |
| `POST /adapt` | adapt (e.g. Caddyfile → JSON) **without** loading — a perfect server-side lint |
| `POST /stop` | graceful shutdown **and process exit** |
| `GET /id/<@id>/...` | address any object tagged `"@id": "..."` in the JSON |
| `GET /pki/ca/<id>` | JSON: `id`, `name`, `root_common_name`, `intermediate_common_name`, `root_certificate`, `intermediate_certificate` |
| `GET /pki/ca/<id>/certificates` | PEM chain, `Content-Type: application/pem-certificate-chain` |
| `GET /reverse_proxy/upstreams` | upstream health, active request counts, fails |
| `GET /metrics` | Prometheus (only if `metrics` global option is on) |

**Concurrency** — the primitive gvm should use if it ever does live PATCHes: `GET` the subtree, keep the `Etag` (an xxhash of the config), send `If-Match: <etag>` on the mutation, retry the whole cycle on `412`.

### `caddy reload` vs API PATCH

They are the same mechanism at different granularity. `caddy reload` is documented as "the same effect as POSTing a document to the `/load` endpoint" — it reads the file, adapts it, and POSTs the whole thing. Flags: `--config`, `--adapter`, `--address` (required when the admin endpoint isn't `localhost:2019`), `--force`.

- **`/load` (whole config)**: idempotent, one source of truth, trivially diffable, rolls back cleanly. Cost: every listener/handler is re-provisioned (still zero-downtime — listeners are reused and in-flight requests finish under the old config), and hijacked streams (websockets) are torn down unless `stream_close_delay` is set.
- **`PATCH /id/...` (surgical)**: touches one route, doesn't disturb the rest. Cost: you now hold live state that exists only in Caddy's memory + `autosave.json`, you must run `caddy-api.service` (`--resume`) or lose it on restart, and you must handle `Etag`/412 yourself.

### Caddy 2.11 SIGUSR1 — read the fine print

`sigtrap_posix.go`:

```go
case syscall.SIGUSR1:
    logger := Log().With(zap.String("signal", "SIGUSR1"))
    file, adapter, reloadCallback := getLastConfig()
    if file == "" {
        logger.Info("last config unknown, ignored SIGUSR1")
        break
    }
    ...
case syscall.SIGUSR2:
    Log().Info("not implemented", zap.String("signal", "SIGUSR2"))
case syscall.SIGHUP:
    // ignore; this signal is sometimes sent outside of the user's control
```

SIGUSR1 reload works **only** when started as `caddy run --config <file>`; it is disabled by `--resume`, and **disabled permanently once any config change arrives via the admin API** (the API handler clears the last-known-config reference). It reports only to the log. So "admin off + `ExecReload=/bin/kill -USR1 $MAINPID`" is a viable maximum-lockdown posture — but it is mutually exclusive with API-driven changes, and gvm must pre-validate and post-verify because the signal returns nothing.

---

## 5. Admin API security — this is the sharp edge

The admin API is **unauthenticated by design** and functionally equivalent to remote code execution: it can load an arbitrary config, and a config can serve any path the `caddy` user can read, write logs/storage anywhere it can write, and relocate the admin listener itself.

**CVE-2026-27589 / GHSA-879p-475x-rqh2** — `POST /load` on `127.0.0.1:2019` accepted cross-origin requests from any web page the operator visited on that host, letting an attacker replace the entire running config. Fixed in the 2.11 line; the v2.11.1 notes describe it as "Cross-origin requests attempted with `no-cors` mode could cause some API requests to succeed; such requests are now blocked."

Current defenses live in `admin.go`:

```go
if r.Header.Get("Origin") == "null" { ... 400 "invalid origin 'null'" }
if strings.Contains(r.Header.Get("Upgrade"), "websocket") { ... 400 "websocket connections aren't allowed" }
if strings.Contains(r.Header.Get("Sec-Fetch-Mode"), "no-cors") { ... 400 }

_, hasOriginHeader := r.Header["Origin"]
_, hasSecHeader := r.Header["Sec-Fetch-Mode"]
if h.enforceOrigin || hasOriginHeader || hasSecHeader {
    // origin allowlist check
}
```

So browser-shaped requests are checked even with `enforce_origin` off, while a plain non-browser HTTP client (curl, gvm's Go client) sends neither header and skips the check.

`AdminConfig` fields:

```go
type AdminConfig struct {
    Disabled      bool             `json:"disabled,omitempty"`
    Listen        string           `json:"listen,omitempty"`
    EnforceOrigin bool             `json:"enforce_origin,omitempty"`
    Origins       []string         `json:"origins,omitempty"`
    Config        *ConfigSettings  `json:"config,omitempty"`   // persist, load, load_delay
    Identity      *IdentityConfig  `json:"identity,omitempty"`
    Remote        *RemoteAdmin     `json:"remote,omitempty"`   // listen (default :2021), access_control
}
```

`Origins` defaulting is a footgun: **nil** ⇒ the listener address is allowed; an explicit **empty list** ⇒ *no* origins allowed; loopback listen ⇒ localhost/127.0.0.1/::1; unix socket listen ⇒ same loopback set.

Lockdown ladder for gvm, weakest to strongest:

1. Leave `localhost:2019`, add `admin { enforce_origin }`, and never expose 2019 in ufw. (Baseline.)
2. Move it to a unix socket: `admin unix//run/caddy/admin.sock|0660`, dir `/run/caddy` `0750 root:caddy`. Now only root and the `caddy` group can talk to it — no localhost SSRF path at all. **You must then add `--address unix//run/caddy/admin.sock` to the reload command** or `systemctl reload caddy` fails (caddy issues #5056, #5694).
3. `admin off` + file config + `ExecReload=/bin/kill -USR1 $MAINPID` (Caddy ≥ 2.11). Maximum lockdown, no introspection endpoints, reload errors only in the journal.

**Do not use `remote` admin (`:2021`).** Two authorization-bypass advisories were patched on 2026-05-13: GHSA-x5w9-xh9r-mvfc (`/config` bypass via array-index normalization) and GHSA-gx7w-56w6-g48x (PKI endpoint bypass via prefix path matching). The permission model there is young. For multi-host gvm, run a gvm agent per host talking to its own loopback Caddy.

---

## 6. Automatic HTTPS and the ACME challenge tradeoff

Default issuers: **Let's Encrypt first, ZeroSSL as fallback**. Default challenges: **HTTP-01 (port 80) and TLS-ALPN-01 (port 443)** — Caddy picks one at random initially and then learns which works.

| Challenge | Needs | Use when | Blocks |
|---|---|---|---|
| **HTTP-01** | inbound TCP/80 to Caddy | default; the normal VPS case | any firewall that closes 80; no wildcards |
| **TLS-ALPN-01** | inbound TCP/443, Caddy owning 443 exclusively | you want 80 closed | cannot share 443 with another listener; no wildcards |
| **DNS-01** | a `caddy-dns/<provider>` **plugin** + API token | CGNAT, closed 80/443, wildcards, cert before DNS points at the box | **not in the apt binary** — needs `caddy add-package github.com/caddy-dns/<p>` or an xcaddy build |

Relevant Caddyfile knobs:

```
tls {
    issuer acme {
        dir  https://acme-staging-v02.api.letsencrypt.org/directory
        email ops@example.com
        disable_http_challenge
        disable_tlsalpn_challenge
        alt_http_port 8080
        dns cloudflare {env.CF_API_TOKEN}
        propagation_timeout 5m
        profile shortlived        # draft ACME profiles spec, subject to change
    }
}
```

Global equivalents: `email`, `acme_ca`, `acme_dns <provider>`, `cert_issuer`, `local_certs`, `skip_install_trust`, `auto_https off|disable_redirects|disable_certs|ignore_loaded_certs`.

**HTTP/3 is on by default** (`servers { protocols }` defaults to `h1 h2 h3`), so Caddy binds **UDP/443** too. The firewall must open `80/tcp`, `443/tcp`, **`443/udp`** — or you pin `protocols h1 h2`.

### The IP-address-only case (no domain)

Caddy applies its **internal CA** to IP addresses, `localhost`, and `.localhost`/`.local`/`.internal`/`.home.arpa`. Public CAs will not issue for these.

Let's Encrypt does now issue IP certs under its `shortlived` profile, but **Caddy cannot use it today**: issue #7399 shows the hard error

```
subject '167.150.90.148' cannot have public IP certificate from
https://acme-v02.api.letsencrypt.org/directory
(if CA's policy has changed, please notify the developers in an issue)
```

That check is in certmagic, not the CA. So for an IP-only VPS the answer is `tls internal` (or global `local_certs`).

Internal CA facts (`modules/caddypki/ca.go`, `tls` docs):
- CA name `Caddy Local Authority`; root CN template `{pki.ca.name} - {time.now.year} ECC Root`
- **root lifetime 10 years**, **intermediate lifetime 7 days**, **leaf lifetime 12h** (`tls internal { lifetime <d> }`)
- stored at `pki/authorities/<ca-id>/root.crt` + `root.key`
- Caddy tries to install the root into the system trust store on first use; `skip_install_trust` / `caddy trust` / `caddy untrust` control that

The nice UX move: gvm fetches `GET http://localhost:2019/pki/ca/local/certificates` and offers "download root CA" in `gvm` CLI output and the dashboard, so the operator installs it once on their laptop and gets a green padlock over the raw IP. If the admin API is off, read `/var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt` directly.

`on_demand` TLS exists (cert obtained during the first handshake, gated by an `ask` endpoint). It's a plausible v2 feature if gvm ever hosts per-instance subdomains that operators create at will — the `ask` endpoint would be a gvm handler that answers 200 only for hostnames in the Postgres instance table. Not needed for v1.

---

## 7. Unix socket vs localhost port

Both work. Caddyfile:

```
reverse_proxy unix//run/gvm/dashboard.sock
reverse_proxy unix+h2c//run/gvm/dashboard.sock    # h2c over the socket
reverse_proxy 127.0.0.1:8443
```

JSON: `{"handler":"reverse_proxy","upstreams":[{"dial":"unix//run/gvm/dashboard.sock"}]}` — module id `http.handlers.reverse_proxy`; handler keys: `upstreams`, `transport`, `load_balancing`, `health_checks`, `flush_interval`, `request_buffers`, `response_buffers`, `stream_timeout`, `stream_close_delay`, `headers`, `trusted_proxies`.

Caddy network-address syntax is `network/address` over `tcp|tcp4|tcp6|udp|unix|unixgram|unixpacket` (plus `fd/`, `fdgram/` for socket activation); a unix path may carry a permission suffix `|0660` (default 0200).

**Recommendation: unix socket at `/run/gvm/dashboard.sock`.**
- Not reachable from the network at all, regardless of firewall state — this removes an entire category of "the dashboard was briefly exposed on 8080" incidents.
- No port to allocate, document, or collide with a game server. Given gvm runs N game servers with their own port ranges, one less port to manage is real.
- systemd gives you the directory for free: `RuntimeDirectory=gvm`, `RuntimeDirectoryMode=0750` in gvm's unit → `/run/gvm` created on start, removed on stop.
- Ownership: socket `root:caddy` mode `0660` (Go: `net.Listen("unix", path)`, then `os.Chown` to the caddy gid, `os.Chmod(0o660)`; remove a stale socket first).

Two caveats: it must **not** be under `/tmp` (`PrivateTmp=true`), and `{http.reverse_proxy.upstream.hostport}` becomes the socket path, so don't use that placeholder for logging identity. The client `Host` header is still passed through untouched — Caddy only sets/augments `X-Forwarded-For`, `X-Forwarded-Proto`, `X-Forwarded-Host`.

Keep a `--listen 127.0.0.1:PORT` fallback in gvm for local debugging and for the "no Caddy installed yet" bootstrap phase.

---

## 8. Live console: websockets and SSE

**Websockets need no configuration.** Caddy handles `Upgrade` transparently, including the hijack.

**SSE also needs no configuration**, because `streaming.go` chooses immediate flush (interval `-1`) when:
- `Content-Type` is `text/event-stream` ("For Server-Sent Events responses, flush immediately"), or
- `res.ContentLength == -1` ("We might have the case of streaming for which Content-Length might be unset"), or
- `isBidirectionalStream()` — h2/h2c both ends, no Content-Length, identity/absent `Accept-Encoding`.

The three real hazards:

1. **`encode` compresses SSE.** The default content-type matcher includes `text/*`, which matches `text/event-stream`. It survives because the encode wrapper's `FlushError()` "delays the actual Flush ... until headers were written" and "also flushes the encoder"; encode also writes headers immediately for SSE responses. Still, make it explicit:
   ```
   encode zstd gzip {
       match { header Content-Type text/* application/json* }
       # and route the console on a path where encode is not applied
   }
   ```
   Default `minimum_length` is 512.

2. **Config reload kills upgraded streams.** `cleanupConnections()` honours `StreamCloseDelay` (immediate close, or `time.AfterFunc` delay); `StreamTimeout` force-closes long-lived streams. If gvm regenerates Caddy config on every instance create, every operator console drops. Set `stream_close_delay 5m` on the console route — and better, design so config changes are rare (§10).

3. **Timeouts.** `servers { timeouts { read_body, read_header, write, idle } }` — idle default 5m, no read/write defaults. Do not set a global `write` timeout; it will guillotine the console stream.

Suggested console route:

```
@console path /api/instances/*/console/stream
reverse_proxy @console unix//run/gvm/dashboard.sock {
    flush_interval -1
    stream_close_delay 5m
}
```

For a game-console tail, **SSE is the better default**: one-way, no hijack, `EventSource` reconnects on its own with `Last-Event-ID`, and it degrades gracefully across every proxy. Reserve websockets for the interactive command-input channel — which, incidentally, is where the PZ knowledge lands: the "type `save` then `quit` into the screen session, never SIGKILL" sequence should be a gvm API action with a confirmation, not a raw stdin passthrough that an operator can fat-finger.

---

## 9. Security headers and rate limiting

**Caddy adds no security headers and no HSTS automatically.** The `header` directive is the tool: `<field> <value>` sets, `+<field>` appends, `-<field>` deletes (wildcards allowed), `?<field>` sets only if absent, `><field>` sets deferred, `<field> <find> <replace>` regex-replaces.

```
header {
    Strict-Transport-Security "max-age=31536000; includeSubDomains"   # only once the domain is final
    X-Content-Type-Options nosniff
    Referrer-Policy strict-origin-when-cross-origin
    Content-Security-Policy "default-src 'self'; frame-ancestors 'none'; base-uri 'none'"
    Cross-Origin-Opener-Policy same-origin
    -Server
}
```

HSTS is a one-way door per hostname — gate it behind an explicit gvm opt-in, and never emit it for the `tls internal` / IP-address case.

**Rate limiting is not built in.** Standard directives are: `abort, acme_server, basic_auth, bind, encode, error, file_server, forward_auth, fs, handle, handle_errors, handle_path, header, import, intercept, invoke, log, log_append, log_name, log_skip, map, method, metrics, php_fastcgi, push, redir, request_body, request_header, respond, reverse_proxy, rewrite, root, route, templates, tls, tracing, try_files, uri, vars`. Note `basic_auth` (underscore), and `request_body { max_size 1MB }` for a cheap body-size DoS guard.

Rate limiting requires **`github.com/mholt/caddy-ratelimit`** (Apache-2.0, *not* under the caddyserver org):

```
rate_limit {
    zone login {
        match { path /api/auth/login }
        key    {client_ip}
        window 1m
        events 5
        ipv4_prefix 32
        ipv6_prefix 64
    }
    jitter 10
}
```

Sliding window, returns 429 + `Retry-After`, JSON handler id `rate_limit`, optional `distributed` mode over shared storage (eventually consistent). Getting it onto the box means replacing the binary:

```bash
xcaddy build --with github.com/mholt/caddy-ratelimit --with github.com/caddy-dns/cloudflare
# or, in place:
caddy add-package github.com/mholt/caddy-ratelimit
```

Either way, **the next `apt upgrade caddy` overwrites `/usr/bin/caddy` and your plugins vanish**. That is a bad property for a tool whose whole selling point is unattended operation (the inherited `nightly.sh` timer mindset). Therefore: **do the authoritative rate limiting and lockout inside the Go app**, where identity and Postgres already live, and treat caddy-ratelimit as optional edge decoration that gvm feature-detects before emitting a `rate_limit` block.

Client IP correctness matters for both: set `servers { trusted_proxies static private_ranges }` (shortcut for 192.168.0.0/16, 172.16.0.0/12, 10.0.0.0/8, 127.0.0.1/8, fd00::/8, ::1), plus `trusted_proxies_strict` if there is ever a CDN in front, and use `{client_ip}` — not the remote address — in gvm's audit log and RBAC decisions.

Metrics are **off by default**; enable with the `metrics` global option and scrape `localhost:2019/metrics` (`caddy_http_requests_total`, `caddy_http_request_duration_seconds`, `caddy_http_response_size_bytes`, `caddy_reverse_proxy_upstreams_healthy`).

---

## 10. Embedding Caddy as a Go library — recommendation: **no**

It is technically real. `github.com/caddyserver/caddy/v2` exports:

```go
func Run(cfg *Config) error                       // "runs the given config, replacing any existing config"
func Load(cfgJSON []byte, forceReload bool) error // runs it only if different, or forceReload
func Stop() error
func Validate(cfg *Config) error

type Config struct {
    Admin      *AdminConfig
    Logging    *Logging
    StorageRaw json.RawMessage
    AppsRaw    ModuleMap
}
```

with `_ "github.com/caddyserver/caddy/v2/modules/standard"` to register the standard module set (caddyhttp/standard, caddytls, caddypki, caddypki/acmeserver, filestorage, logging, metrics, caddyevents, caddyfs, and the caddyfile adapter). FrankenPHP does exactly this in production, so it is a trodden path.

**Against it, for gvm specifically:**

1. **Privilege coupling.** Binding 80/443 needs `CAP_NET_BIND_SERVICE` or root. The apt Caddy gets that via `AmbientCapabilities` on a dedicated `caddy` user with `ProtectSystem=full`. Embedding puts TLS termination, ACME account keys, and the public listener into the same process as the thing that shells out to SteamCMD, runs `screen` sessions, mutates systemd units, and executes CurseForge modpack installers. That is the wrong direction for a tool whose selling point is safe, unattended operation.
2. **Blast radius.** A panic in gvm's mod-sync path currently costs you a background job. Embedded, it takes down HTTPS, the dashboard, and the ACME renewal loop. `systemd Restart=` recovers it, but the cert-renewal state machine and every live console reconnect at once.
3. **Single-global-config model.** `caddy.go` keeps one active config in package globals (`currentCtx`, `currentCtxMu`, `rawCfgMu`). You cannot run two independent Caddy configurations in one process, and every reconfigure is a global mutex-guarded swap inside your own process.
4. **Dependency mass.** 46 direct requires, including quic-go, smallstep/certificates, cel-go, the OpenTelemetry SDK + autoexport, prometheus client, chroma, goldmark, sprig. Your "single static binary" becomes a very large binary with an enormous transitive surface, and every Caddy CVE (there have been ~10 advisories in the recent window) becomes a gvm release rather than an `apt upgrade`.
5. **Operator expectations.** Debian/Ubuntu admins expect `systemctl status caddy`, `journalctl -u caddy`, `/etc/caddy/`, and their own extra site blocks. Embedding takes all of that away and makes gvm the only way to touch the web tier. A live-PZ-server operator adopting gvm should not have to give up their existing proxy config.
6. **Not the blessed path.** When asked directly about embedding in v2, a Caddy maintainer steered the questioner to the plugin model — "your code can be a Caddy app, which plugs into the Caddy lifecycle" — and admitted "I'm not sure how the 'Caddy as an embedded server' usecase looks as of v2." The library API is exported and used, but it is not a documented, stability-guaranteed integration surface.

**Where embedding would win** (and does not apply here): a self-contained appliance binary with no package manager, no other web services, and no expectation of operator-editable proxy config. gvm is the opposite — it is a fleet tool on a general-purpose VPS.

**The middle path, if in-process behavior is ever genuinely needed:** ship a Caddy *plugin* (a `caddy.Module` implementing `caddy.App` or an HTTP handler), built with xcaddy, that talks to gvm over the same unix socket. That keeps the process boundary while letting gvm influence request handling — e.g. an `on_demand` TLS `ask` handler, or an auth handler that consults gvm's RBAC. Weigh it against the apt-upgrade-clobbers-custom-binary problem before committing.

---

## 11. Concrete recommended shape for gvm v1

**Process model.** apt Caddy as a separate systemd service, drop-in overridden to `/etc/gvm/caddy/Caddyfile`. Admin API on `localhost:2019` with `enforce_origin`, firewalled to loopback (upgrade path to a unix socket documented and one config flag away). gvm daemon listens on `/run/gvm/dashboard.sock` (0660 root:caddy).

**Config flow.** gvm renders a Caddyfile from a Go template → writes atomically to `/etc/gvm/caddy/Caddyfile.new` → `runuser -u caddy -- caddy validate --config ... --adapter caddyfile` (or `POST /adapt` for a fast syntax-only check) → rename over the live file → `systemctl reload caddy` → poll `GET /config/apps/http` and a gvm health endpoint → on failure, restore `.bak` and reload again. Store the rendered config and its hash in Postgres so the dashboard can show a diff and an "out of sync with disk" warning.

**Routing.** One site block, one upstream. All per-instance routing happens inside the Go app (`/i/{instance}/...`). Caddy config changes only when the operator adds a hostname, changes TLS mode, or adds a public endpoint — a rare, operator-initiated, auditable event. This is what makes the file+reload model cheap and makes `stream_close_delay` sufficient rather than load-bearing.

**Skeleton Caddyfile gvm would generate:**

```
{
    email {$GVM_ACME_EMAIL}
    admin localhost:2019 {
        enforce_origin
    }
    servers {
        trusted_proxies static private_ranges
        timeouts { read_header 10s; idle 10m }
        protocols h1 h2 h3
    }
}

gvm.example.com {
    encode zstd gzip {
        match { header Content-Type text/html* text/css* application/json* application/javascript* }
    }
    header {
        X-Content-Type-Options nosniff
        Referrer-Policy strict-origin-when-cross-origin
        Content-Security-Policy "default-src 'self'; frame-ancestors 'none'; base-uri 'none'"
        -Server
    }
    @stream path /api/*/console/stream
    reverse_proxy @stream unix//run/gvm/dashboard.sock {
        flush_interval -1
        stream_close_delay 5m
    }
    reverse_proxy unix//run/gvm/dashboard.sock
    log { output file /var/log/caddy/gvm.log }
}
```

For the IP-only variant, replace the site address with the IP and add `tls internal`.

**Firewall (extending the inherited `firewall.sh` logic).** Keep the SSH-port autodetection order exactly as it is (`SSH_PORT` from config → `SSH_CONNECTION` field 4 → `sshd_config` → 22) and keep `ufw limit <ssh>/tcp`; that ordering is what prevents locking yourself out and must survive generalization verbatim. Add `ufw allow 80/tcp`, `ufw allow 443/tcp`, `ufw allow 443/udp`. Never open 2019. Game ports stay generated per instance from the game definition (the PZ 16261/udp + 16262/udp pair generalizes to a per-game port-set descriptor — and the comment explaining *why both* are needed is exactly the kind of knowledge to carry into the game definition file as a doc string).

**PZ migration interaction.** The adoption path imports a live PZ server that has no web tier at all. Installing Caddy must be a separate, opt-in step from adopting the instance — `gvm adopt` should never touch the firewall or install Caddy as a side effect, because that is the step most likely to interrupt a running server or lock out the operator's SSH session.

## Sources

- https://caddyserver.com/docs/install
- https://caddyserver.com/docs/running
- https://caddyserver.com/docs/api
- https://caddyserver.com/docs/command-line
- https://caddyserver.com/docs/automatic-https
- https://caddyserver.com/docs/conventions
- https://caddyserver.com/docs/metrics
- https://caddyserver.com/docs/caddyfile/options
- https://caddyserver.com/docs/caddyfile/directives
- https://caddyserver.com/docs/caddyfile/directives/reverse_proxy
- https://caddyserver.com/docs/caddyfile/directives/tls
- https://caddyserver.com/docs/caddyfile/directives/header
- https://caddyserver.com/docs/caddyfile/directives/encode
- https://caddyserver.com/docs/caddyfile/directives/import
- https://raw.githubusercontent.com/caddyserver/dist/master/init/caddy.service
- https://raw.githubusercontent.com/caddyserver/dist/master/init/caddy-api.service
- https://raw.githubusercontent.com/caddyserver/dist/master/scripts/postinstall.sh
- https://raw.githubusercontent.com/caddyserver/caddy/master/.goreleaser.yml
- https://raw.githubusercontent.com/caddyserver/caddy/master/admin.go
- https://raw.githubusercontent.com/caddyserver/caddy/master/caddy.go
- https://raw.githubusercontent.com/caddyserver/caddy/master/sigtrap_posix.go
- https://raw.githubusercontent.com/caddyserver/caddy/master/go.mod
- https://raw.githubusercontent.com/caddyserver/caddy/master/modules/standard/imports.go
- https://raw.githubusercontent.com/caddyserver/caddy/master/modules/caddyhttp/reverseproxy/streaming.go
- https://raw.githubusercontent.com/caddyserver/caddy/master/modules/caddyhttp/reverseproxy/reverseproxy.go
- https://raw.githubusercontent.com/caddyserver/caddy/master/modules/caddyhttp/encode/encode.go
- https://raw.githubusercontent.com/caddyserver/caddy/master/modules/caddypki/ca.go
- https://raw.githubusercontent.com/caddyserver/caddy/master/modules/caddypki/adminapi.go
- https://raw.githubusercontent.com/caddyserver/caddy/master/caddytest/integration/caddyfile_adapt/global_options_admin.caddyfiletest
- https://raw.githubusercontent.com/caddyserver/caddy/master/caddytest/integration/caddyfile_adapt/handle_path.caddyfiletest
- https://github.com/caddyserver/caddy/security/advisories/GHSA-879p-475x-rqh2
- https://github.com/caddyserver/caddy/security/advisories
- https://github.com/caddyserver/caddy/issues/7399
- https://github.com/caddyserver/caddy/pull/7258
- https://github.com/mholt/caddy-ratelimit
- https://github.com/caddyserver/xcaddy
- https://api.github.com/repos/caddyserver/caddy/releases/latest
- https://caddy.community/t/embedding-caddy-v2-web-server-in-go/9048
- https://pkg.go.dev/github.com/caddyserver/caddy/v2
