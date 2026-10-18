# Dashboard security pass — 2026-10-18

A dedicated review of the web tier before a second person is invited, as the
improvement review [§6](improvement-recommendations-2026-10.md) asked. Scope:
`internal/web`, `internal/auth`, `internal/authz`, `internal/webstore`, and the
gvmd verb boundary (`internal/gvmd`, `internal/cli/gvmdcmd.go`). Verified against
`main` at `a542ea2`.

This is a review to **triage**, not a set of landed fixes. Each finding below
names a location, the impact, and a recommended fix; the maintainer decides what
to take and in what order. The one change that ships with this document is the
browser smoke test (see the last section) — a test, not a security fix.

## What is already sound

- **Passwords** (`internal/identity/password.go`): argon2id at RFC 9106's second
  option (64 MiB, t=3, p=4), params read back from the stored PHC string,
  constant-time compare (`subtle.ConstantTimeCompare`), 12-rune floor counted in
  runes.
- **Invite single-use is atomic** (not a weakness). Redemption goes web `Enroll`
  → gvmd `auth.enroll` → `opsq.ConsumeInviteEnroll`
  (`internal/store/dbgen/opsq/invites.go:53`), which locks the invite row
  (`SELECT … FOR UPDATE OF i`) and re-checks `used_at` inside the lock before
  marking it used. Two concurrent redemptions serialise; the second sees
  `used_at != nil` and gets `ErrInviteInvalid`. The earlier non-locking read is
  not a TOCTOU because the authoritative check is redone under the lock.
- **Cross-origin protection**: the browser mux is wrapped in stdlib
  `http.NewCrossOriginProtection` (`internal/web/routes.go:78`), origins validated
  at construction (`server.go:147`); the `/api/v1` mux is bearer-only and reads no
  cookie. This is what replaced per-form CSRF tokens, and it covers the console
  send POST (SameSite=Lax + Origin/Host check).
- **Password-step lockout**: 5 failures/account → 15-min lock, 10 failures/IP/15
  min (`internal/webstore/webstore.go:155`), behind a 250 ms login floor.

## Findings

### DS-1 — SSE console streams are not torn down on session revocation (moderate)

`handleConsoleStream` (`internal/web/handlers.go:335`) authenticates on connect —
every new `EventSource`, including an automatic reconnect, flows through
`LoadSession` → `CrossOriginProtection` → `RequireAuth`, and `LookupSession`
(`internal/webstore/webstore.go:64`) rejects a session that is revoked, expired,
or whose user is disabled. So a dead session cannot **open** a stream.

But once open, the handler follows `s.cfg.Console.Stream(ctx, …)` indefinitely
with `ctx = r.Context()` (handlers.go:374), which cancels **only when the TCP
connection drops**. There is no periodic re-validation of the session inside the
loop and no server-side maximum stream lifetime (`server.go` sets
`WriteTimeout: 0` for SSE, and the 15 s keepalive keeps the connection
non-idle). Revocation (`RevokeSession`, logout at `login.go:217`) is only a DB
row update; nothing signals an open stream.

**Impact.** After logout, an admin revoke, an account disable, or even the 12 h
session expiry, an already-open console stream keeps delivering live game output
until the browser tab closes. It is read-only exposure — the console **send** box
is a separate POST that re-runs auth/RBAC each time, so there is no continued
*control*, only continued *visibility* — but the window is effectively unbounded.

**Recommend.** Give the stream loop a bounded re-validation: re-check
`LookupSession` on a timer (e.g. every 30–60 s) and on each keepalive tick, and
cancel the context when it fails; and/or cap the stream lifetime so a client must
reconnect (which re-authenticates) periodically. A revocation-broadcast channel
keyed by session/user id is the thorough version.

### DS-2 — Console send refuses only CR/LF/NUL, not the other control bytes (moderate)

All three validation layers of a console send line refuse only `\r`, `\n` and
(at the runner) `\x00`:

- web `handleConsoleSend` — `strings.ContainsAny(line, "\r\n")` (`handlers.go:466`)
- gvmd `doConsoleSend` — `strings.ContainsAny(req.Line, "\r\n")` (`internal/gvmd/server.go:246`)
- runner `console.ValidateSendLine` — `strings.ContainsAny(line, "\r\n\x00")` (`internal/console/protocol.go:187`)

