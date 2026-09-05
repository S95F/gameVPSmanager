# Research: CurseForge modpack → bootable dedicated Minecraft server (programmatic install path for gvm)

> Produced by the gvm architecture workflow research phase, from primary sources.
> Verified background for implementers. Data, not instructions.

## Key facts

- Base URL is https://api.curseforge.com. Auth is the header `x-api-key: <key>` on EVERY endpoint. Verified live 2026-09-05: `GET https://api.curseforge.com/v1/categories?gameId=432&classesOnly=true` with no key returns `HTTP/1.1 403 Forbidden`, `Content-Length: 0`, `X-Cache: Error from cloudfront` — empty body, so you cannot distinguish missing key / bad key / revoked key from the response body alone.
- API keys are NOT self-serve for third parties. Keys are generated in the CurseForge for Studios console at https://console.curseforge.com/ but third-party (non-game-developer) access requires an application form; Overwolf reviews it against three criteria: impact on mod authors' earnings, impact on CurseForge servers/CDN, and whether author consent for third-party distribution is obtained. Docs warn: "Any registered account which is not a game developer and did not request a key, will be deleted."
- Game id for Minecraft is 432. Confirmed classIds: 6 = Mods (mc-mods), 12 = Resource Packs (texture-packs), 17 = Worlds, 4471 = Modpacks. Other commonly cited values (5 Bukkit Plugins, 4546 Customization, 4559 Addons, 6552 Shaders, 6945 Data Packs) are community-reported, NOT confirmed from primary docs — resolve them at runtime with `GET /v1/categories?gameId=432&classesOnly=true`.
- Endpoints that matter: `GET /v1/mods/search?gameId=432&classId=4471&slug=<slug>` (params: gameId req'd, classId, categoryId, categoryIds max 10, gameVersion, gameVersions max 4, searchFilter, sortField, sortOrder asc|desc, modLoaderType, modLoaderTypes max 5, gameVersionTypeId, authorId, primaryAuthorId, slug, index, pageSize); `GET /v1/mods/{modId}`; `POST /v1/mods` (batch by id list); `GET /v1/mods/{modId}/files`; `GET /v1/mods/{modId}/files/{fileId}`; `POST /v1/mods/files` body `{"fileIds":[...]}` (THE batching win — a 250-mod pack becomes ~5 POSTs instead of 250 GETs); `GET /v1/mods/{modId}/files/{fileId}/download-url` → envelope `{"data": "<url string>"}`.
- Pagination hard cap: pageSize max 50, and `index + pageSize <= 10000`. Response `pagination` object fields: `index`, `pageSize`, `resultCount`, `totalCount`.
- File object fields (exact): `id`, `gameId`, `modId`, `isAvailable`, `displayName`, `fileName`, `releaseType`, `fileStatus`, `hashes[]` (`{"value":str,"algo":int}` — algo 1=Sha1, 2=Md5), `fileDate`, `fileLength`, `fileSizeOnDisk`, `downloadCount`, `downloadUrl`, `gameVersions[]`, `sortableGameVersions[]`, `dependencies[]` (`{modId, relationType}`), `exposeAsAlternative`, `parentProjectFileId`, `alternateFileId`, `isServerPack`, `serverPackFileId`, `isEarlyAccessContent`, `earlyAccessEndDate`, `fileFingerprint`, `modules[]` (`{name, fingerprint}`).
- Mod object fields that gate the whole design: `allowModDistribution` (bool), `classId`, `slug`, `links.websiteUrl`, `latestFiles[]`, `latestFilesIndexes[]` (`{gameVersion, fileId, filename, releaseType, gameVersionTypeId, modLoader}`).
- Distribution denial is explicit and documented by the aternos client: the download URL "will be null … or a 403 error … if the mod does not allow project distribution." So the signal is three-way: `mod.allowModDistribution == false`, and/or `file.downloadUrl == null`, and/or `GET .../download-url` returning 403. `file.isAvailable == false` is a SEPARATE condition (file withdrawn/unavailable), not a distribution opt-out.
- CurseForge announced CDN API-key authentication (blog post by Niv Hajaj, Jun 10 2026): "The change is optional today and will only become required on July 16th" (2026). Auth by header `x-api-key: YOUR_API_KEY` (recommended) or query param `?api-key=YOUR_API_KEY`. Failure mode once enforced is `401 Unauthorized` with body `"error": "Unauthorized"`. Affected host named in the post is edge.forgecdn.net.
- VERIFIED LIVE 2026-09-05, contradicting the announced timeline: `GET https://edge.forgecdn.net/files/3847/103/jei-1.18.2-9.7.0.209.jar` with NO key returns `302 Found` → `Location: https://mediafilez.forgecdn.net/files/3847/103/jei-...jar`, and mediafilez returns `200 OK` (993424 bytes, S3/CloudFront/Fastly). So CDN key enforcement is not (yet) active on this path. Build the key-passing in anyway — treat unauthenticated CDN success as a transitional accident, not a contract.
- CDN URL shape: `https://edge.forgecdn.net/files/{first-4-digits-of-fileId}/{remaining-digits-of-fileId}/{fileName}` — e.g. fileId 3847103 → /files/3847/103/. Note edge 302s to mediafilez.forgecdn.net.
- manifest.json (CurseForge client modpack zip, at zip root): `{"minecraft":{"version":"1.20.1","modLoaders":[{"id":"forge-47.2.0","primary":true}]},"manifestType":"minecraftModpack","manifestVersion":1,"name":"...","version":"...","author":"...","files":[{"projectID":238222,"fileID":5246076,"required":true}],"overrides":"overrides"}`. `modLoaders[].id` is `<loader>-<version>` (forge-47.2.0, neoforge-21.1.72, fabric-0.16.9). Only ONE entry has `primary:true`.
- Server packs are a separate uploaded file on the SAME project, linked from the client file via `file.serverPackFileId`; the server pack file itself has `isServerPack: true` and `parentProjectFileId` pointing back. A server pack zip has NO manifest.json — it is a pre-resolved tree (mods/, config/, kubejs/, often server.properties, server-icon.png, and start scripts run.sh/run.bat/ServerStart.sh). CurseForge's own tutorial recommends but does not require start scripts. itzg's docs explicitly warn for the automated path: "do not select a server file -- they lack" the manifest.
- Modrinth is the escape hatch and needs no key: base `https://api.modrinth.com`, read endpoints unauthenticated, documented rate limit 300 req/min per IP with headers `X-Ratelimit-Limit`, `X-Ratelimit-Remaining`, `X-Ratelimit-Reset`. A uniquely-identifying `User-Agent` is REQUIRED policy (e.g. `sfunk95/gvm/1.0 (contact)`). Endpoints: `GET /v2/search`, `GET /v2/project/{id|slug}/version`, `GET /v2/version/{id}`, and critically `GET /v2/version_file/{hash}?algorithm=sha1`.
- modrinth.index.json (.mrpack, MIME application/x-modrinth-modpack+zip, UTF-8, at zip root): `formatVersion` (1), `game` ("minecraft"), `versionId`, `name`, `summary?`, `files[]` = `{path, hashes:{sha1, sha512}, env?:{client, server} each of required|optional|unsupported, downloads:[https urls], fileSize}`, `dependencies` map keyed by `minecraft`, `forge`, `neoforge`, `fabric-loader`, `quilt-loader`. `path` must not contain `..`, drive letters, or absolute prefixes.
- Modrinth download whitelist is enforced by spec — only `cdn.modrinth.com`, `github.com`, `raw.githubusercontent.com`, `gitlab.com`. Implementations must follow at least 3 redirects. There is NO per-mod distribution opt-out in the Modrinth format: every entry carries a working `downloads[]` array.
- Modrinth override layering (apply in this order): `overrides/` → `server-overrides/` (server only) → `client-overrides/` (client only). This is why .mrpack needs no separate server pack. Verified live: version objects also carry a top-level `environment` field (e.g. `"client_only"`).
- Loader server-install commands. NeoForge: installer at `https://maven.neoforged.net/releases/net/neoforged/neoforge/{VERSION}/neoforge-{VERSION}-installer.jar`, run `java -jar neoforge-installer.jar --installServer`; produces `run.sh`, `run.bat`, `user_jvm_args.txt`, `libraries/`. Version list: `GET https://maven.neoforged.net/api/maven/versions/releases/net/neoforged/neoforge` → `{"isSnapshot":false,"versions":[...]}` (verified live). Forge: same `--installServer` pattern; versions at `https://maven.minecraftforge.net/net/minecraftforge/forge/maven-metadata.xml` (verified live, entries like `1.20.1-47.4.23`) and `https://files.minecraftforge.net/net/minecraftforge/forge/promotions_slim.json` with keys `"<mcver>-latest"` / `"<mcver>-recommended"` (verified live).
- Fabric: `java -jar fabric-installer-{VER}.jar server -dir <dir> -mcversion <mc> -loader <loader> -downloadMinecraft`, OR skip the installer entirely with the prebuilt launcher jar `https://meta.fabricmc.net/v2/versions/loader/{mcVersion}/{loaderVersion}/server/jar`. Loader list: `GET https://meta.fabricmc.net/v2/versions/loader/{mcVersion}` (verified live; objects carry `loader.version`, `loader.stable`, `intermediary`, `launcherMeta.min_java_version`). Quilt: `GET https://meta.quiltmc.org/v3/versions/loader` (verified live; carries `version`, `file_size`, `hashes.sha1`).
- Vanilla server jar: `https://piston-meta.mojang.com/mc/game/version_manifest_v2.json` (verified live 2026-09-05; latest release "26.2") → per-version JSON at `versions[].url` → `downloads.server.url` + `downloads.server.sha1`. v2 additionally has a top-level `sha1` per version entry.
- CurseForge also exposes loader metadata directly: `GET /v1/minecraft/version`, `GET /v1/minecraft/version/{gameVersionString}`, `GET /v1/minecraft/modloader`, `GET /v1/minecraft/modloader/{modLoaderName}`. The detail object gives `name`, `gameVersion`, `latest`, `recommended`, `downloadUrl`, `installMethod`, `additionalFilesJson`, `modLoaderGameVersionId`, `mavenVersionString`, `versionJson`, `librariesInstallLocation`, `minecraftVersion`, `forgeVersion` — enough to resolve `modLoaders[0].id` to an installer URL without leaving the CF API.
- Fingerprints (for adopting an already-installed server, the Minecraft analogue of the PZ import path): `POST /v1/fingerprints/matches`, `POST /v1/fingerprints/matches/by-game/{gameId}`, `POST /v1/fingerprints/fuzzy`, `POST /v1/fingerprints/fuzzy/by-game/{gameId}`. Response carries `exactMatches`, `exactFingerprints`, `partialMatches`, `unmatchedFingerprints`. Algorithm is murmur2 (32-bit) with seed 1, computed over the file bytes with 0x09 (tab), 0x0A (LF), 0x0D (CR), 0x20 (space) REMOVED, and the length passed to murmur2 being the post-strip normalized length. Not stated in CF's own docs — sourced from the meza/curseforge-fingerprint binding and aternos client; verify against a known jar before trusting it.
- Prior art for the undistributable-file problem, in order of usefulness: itzg/mc-image-helper `install-curseforge` with `--downloads-repo=DIR` (docker env `CF_DOWNLOADS_REPO`, default container path `/downloads` with subdirs `mods`, `modpacks`, `worlds`); Prism Launcher shows a modal listing every blocked mod with a browser link, then watches the user's Downloads folder and copies matches in; packwiz marks the entry `noDistribution: true` and stores `websiteUrl: mod.Links.WebsiteURL + "/files/" + fileID`, then returns a `core.ManualDownload{Name, FileName, URL}` at install time.
- Client-only mod exclusion is a real, curated dataset, not something you can infer reliably. itzg ships a default exclude/include JSON whose schema (src/main/java/me/itzg/helpers/curseforge/ExcludeIncludesContent.java, verified live) is: `{"globalExcludes": ["slug|id"], "globalForceIncludes": ["slug|id"], "modpacks": {"<modpack-slug>": {"excludes": [...], "forceIncludes": [...]}}}`. Docker knobs: `CF_EXCLUDE_MODS`, `CF_FORCE_INCLUDE_MODS`, `CF_EXCLUDE_ALL_MODS`, `CF_EXCLUDE_INCLUDE_FILE`, `CF_OVERRIDES_EXCLUSIONS` (ant-style), `CF_SET_LEVEL_FROM=WORLD_FILE|OVERRIDES`, `CF_PARALLEL_DOWNLOADS` (default 4; mc-image-helper's own `--max-concurrent-downloads` defaults to 10), `CF_FILE_DOWNLOAD_RETRIES` (5), `CF_FORCE_SYNCHRONIZE`, `CF_OVERRIDES_SKIP_EXISTING`.
- Ports for the firewall generalization: Minecraft Java default 25565/tcp (and 25565/udp only if `enable-query=true`), RCON 25575/tcp — RCON is the Minecraft analogue of PZ's screen-stuffed "save"/"quit" clean shutdown, but it must be treated as an optional capability since it is off by default.
- Existing Go CF clients are not production-grade: github.com/ASjet/go-curseforge (a.k.a. sjet47/go-curseforge), MIT, last release v0.5.2 on 2024-02-03, pre-1.0, not actively maintained; covers api/mods, api/files, api/minecraft, api/games, api/categories, api/fingerprints, schema/enum, key via `NewClient(apiKey)`/`InitDefault(apiKey)`.

## Pitfalls

- Shipping a CurseForge API key inside the gvm binary. The 3rd-party terms state the key is "non-transferable and may not be shared with any third party." Embedding one in a distributed binary is a straight violation and the key gets revoked (this is exactly what happened to the pre-Java-17 itzg images). gvm MUST require the operator to supply their own key; there is no legitimate keyless path to api.curseforge.com.
- Assuming the CDN stays unauthenticated. My live probe today shows edge.forgecdn.net 302→mediafilez.forgecdn.net returning 200 with no key, but CurseForge's own blog says a key is required as of 2026-07-16 and that failures will be 401 with `"error": "Unauthorized"`. Code that only handles 403/404 will fail silently-ish the day enforcement lands. Send `x-api-key` on CDN GETs from day one and treat 401 as a distinct, actionable error class.
- Conflating the three failure modes. `file.isAvailable == false` (file withdrawn — the file is GONE, retrying and manual download both fail; packwiz has an open bug where it doesn't skip these and produces an unreviewable manifest), `mod.allowModDistribution == false` (author opted out — the file exists, a human can fetch it from the website), and a plain HTTP 404 (bad id). These need three different user-facing messages and three different fallback branches.
- Constructing edge.forgecdn.net URLs from fileId+fileName to bypass a null downloadUrl. This is technically trivial and is exactly the circumvention the opt-out exists to prevent; it violates the 3rd-party terms and burns the operator's key. The CDN-path trick is legitimate ONLY as a mirror/resume optimization for files whose API already returned a downloadUrl.
- Picking the server pack blindly. `serverPackFileId` server packs are unstandardized author-authored zips: no manifest.json, arbitrary root nesting (sometimes everything is inside a single top-level folder), sometimes a bundled Forge installer that must be run, sometimes ServerStart.sh with hardcoded paths and `-Xmx` values, sometimes a broken start script. itzg deprecated the whole CURSEFORGE server-pack mode for this reason and made AUTO_CURSEFORGE (client manifest + resolve) the supported path.
- Assuming a server pack exists. Many packs never publish one; `serverPackFileId` is absent or 0. The fallback is: install from the CLIENT manifest and strip client-only mods — which cannot be inferred from CF metadata (there is no server/client env flag in the CF file object, unlike Modrinth's `files[].env.server`). You need a curated exclude list plus operator overrides, or the server crashes on first boot with a client-only mod's NoClassDefFoundError.
- The 3rd-party terms literally say developers may not "save or cache any data obtained through the API or SDK." A modpack installer inherently caches manifests and jars. Read narrowly this forbids building a metadata mirror/competing index; read broadly it forbids the on-disk mod cache every installer has. Do not build a persistent shared metadata mirror or expose CF search results as a gvm-hosted API — that also trips "build any product or service that competes … with CF."
- No numeric rate limit is published anywhere in CF's docs or terms — only 429 on excess and a discretionary "quota … upon Overwolf's sole discretion." Anything you hardcode is a guess. Naively firing 250 concurrent file lookups for a big pack is how keys get throttled; that is what `POST /v1/mods/files` with `{"fileIds":[...]}` exists for.
- `pageSize` silently caps at 50 and the API rejects `index + pageSize > 10000`. Paging code that assumes a requested pageSize of 200 was honored will silently install a truncated mod list.
- modLoaders[].id is a string with an embedded version (`forge-47.2.0`), and the loader version does NOT always match the loader's own maven coordinate scheme — NeoForge uses `21.1.72` style (no MC prefix) while Forge maven uses `1.20.1-47.4.23`. Splitting on the first `-` and pasting into a maven URL breaks on Forge. Resolve via `GET /v1/minecraft/modloader/{name}` or the loader's own metadata endpoint instead of string surgery.
- Server packs and overrides can contain a world. `CF_SET_LEVEL_FROM=OVERRIDES|WORLD_FILE` exists because re-running an install can otherwise overwrite `world/`. Given constraint 8 (a live PZ world must survive adoption), the same rule has to be structural in gvm: overrides extraction must never clobber the live world/, server.properties, ops.json, whitelist.json, or banned-*.json without an explicit flag — the direct analogue of rebuild.sh's archive-before-destroy.
- Modrinth requires a uniquely-identifying User-Agent as policy, not as advice. Sending Go's default `Go-http-client/2.0` is the documented anti-pattern and is the kind of thing that gets a client IP-blocked at 300 req/min.
- The `download-url` endpoint's documented response codes are only 200/404/500 — the 403 distribution-denied case is undocumented in the endpoint reference but real (confirmed by the aternos client). Don't code to the doc'd status list.
- Java version drift. NeoForge/Forge for 1.20.5+ need Java 21, 1.17–1.20.4 need 17, older need 8/11. Fabric's `launcherMeta.min_java_version` (present in the meta response) is the only machine-readable hint of the four loaders. Installing the wrong JDK is the single most common first-boot failure and it is not visible from the modpack manifest at all.

## Recommendations

- Treat the CurseForge API key as per-operator credential, never as a gvm asset. Config surface: `gvm config set curseforge.api_key <key>` writing 0600 to /etc/gvm/secrets.env, plus env `GVM_CURSEFORGE_API_KEY`. On 403-with-empty-body, emit a specific error naming https://console.curseforge.com/ and the third-party application form rather than a generic HTTP error.
- Make Modrinth the first-class, default modded-Minecraft path and CurseForge the second. Modrinth needs no key, has no distribution opt-out, publishes a real rate limit (300/min) with headers, ships sha1+sha512+fileSize on every file, and its `server-overrides/` layering plus `files[].env.server` removes the entire client-only-mod guessing problem. Ship .mrpack support first; it is a fraction of the work and it always succeeds.
- Use `POST /v1/mods/files {"fileIds":[...]}` in chunks of 50 to resolve a whole manifest, and `POST /v1/mods {"modIds":[...]}` to fetch `allowModDistribution` + `slug` + `links.websiteUrl` for the same set. Two batched round-trips per 50 mods instead of two per mod. Cap download concurrency at 4–8 (itzg defaults 4 in the image, 10 in the helper) with exponential backoff on 429 and 5 retries on transient failures.
- Implement the fallback ladder explicitly as a typed resolution result, not as nested error handling — the ladder itself is the feature. Order: (1) `file.downloadUrl` from the batch response; (2) `GET /v1/mods/{modId}/files/{fileId}/download-url`; (3) local content-addressed cache hit by sha1; (4) operator downloads-repo (`/var/lib/gvm/downloads/{mods,modpacks,worlds}`), matched by exact `fileName` then verified by sha1; (5) Modrinth by hash — `GET /v2/version_file/{sha1}?algorithm=sha1` using the sha1 CF gave you in `file.hashes`, which legitimately fetches the identical jar from the author's own Modrinth CDN when they publish both places; (6) fail the file into a blocked-list and continue resolving everything else. Never synthesize a forgecdn URL at step 5 or 6.
- Never abort the whole install on the first blocked mod. Resolve every file first, then present one consolidated report: N ok, M blocked, listing for each blocked mod its name, fileName, expected sha1, size, and the browser URL `https://www.curseforge.com/minecraft/mc-mods/{slug}/download/{fileId}`. Write that same list to `<instance>/.gvm/blocked-mods.json` and expose it as a first-class dashboard state ("awaiting 3 manual downloads") plus a CLI `gvm mc resolve-blocked <instance>` that rescans the downloads repo, verifies hashes, and finishes the install. This is Prism's UX made headless and is the only honest answer to the opt-out.
- Default the server-build strategy to `client-manifest` (resolve manifest.json + strip client-only mods + run the loader installer), not `server-pack`. Offer `--server-pack=prefer|only|never`; when `prefer` finds a `serverPackFileId`, still validate the extracted tree (does mods/ exist? is there a run.sh? does a bundled installer need running?) and fall back to client-manifest with a warning rather than trusting an arbitrary author zip.
- Adopt itzg's exclude/include schema verbatim so operators can reuse community lists: `{globalExcludes[], globalForceIncludes[], modpacks:{<slug>:{excludes[], forceIncludes[]}}}`, entries matching by slug OR numeric project id. Ship a vendored default list, allow `--exclude-include-file`, and log every exclusion decision with its source (default list / operator file / CLI flag) so a crash-on-boot is diagnosable.
- Build the loader install as a small strategy interface with four implementations (vanilla, forge, neoforge, fabric, quilt) that each answer: resolve version → fetch installer or prebuilt jar → verify → install → report the launch command and the required Java major version. Prefer Fabric's prebuilt `meta.fabricmc.net/v2/versions/loader/{mc}/{loader}/server/jar` over running the installer jar. For Forge/NeoForge you must shell out to `java -jar ... --installServer`, so gvm needs a JDK-provisioning step (Temurin/Adoptium apt repo) keyed off the resolved loader's Java requirement.
- Verify everything and make it idempotent: check `fileLength`/`fileSizeOnDisk` then sha1 (algo 1) from `file.hashes` on every download; store jars in a shared content-addressed cache at /var/lib/gvm/cache/sha1/<aa>/<full-sha1> and hardlink into each instance's mods/ so N instances of the same pack cost one copy. On re-install, murmur2-fingerprint the existing mods/ and POST to `/v1/fingerprints/matches/by-game/432` to skip already-correct files — that same fingerprint pass is the Minecraft counterpart of the PZ adoption path and lets gvm import a hand-installed server without touching its world.
- Port the PZ operational knowledge across the loader boundary explicitly: (a) clean shutdown — RCON `save-all flush` then `stop`, then wait for process exit with a timeout, then SIGTERM, and never SIGKILL (direct analogue of the screen-stuffed save/quit); (b) archive-before-destroy on rebuild, with world/, server.properties, ops.json, whitelist.json, banned-players.json, banned-ips.json, usercache.json and any KubeJS/config edits in the protected set; (c) SSH-port autodetecting ufw rules, adding 25565/tcp per instance and 25575/tcp bound to 127.0.0.1 only (RCON must never be world-reachable); (d) the nightly update timer becomes a pack-version-check timer that must be opt-in per instance, because a modpack update is world-affecting in a way a PZ dedicated-server patch is not.
- Write the CurseForge client as pure Go with no cgo (murmur2 is ~30 lines; zip/sha1/http are all stdlib) so Windows→Linux cross-compilation stays trivial. Do not depend on github.com/ASjet/go-curseforge — it is pre-1.0, unmaintained since Feb 2024, and predates both the CDN key change and the current file schema. Write your own thin typed client against the documented shapes and put the interesting logic behind an interface so a Modrinth backend and a CurseForge backend satisfy the same `PackSource`.
- Record provenance for every installed file — projectID, fileID, sha1, resolution path taken (api / download-url / cache / manual / modrinth-mirror), and timestamp — into `<instance>/.gvm/lockfile.json`. This is what makes a rebuild reproducible, makes a blocked-mod list survivable across gvm upgrades, and is the direct generalization of mods.txt + the WorkshopItems/Mods dual-list problem: the pack's projectID list and the on-disk jar filenames are different namespaces, exactly like PZ's workshop IDs vs internal Mod IDs, and both must be persisted.

## Detail

## 1. The CurseForge Eternal API (api.curseforge.com)

### Access
- Base: `https://api.curseforge.com`. Auth header `x-api-key` on **every** endpoint.
- Key is **mandatory**. Verified live 2026-09-05: unauthenticated `GET /v1/categories?gameId=432&classesOnly=true` → `403 Forbidden`, `Content-Length: 0`, `X-Cache: Error from cloudfront`. Empty body — no machine-readable reason.
- Keys are generated at `https://console.curseforge.com/`, but a non-game-developer must first submit a third-party application (contact info, project description + links, expected usage, ToS acceptance). Overwolf reviews against: impact on mod-author earnings, load on CF servers/CDN, and whether author consent for third-party distribution is obtained. Approval arrives by email. Accounts that registered without requesting a key are deleted.
- **Consequence for gvm**: there is no legitimate keyless path. The key is operator-supplied config, full stop. The terms make it "non-transferable and may not be shared with any third party" — embedding one in a released binary is both a ToS breach and an operational time bomb (revocation).

### Endpoints actually needed
| Purpose | Call |
|---|---|
| Find pack by slug | `GET /v1/mods/search?gameId=432&classId=4471&slug={slug}` |
| Pack metadata | `GET /v1/mods/{modId}` |
| Pack versions | `GET /v1/mods/{modId}/files?index=&pageSize=50` |
| One file | `GET /v1/mods/{modId}/files/{fileId}` |
| **Batch files** | `POST /v1/mods/files` body `{"fileIds":[...]}` |
| **Batch mods** | `POST /v1/mods` body `{"modIds":[...]}` |
| Download URL | `GET /v1/mods/{modId}/files/{fileId}/download-url` → `{"data":"<url>"}` |
| Class ids | `GET /v1/categories?gameId=432&classesOnly=true` |
| Loader resolution | `GET /v1/minecraft/modloader/{modLoaderName}` |
| Adoption / dedup | `POST /v1/fingerprints/matches/by-game/432` |

Search params (all exact): `gameId` (required), `classId`, `categoryId`, `categoryIds` (≤10), `gameVersion`, `gameVersions` (≤4), `searchFilter`, `sortField`, `sortOrder` (`asc`/`desc`), `modLoaderType`, `modLoaderTypes` (≤5), `gameVersionTypeId`, `authorId`, `primaryAuthorId`, `slug`, `index`, `pageSize`. Pagination response: `index`, `pageSize`, `resultCount`, `totalCount`. Hard limits: `pageSize` ≤ 50, `index + pageSize ≤ 10000`.

### IDs
`gameId = 432` (Minecraft). Confirmed classIds: **6** mods, **12** resource packs, **17** worlds, **4471** modpacks. Widely-cited but unconfirmed from primary docs: 5 bukkit-plugins, 4546 customization, 4559 addons, 6552 shaders, 6945 data-packs — **resolve at runtime** via the categories endpoint rather than hardcoding.

### Enums (from the published schema tables — re-verify at implementation)
- `HashAlgo`: 1 = Sha1, 2 = Md5.
- `FileReleaseType`: 1 Release, 2 Beta, 3 Alpha.
- `ModLoaderType`: documented range 0–6 → 0 Any, 1 Forge, 2 Cauldron, 3 LiteLoader, 4 Fabric, 5 Quilt, 6 NeoForge.
- `FileRelationType`: 1 EmbeddedLibrary, 2 OptionalDependency, 3 RequiredDependency, 4 Tool, 5 Incompatible, 6 Include.
- `ModsSearchSortField`: range 1–12. `SortOrder`: `asc` | `desc`.
- `FileStatus`: range starting at 1; the values you care about are Approved/Released — treat anything else as "not installable" and say why.

### Rate limits and terms
There is **no published numeric rate limit** — neither the API docs nor the 3rd-party terms give a number; excess returns 429 and the terms reserve a discretionary quota ("upon Overwolf's sole discretion"). Two clauses shape architecture:
- *"save or cache any data obtained through the API or SDK"* is prohibited. Narrow reading = don't build a metadata mirror; broad reading = don't persist manifests. Practical stance: short-TTL in-memory metadata cache, persist only what the operator's own install needs (lockfile + jars), never expose a gvm-hosted CF search API.
- *"build any product or service that competes, directly or indirectly, with CF"* — rules out gvm becoming a mod browser/index.

## 2. The archive format

### CurseForge client modpack zip
Root contains `manifest.json` and `overrides/`.

```json
{
  "minecraft": {
    "version": "1.20.1",
    "modLoaders": [{ "id": "forge-47.2.0", "primary": true }]
  },
  "manifestType": "minecraftModpack",
  "manifestVersion": 1,
  "name": "Example Pack",
  "version": "1.0.0",
  "author": "someone",
  "files": [{ "projectID": 238222, "fileID": 5246076, "required": true }],
  "overrides": "overrides"
}
```

`overrides/` is copied verbatim over the instance root: `config/`, `scripts/`, `kubejs/`, `mods/` (for jars not hosted on CF, which CF staff must approve), `defaultconfigs/`, sometimes `saves/`.

The critical gap versus Modrinth: **`files[]` carries no client/server flag.** `required` is about pack optionality, not environment.

### Server pack
- Uploaded by the author as an *Additional File* attached to a specific client file version ("The pack and server should match pack versions").
- Discovered via the client file's `serverPackFileId`; the server-pack file itself has `isServerPack: true` and `parentProjectFileId` back-referencing the client file.
- **No manifest.json.** It's a pre-resolved tree: `mods/`, `config/`, `kubejs/` (if used), plus recommended-not-required `run.sh`/`run.bat`, `server.properties`, `server-icon.png`.
- Not standardized: nesting depth varies, some bundle a Forge installer to run, some ship start scripts with hardcoded absolute paths and fixed `-Xmx`. This unpredictability is exactly why itzg deprecated `TYPE=CURSEFORGE` (`CF_SERVER_MOD`, `CF_BASE_DIR`, `USE_MODPACK_START_SCRIPT=false` to bypass "buggy bundled scripts") in favour of `AUTO_CURSEFORGE`, whose docs say plainly: **"do not select a server file — they lack"** the manifest.
- Griefed's ServerPackCreator is the author-side tool for producing these; notably it requires a *launcher-installed* modpack directory — "The ZIP-file downloaded from CurseForge does not work."

### When there is no server pack
This is the common case, and it is the path gvm should default to anyway:
1. Take the client `manifest.json`.
2. Resolve every `files[]` entry.
3. Remove client-only mods using a curated list (there is no metadata to infer this from).
4. Copy `overrides/` minus client-only paths (`resourcepacks/`, `shaderpacks/`, client config).
5. Install the loader server-side.

## 3. The central problem: undistributable files

### The signal, three-way
| Condition | Meaning | Recoverable? |
|---|---|---|
| `mod.allowModDistribution == false` | Author opted out of third-party distribution | Yes — a human can download it |
| `file.downloadUrl == null` / `download-url` → **403** | Same, at file granularity | Yes — manual |
| `file.isAvailable == false` | File withdrawn/unavailable | **No** — gone for everyone |

The aternos client documents it exactly: the download URL *"will be null … or a 403 error … if the mod does not allow project distribution."* Note the `download-url` endpoint reference only documents 200/404/500 — the 403 is real but undocumented there.

### How the ecosystem works around it
- **CurseForge's own app**: first-party, not subject to the restriction. Not a model you can copy.
- **Prism Launcher**: on install, builds a per-mod browser link, shows a modal listing everything blocked, then watches the user's Downloads folder and copies matching files into place as they arrive. Prism's docs put it mildly — *"Some modpacks may require the downloading of certain components via browser."*
- **packwiz**: at import time, when `file.DownloadURL == ""` it looks up the mod, writes the metadata entry with `noDistribution: true` and `websiteUrl: mod.Links.WebsiteURL + "/files/" + fileID`; at install time it returns `core.ManualDownload{Name, FileName, URL}` instead of a download.
- **itzg/mc-image-helper**: `--downloads-repo=DIR` — "will supply pre-downloaded mod and modpack files that are marked disallowed for automated download." In the Docker image that's `CF_DOWNLOADS_REPO`, default `/downloads`, with subdirectories `mods`, `modpacks`, `worlds` checked accordingly.

### What a headless installer may legitimately do
Permitted: use the API key you were granted; use `downloadUrl` when the API gives one; cache/hardlink files the operator already has; accept operator-supplied files from a local directory; look the same artifact up on Modrinth **by the sha1 CurseForge itself returned** and download from the author's own Modrinth CDN; tell the user precisely what is blocked and where to get it.

Not permitted, and gvm should refuse to do it: synthesize `https://edge.forgecdn.net/files/{fileId[:4]}/{fileId[4:]}/{fileName}` for a file whose `downloadUrl` was withheld. The URL shape is trivially derivable and widely known — which is exactly why using it against an opt-out is circumvention of the author's expressed choice and a terms breach that will cost the operator their key.

### CDN authentication — a live discrepancy worth flagging
CurseForge announced (blog, Niv Hajaj, **Jun 10 2026**) that CDN downloads now accept `x-api-key` header or `?api-key=` query param, "optional today and will only become required on **July 16th**", failing thereafter with `401 Unauthorized` / `"error": "Unauthorized"`, and told integrators to "handle 401 responses gracefully and surface a clear, actionable message rather than failing silently."

My probe today (2026-09-05) shows enforcement is **not** active on at least this path:
```
GET https://edge.forgecdn.net/files/3847/103/jei-1.18.2-9.7.0.209.jar   (no key)
→ 302 Found, Location: https://mediafilez.forgecdn.net/files/3847/103/jei-1.18.2-9.7.0.209.jar
→ 200 OK, 993424 bytes
```
Design as if enforcement is on: send the key, follow redirects (mediafilez is a different host, so a naive client that strips auth headers cross-host will break the day it matters — but sending a key to mediafilez is also correct since it's the same CDN family), and treat 401 as its own error class with an actionable message.

## 4. Modrinth .mrpack — the comparison that should change the roadmap

- No key for reads. Documented **300 req/min per IP**, with `X-Ratelimit-Limit` / `-Remaining` / `-Reset`. Uniquely-identifying `User-Agent` is required policy.
- `.mrpack` = ZIP, MIME `application/x-modrinth-modpack+zip`, UTF-8 `modrinth.index.json` at root:

```json
{
  "formatVersion": 1,
  "game": "minecraft",
  "versionId": "1.0.0",
  "name": "Pack",
  "summary": "optional",
  "files": [{
    "path": "mods/Mod.jar",
    "hashes": { "sha1": "...", "sha512": "..." },
    "env": { "client": "required", "server": "unsupported" },
    "downloads": ["https://cdn.modrinth.com/..."],
    "fileSize": 163282
  }],
  "dependencies": {
    "minecraft": "1.20.1",
    "fabric-loader": "0.16.9"
  }
}
```
`dependencies` keys: `minecraft`, `forge`, `neoforge`, `fabric-loader`, `quilt-loader`. `env.client`/`env.server` ∈ `required|optional|unsupported`. `path` must not contain `..`, drive letters or absolute prefixes.
- Download whitelist is part of the spec: **only** `cdn.modrinth.com`, `github.com`, `raw.githubusercontent.com`, `gitlab.com`. Follow ≥3 redirects.
- Overrides layer in order `overrides/` → `server-overrides/` → `client-overrides/`. **Server packs are unnecessary by construction.**
- **There is no distribution opt-out.** Every entry has a working `downloads[]`.

Verified live shape of a version object (Fabulously Optimized, project `1KVo5zza`): `files[].hashes.sha1/sha512`, `url`, `filename`, `primary`, `size`, plus a top-level `environment` field (`"client_only"`) and `loaders[]`, `game_versions[]`, `version_type`.

**The cross-platform trick worth building**: CF's file object includes `hashes` even for undistributable files. `GET https://api.modrinth.com/v2/version_file/{sha1}?algorithm=sha1` resolves that exact jar on Modrinth if the author dual-publishes — a large fraction of modern mods do. This is fully legitimate (the author opted into Modrinth's CDN) and it silently rescues many "blocked" mods.

## 5. Tooling landscape

| Tool | Role | Take-away for gvm |
|---|---|---|
| **itzg/mc-image-helper `install-curseforge`** | The reference headless CF installer. Flags: `--modpack-zip`, `--modpack-manifest`, `--modpack-page-url`, `--slug`, `--file-id`, `--excludes`, `--force-includes`, `--exclude-all-mods`, `--mod-loader-version`, `--max-concurrent-downloads` (10), `--file-download-retries` (5), `--downloads-repo` | Copy the whole model, including the downloads-repo escape hatch and the exclude/include file |
| **packwiz** (Go!) | Git-friendly TOML pack format, CF + Modrinth | Read its `noDistribution` / `ManualDownload` handling; it's already idiomatic Go |
| **ferium** (Rust) | CLI mod/modpack manager; `CURSEFORGE_API_KEY` / `GITHUB_TOKEN` env; `ferium modpack add/upgrade/configure/switch/remove` | Confirms bring-your-own-key is the norm; client-oriented, no server mode |
| **mcman** | Config-based server manager, git/Docker; imports/exports mrpack and packwiz; sources incl. Modrinth, CurseRinth, Spigot, Hangar, GitHub, Jenkins, Maven | Closest existing analogue to gvm's Minecraft half; good source-abstraction prior art |
| **ServerPackCreator** (Griefed) | Author-side: builds a server pack from a *launcher-installed* modpack dir (Forge/NeoForge/Fabric/LegacyFabric/Quilt), generates start scripts with configurable `-Xms/-Xmx` | Not usable server-side (needs a launcher install), but its client-only exclusion lists are a data source |
| **Prism / ATLauncher** | Client launchers; Prism's blocked-mod modal + Downloads-folder watcher is the best UX for the opt-out problem | Port the UX headlessly |
| **CurseRinth** | Third-party wrapper re-serving the CF API in Modrinth's shape | **Do not depend on it.** Unaffiliated with Modrinth, legally grey, single point of failure, and routing your traffic through it doesn't make the ToS go away |

Go clients: `github.com/ASjet/go-curseforge` (MIT, packages `api/mods`, `api/files`, `api/minecraft`, `api/games`, `api/categories`, `api/fingerprints`, `schema/enum`; key via `NewClient(apiKey)` / `InitDefault(apiKey)`) — last release **v0.5.2, 2024-02-03**, pre-1.0, not actively maintained, predates the CDN key change. Write your own thin client.

## 6. Ordered algorithm: "CurseForge pack + version" → bootable server directory

Instance root convention: `/srv/gvm/instances/<name>/`, state in `<root>/.gvm/`, shared cache `/var/lib/gvm/cache/sha1/<aa>/<sha1>`, operator drop-box `/var/lib/gvm/downloads/{mods,modpacks,worlds}`.

**Phase A — resolve the pack**
1. Require a key. If absent → fail fast with the console URL + application-form guidance.
2. Resolve identity: page URL → parse slug; else `GET /v1/mods/search?gameId=432&classId=4471&slug={slug}`; else `GET /v1/mods/{modId}`. Persist `modId`, `slug`, `links.websiteUrl`, `allowModDistribution`.
3. Resolve the pack file: explicit `fileId`, or page `GET /v1/mods/{modId}/files` filtered by `gameVersion`/`releaseType` and a filename matcher, newest first. Reject `isAvailable == false` and non-Approved/Released `fileStatus` with a specific message.
4. Record `serverPackFileId`.

**Phase B — obtain the pack archive**
5. Strategy `prefer`/`only` **and** `serverPackFileId` present → `GET /v1/mods/{modId}/files/{serverPackFileId}`, download, verify sha1 + `fileLength`, extract to a staging dir, then **validate**: does a `mods/` exist (possibly one level down — flatten a single top-level wrapper dir)? Is there a bundled `*installer*.jar`? Is there a start script? If validation fails and strategy is `prefer`, warn and fall through to step 6. Skip to Phase E.
6. Otherwise download the **client** pack zip (`downloadUrl`, else `/download-url`), verify sha1 + length, extract, parse `manifest.json`. Reject `manifestType != "minecraftModpack"`.

**Phase C — resolve every mod file**
7. Chunk `files[]` into groups of 50 → `POST /v1/mods/files {"fileIds":[...]}`. Chunk the distinct `projectID`s into 50s → `POST /v1/mods {"modIds":[...]}` for `slug`, `allowModDistribution`, `links.websiteUrl`, `classId`.
8. Apply exclusions in order: default client-only list → operator exclude/include file (`globalExcludes`, `globalForceIncludes`, `modpacks.<slug>.excludes/.forceIncludes`, matching by slug **or** id) → CLI flags. `forceIncludes` beats `excludes`. Log every decision with its source.
9. For each surviving file, classify: `isAvailable == false` → **UNAVAILABLE** (terminal); `downloadUrl` non-empty → **DIRECT**; else → **BLOCKED**.

**Phase D — the fallback ladder (per BLOCKED file, in order)**
10. `GET /v1/mods/{modId}/files/{fileId}/download-url` → on 200 promote to DIRECT; on 403 continue.
11. Cache hit: `/var/lib/gvm/cache/sha1/<aa>/<sha1>` → hardlink, done.
12. Downloads repo: scan `/var/lib/gvm/downloads/mods` for exact `fileName`; verify sha1; on match ingest into cache, done. (Optionally also match by murmur2 fingerprint against `file.fileFingerprint` to catch renamed files.)
13. Modrinth mirror by hash: `GET /v2/version_file/{sha1}?algorithm=sha1` with a proper User-Agent; on 200 download from `files[].url` (whitelisted host), verify sha1, done.
14. Still blocked → record `{projectID, fileID, name, fileName, sha1, fileLength, browserUrl: "https://www.curseforge.com/minecraft/mc-mods/{slug}/download/{fileId}"}`. **Do not abort** — finish resolving the rest.
15. Download all DIRECT files with bounded concurrency (4–8), `x-api-key` on the CDN request, follow redirects, 5 retries with exponential backoff, honour 429 with `Retry-After` if present. Verify `fileLength` then sha1 (`hashes[].algo == 1`) on every file; delete and retry once on mismatch, then fail that file.
16. If the blocked list is non-empty: write `<root>/.gvm/blocked-mods.json`, print the actionable table, set instance state `AWAITING_MANUAL_FILES`, and **stop before starting the server**. `gvm mc resolve-blocked <name>` re-runs steps 11–13 and continues.

**Phase E — assemble the server tree**
17. Copy `overrides/` → instance root, minus `resourcepacks/`, `shaderpacks/`, and any operator `overrides-exclusions` globs. **Protected paths are never overwritten without `--force`**: `world/`, `server.properties`, `ops.json`, `whitelist.json`, `banned-players.json`, `banned-ips.json`, `usercache.json`. Honour a `--level-from=world-file|overrides` switch for packs that ship a world.
18. Hardlink resolved jars into `<root>/mods/`. On re-install, fingerprint the existing `mods/` (murmur2 seed 1 over bytes with 0x09/0x0A/0x0D/0x20 stripped, normalized length) and `POST /v1/fingerprints/matches/by-game/432` to identify what's already correct; delete only jars that belong to the previous pack version per the lockfile, never unknown operator-added jars without confirmation.

**Phase F — loader and runtime**
19. Parse `modLoaders[]`, take `primary: true`. Split on the **last** `-` is unsafe; instead map by name and resolve via `GET /v1/minecraft/modloader/{name}` (gives `downloadUrl`, `installMethod`, `mavenVersionString`, `minecraftVersion`, `forgeVersion`) or the loader's own metadata:
   - **Fabric** → prefer prebuilt `https://meta.fabricmc.net/v2/versions/loader/{mc}/{loader}/server/jar`; versions/`min_java_version` from `https://meta.fabricmc.net/v2/versions/loader/{mc}`.
   - **Quilt** → `https://meta.quiltmc.org/v3/versions/loader` (carries `hashes.sha1`, `file_size`).
   - **NeoForge** → `https://maven.neoforged.net/releases/net/neoforged/neoforge/{V}/neoforge-{V}-installer.jar`, `java -jar ... --installServer`; versions at `https://maven.neoforged.net/api/maven/versions/releases/net/neoforged/neoforge`.
   - **Forge** → same `--installServer`; versions at `https://maven.minecraftforge.net/net/minecraftforge/forge/maven-metadata.xml`, latest/recommended at `https://files.minecraftforge.net/net/minecraftforge/forge/promotions_slim.json` (keys `"<mc>-latest"`, `"<mc>-recommended"`).
   - **Vanilla base jar** (when needed) → `https://piston-meta.mojang.com/mc/game/version_manifest_v2.json` → version JSON → `downloads.server.url` + `.sha1`.
20. Provision the correct JDK for the resolved MC/loader (1.20.5+ → 21, 1.17–1.20.4 → 17, older → 8/11; Fabric exposes `min_java_version` directly). This is the #1 first-boot failure and it is invisible in the manifest.
21. Write `eula.txt` only after an explicit operator acceptance flag — never auto-accept silently.
22. Emit the systemd unit and launch command from the loader strategy (`run.sh` with `@user_jvm_args.txt` for Forge/NeoForge; `java -jar fabric-server-launch.jar nogui` for Fabric), plus `server.properties` with `server-port`, `enable-rcon=true`, `rcon.port`, `rcon.password` (generated), and RCON bound to loopback.
23. Write `<root>/.gvm/lockfile.json`: pack `modId`/`fileId`/`version`, MC version, loader + version, and one record per installed file — `projectID`, `fileID`, `fileName`, `sha1`, `fileLength`, `source` ∈ `api|download-url|cache|manual|modrinth-mirror`, timestamp. This is what makes rebuild reproducible and is the direct generalization of the PZ `WorkshopItems=` vs `Mods=` dual-namespace lesson: pack project IDs and on-disk jar filenames are different namespaces and **both** must be persisted.

**Phase G — lifecycle (carrying the PZ knowledge forward)**
24. Clean shutdown: RCON `save-all flush`, then `stop`, wait for exit with timeout, then SIGTERM, **never SIGKILL** — the structural analogue of PZ's screen-stuffed `save`/`quit`.
25. Rebuild/update: archive-before-destroy the protected set (step 17) to a timestamped tarball before any destructive step.
26. Firewall: reuse the SSH-port-autodetecting ufw logic; open `25565/tcp` per instance (`25565/udp` only when `enable-query=true`), keep `25575/tcp` loopback-only.
27. Nightly timer: a modpack version bump is world-affecting in a way a PZ dedicated-server patch is not. Make auto-update opt-in per instance, and make the default a *check-and-notify* that surfaces in the dashboard rather than an unattended apply.

## Sources

- https://docs.curseforge.com/rest-api/
- https://console.curseforge.com/
- https://support.curseforge.com/support/solutions/articles/9000208346-about-the-curseforge-api-and-how-to-apply-for-a-key
- https://support.curseforge.com/support/solutions/articles/9000207405-curse-forge-3rd-party-api-terms-and-conditions
- https://blog.curseforge.com/introducing-api-key-authentication-for-curseforge-file-downloads/
- https://blog.curseforge.com/server-packs-tutorial/
- https://support.curseforge.com/en/support/solutions/articles/9000197908-exporting-a-modpack-for-curseforge-project-submission
- https://gdlauncher.com/docs/modpack-manifest-format/
- https://gist.github.com/crapStone/9a423f7e97e64a301e88a2f6a0f3e4d9
- https://support.modrinth.com/en/articles/8802351-modrinth-modpack-format-mrpack
- https://docs.modrinth.com/api/
- https://api.modrinth.com/v2/project/1KVo5zza/version
- https://docker-minecraft-server.readthedocs.io/en/latest/types-and-platforms/mod-platforms/auto-curseforge/
- https://docker-minecraft-server.readthedocs.io/en/latest/types-and-platforms/mod-platforms/curseforge/
- https://github.com/itzg/mc-image-helper
- https://raw.githubusercontent.com/itzg/mc-image-helper/main/src/main/java/me/itzg/helpers/curseforge/ExcludeIncludesContent.java
- https://raw.githubusercontent.com/packwiz/packwiz/main/curseforge/curseforge.go
- https://raw.githubusercontent.com/packwiz/packwiz/main/curseforge/install.go
- https://github.com/aternosorg/php-curseforge-api
- https://github.com/gorilla-devs/ferium
- https://github.com/ParadigmMC/mcman
- https://github.com/Griefed/ServerPackCreator
- https://prismlauncher.org/wiki/help-pages/flame-platform/
- https://github.com/PrismLauncher/PrismLauncher/issues/599
- https://github.com/meza/curseforge-fingerprint
- https://docs.neoforged.net/user/docs/server/
- https://maven.neoforged.net/api/maven/versions/releases/net/neoforged/neoforge
- https://maven.minecraftforge.net/net/minecraftforge/forge/maven-metadata.xml
- https://files.minecraftforge.net/net/minecraftforge/forge/promotions_slim.json
- https://meta.fabricmc.net/v2/versions/loader/1.20.1
- https://meta.quiltmc.org/v3/versions/loader
- https://piston-meta.mojang.com/mc/game/version_manifest_v2.json
- https://pkg.go.dev/github.com/ASjet/go-curseforge
- https://github.com/kuylar/curserinth
