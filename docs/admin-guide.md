# Admin guide: standing up the dashboard (v0)

This covers only commands that work in this build. The dashboard's v0 scope:
sign in with password + TOTP, see every instance's state in plain words,
start / stop / restart with role checks (a stop always walks the save
ladder), watch and type into the live console, and invite friends who enrol
their own factors. The web settings editor lands in a later phase and refuses
honestly until then.

The one rule holds throughout: kill `gvmd`, kill `gvm-web`, kill Postgres —
**every game server keeps running**, and `sudo gvm` over SSH still does
everything.

## 1. Install the control plane

```
sudo gvm host init --dashboard
```

On top of the games-only install, this writes `gvmd.service`,
`gvm-web.service`, `gvm-db-migrate.service` and the dashboard's tmpfiles
lines, creates `/etc/gvm/master.key` (0400 root — losing it costs every TOTP
enrolment, never a world), and prints the postgresql lines below. It starts
nothing and installs no packages (rule 7).

## 2. Postgres, once

Install it, create the database and roles, and add peer auth — `host init
--dashboard` prints all of these to paste:

```
sudo apt-get install postgresql
sudo -u postgres psql -c "CREATE ROLE gvm_migrate LOGIN; CREATE ROLE gvm_ops LOGIN; CREATE ROLE gvm_web LOGIN;"
sudo -u postgres psql -c "CREATE DATABASE gvm OWNER gvm_migrate;"
```

In `/etc/postgresql/*/main/pg_hba.conf`, **above** `local all all peer`:

```
local  gvm  gvm_migrate  peer map=gvm
local  gvm  gvm_ops      peer map=gvm
local  gvm  gvm_web      peer map=gvm
```

and in `pg_ident.conf`:

```
gvm  root  gvm_migrate
gvm  root  gvm_ops
gvm  gvm   gvm_web
```

then `sudo systemctl reload postgresql` and:

```
sudo gvm db migrate
```

Migrations run only this way, or through the explicit
`gvm-db-migrate.service` oneshot — never automatically (rule 8). `gvm db
status` says where the schema stands.

## 3. Start the daemons

```
sudo systemctl enable --now gvmd gvm-web
```

`gvmd` (root) serves the closed verb set on `/run/gvm/admin.sock`; `gvm-web`
(the unprivileged `gvm` account) serves the dashboard on
`/run/gvm/web/dashboard.sock`, or on `edge.listen` when set — a loopback
TCP port for a proxy that fronts ports, e.g. a serverGitUpdater-managed Caddy
(see [deploy-behind-servergitupdater.md](deploy-behind-servergitupdater.md)).
A non-loopback `listen` is refused at validation.

The reverse proxy is the public face. With `edge.mode: caddy_managed`:

```
sudo gvm edge plan
sudo gvm edge apply
```

`plan` is read-only: the resolved TLS mode, the ports the firewall must admit
(80/tcp, 443/tcp, 443/udp with HTTP/3 — open them yourself until `gvm
firewall` lands), and whether the live Caddyfile matches. `apply` renders,
validates the staged file as the caddy user, swaps atomically, reloads, and
verifies the config came up — rolling back to the previous config when it did
not. It owns `/etc/gvm/caddy/Caddyfile` only, never `/etc/caddy/Caddyfile`,
and never installs Caddy.

With `caddy_snippet`, `edge apply` writes the site block to
`/etc/gvm/caddy/gvm.caddyfile` and stops — your Caddyfile imports it, your
reload. With `proxy_external` (e.g. a serverGitUpdater-managed Caddy), gvm
writes nothing; set `edge.trusted_origins` or form POSTs are refused.

On a bare IP (`tls internal`), export the internal CA's public certificate
for each moderator's machine:

```
sudo gvm ca export --out gvm-ca.crt
```

## 4. Accounts

```
sudo gvm user create --email you@example.com --role owner
```

Reads the password at the terminal (argon2id, 12-character minimum) and walks
TOTP enrolment — it prints the `otpauth://` line, and one valid code must be
typed before anything is stored. `create` makes owners and admins only;
friends are invited:

```
sudo gvm user invite alice@example.com --instance pz1 --role operator --ttl 24h
```

prints a single-use URL. Alice opens it, chooses her own password, scans her
own TOTP secret and proves one code — you never know either factor. The
account has a viewer ceiling plus exactly the grants you gave it.

Day-2:

```
sudo gvm user list
sudo gvm user show alice@example.com
sudo gvm user roles                      # the §15.2 matrix, in plain English
sudo gvm user grant alice@example.com pz1 operator
sudo gvm user revoke-grant alice@example.com pz1
sudo gvm user disable alice@example.com  # also revokes sessions and tokens
sudo gvm user reset alice@example.com --totp [--password]
sudo gvm user sessions [alice@example.com]
sudo gvm user revoke-sessions alice@example.com
```

`reset` clears the named factors, revokes every session, token and old
invite, and prints a fresh single-use invite URL — root never chooses
anyone's password. With Postgres down, every `gvm user` command fails fast
and says your game servers are unaffected.

## 5. API tokens

```
sudo gvm token create --user you@example.com --name ci-bot --role operator --expires 90d
sudo gvm token list
sudo gvm token revoke ci-bot      # by name or prefix; an ambiguous match is refused
```

The token is shown once. `/api/v1` accepts bearer tokens only — a session
cookie gets 401 on every API route (D-40) — and the token's role ceiling caps
it whatever its user may do.

## 6. What each role can do

Roles nest viewer ⊂ operator ⊂ admin ⊂ owner; `gvm user roles` prints the
full matrix. The line that matters: an **operator** (the moderator role) can
start, stop, restart, watch and take a backup — and can NOT restore over a
world, prune backups, rebuild, apply config, edit mods or touch secrets.
Those are admin and above, and in this release they remain root-CLI only.