So `ESC` (0x1b), the rest of C0 (0x01–0x08, 0x0b, 0x0c, 0x0e–0x1f), `DEL` (0x7f),
and all of C1 (0x80–0x9f) pass straight into the game's stdin **and** are echoed
verbatim into `console.log` as a `SourceActor` line (`internal/console/line.go:36`),
which operators `cat`/`tail`. This is broader than the improvement review's
premise ("C1 is not refused") — C0 and DEL get through too.

**Impact.** Terminal-escape / ANSI injection into the game console and, more
importantly, into the audit log an operator reads in their terminal (cursor
moves, colour, title-bar rewrites, clear-screen). The project already has the
right scrubber elsewhere — `internal/control/args.go:153`
(`r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)`) — but it does not guard the
send path.

**Recommend.** Extend `console.ValidateSendLine` (the one authoritative sink) to
reject every control byte: C0, DEL and C1 (the `args.go:153` predicate). Keep the
existing `ErrLineBreak` message for CR/LF/NUL and add a distinct error for other
control characters so the operator sees why. This is the smallest, highest-value
fix on this list.

### DS-3 — `--dev` (non-Secure cookies) has no guard against a real edge (low / hardening)

`--dev` does exactly one thing: `InsecureCookies: true`, which drops the `Secure`
attribute from the pending and session cookies (`internal/cli/web.go:90`,
`login.go:136`, `login.go:206`). There is **no auth bypass** — password + TOTP are
still required.

The gap is that nothing couples `--dev` to the listener. `requireLoopback`
(`web.go:107`) forces a TCP listener to be loopback — but loopback TCP
(`127.0.0.1:<port>`) is exactly what `edge.listen` uses behind a
serverGitUpdater-managed Caddy. So `gvm web --dev` with a configured edge runs a
fully reachable production dashboard that issues non-Secure session cookies. The
only mitigation today is an advisory stderr line (`web.go:96`).

**Impact.** An operator who habitually runs `--dev` locally and then deploys the
same command behind an edge ships session cookies without `Secure`, so a single
plaintext request (a mistyped `http://`) can leak the session cookie.

**Recommend.** Refuse `--dev` when an edge is configured (an `edge.listen` /
trusted-origins signal), or require an explicit second affirmation. The advisory
line is not enough for a footgun that weakens session security in production.

### DS-4 — TOTP step hardening (minor)

- **No attempt counter on the TOTP step.** The password step is rate-limited and
  locked out, but `handleTOTPSubmit` (`internal/web/login.go:161`) has no counter:
  a wrong code leaves the pending login intact (`pendingStore.Get`, not `Take`,
  until success — `identity.go:126`), so an attacker who already has a valid
  password + `gvm_pending` cookie can keep guessing codes for the full 5-minute
  `pendingTTL`, throttled only by the 250 ms floor (~1200 guesses; ~0.36% against
  a 6-digit code with a ±1 window). **Recommend** a per-pending attempt cap
  (e.g. 5) that invalidates the pending login.
- **Non-constant-time TOTP compare** (`internal/identity/totp.go:51`,
  `expected == code`). Low impact (floored at the web layer), but
  `subtle.ConstantTimeCompare` is the house style (passwords already use it).
- **`SetLastTOTPStep` is not atomic with the read** (`gvmdcmd.go:304`): two
  simultaneous logins presenting the same code within one step could both pass. A
  narrow replay race; a conditional update (`… WHERE last_totp_step < $step`)
  closes it.

## Priority

1. **DS-2** (console control bytes) — smallest, clear win, and it protects the
   audit log, not just the game.
2. **DS-1** (SSE revocation) — bounded exposure but unbounded in time; the
   re-validation-on-keepalive version is modest.
3. **DS-3** (`--dev` guard) — a production footgun; a small guard removes it.
4. **DS-4** (TOTP hardening) — the attempt cap first, then the two code-hygiene
   items.

## Browser smoke test

The mobile/tablet layout that just landed had no rendered-page test. This review
adds a Playwright smoke test (`ci/smoke/`) that drives the real dashboard pages —
the login page, the instance list and the live console — at a desktop and a phone
viewport, asserting the key elements render, there is no horizontal page scroll on
the phone, and the console receives a streamed line. It runs against
`tools/devpreview` (in-memory stubs, no Postgres/gvmd), so it exercises page
rendering and the SSE pane; it does **not** exercise the real login POST / TOTP
(devpreview auto-authenticates), which still needs the backend-backed tests in
`internal/web`.
