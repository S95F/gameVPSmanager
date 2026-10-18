# Bug sweep — phases 18–19

> **Archived 2026-10-17 — reviewed and closed.** All nine findings were fixed in the sweep commit. No pending items left; nothing moved to [`docs/backlog.md`](../backlog.md).

Sweep performed after phases 18 (Minecraft providers, Modrinth, telnet/mcmgmt) and 19 (CurseForge, nftables, Proton, btrfs/ZFS, egress) were committed. Nine findings were confirmed; all nine were fixed in this sweep commit.

---

## Fixed

### BUG-010 — CurseForge: dependency checks not enforced (CRITICAL · correctness)

**File:** `internal/content/curseforge/ladder.go`, `internal/content/curseforge/source.go`

**Root cause:** `depRelationEmbedded` (1) and `depRelationIncompatible` (5) are defined as constants in `files.go` but `resolveRef` never inspects `file.Dependencies`. Two spec rules were silently violated:

- A `depRelationIncompatible` dependency means two mods conflict and must not be installed together. Without the check, an incompatible pair would both be planned and downloaded, producing a broken server.
- A `depRelationEmbedded` dependency means the dep is physically bundled inside the parent JAR. Without tracking, a plan that lists both the parent and the embedded dep would download the same file twice, writing it to two paths.

**Fix (ladder.go):** `resolveRef` now checks `file.Dependencies` after the API call, before falling through to the rungs. A `depRelationIncompatible` dep returns an error immediately. `depRelationEmbedded` dep project IDs are collected and returned as a second return value alongside the `PlanEntry`.

**Fix (source.go):** `Resolve` maintains a local `embeddedProjects map[int]bool`. After each successful `resolveRef`, it records the returned embedded IDs. At the start of each iteration it checks whether the ref's project ID is already marked embedded — if so, the ref is skipped with an `Unaccounted` entry explaining why.

---

### BUG-011 — Fabric installer: all-zero SHA256 causes blob-store apply failure (HIGH · correctness)

**File:** `internal/provider/minecraft/fabric.go`

**Root cause:** The Fabric plan artifact used `sha256:0000000000000000000000000000000000000000000000000000000000000000` as a placeholder. The blob store's `Put` verifies the hash of every stream before publishing; a zero digest will never match the real file, so every `gvm apply` targeting a Fabric instance fails with `ErrDigestMismatch`.

**Fix:** `fabric.plan()` calls the new `fetchURLDigest` helper (see `java.go`) which streams the server launcher JAR through a `sha256.New()` hasher, discards the bytes, and returns the real `sha256:…` digest. `Apply` re-downloads the URL through `fetchToBlobs`; the blob store verifies the hash on `Put`.

---

### BUG-012 — NeoForge installer: all-zero SHA1 causes blob-store apply failure (HIGH · correctness)

**File:** `internal/provider/minecraft/neoforge.go`

**Root cause:** Same as BUG-011 but for the NeoForge Maven installer JAR. All NeoForge installs fail at Apply time.

**Fix:** Maven repositories publish a `<artifact>.sha1` sidecar file adjacent to every artifact. `neoforge.plan()` calls the new `fetchSHA1File` helper (see `java.go`) to fetch the 40-char `.sha1` file at plan time, providing the real digest without downloading the full JAR twice.

---

### BUG-013 — Forge installer: all-zero SHA1 causes blob-store apply failure (HIGH · correctness)

**File:** `internal/provider/minecraft/forge.go`

**Root cause:** Identical to BUG-012 for the legacy Forge Maven installer.

**Fix:** Same as BUG-012 — `forge.plan()` fetches the `.sha1` sidecar via `fetchSHA1File`.

---

### BUG-014 — Purpur: MD5 stored under `sha1:` prefix fails plan validation (HIGH · correctness)

**File:** `internal/provider/minecraft/purpur.go`

**Root cause:** The Purpur API v2 exposes only an MD5 hash (`buildResp.MD5`, 32 hex chars). The code stored it as `"sha1:" + buildResp.MD5`. `provider.ParseDigest` rejects this: SHA1 expects 40 hex chars, not 32. `plan.Validate()` → `plan.Authorize()` fails with `ErrBadDigest` before `Apply` is ever called. No Purpur server can be installed.

Additionally, MD5 is not a supported blob-store algorithm even if the length were correct.

**Fix:** `purpur.plan()` calls `fetchURLDigest` (same approach as Fabric) to stream the download URL and compute the real SHA256. The `buildResp.MD5` field is no longer used.

---

### BUG-015 — Modrinth `downloadURLs` map unprotected against concurrent access (MEDIUM · race)

**File:** `internal/content/modrinth/source.go`

**Root cause:** `Source.downloadURLs` is written in `resolveOne` (called from `Resolve`) and read in `modrinthArtifactURL` (called from `Materialize`). There was no mutex, so a caller that invoked `Resolve` and `Materialize` concurrently on the same `Source` instance would race. The CurseForge source has the same pattern with a mutex; Modrinth was inconsistent.

**Fix:** Added `mu sync.Mutex` to `Source`. The write in `resolveOne` and the read in `modrinthArtifactURL` are now wrapped with `s.mu.Lock()`/`s.mu.Unlock()`.

---

### BUG-016 — telnetctl: `Exec` dials before checking the gate (MEDIUM · correctness)

**File:** `internal/control/telnetctl/telnetctl.go`

**Root cause:** The package comment states "The gate (r.ControlGateOpen) prevents dialling before the server is ready to accept connections." `Ready()` and `Unready()` both honour the gate, but `Exec` skipped the gate check entirely and dialled immediately. A command issued while 7 Days to Die was still starting would receive a connection refusal or an incomplete response rather than `control.ErrNotReady`, which the caller cannot distinguish from a real error.

**Fix:** Added a gate check at the top of `Exec` (inside the mutex, before `c.dial`). If the gate is closed, `Exec` returns an error immediately using the configured `NotReadyReason`.

---

### BUG-017 — mcmgmt: `Exec` dials before checking the gate (MEDIUM · correctness)

**File:** `internal/control/mcmgmt/mcmgmt.go`

**Root cause:** Identical to BUG-016 for the MCMgmt JSON-RPC channel. A command issued while the Paper/Fabric/Purpur server was still starting would trigger a real dial rather than returning immediately with a not-ready error.

**Fix:** Same as BUG-016 — gate check added at the top of `Exec`.

---

### BUG-018 — mcmgmt: JSON-RPC response `id` never validated (LOW · correctness)

**File:** `internal/control/mcmgmt/mcmgmt.go`

**Root cause:** JSON-RPC 2.0 §7 requires the response `id` to match the request `id`. `Exec` decoded the response but never checked `resp.ID != id`. If the server sent a late response for a previous request (e.g. due to a connection reuse bug), `Exec` would silently accept it and return stale data to the caller.

**Fix:** After decoding the response, `Exec` now checks `resp.ID != id` and returns an error if they differ, before inspecting the `Error` or `Result` fields.
