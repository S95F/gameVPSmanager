# Bug sweep — phases 14–17

> **Archived 2026-10-17 — reviewed and closed.** All nine findings are resolved: seven fixed in `2807117`, two documented below as accepted will-not-fix decisions. No pending items left; nothing moved to [`docs/backlog.md`](../backlog.md).

Sweep performed after phases 14 (datastore), 15 (identity/authz), 16 (web tier), and 17 (hardening) were committed. Nine findings were confirmed; seven were fixed in commit `2807117`. The remaining two are documented below as accepted / will-not-fix.

---

## Fixed

### BUG-001 — Unauthenticated access to mutation endpoints (CRITICAL · security)

**File:** `internal/auth/auth.go`

**Root cause:** The auth middleware only enforced D-40 (session-present + no bearer → 401) when a session cookie was present. A request with *no* cookie at all bypassed the middleware and reached mutation handlers (`/api/v1/instances/{name}/start|stop|restart`) with only a CSRF check. An attacker could obtain a CSRF token via the HMAC endpoint (which uses an empty session ID for the nonce binding), then POST to any mutation endpoint without any credentials.

**Fix:** When no session cookie is present *and* the path starts with `/api/v1/`, the middleware now returns 401 immediately, before calling the next handler.

---

### BUG-002 — Data race on `PgSink.pool` (HIGH · race)

**File:** `internal/audit/pgsink.go`

**Root cause:** `SetPool()` (called from the store's background prober goroutine when Postgres becomes ready) and `Write()` (called on the audit goroutine for every event) both accessed `p.pool` with no synchronisation. The Go race detector would flag a write/read race.

**Fix:** Added `sync.RWMutex` to `PgSink`. `SetPool` acquires the write lock; `Write` acquires the read lock, copies the pool pointer to a local, then releases the lock before doing any I/O.

---

### BUG-003 — `Started` outcome silently stored as `"ok"` (HIGH · correctness)

**Files:** `internal/audit/pgsink.go`, `internal/store/dbgen/opsq/opsq.go`

**Root cause:** `audit.Started` (`"started"`) has no counterpart in the Postgres `outcome_t` enum (`ok`, `failed`, `denied`, `degraded`). The `pgOutcome`/`mapOutcome` switch-default fell through to `"ok"`, silently storing started-phase rows as successful. An operation that crashed gvmd mid-run left no distinguishable started row. Additionally, the default masked any future unknown Outcome values as successful rather than failing loudly.

**Fix:** `PgSink.Write` now returns `nil` immediately for `Started` records — they are captured by the spool sink and will be superseded by a terminal record. The `default` branch in both `pgOutcome` and `mapOutcome` is now `"failed"` instead of `"ok"`.

---

### BUG-004 — `check-grants` false SKIP when users table is empty (MEDIUM · correctness)

**File:** `internal/cli/db.go` — `runDBCheckGrants`

**Root cause:** `SELECT totp_secret_ct FROM users LIMIT 1` returning `pgx.ErrNoRows` means the query *executed* (gvm_web has SELECT on that column; permission was not denied). The original code treated `ErrNoRows` as `SKIP`, silently passing a D-27 violation when the users table happened to be empty.

**Fix:** `ErrNoRows` is now treated as `FAIL` with an explicit message explaining that the empty table proves the SELECT was permitted.

---

### BUG-005 — `MirrorInstances`/`MarkInstancesAbsent` no-op on nil slice (MEDIUM · correctness)

**Files:** `internal/store/mirror.go`, `internal/store/dbgen/opsq/opsq.go`

**Root cause:** A nil `[]string` slice is encoded by pgx as SQL `NULL`. `NOT (name = ANY(NULL))` evaluates to `NULL` in Postgres (not `TRUE`), so the WHERE clause matches nothing. Calling `MirrorInstances(ctx, pool, nil)` — the correct way to express "no instances are running" — silently updated zero rows, leaving all instances with `absent_since = NULL`.

**Fix:** Both functions now replace a nil slice with `[]string{}` before passing it to the driver.

---

### BUG-006 — goose package-global race (LOW · race)

**File:** `internal/store/migrate.go`

**Root cause:** `goose.SetBaseFS` and `goose.SetDialect` mutate goose's package-level globals. Two concurrent calls to `Migrator.Migrate()` or `Migrator.Version()` (e.g. parallel CLI invocations or test parallelism with `-parallel`) would race on those globals.

**Fix:** Added a `gooseMu sync.Mutex` variable in the package. Both `Migrate` and `Version` lock it before calling `goose.SetBaseFS`/`SetDialect` and hold it for the duration of the goose call.

---

### BUG-007 — `lazyPool` leaks `Store` channels; misleading fix hint (LOW · correctness)

**File:** `internal/doctor/checks_host_db.go`

**Root cause (a):** `lazyPool.get` called `store.Open`, extracted the `*pgxpool.Pool`, and discarded the `*store.Store`. The Store's internal channels (`ready`, `stop`, `done`) were never closed. The probe goroutine itself exited normally, but the struct lived for the process lifetime without honouring the `Close` contract.

**Root cause (b):** HOST-011's fix hint said `"run gvm db migrate"`. That command applies SQL schema migrations and does not call `ReapStaleRuns`. The correct remedy is restarting gvmd, which calls `ReapStaleRuns` when its pool becomes ready.

**Fix (a):** `lazyPool` now stores `*store.Store` and exposes a `close()` method. `get` sets `p.store` on success.

**Fix (b):** Fix hint changed to `"restart gvmd — it calls ReapStaleRuns when the pool becomes ready"`.

---

## Will not fix

### BUG-008 — Keepalive goroutine may write to ResponseWriter after handler returns (LOW · race)

**File:** `internal/web/handlers.go` — `handleAPIConsoleEvents`

`RunKeepalive` is launched as a goroutine that writes `: keepalive` comments every 15 seconds. When the main handler returns (client disconnect or ring closed), the goroutine checks `ctx.Done()` only on its next ticker fire (up to 15 s later), giving a window where it may call `sse.WriteComment` on an already-concluded ResponseWriter.

**Why not fixed:** Go's `net/http` server detects the closed connection and `Write` returns an error; `RunKeepalive` then returns. No panic, no data corruption, no client impact. The window is bounded at 15 s and is a cosmetic goroutine lifetime issue, not a correctness or security problem. The proper fix (a dedicated stop channel) would complicate the SSE plumbing without changing observable behaviour.

---

### BUG-009 — `port_allocations` has no cross-instance UNIQUE constraint (architecture · noted)

**File:** `internal/store/migrations/0003_instances.sql`

The PRIMARY KEY on `port_allocations` is `(instance_id, proto, port)`. Two instances can be assigned the same port; conflicts surface only through the `port_conflicts` view, not as a constraint violation at INSERT time.

**Why not fixed:** This is an explicit architectural decision documented in the spec (no cross-instance UNIQUE on port_allocations). Port allocation policy is enforced by the daemon layer, not the database. Adding a cross-instance UNIQUE would require a migration and would break the "each host is independent" principle (two hosts can legitimately bind the same port). The `port_conflicts` view gives operators visibility; it does not need to be a hard constraint.
