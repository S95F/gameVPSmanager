# Research: Go + PostgreSQL + RBAC + web-auth stack for gvm (self-hosted game server admin dashboard), 2026

> Produced by the gvm architecture workflow research phase, from primary sources.
> Verified background for implementers. Data, not instructions.

## Key facts

- pgx v5 latest is v5.10.0 (2026-06-03). Requires Go 1.25+, PostgreSQL 14+. Pure Go, no cgo. Module: github.com/jackc/pgx/v5; pool at github.com/jackc/pgx/v5/pgxpool; database/sql adapter at github.com/jackc/pgx/v5/stdlib.
- SECURITY: pgx v5.9.2 (2026-04-18) fixed SQL injection via placeholder confusion with dollar-quoted string literals. v5.10.0 added hardening against malicious/compromised Postgres servers (auth-method restrictions, binary decoder protections). Pin >= v5.10.0 in go.mod; do not start on an older v5.
- stdlib.OpenDBFromPool(pool *pgxpool.Pool, opts ...OptionOpenDB) *sql.DB - gives a *sql.DB from an existing pgxpool for tools that need database/sql (goose). Closing the *sql.DB does NOT close the pool; it forces MaxIdleConns=0.
- sqlc latest is v1.31.1 (2026-04-22); v1.31.0 was 2026-04-20; v1.30.0 was 2025-09-01. sqlc.yaml uses engine: postgresql and sql_package: "pgx/v5" to emit pgx-native code (pgtype, pgx.Rows) instead of database/sql.
- goose latest is v3.28.0 (2026-09-02), module github.com/pressly/goose/v3. Non-global API: goose.NewProvider(dialect Dialect, db *sql.DB, fsys fs.FS, opts ...ProviderOption) (*Provider, error). fsys accepts embed.FS directly (or fs.Sub). Legacy global API uses goose.SetBaseFS(fsys).
- golang-migrate latest is v4.19.1 (2025-11-29). embed.FS support via github.com/golang-migrate/migrate/v4/source/iofs; native pgx v5 DB driver at github.com/golang-migrate/migrate/v4/database/pgx/v5. Entry point migrate.NewWithSourceInstance(sourceName string, src source.Driver, databaseURL string).
- jackc/tern v2 exists (github.com/jackc/tern/v2/migrate) and is Postgres-only with a separate 'database code' (functions/views) workflow, but has a much smaller ecosystem than goose.
- Distro Postgres majors: Debian 12 bookworm -> postgresql-15 (metapackage 15+248+deb12u1). Ubuntu 22.04 jammy -> postgresql-14. Ubuntu 24.04 noble -> postgresql-16. All >= 14, so all satisfy pgx's PostgreSQL 14+ floor and all default to scram-sha-256.
- PGDG repo (apt.postgresql.org) supports jammy/noble/resolute/stonking. Easiest setup: `apt install -y postgresql-common` then `/usr/share/postgresql-common/pgdg/apt.postgresql.org.sh`. Manual: key to /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc from https://www.postgresql.org/media/keys/ACCC4CF8.asc, deb822 file /etc/apt/sources.list.d/pgdg.sources with Suites: <codename>-pgdg.
- PostgreSQL 18 released 2025-09-25; 18.6 is current minor (2026-08-13). PG19 was in beta mid-2026. Debian-packaged paths: config /etc/postgresql/<VER>/main/{postgresql.conf,pg_hba.conf}, data /var/lib/postgresql/<VER>/main, socket /var/run/postgresql, port 5432.
- postgresql.conf default listen_addresses is localhost (loopback only) - the upstream default and what Debian ships. No change needed to keep Postgres off the network; verify with `ss -lntp | grep 5432`.
- pg_hba.conf record format: TYPE DATABASE USER [ADDRESS] METHOD [OPTIONS]; first match wins, and a failed match does NOT fall through. `local` = unix socket (no ADDRESS field); `peer` reads the client's OS uid and requires it to equal the DB role name; `peer` is local-only.
- Go 1.25 added net/http.CrossOriginProtection (constructor http.NewCrossOriginProtection(); methods AddTrustedOrigin(string) error, AddInsecureBypassPattern(string), SetDenyHandler(http.Handler), Check(*http.Request) error, Handler(http.Handler) http.Handler). Default rejection is 403.
- CrossOriginProtection logic: GET/HEAD/OPTIONS always allowed. Sec-Fetch-Site values "same-origin" and "none" allowed. If Sec-Fetch-Site is absent it falls back to comparing the Origin header host against the request Host. Requests with NEITHER header are allowed (assumed non-browser).
- gorilla/csrf: v1.7.2 and older were vulnerable (CVE-2025-24358) - a valid token alone was accepted with no origin check. v1.7.3 added same-origin enforcement but then GO-2025-3884 showed TrustedOrigins ignores scheme (adding a host trusts both http:// and https:// origins). Upstream advice is to migrate to net/http.CrossOriginProtection; filippo.io/csrf is the pre-1.25 backport and filippo.io/csrf/gorilla is a drop-in for the gorilla API.
- Caddy reverse_proxy passes the incoming Host header through unmodified by default; the only headers it sets/augments are X-Forwarded-For, X-Forwarded-Proto, X-Forwarded-Host. (Since Caddy v2.11.0 Host is rewritten only when the upstream itself is HTTPS.) This is why http.CrossOriginProtection's Origin-vs-Host comparison works behind Caddy without extra config - the opposite of nginx's default.
- OWASP Password Storage Cheat Sheet current Argon2id options (pick one): m=47104 (46 MiB) t=1 p=1; m=19456 (19 MiB) t=2 p=1; m=12288 t=3 p=1; m=9216 t=4 p=1; m=7168 t=5 p=1. bcrypt: minimum work factor 10, hard 72-byte input limit, pre-hash long inputs as bcrypt(base64(hmac-sha384(password, pepper)), salt, cost).
- golang.org/x/crypto/argon2: func IDKey(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte. memory is in KiB, time is passes. Docs point at RFC 9106 defaults (t=1 m=2GiB p=4, or t=3 m=64MiB p=4) which are far heavier than the OWASP web-app numbers - use the OWASP ones.
- github.com/alexedwards/argon2id wraps x/crypto/argon2 with PHC-format encode/decode: CreateHash(password string, params *Params) (string, error) and ComparePasswordAndHash(password, hash string) (bool, error). Output format $argon2id$v=19$m=65536,t=3,p=2$<b64salt>$<b64hash> - self-describing, so parameters can be upgraded per-user on next login.
- Sessions: github.com/alexedwards/scs/v2 with github.com/alexedwards/scs/pgxstore (pgxstore is pgxpool-native: New(pool *pgxpool.Pool) *PostgresStore, NewWithCleanupInterval(pool, d), NewWithConfig(pool, Config{CleanUpInterval, TableName})). Required DDL: CREATE TABLE sessions (token TEXT PRIMARY KEY, data BYTEA NOT NULL, expiry TIMESTAMPTZ NOT NULL); CREATE INDEX sessions_expiry_idx ON sessions (expiry). Background cleanup goroutine runs every 5 min by default.
- TOTP: github.com/pquerna/otp/totp. totp.Validate(code, secret) is the Google-Authenticator-compatible shortcut (period 30s, skew 1, SHA1, 6 digits). totp.ValidateCustom + totp.ValidateOpts{Period, Skew, Digits, Algorithm} for control. Skew is periods either side; skew>1 is explicitly discouraged in the docs. Validated against RFC 6238 test vectors.
- crypto/cipher.NewGCMWithRandomNonce(cipher Block) (AEAD, error) was added in Go 1.24. It generates a random 96-bit nonce, prepends it to the ciphertext in Seal, and extracts it in Open. NonceSize() is 0, Overhead() is 28 bytes. The Block must come from crypto/aes.NewCipher. Hard limit: one key must not encrypt more than 2^32 messages.
- crypto/hkdf (Go 1.24+ stdlib): hkdf.Key[H hash.Hash](h func() H, secret, salt []byte, info string, keyLength int) ([]byte, error), plus Extract and Expand. Lets one 32-byte master key fan out into per-purpose subkeys with no extra dependency.
- golang.org/x/crypto/nacl/secretbox: Seal(out, message []byte, nonce *[24]byte, key *[32]byte) []byte and Open(out, box []byte, nonce *[24]byte, key *[32]byte) ([]byte, bool). 32-byte key, 24-byte nonce, XSalsa20-Poly1305. Nonce uniqueness is entirely the caller's problem - a footgun NewGCMWithRandomNonce removes.
- filippo.io/age: Encrypt/Decrypt with X25519Recipient/X25519Identity (public-key), or NewScryptRecipient(password) + SetWorkFactor(logN) for passphrase mode. File/stream oriented, per-file header overhead - right tool for backup archives, wrong tool for per-column database values.
- systemd credentials: LoadCredential= exists since systemd v247, but systemd-creds and the whole encrypted-credential path (LoadCredentialEncrypted=, SetCredentialEncrypted=) are v250. Ubuntu 22.04 jammy ships systemd 249.11 -> encrypted credentials are NOT available there. Debian 12 ships 252.39; Ubuntu 24.04 ships 255. Plain LoadCredential= works on all three targets.
- systemd credential mechanics: files appear under $CREDENTIALS_DIRECTORY (/run/credentials/<unit>.service/), access restricted to the service's user, not propagated down the process tree, cleaned up on stop. Overall limit 1 MiB per service. Encryption key is /var/lib/systemd/credential.secret and/or the local TPM2. Encrypt with: systemd-creds encrypt --name=foobar plaintext.txt ciphertext.cred (or `-p --name=... plaintext -` for inline SetCredentialEncrypted=).
- Postgres RLS: ALTER TABLE t ENABLE ROW LEVEL SECURITY is default-deny; CREATE POLICY name ON t [TO role] [FOR cmd] [AS PERMISSIVE|RESTRICTIVE] USING (visibility_expr) [WITH CHECK (write_expr)]. Superusers and BYPASSRLS roles always bypass; the TABLE OWNER bypasses too unless you add ALTER TABLE t FORCE ROW LEVEL SECURITY. Permissive policies OR together; restrictive AND.
- RLS + pooling: context must be set with SET LOCAL (or set_config(k,v,true)) inside an explicit transaction, because plain SET persists on the pooled backend connection and leaks between requests/tenants. SET LOCAL is discarded at COMMIT/ROLLBACK.
- Go 1.26 released 2026-02-10 (Green Tea GC on by default, rewritten `go fix` with modernizers, new crypto/hpke and crypto/mlkem/mlkemtest, experimental simd/archsimd). Nothing in 1.26 changes this stack; everything recommended here is available in 1.25+. Go 1.27.1 exists as of 2026-09-01 if you want to move up later.
- lib/pq README carries no deprecation notice, but the project has had no feature development for years and its own docs steer nothing toward it; pgx is the actively developed driver and the one sqlc, goose, scs/pgxstore and golang-migrate all have first-class support for.

## Pitfalls

- CrossOriginProtection allows any request that sends NEITHER Sec-Fetch-Site NOR Origin. Your own gvm CLI is exactly such a client. If the CLI is allowed to authenticate with the session cookie, CSRF protection is effectively opt-out for anything that can be made to look header-less. Fix: cookie auth is accepted ONLY on browser routes and ONLY after CrossOriginProtection.Check passes; the CLI authenticates with an Authorization: Bearer token and cookies are ignored on those routes.
- Do not put gorilla/csrf v1.7.2 or older in go.mod (CVE-2025-24358: valid token accepted from any origin). If you use gorilla/csrf at all, note GO-2025-3884: TrustedOrigins ignores the URL scheme, so trusting a host trusts its plaintext http:// origin too.
- Encrypting RCON/join passwords in Postgres is defense-in-depth only, NOT a boundary: the same secrets must be written in cleartext into game config files (e.g. <ServerName>.ini) for the game to use them. Anyone with read access to the instance user's filesystem or /proc has them regardless. The real controls are 0600 files owned by the per-instance user, and Postgres never leaving loopback.
- Never use pgcrypto (pgp_sym_encrypt/encrypt) for this: the key travels in the SQL statement text and lands in the Postgres log, pg_stat_statements, and any query trace. Encrypt in Go, store bytea.
- peer authentication maps the connecting OS uid to the DB role NAME. If gvmd runs as root and the DB role is `gvm`, plain `peer` fails. You need either a pg_ident map (`local gvm gvm peer map=gvmmap` plus `gvmmap root gvm` in pg_ident.conf) or a privilege-dropped DB-touching process. Decide this before writing the installer.
- Ubuntu 22.04 ships systemd 249.11, which predates systemd-creds (v250). Any design that requires LoadCredentialEncrypted= silently loses one of your three declared target platforms. Plain LoadCredential= (v247) is the portable subset.
- goose's global API (goose.Up, goose.SetBaseFS) uses package-level state and is not safe with multiple databases or concurrent use. Use goose.NewProvider. Also: goose needs a *sql.DB, so with pgx you must bridge via stdlib.OpenDBFromPool - and closing that *sql.DB does not close your pool, so don't defer db.Close() expecting cleanup.
- Under RLS with a single application role, the app role usually OWNS the tables, and table owners bypass RLS. Forgetting ALTER TABLE ... FORCE ROW LEVEL SECURITY gives you policies that look enforced and enforce nothing.
- With RLS + pgxpool, using SET instead of SET LOCAL, or issuing SET LOCAL outside an explicit transaction, leaves the tenant/user context on the pooled connection for the next request. This is a silent authorization bypass, not an error.
- argon2.IDKey's `memory` parameter is KiB, not MiB or bytes. Passing 19456 gives 19 MiB (correct); passing 19 gives 19 KiB and a hash worth nothing. The x/crypto docs quote RFC 9106's 2 GiB and 64 MiB figures, which will OOM or stall a small VPS that also runs game servers - use the OWASP m=19456,t=2,p=1 profile and benchmark on the actual VPS.
- bcrypt silently truncates at 72 bytes. A user with a 100-character passphrase gets 72 bytes of it checked, and two different long passwords can collide. If you pick bcrypt, you must pre-hash.
- NewGCMWithRandomNonce has NonceSize()==0; you pass a nil/empty nonce to Seal and Open. Passing a 12-byte nonce as you would with NewGCM panics or errors. Also honour the 2^32-messages-per-key ceiling - derive per-purpose subkeys with HKDF rather than encrypting everything under one key.
- nacl/secretbox with a nonce generated per-call and never recorded is fine, but any code path that reuses a (key, nonce) pair destroys confidentiality AND authenticity. There is no runtime error for this. It's the main reason to prefer NewGCMWithRandomNonce.
- An audit table the application role can UPDATE or DELETE is not an audit log. Own the table with a separate role and GRANT only INSERT, SELECT to the app role, or a compromised app quietly rewrites its own history.
- Redact audit params by allowlist, not denylist. A jsonb params blob that dumps a whole request body will eventually contain a password, an RCON secret, or the CurseForge API key, and it is now in an append-only table you deliberately cannot delete from.
- CGO_ENABLED=0 switches os/user to a pure-Go /etc/passwd and /etc/group parser. Users that exist only through NSS (LDAP, SSSD, systemd-homed) become invisible to user.Lookup. gvm creates local system users so this is fine, but do not rely on user.Lookup as proof a name is unused - check /etc/passwd and let useradd fail authoritatively.
- CGO_ENABLED=0 also switches to the pure-Go DNS resolver, which reads /etc/resolv.conf and /etc/nsswitch.conf itself and ignores NSS modules (mDNS, LDAP hosts). Fine for a VPS talking to api.curseforge.com and steamcmd mirrors; would break a host relying on nss-mdns.
- On Windows, syscall.SysProcAttr has no Credential field and syscall.Setuid/Setgid do not exist. Any privilege-drop, setsid, or unix.Flock code must live in a *_linux.go file with a //go:build linux tag and a non-Linux stub, or the whole module stops compiling on the dev box.
- pgx v5 defaults to QueryExecModeCacheStatement (server-side prepared statements). That is correct against Postgres directly, but if a pgbouncer in transaction mode is ever inserted in front you will get "unnamed prepared statement does not exist" / "bind message has result formats but query has columns". Connect directly over the unix socket and don't introduce pgbouncer.
- A __Host- prefixed cookie requires Secure, Path=/, and NO Domain attribute. If you ever test the dashboard over plain http://localhost the browser silently drops the cookie and login appears to succeed but never sticks. Make the cookie name (and the Secure flag) configurable for local dev, defaulting to secure.

## Recommendations

- Driver: pgx v5 (>= v5.10.0) used directly through pgxpool. Do NOT go through database/sql for application queries. Keep a stdlib.OpenDBFromPool bridge only for goose.
- Query layer: sqlc v1.31.1 with engine: postgresql and sql_package: "pgx/v5". Hand-written SQL, generated typed Go. No ORM - GORM/ent buy nothing here and cost you control over the exact SQL that an audit log has to describe.
- Postgres install: apt install postgresql from the distro (15 on Debian 12, 14 on jammy, 16 on noble). Do NOT add PGDG unless you need a feature from 17/18 - one fewer repo to break on upgrade. Leave listen_addresses=localhost, connect over the unix socket /var/run/postgresql, create a dedicated role gvm owning a dedicated database gvm, plus a separate gvm_migrate role that owns the schema. Never connect as postgres.
- Auth to Postgres: unix socket + peer, with a pg_ident map if gvmd runs as root. This means there is no database password to store anywhere - remove a whole class of secret from the design. Fall back to scram-sha-256 over 127.0.0.1 only if peer proves awkward, and then deliver the password via systemd LoadCredential=.
- Migrations: pressly/goose v3.28.0, SQL-only migrations in //go:embed migrations/*.sql, applied at startup through goose.NewProvider(goose.DialectPostgres, stdlib.OpenDBFromPool(pool), migrationsFS). Ship `gvm db migrate` / `gvm db status` subcommands so the same binary is also the migration CLI - that is the single-static-binary payoff golang-migrate's separate CLI does not give you.
- Passwords: Argon2id via golang.org/x/crypto/argon2.IDKey with OWASP m=19456 (KiB), t=2, p=1, 16-byte random salt, 32-byte output, stored as a PHC string in a text column. Use github.com/alexedwards/argon2id unless you want to hand-roll the encoding. Benchmark on the actual VPS and drop to m=12288,t=3 if p95 login exceeds ~150 ms while game servers are loaded.
- Sessions, not JWT. Server-rendered dashboard + long-lived admin privileges means you need instant revocation, which JWT cannot give you without a revocation table (at which point you have sessions with extra steps). Use scs/v2 + scs/pgxstore. Cookie: name __Host-gvm_session, Secure, HttpOnly, SameSite=Lax, Path=/, no Domain; Lifetime 12h, IdleTimeout 2h, and RenewToken on privilege change and on login.
- CSRF: net/http.NewCrossOriginProtection() wrapping the whole browser mux, no token plumbing. Caddy's Host passthrough makes the Origin/Host comparison correct out of the box. Add AddTrustedOrigin only if the dashboard is ever served from a second hostname. Critically, pair it with the rule that session cookies are only honoured on routes behind that middleware.
- CLI auth: separate api_tokens table. Token = 32 random bytes, base64url, prefixed (gvm_pat_...). Store SHA-256 of the token (high entropy, no Argon2 needed) plus an indexed 8-char lookup prefix, owner user_id, an optional role ceiling that cannot exceed the owner's role, expires_at, last_used_at, revoked_at. Sent as Authorization: Bearer. This is what makes the header-less-request gap in CrossOriginProtection harmless.
- TOTP: pquerna/otp/totp, optional per-user, mandatory for the owner role. Store the base32 secret encrypted (see secrets below), 10 single-use recovery codes stored as SHA-256 hashes, and record a used-step counter per user so a code cannot be replayed inside the skew window.
- RBAC: four roles owner > admin > operator > viewer as a Postgres ENUM. A global role on users, plus a per-instance override table instance_grants(user_id, instance_id, role) with a composite PK. Effective role for (user, instance) = greatest(global, grant). owner: everything incl. user management and instance deletion. admin: create/delete instances, edit secrets. operator: start/stop/restart/backup/update/mod-sync on granted instances, no secret reads. viewer: read status, logs, player lists; secrets always redacted.
- Enforce authorization in exactly one place by making it impossible to reach the data layer without it: handlers never receive *db.Queries. They receive a scoped service obtained from svc.ForActor(actor).Instance(ctx, id), whose constructor performs the role check and whose every method re-asserts a declared Permission against a single package-level matrix map[Role]map[Permission]bool. Add a test that walks the router (chi.Walk) and fails if any registered route has no declared permission - that test is what actually keeps checks from scattering.
- Skip Postgres RLS for v1. You have one trusted application, one DB role, and an authorization model (effective-role-per-instance with a max() of two sources) that expresses badly as a policy predicate. RLS would additionally require FORCE ROW LEVEL SECURITY, a SET LOCAL inside every transaction, and would make debugging silent empty result sets. Revisit only if you ever expose SQL access to a second consumer.
- Secrets: AES-256-GCM via crypto/cipher.NewGCMWithRandomNonce over a per-purpose subkey derived with crypto/hkdf from a single 32-byte master key read from /etc/gvm/master.key (0400 root:root), ciphertext stored in bytea columns with a key_id column for rotation. Bind each ciphertext to its row with AAD ("instances:rcon_password:<uuid>") so a row's ciphertext cannot be copied onto another row.
- Deliver the master key to the daemon with systemd LoadCredential=master_key:/etc/gvm/master.key (v247, works on all three targets) and read it from $CREDENTIALS_DIRECTORY/master_key rather than opening /etc/gvm directly. Optionally, on Debian 12 / Ubuntu 24.04 only, upgrade to LoadCredentialEncrypted= - but treat it as a hardening extra, never a requirement.
- Reject age and nacl/secretbox for column encryption: age is stream/file oriented with per-file header overhead, secretbox makes you own nonce uniqueness. Do use filippo.io/age for one thing though - encrypting offsite backup archives to an X25519 recipient public key, so the VPS can encrypt backups it cannot itself decrypt. That is a genuine security win the old backup.sh does not have.
- Audit log: an append-only audit_log table owned by gvm_migrate, with only INSERT and SELECT granted to gvm. Columns: id bigserial, at timestamptz default now(), request_id, actor_kind (user|token|system|timer), actor_user_id, actor_token_id, actor_ip inet, instance_id, action text, target text, params jsonb (allowlist-redacted), outcome (pending|ok|error), error text, duration_ms. For side-effectful operations write the pending row in the request transaction and UPDATE... no: insert a second row referencing the first, so the table stays insert-only. Mirror everything to journald via log/slog's JSONHandler so there is an off-database copy.
- Also audit-log the things the old bash scripts did silently: rebuild.sh's archive-before-destroy, nightly restart timer firing, mod list changes with the exact before/after workshop ID sets, RCON password rotation, and the PZ adoption/import path. Those are precisely the actions where 'who did this and when' matters.
- Build: CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=$VER -X main.commit=$SHA" -o gvm ./cmd/gvm. No -extldflags -static, no netgo/osusergo tags needed - those are for CGO_ENABLED=1. Add -tags osusergo,netgo only as documentation of intent.
- Structure Linux-only code behind interfaces with //go:build linux implementations and //go:build !linux stubs that return errors: systemd unit management, useradd/privilege drop (SysProcAttr.Credential), ufw, unix.Flock instance locks, unix.Statfs disk checks. Keep golang.org/x/sys/unix out of any file that compiles on Windows so `go build ./... && go vet ./...` stays green on the dev machine.

## Detail

# gvm: Go + Postgres + RBAC + web-auth stack (verified Sept 2026)

I read `C:/projectZomboid/lib/common.sh`, `config.env`, and `systemd/zomboid.service.template` for grounding. Two things from there constrain this research directly: (a) `config.env` currently holds `SERVER_PASSWORD`, `ADMIN_PASSWORD`, `RCON_PASSWORD` in cleartext next to the scripts — that's what section 5 has to replace; (b) the `ExecStop` screen-stuffed `save`/`quit` + `KillSignal=SIGCONT` + `TimeoutStopSec=180` trick means gvm's stop path is long-running and asynchronous, which shapes the audit-log design in section 6.

---

## 1. Postgres access layer

### Driver: pgx v5, used natively

**`github.com/jackc/pgx/v5` v5.10.0** (2026-06-03). Requires Go 1.25+, PostgreSQL 14+. Pure Go — no cgo, so it costs nothing against the `CGO_ENABLED=0` constraint.

Pin `>= v5.10.0`, not "whatever v5". Two recent security-relevant releases:
- **v5.9.2** (2026-04-18): SQL injection via placeholder confusion with dollar-quoted string literals.
- **v5.10.0** (2026-06-03): hardening against malicious/compromised Postgres servers — auth-method restrictions and binary-decoder protections.

Also useful: **v5.9.0** (2026-03-21) added SCRAM-SHA-256-PLUS, OAuth, and PostgreSQL protocol 3.2; **v5.8.0** (2025-12-26) dropped the `golang.org/x/crypto` dependency.

**Ranking for gvm:**

| Option | Verdict |
|---|---|
| pgx v5 + pgxpool (native) | **Use this.** Native `LISTEN/NOTIFY` (useful for pushing instance state changes to the dashboard over SSE), `COPY`, real `jsonb`/`inet`/`uuid` types via pgtype, no `database/sql` interface tax. |
| database/sql + lib/pq | No. `lib/pq` has had no feature development in years; its README doesn't even mention alternatives. Every tool you want (sqlc, goose, scs) has first-class pgx support. |
| database/sql + pgx stdlib | Only as a bridge for goose. Not for app queries. |
| sqlc | **Use, layered on pgx.** v1.31.1 (2026-04-22). `sql_package: "pgx/v5"` emits pgx-native code. |
| ORM (GORM/ent/bun) | No. You're going to be writing audit records describing exactly what changed; you want the SQL to be literal and reviewable. |

`sqlc.yaml`:

```yaml
version: "2"
sql:
  - engine: postgresql
    schema: "internal/db/migrations"
    queries: "internal/db/queries"
    gen:
      go:
        package: "dbgen"
        out: "internal/db/dbgen"
        sql_package: "pgx/v5"
        emit_pointers_for_null_types: true
        emit_prepared_queries: false
```

sqlc reads the same `migrations/` directory goose applies, so schema and generated code cannot drift.

### Running Postgres on the same VPS

**Use the distro package. Do not add PGDG unless you need a specific 17/18 feature.**

| Target | `postgresql` metapackage pulls |
|---|---|
| Debian 12 bookworm | `postgresql-15` (`15+248+deb12u1`) |
| Ubuntu 22.04 jammy | `postgresql-14` |
| Ubuntu 24.04 noble | `postgresql-16` |

All ≥ 14, so all satisfy pgx's floor and all default to `scram-sha-256`. PostgreSQL 18 (released 2025-09-25, current minor 18.6 as of 2026-08-13) is available from PGDG if wanted; PG19 was in beta mid-2026. For a box whose real job is running game servers, the distro version is the right call — one fewer third-party apt repo to break on `do-release-upgrade`.

If you do want PGDG:
```bash
apt install -y postgresql-common
/usr/share/postgresql-common/pgdg/apt.postgresql.org.sh
```
(Manual: key from `https://www.postgresql.org/media/keys/ACCC4CF8.asc` into `/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc`, deb822 stanza in `/etc/apt/sources.list.d/pgdg.sources` with `Suites: <codename>-pgdg`. Supported: jammy, noble, resolute, stonking.)

**Paths (Debian-packaged, all three targets):**
- config: `/etc/postgresql/<VER>/main/postgresql.conf`, `.../pg_hba.conf`, `.../pg_ident.conf`
- data: `/var/lib/postgresql/<VER>/main`
- socket dir: `/var/run/postgresql`
- port: `5432`

**Bind to localhost only — which is already the default.** Upstream `listen_addresses` defaults to `localhost`; Debian keeps it. Don't touch it. Verify in the installer:

```bash
ss -lntp | grep -E ':5432' | grep -v '127.0.0.1\|::1' && die "postgres is listening on a public interface"
```

Then don't open 5432 in ufw. Your existing `firewall.sh` SSH-port-autodetect logic carries over unchanged; gvm's firewall module just adds per-instance game ports (PZ: 16261/tcp+udp, 16262/udp; RCON 27015/tcp stays closed to the world) plus 80/443 for Caddy.

**Role/database setup — no password anywhere.** `pg_hba.conf` records are `TYPE DATABASE USER [ADDRESS] METHOD [OPTIONS]`, first match wins, and a failed match does **not** fall through to later records. `local` means unix socket (no ADDRESS field) and `peer` reads the client's OS uid, requiring it to equal the DB role name.

```bash
# installer
adduser --system --group --home /var/lib/gvm --shell /usr/sbin/nologin gvm
sudo -u postgres createuser gvm_migrate            # owns the schema
sudo -u postgres createuser gvm                     # runtime app role
sudo -u postgres createdb -O gvm_migrate gvm
sudo -u postgres psql -d gvm -c "GRANT CONNECT ON DATABASE gvm TO gvm;"
sudo -u postgres psql -d gvm -c "GRANT USAGE ON SCHEMA public TO gvm;"
```

Neither role gets `SUPERUSER`, `CREATEDB`, `CREATEROLE`, or `BYPASSRLS`.

`pg_hba.conf` — put these **above** the shipped `local all all peer` line:

```
# TYPE  DATABASE  USER          ADDRESS  METHOD  OPTIONS
local   gvm       gvm                    peer    map=gvmmap
local   gvm       gvm_migrate            peer    map=gvmmap
```

`pg_ident.conf` — needed because the gvm daemon runs as root (it manages systemd units, creates per-instance users, writes ufw rules):

```
# MAPNAME  SYSTEM-USERNAME  PG-USERNAME
gvmmap     root             gvm
gvmmap     root             gvm_migrate
gvmmap     gvm              gvm
```

Connection string (either form works with `pgxpool.New`):
```
postgres://gvm@/gvm?host=/var/run/postgresql&pool_max_conns=8&pool_max_conn_lifetime=1h&pool_max_conn_idle_time=30m
```

**This is the single highest-leverage decision in the whole document:** peer auth over the unix socket means there is no database password to store, rotate, leak, or put in `config.env`. It deletes an entire secret from the design.

`postgresql.conf` tuning for a box whose RAM belongs to JVMs: `shared_buffers = 128MB`, `max_connections = 50`, `work_mem = 4MB`. The 8 GB VPS in your `config.env` is already committing most of its memory to `JVM_XMX`.

**Do not put pgbouncer in front.** pgx v5 defaults to `QueryExecModeCacheStatement` (server-side prepared statements), which is correct against Postgres directly but breaks under transaction-mode pooling.

---

## 2. Migrations

**Use `github.com/pressly/goose/v3` v3.28.0** (2026-09-02).

The API you want is the **non-global** provider:

```go
func NewProvider(dialect Dialect, db *sql.DB, fsys fs.FS, opts ...ProviderOption) (*Provider, error)
```

`fsys` takes an `embed.FS` directly (or `fs.Sub` to strip a prefix). Avoid the legacy globals (`goose.SetBaseFS`, `goose.Up`) — package-level state, not safe with multiple DBs or concurrent use.

```go
//go:embed migrations/*.sql
var migrationsFS embed.FS

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
    sub, err := fs.Sub(migrationsFS, "migrations")
    if err != nil { return err }

    db := stdlib.OpenDBFromPool(pool)   // does NOT take ownership of the pool
    p, err := goose.NewProvider(goose.DialectPostgres, db, sub)
    if err != nil { return err }

    results, err := p.Up(ctx)
    for _, r := range results {
        slog.Info("migration applied", "version", r.Source.Version, "file", r.Source.Path, "ms", r.Duration.Milliseconds())
    }
    return err
}
```

`stdlib.OpenDBFromPool(pool *pgxpool.Pool, opts ...OptionOpenDB) *sql.DB` — note two documented behaviours: closing the returned `*sql.DB` does **not** close the pool, and it forces `MaxIdleConns` to 0 since idling is the pool's job.

**Comparison:**

| Tool | Version | embed.FS | Verdict |
|---|---|---|---|
| **goose v3** | v3.28.0 (2026-09-02) | `NewProvider(..., fs.FS, ...)` | **Pick.** Actively released, clean provider API, Go migrations available if you ever need a data backfill that SQL can't express. |
| golang-migrate v4 | v4.19.1 (2025-11-29) | `source/iofs` + `database/pgx/v5` | Fine, but slower release cadence, and its value proposition (language-agnostic CLI) is worthless when the migrations ship inside your Go binary. |
| jackc/tern v2 | — | `migrate` library | Postgres-only, nice "database code" (functions/views) workflow, but a much smaller ecosystem. |
| Hand-rolled version table | — | trivially | Don't. You'll reimplement advisory locking, dirty-state detection, and down-migrations badly. |

**Ship the migrator as subcommands of the same binary** — `gvm db migrate`, `gvm db status`, `gvm db version`. That's the actual single-static-binary payoff, and it's the thing golang-migrate's separate CLI doesn't give you.

Run migrations as `gvm_migrate` (which owns the schema), then have the daemon connect as `gvm`. Set `ALTER DEFAULT PRIVILEGES FOR ROLE gvm_migrate IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO gvm;` **before** the first migration, or new tables won't be reachable by the app role. This split is also what makes the audit log genuinely append-only (§6).

---

## 3. AuthN

### Password hashing

OWASP's current Password Storage Cheat Sheet lists these as equivalent Argon2id configurations:

| memory | iterations | parallelism |
|---|---|---|
| 47104 KiB (46 MiB) | 1 | 1 |
| **19456 KiB (19 MiB)** | **2** | **1** |
| 12288 KiB (12 MiB) | 3 | 1 |
| 9216 KiB | 4 | 1 |
| 7168 KiB | 5 | 1 |

**Use m=19456, t=2, p=1**, 16-byte salt, 32-byte output.

```go
func IDKey(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte
```

`memory` is **KiB**. `time` is passes. Note the `x/crypto/argon2` docs quote RFC 9106's recommendations — t=1/m=2 GiB/p=4, or t=3/m=64 MiB/p=4 — which are sized for standalone KDF use, not for a login endpoint on a VPS whose RAM is committed to a JVM heap. Ignore them; use the OWASP web-app profile and benchmark on the real box under load.

Store the **PHC string**, not raw bytes: `$argon2id$v=19$m=19456,t=2,p=1$<b64salt>$<b64hash>`. It's self-describing, so you can raise parameters later and silently re-hash each user on their next successful login. `github.com/alexedwards/argon2id` gives you `CreateHash(password string, params *Params) (string, error)` and `ComparePasswordAndHash(password, hash string) (bool, error)` (constant-time) with exactly that format — about 120 lines you don't have to review.

**bcrypt:** only if Argon2's memory cost is genuinely unacceptable. Minimum work factor 10, and a hard **72-byte** input limit — longer passwords are silently truncated. If you use it, pre-hash per OWASP: `bcrypt(base64(hmac-sha384(password, pepper)), salt, cost)`. Not worth the complexity; use Argon2id.

### Sessions, not JWT

For a server-rendered dashboard controlling a live game server, **server-side sessions**. The deciding argument is revocation: an admin who removes a user's `operator` grant needs it to take effect now, not at token expiry. JWT gets there only via a revocation table — i.e. sessions with extra latency and more code.

**`github.com/alexedwards/scs/v2` + `github.com/alexedwards/scs/pgxstore`.** pgxstore is pgxpool-native:

```go
func New(pool *pgxpool.Pool) *PostgresStore
func NewWithCleanupInterval(pool *pgxpool.Pool, cleanupInterval time.Duration) *PostgresStore
func NewWithConfig(pool *pgxpool.Pool, config Config) *PostgresStore   // Config{CleanUpInterval, TableName}
```

Required DDL (put it in migration 0001):

```sql
CREATE TABLE sessions (
    token  TEXT PRIMARY KEY,
    data   BYTEA NOT NULL,
    expiry TIMESTAMPTZ NOT NULL
);
CREATE INDEX sessions_expiry_idx ON sessions (expiry);
```

Cleanup goroutine defaults to every 5 minutes.

```go
sm := scs.New()
sm.Store = pgxstore.New(pool)
sm.Lifetime = 12 * time.Hour
sm.IdleTimeout = 2 * time.Hour
sm.Cookie.Name = "__Host-gvm_session"
sm.Cookie.Path = "/"          // required by __Host-
sm.Cookie.Domain = ""         // required by __Host-
sm.Cookie.Secure = true       // required by __Host-
sm.Cookie.HttpOnly = true
sm.Cookie.SameSite = http.SameSiteLaxMode
sm.Cookie.Persist = false
```

The `__Host-` prefix forces Secure + `Path=/` + no `Domain`, so the cookie is host-locked and cannot be set by a sibling subdomain. Caddy's automatic HTTPS makes `Secure` free in production — but make the cookie name and `Secure` flag config-overridable for `http://localhost` dev, defaulting to secure, or your first local login will silently fail to stick.

Call `sm.RenewToken(ctx)` on login **and** on any privilege change (session fixation).

### CSRF: use the standard library

**Go 1.25 added `net/http.CrossOriginProtection`.** Exact API:

```go
func NewCrossOriginProtection() *CrossOriginProtection
func (c *CrossOriginProtection) AddTrustedOrigin(origin string) error
func (c *CrossOriginProtection) AddInsecureBypassPattern(pattern string)
func (c *CrossOriginProtection) SetDenyHandler(h Handler)
func (c *CrossOriginProtection) Check(req *Request) error
func (c *CrossOriginProtection) Handler(h Handler) Handler
```

Logic: GET/HEAD/OPTIONS always allowed. `Sec-Fetch-Site: same-origin` or `none` allowed. If `Sec-Fetch-Site` is absent, it compares the `Origin` header's host against `Host`. Default rejection is **403**.

**This works behind Caddy without configuration**, and that is not an accident worth glossing over: Caddy's `reverse_proxy` passes incoming headers — *including `Host`* — through unmodified, setting only `X-Forwarded-For`, `X-Forwarded-Proto`, and `X-Forwarded-Host`. (Since v2.11.0 it rewrites `Host` only when the upstream itself is HTTPS — yours will be plain HTTP on loopback.) nginx's default `proxy_set_header Host $proxy_host` would break the Origin/Host comparison; Caddy's default doesn't. Your constraint #2 pays off here.

**Do not use `gorilla/csrf`.** v1.7.2 and older accepted a valid token from *any* origin (CVE-2025-24358). v1.7.3 added same-origin enforcement, but GO-2025-3884 then showed `TrustedOrigins` ignores the URL scheme — adding a host trusts its plaintext `http://` origin too. Upstream guidance is to migrate to `net/http.CrossOriginProtection`; `filippo.io/csrf` is the pre-1.25 backport and `filippo.io/csrf/gorilla` a drop-in for the old API. You're on Go 1.26; use the stdlib.

**The gap you must close.** `CrossOriginProtection` allows any request carrying *neither* `Sec-Fetch-Site` *nor* `Origin`, on the assumption it isn't from a browser. Your own `gvm` CLI is exactly such a client. If cookie auth is accepted on those requests, CSRF protection becomes opt-out. So:

- Browser routes (`/`, `/instances/...`): wrapped in `cop.Handler`, session cookie honoured.
- API routes (`/api/v1/...`): **cookies ignored entirely**, `Authorization: Bearer` only.

```go
browser := http.NewServeMux()   // dashboard
cop := http.NewCrossOriginProtection()
// cop.AddTrustedOrigin("https://gvm.example.net")  // only if a second hostname is served
root.Handle("/", sm.LoadAndSave(cop.Handler(browser)))
root.Handle("/api/v1/", bearerAuth(apiMux))   // no LoadAndSave, no cookies
```

### CLI tokens

```sql
CREATE TABLE api_tokens (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         text NOT NULL,
    prefix       text NOT NULL UNIQUE,      -- first 8 chars, indexed lookup
    token_sha256 bytea NOT NULL,            -- SHA-256 of the full token
    role_ceiling role_t,                    -- NULL = inherit user's role
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz,
    last_used_at timestamptz,
    revoked_at   timestamptz
);
```

Token = 32 random bytes, base64url, displayed once as `gvm_pat_<prefix><rest>`. Store SHA-256 — the token is already 256 bits of entropy, so Argon2 buys nothing and costs 100 ms per API call. Compare with `crypto/subtle.ConstantTimeCompare`. `role_ceiling` is clamped: `min(user.effective_role, token.role_ceiling)`.

### TOTP 2FA — feasible and cheap

**`github.com/pquerna/otp/totp`**, validated against RFC 6238 test vectors.

```go
key, err := totp.Generate(totp.GenerateOpts{Issuer: "gvm", AccountName: user.Email})
// key.Secret() -> base32, key.URL() -> otpauth:// for the QR code
ok := totp.Validate(code, secret)   // Google-Authenticator-compatible shortcut
```

`totp.Validate` is a shortcut for `ValidateCustom` with period 30 s, **skew 1**, 6 digits, SHA1. `ValidateOpts{Period, Skew, Digits, Algorithm}` gives control. The docs explicitly warn that `Skew > 1` is "likely sketchy" — leave it at 1.

Make TOTP mandatory for `owner`, optional elsewhere. Store the base32 secret **encrypted** (§5) in a `bytea` column, since a leaked DB dump with plaintext TOTP secrets defeats the whole point. Ten single-use recovery codes stored as SHA-256 hashes. Store a `last_totp_step bigint` per user and reject any code whose step ≤ the last accepted one — otherwise a code stays replayable for the full ±30 s skew window.

Since gvm renders QR codes server-side and you're on `CGO_ENABLED=0`, use a pure-Go QR encoder and inline the PNG as a data URI; `key.Image(w, h)` from pquerna/otp does this already.

---

## 4. AuthZ

### Role model

Four ordered roles, per-instance scoped:

| Role | Can |
|---|---|
| `owner` | Everything: user management, role grants, instance deletion, secret rotation, host-level config. Exactly one, TOTP required. |
| `admin` | Create/delete instances, read/write secrets, edit firewall, manage backups, grant `operator`/`viewer`. |
| `operator` | On **granted instances only**: start / stop / restart / update / mod-sync / backup / RCON command. Cannot read secrets, cannot delete an instance, cannot rebuild. |
| `viewer` | Read status, logs, player lists, mod lists. Secrets always rendered as `••••`. |

### SQL

```sql
CREATE TYPE role_t AS ENUM ('viewer','operator','admin','owner');   -- ordinal ordering matters

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email         citext NOT NULL UNIQUE,
    password_hash text   NOT NULL,               -- PHC argon2id string
    global_role   role_t NOT NULL DEFAULT 'viewer',
    totp_secret   bytea,                         -- encrypted; NULL = 2FA off
    totp_key_id   int,
    last_totp_step bigint NOT NULL DEFAULT 0,
    disabled_at   timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE instances (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug      text NOT NULL UNIQUE,              -- systemd unit name component
    game      text NOT NULL,                     -- 'pz' | 'minecraft-cf' | ...
    unix_user text NOT NULL,
    ...
);

CREATE TABLE instance_grants (
    user_id     uuid   NOT NULL REFERENCES users(id)     ON DELETE CASCADE,
    instance_id uuid   NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    role        role_t NOT NULL,
    granted_by  uuid   REFERENCES users(id),
    granted_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, instance_id)
);
```

Effective role for `(user, instance)` = `GREATEST(global_role, COALESCE(grant.role, 'viewer'))`, which the enum's ordinal ordering makes a one-line SQL expression. An `admin` needs no explicit grants; an `operator` needs one per instance.

### Enforce in one place

Scattered `if role != admin` checks are the failure mode. Two mechanisms:

**(a) A single permission matrix.** One package-level table, and the only place a role is ever compared:

```go
package authz

type Permission string

const (
    InstanceView    Permission = "instance:view"
    InstanceStart   Permission = "instance:start"
    InstanceStop    Permission = "instance:stop"
    InstanceRebuild Permission = "instance:rebuild"
    InstanceDelete  Permission = "instance:delete"
    SecretRead      Permission = "secret:read"
    SecretWrite     Permission = "secret:write"
    UserManage      Permission = "user:manage"
    ModSync         Permission = "mod:sync"
    // ...
)

var matrix = map[Role]map[Permission]bool{
    RoleViewer:   {InstanceView: true},
    RoleOperator: {InstanceView: true, InstanceStart: true, InstanceStop: true, ModSync: true},
    RoleAdmin:    {/* operator ∪ */ InstanceRebuild: true, InstanceDelete: true, SecretRead: true, SecretWrite: true},
    RoleOwner:    {/* admin ∪ */ UserManage: true},
}
```

**(b) Make the data layer unreachable without it.** Handlers never receive `*dbgen.Queries`. They receive a service scoped to an actor, and the scoping constructor is where the check lives:

```go
// svc.ForActor(a).Instance(ctx, id) resolves the effective role ONCE and
// returns a handle whose every method re-asserts its declared permission.
func (s *Service) ForActor(a Actor) *ActorScope { return &ActorScope{s: s, a: a} }

func (sc *ActorScope) Instance(ctx context.Context, id uuid.UUID) (*InstanceHandle, error) {
    eff, err := sc.s.effectiveRole(ctx, sc.a, id)   // one query, GREATEST(...)
    if err != nil { return nil, err }
    if !authz.Can(eff, authz.InstanceView) { return nil, authz.ErrForbidden }
    return &InstanceHandle{sc: sc, id: id, role: eff}, nil
}

func (h *InstanceHandle) Stop(ctx context.Context, reason string) error {
    if err := h.require(authz.InstanceStop); err != nil { return err }
    // ... audit, then screen-stuff save/quit via systemctl stop
}
```

**(c) A test that catches drift.** Walk the router and fail if any registered route lacks a declared permission:

```go
func TestEveryRouteDeclaresAPermission(t *testing.T) {
    chi.Walk(router, func(method, route string, h http.Handler, mw ...func(http.Handler) http.Handler) error {
        if _, ok := routePermissions[method+" "+route]; !ok && !publicRoutes[route] {
            t.Errorf("route %s %s declares no permission", method, route)
        }
        return nil
    })
}
```

That test, not discipline, is what keeps the checks in one place six months from now.

### Row-level security: skip it for v1

RLS is real and works: `ALTER TABLE t ENABLE ROW LEVEL SECURITY` is default-deny, and

```sql
CREATE POLICY p ON t [TO role] [FOR ALL|SELECT|INSERT|UPDATE|DELETE]
    [AS PERMISSIVE|RESTRICTIVE] USING (visible_expr) [WITH CHECK (write_expr)];
```

with permissive policies OR'd and restrictive AND'd.

**Three reasons it's the wrong trade here:**

1. **Owner bypass.** Superusers and `BYPASSRLS` roles always bypass, and so does the **table owner** unless you add `ALTER TABLE t FORCE ROW LEVEL SECURITY`. Miss that and you have policies that look enforced and enforce nothing. (The `gvm_migrate`/`gvm` role split in §1 actually helps — `gvm` doesn't own the tables — but it's one more invariant to hold.)
2. **The `SET LOCAL` discipline.** Context must be set with `SET LOCAL` (or `set_config(k, v, true)`) **inside an explicit transaction**, because plain `SET` persists on the pooled backend and leaks into the next request. `SET LOCAL` is discarded at COMMIT/ROLLBACK. Every read path in gvm would have to open a transaction it otherwise doesn't need, and a single missed one is a silent authorization bypass with no error.
3. **Poor fit for the model.** Your effective role is `GREATEST(global_role, grant_role)` across two tables. As a policy predicate that's a correlated subquery on every row of every query — ugly to write, ugly to debug (wrong answers appear as *empty result sets*, not errors), and a planner cost on tables that are tiny anyway.

RLS earns its keep when untrusted parties hold DB credentials or a second consumer queries the same database. You have one trusted app and one role. Revisit only if that changes. If you ever do, the pattern is:

```sql
ALTER TABLE instances ENABLE ROW LEVEL SECURITY;
ALTER TABLE instances FORCE ROW LEVEL SECURITY;
CREATE POLICY instance_visibility ON instances FOR SELECT
  USING (current_setting('gvm.actor_role', true) IN ('admin','owner')
     OR  EXISTS (SELECT 1 FROM instance_grants g
                 WHERE g.instance_id = instances.id
                   AND g.user_id = current_setting('gvm.actor_id', true)::uuid));
```

...with every request wrapped in `BEGIN; SET LOCAL gvm.actor_id = ...; ... COMMIT;`.

---

## 5. Secrets

### What has to be held

RCON passwords (per instance), server join passwords, admin passwords, a CurseForge API key (host-global), possibly Steam credentials for `steamcmd +login`. Today these sit in cleartext in `config.env` next to the scripts.

### Be honest about the threat model first

RCON and join passwords **must be written in cleartext into game config files** — `$DATA_DIR/Server/<SERVER_NAME>.ini` for PZ, `server.properties` for Minecraft — because the game process reads them. Encrypting them in Postgres does **not** create a boundary against someone with filesystem access. It protects against: a DB backup leaking, a SQL injection reaching the secrets table, an operator-role user reading rows they shouldn't, and `pg_dump` output landing in the wrong place. Those are real, and worth defending. Just don't oversell it.

The actual boundaries are: per-instance unix users, `0600` config files, `NoNewPrivileges=true`/`PrivateTmp=true` in the unit (already in your template), and Postgres never leaving loopback.

### Compare the three options

| Approach | Verdict |
|---|---|
| **Encrypted-at-rest in Postgres, app key from a root-only file** | **Use this for per-instance and user-entered secrets.** Handles secrets created at runtime through the dashboard, supports rotation, one storage mechanism for everything, survives `pg_dump` leaks. |
| **systemd credentials** | **Use this to deliver the master key**, not the individual secrets. `LoadCredential=` exists since **v247** — available on all three targets. But `systemd-creds` and `LoadCredentialEncrypted=`/`SetCredentialEncrypted=` are **v250**, and **Ubuntu 22.04 ships systemd 249.11** — so encrypted credentials silently exclude one of your declared targets. (Debian 12: 252.39. Ubuntu 24.04: 255.) Also: credentials are unit-file configuration; the dashboard can't create a new RCON password at runtime by editing a unit file and reloading systemd. |
| **Plain files, 0600** | Where the secrets end up anyway (game config files). Fine as the *materialized* form; not the system of record. |

Do **not** use `pgcrypto` (`pgp_sym_encrypt`, `encrypt`): the key travels in the SQL statement text and lands in the Postgres log, `pg_stat_statements`, and any query trace.

### Crypto choice

**AES-256-GCM via `crypto/cipher.NewGCMWithRandomNonce`** (Go 1.24+):

```go
func NewGCMWithRandomNonce(cipher Block) (AEAD, error)
```

It generates a random 96-bit nonce, **prepends it to the ciphertext** in `Seal`, and extracts it in `Open`. `NonceSize()` is **0**; `Overhead()` is **28** bytes (12 nonce + 16 tag). The `Block` must come from `crypto/aes.NewCipher`. Hard limit: **one key must not encrypt more than 2^32 messages** — hence HKDF-derived per-purpose subkeys.

Rejected alternatives:
- **`nacl/secretbox`**: `Seal(out, message []byte, nonce *[24]byte, key *[32]byte)` / `Open(...) ([]byte, bool)`, 32-byte key, 24-byte nonce, XSalsa20-Poly1305. Correct crypto, but nonce uniqueness is entirely the caller's problem and there is no runtime error when you get it wrong. `NewGCMWithRandomNonce` deletes that footgun, and AES-GCM gets AES-NI acceleration.
- **`filippo.io/age`**: `Encrypt`/`Decrypt` with `X25519Recipient`/`X25519Identity`, or `NewScryptRecipient(password)` + `SetWorkFactor(logN)`. File/stream oriented with a per-file header — wrong shape for a `bytea` column. **But use it for one thing:** encrypt offsite backup archives to an X25519 *recipient public key* stored on the VPS, so the box can produce backups it cannot itself decrypt. That's a genuine improvement over `backup.sh`.

### Go sketch

```go
// internal/secrets/box.go
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"   // Go 1.24+ stdlib
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
)

// Box holds the master key and derives per-purpose AEADs from it.
type Box struct {
	keyID  int
	master []byte
}

// Load reads the 32-byte master key. Refuses group/other-readable files.
func Load(path string, keyID int) (*Box, error) {
	fi, err := os.Stat(path)
	if err != nil { return nil, err }
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("secrets: %s is mode %04o; want 0400 or 0600", path, fi.Mode().Perm())
	}
	k, err := os.ReadFile(path)
	if err != nil { return nil, err }
	if len(k) < 32 { return nil, errors.New("secrets: master key must be at least 32 bytes") }
	return &Box{keyID: keyID, master: k}, nil
}

// LoadFromSystemd prefers the systemd-delivered credential, falls back to /etc.
func LoadFromSystemd(name, fallback string, keyID int) (*Box, error) {
	if d := os.Getenv("CREDENTIALS_DIRECTORY"); d != "" {
		if b, err := Load(d+"/"+name, keyID); err == nil { return b, nil }
	}
	return Load(fallback, keyID)
}

func (b *Box) aead(purpose string) (cipher.AEAD, error) {
	// One master key -> independent subkey per purpose. Keeps each key well
	// under the 2^32-message ceiling and contains the blast radius.
	dk, err := hkdf.Key(sha256.New, b.master, nil, "gvm/v1/"+purpose, 32)
	if err != nil { return nil, err }
	blk, err := aes.NewCipher(dk)
	if err != nil { return nil, err }
	return cipher.NewGCMWithRandomNonce(blk) // nonce is prepended by Seal
}

// Seal binds the ciphertext to its row via AAD, so a ciphertext moved to a
// different row (or column) fails to open. aad e.g.
// "instances:rcon_password:8f3c...".
func (b *Box) Seal(purpose, aad string, plaintext []byte) ([]byte, error) {
	a, err := b.aead(purpose)
	if err != nil { return nil, err }
	return a.Seal(nil, nil, plaintext, []byte(aad)), nil // NonceSize()==0 -> nil nonce
}

func (b *Box) Open(purpose, aad string, ciphertext []byte) ([]byte, error) {
	a, err := b.aead(purpose)
	if err != nil { return nil, err }
	pt, err := a.Open(nil, nil, ciphertext, []byte(aad))
	if err != nil { return nil, fmt.Errorf("secrets: open %s: %w", purpose, err) }
	return pt, nil
}

func (b *Box) KeyID() int { return b.keyID }

func NewMasterKey() ([]byte, error) {
	k := make([]byte, 32)
	_, err := rand.Read(k)
	return k, err
}
```

`hkdf.Key[H hash.Hash](h func() H, secret, salt []byte, info string, keyLength int) ([]byte, error)` — stdlib since Go 1.24, no extra dependency.

Storage:

```sql
CREATE TABLE instance_secrets (
    instance_id uuid  NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    name        text  NOT NULL,          -- 'rcon_password' | 'join_password' | 'admin_password'
    ciphertext  bytea NOT NULL,          -- nonce||ct||tag
    key_id      int   NOT NULL,          -- for rotation
    updated_at  timestamptz NOT NULL DEFAULT now(),
    updated_by  uuid REFERENCES users(id),
    PRIMARY KEY (instance_id, name)
);
```

AAD = `"instance_secrets:" || name || ":" || instance_id`. Host-global secrets (CurseForge API key, Steam credentials) go in a parallel `host_secrets(name, ciphertext, key_id, ...)` table with AAD `"host_secrets:" || name`.

Installer + unit:

```bash
install -d -m 0700 -o root -g root /etc/gvm
gvm keygen > /etc/gvm/master.key      # 32 random bytes
chmod 0400 /etc/gvm/master.key
```

```ini
[Service]
User=root
LoadCredential=master_key:/etc/gvm/master.key
ExecStart=/usr/local/bin/gvm serve
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ProtectHome=read-only
```

The daemon reads `$CREDENTIALS_DIRECTORY/master_key`. Credentials land under `/run/credentials/gvm.service/`, access restricted to the service's user, not propagated down the process tree, cleaned up on stop, 1 MiB total limit per service. On Debian 12 / Ubuntu 24.04 you can optionally upgrade to `LoadCredentialEncrypted=` (`systemd-creds encrypt --name=master_key /etc/gvm/master.key /etc/gvm/master.key.cred`, key at `/var/lib/systemd/credential.secret` and/or TPM2) — treat that as opt-in hardening, never a requirement, since jammy can't do it.

**Rotation:** `gvm secrets rotate-key` writes a new master key with an incremented `key_id`, then re-seals every row under it in one transaction. The `key_id` column is what makes this a five-minute operation instead of a migration project.

**Never log a decrypted secret.** Give the type a `String() string { return "•••" }` and a `LogValue() slog.Value` returning the same, so an accidental `slog.Info("x", "secret", s)` is safe.

---

## 6. Audit logging

```sql
CREATE TABLE audit_log (
    id           bigserial PRIMARY KEY,
    at           timestamptz NOT NULL DEFAULT now(),
    request_id   text,
    actor_kind   text NOT NULL,          -- 'user' | 'token' | 'system' | 'timer'
    actor_user_id  uuid REFERENCES users(id),
    actor_token_id uuid REFERENCES api_tokens(id),
    actor_ip     inet,
    instance_id  uuid REFERENCES instances(id) ON DELETE SET NULL,
    instance_slug text,                  -- denormalised: survives instance deletion
    action       text NOT NULL,          -- 'instance.stop', 'secret.rotate', 'mods.sync'
    target       text,
    params       jsonb NOT NULL DEFAULT '{}'::jsonb,   -- allowlist-redacted
    outcome      text NOT NULL,          -- 'started' | 'ok' | 'error'
    error        text,
    duration_ms  integer,
    prev_hash    bytea,                  -- optional tamper-evident chain
    row_hash     bytea
);
CREATE INDEX audit_log_at_idx          ON audit_log (at DESC);
CREATE INDEX audit_log_instance_at_idx ON audit_log (instance_id, at DESC);
CREATE INDEX audit_log_actor_at_idx    ON audit_log (actor_user_id, at DESC);
```

**Make it genuinely append-only.** The table is owned by `gvm_migrate`; the app role gets insert and read only:

```sql
REVOKE ALL          ON audit_log     FROM gvm;
GRANT  SELECT,INSERT ON audit_log     TO   gvm;
GRANT  USAGE         ON SEQUENCE audit_log_id_seq TO gvm;
```

No `UPDATE`, no `DELETE`. An audit table the application can rewrite is not an audit log.

**Handling long-running operations.** This is where your PZ shutdown sequence matters: `ExecStop` stuffs `save`, sleeps 10, stuffs `quit`, and `TimeoutStopSec=180`. A stop can take three minutes. Since the table is insert-only, don't UPDATE an outcome — insert a second row:

```
id=1041 action=instance.stop outcome=started  params={"reason":"nightly"}
id=1043 action=instance.stop outcome=ok       target="audit:1041" duration_ms=48210
```

Correlate on `request_id` and `target`.

**Transactionality.** For pure DB mutations (grant a role, rotate a secret) write the audit row in the *same transaction* as the change — no possibility of a change without a record. For side-effectful ops (systemctl, steamcmd, ufw) commit the `started` row first so an operation that crashes the daemon still leaves a trace.

**Redact by allowlist.** `params` gets an explicit per-action field allowlist, never "everything except a denylist". Otherwise a `secret.write` handler eventually serialises the plaintext into a table you deliberately cannot delete from.

**Mirror off-database.** `log/slog` with a `JSONHandler` to stderr → journald. Two independent copies, and journald survives Postgres being down.

**Audit the things the bash scripts did silently** — this is the operational-knowledge carry-over:
- `rebuild.sh`'s archive-before-destroy: log the archive path, size, and checksum *before* the destroy.
- Nightly restart timer firing (`actor_kind='timer'`), and `update.sh` outcomes.
- Mod list changes with the exact before/after **workshop numeric ID** sets *and* the **internal Mod ID** sets — they're different lists and conflating them is the classic PZ failure. Store both, diff both.
- RCON/admin password rotation (which secret, by whom — never the value).
- The PZ adoption/import path: what was found on disk, what was imported, what was left untouched.

**Optional tamper-evidence:** `row_hash = SHA256(prev_hash || canonical_json(row))`. Cheap, and `gvm audit verify` becomes a real command. Skip if it complicates the insert path.

**Retention:** don't auto-delete (the role can't anyway). Add `gvm audit export --before <date>` writing an age-encrypted JSONL archive, and let the owner run a manual `DELETE` as `gvm_migrate` if the table ever gets large. On this workload it won't.

---

## 7. Static-binary implications

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags "-s -w -X main.version=$VER -X main.commit=$SHA" \
  -o dist/gvm ./cmd/gvm
```

No `-extldflags "-static"`, no `-tags netgo,osusergo` — those are for `CGO_ENABLED=1` builds. With cgo off the binary is already fully static. Adding `-tags osusergo,netgo` is harmless and documents intent.

From Windows PowerShell:
```powershell
$env:CGO_ENABLED="0"; $env:GOOS="linux"; $env:GOARCH="amd64"; go build -trimpath -o dist/gvm ./cmd/gvm
```

### What CGO_ENABLED=0 rules out

| Affected | Consequence for gvm |
|---|---|
| **cgo DNS resolver** | Pure-Go resolver reads `/etc/resolv.conf` and `/etc/nsswitch.conf` itself and **ignores NSS modules** (mDNS, LDAP hosts, nss-myhostname edge cases). Irrelevant — gvm resolves `api.curseforge.com`, Steam CDN mirrors, and the Workshop API via ordinary DNS. |
| **`os/user`** | Pure-Go implementation parses `/etc/passwd` and `/etc/group` directly. Users existing only via NSS (LDAP/SSSD/systemd-homed) become invisible to `user.Lookup`. gvm creates local system users, so fine — but don't treat a `user.Lookup` miss as proof a name is free. Check `/etc/passwd` and let `useradd` fail authoritatively. |
| **`mattn/go-sqlite3`** | Unusable. Irrelevant (Postgres). If you ever want embedded SQLite for a test fixture, `modernc.org/sqlite` is pure Go. |
| **Any C library binding** | None needed. Every dependency above is pure Go: pgx, argon2, scs, otp, goose, age. |
| **glibc `getaddrinfo` behaviours** | e.g. `/etc/hosts` aliasing edge cases. Not load-bearing here. |

Nothing in this stack is blocked. Static linking is a pure win: one binary runs identically on Debian 12 (glibc 2.36), Ubuntu 22.04 (2.35), and Ubuntu 24.04 (2.39), with no `GLIBC_2.xx not found` at deploy time.

### Cross-compiling from Windows

Put every Linux-only syscall behind an interface with a `//go:build linux` implementation and a `//go:build !linux` stub returning `errors.New("not supported on this platform")`:

- `internal/platform/systemd_linux.go` — unit write, `daemon-reload`, `systemctl start/stop`, journal reads
- `internal/platform/user_linux.go` — `useradd`, and privilege drop via `exec.Cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid, Gid}}` (**`SysProcAttr` has no `Credential` field on Windows**, and `syscall.Setuid`/`Setgid` don't exist there)
- `internal/platform/fs_linux.go` — `unix.Flock` for per-instance locks, `unix.Statfs` for disk-space preflight before a rebuild
- `internal/platform/firewall_linux.go` — ufw with the SSH-port autodetect logic ported from `firewall.sh`

Keep `golang.org/x/sys/unix` out of any file that compiles on Windows, or the whole module stops building on the dev box. With that discipline, `go build ./... && go vet ./... && go test ./...` all stay green on Windows 11, and only the integration tests need a Linux box or container.

### Embedding the dashboard

```go
//go:embed all:web/dist
var webFS embed.FS
```

Templates, CSS, JS, and the migrations all ship inside `gvm`. Deployment is: copy one file, `systemctl restart gvm`. That is the whole argument for constraint #1, and nothing in this stack compromises it.

---

## Concrete dependency set

```
go 1.26

require (
    github.com/jackc/pgx/v5              v5.10.0   // >= 5.10.0 for the 2026 hardening
    github.com/pressly/goose/v3          v3.28.0
    github.com/alexedwards/scs/v2        v2.x
    github.com/alexedwards/scs/pgxstore  v0.0.0-20251002162104-209de6e426de
    github.com/alexedwards/argon2id      v1.x
    github.com/pquerna/otp               v1.x
    golang.org/x/crypto                  latest    // argon2
    golang.org/x/sys                     latest    // unix, linux-tagged files only
    filippo.io/age                       v1.x      // backup archives only
    github.com/google/uuid               v1.x
    github.com/go-chi/chi/v5             v5.x      // or net/http ServeMux + Go 1.22 patterns
)
// tool: sqlc v1.31.1 (via `go tool` directive or a pinned binary)
```

Stdlib does the rest: `net/http.CrossOriginProtection` (CSRF), `crypto/cipher.NewGCMWithRandomNonce` + `crypto/hkdf` (secrets), `log/slog` (audit mirror), `embed` (assets + migrations), `html/template` (dashboard).

No JWT library. No CSRF library. No ORM. No secrets manager.

## Uncertainty flags

- **pgx v5.10.0 (2026-06-03)** is from the CHANGELOG; verify `go list -m -versions github.com/jackc/pgx/v5` at implementation time in case a v5.10.x patch has landed.
- **scs/pgxstore** is versioned as a pseudo-version (`v0.0.0-20251002162104-...`), i.e. an untagged submodule of the scs repo. That's normal for scs's store submodules, but it means `go get -u` gives you whatever master is — pin it explicitly.
- **`LoadCredentialEncrypted=` being systemd v250** is confirmed indirectly: the `systemd-creds` man page is tagged `v250` throughout, while the shared `LoadCredential=`/`LoadCredentialEncrypted=` listitem in `systemd.exec.xml` carries the earlier `v247` tag of its first term. Either way, jammy's systemd 249.11 predates `systemd-creds` entirely, so the recommendation (deliver the master key via plain `LoadCredential=`) holds regardless.
- **Go 1.27.1 exists** (pkg.go.dev was serving 1.27.1 docs as of 2026-09-01). Everything above targets 1.25+ APIs and runs unchanged on your 1.26.
- OWASP's Argon2id numbers are periodically revised; the five-row table above is what the cheat sheet serves today. Re-check before shipping.

## Sources

- https://pkg.go.dev/github.com/jackc/pgx/v5
- https://github.com/jackc/pgx/blob/master/CHANGELOG.md
- https://pkg.go.dev/github.com/jackc/pgx/v5/stdlib
- https://pkg.go.dev/github.com/jackc/pgx/v5/pgxpool
- https://github.com/sqlc-dev/sqlc/releases
- https://docs.sqlc.dev/en/stable/guides/using-go-and-pgx.html
- https://pkg.go.dev/github.com/pressly/goose/v3
- https://pressly.github.io/goose/blog/2021/embed-sql-migrations/
- https://pkg.go.dev/github.com/golang-migrate/migrate/v4
- https://pkg.go.dev/github.com/golang-migrate/migrate/v4/source/iofs
- https://github.com/jackc/tern
- https://packages.debian.org/bookworm/postgresql
- https://packages.ubuntu.com/jammy/postgresql
- https://packages.ubuntu.com/noble/postgresql
- https://www.postgresql.org/download/linux/ubuntu/
- https://www.postgresql.org/docs/16/auth-pg-hba-conf.html
- https://www.postgresql.org/docs/16/ddl-rowsecurity.html
- https://www.postgresql.org/docs/current/role-attributes.html
- https://www.postgresql.org/about/news/postgresql-18-released-3142/
- https://wiki.debian.org/PostgreSql
- https://pkg.go.dev/net/http#CrossOriginProtection
- https://go.dev/src/net/http/csrf.go
- https://osv.dev/vulnerability/GO-2025-3884
- https://advisories.gitlab.com/pkg/golang/github.com/gorilla/csrf/CVE-2025-24358
- https://pkg.go.dev/filippo.io/csrf/gorilla
- https://caddyserver.com/docs/caddyfile/directives/reverse_proxy
- https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html
- https://pkg.go.dev/golang.org/x/crypto/argon2
- https://pkg.go.dev/github.com/alexedwards/argon2id
- https://pkg.go.dev/github.com/alexedwards/scs/v2
- https://pkg.go.dev/github.com/alexedwards/scs/pgxstore
- https://pkg.go.dev/github.com/pquerna/otp/totp
- https://pkg.go.dev/crypto/cipher
- https://pkg.go.dev/crypto/hkdf
- https://pkg.go.dev/golang.org/x/crypto/nacl/secretbox
- https://pkg.go.dev/filippo.io/age
- https://systemd.io/CREDENTIALS/
- https://manpages.debian.org/unstable/systemd/systemd-creds.1.en.html
- https://packages.debian.org/bookworm/systemd
- https://packages.ubuntu.com/jammy/systemd
- https://go.dev/doc/go1.26
- https://owasp.org/www-project-web-security-testing-guide/latest/4-Web_Application_Security_Testing/06-Session_Management_Testing/02-Testing_for_Cookies_Attributes
- https://eli.thegreenplace.net/2024/building-static-binaries-with-go-on-linux/
- https://github.com/lib/pq
