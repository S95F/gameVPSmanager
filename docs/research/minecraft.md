# Research: Modded Minecraft: Java Edition dedicated servers on Linux — acquisition APIs, launch mechanics, control plane, and mod/plugin sourcing for gvm

> Produced by the gvm architecture workflow research phase, from primary sources.
> Verified background for implementers. Data, not instructions.

## Key facts

- MINECRAFT VERSIONING CHANGED. Since 26.1 (March 2026) Mojang uses year.drop[.hotfix]: current release is 26.2, latest snapshot 26.3-pre-2 (verified live in version_manifest_v2.json). Pre-26 versions (1.21.11 etc.) still exist and are still receiving builds. gvm's version comparator MUST handle both eras and MUST NOT assume a leading '1.'.
- JAVA 25 IS REQUIRED for 26.1+. Verified from primary source: piston-meta version JSON for 26.3-pre-2 has javaVersion = {"component":"java-runtime-epsilon","majorVersion":25}. Matrix: <1.17 = Java 8; 1.17 = 16; 1.18-1.20.4 = 17; 1.20.5-1.21.11 = 21; 26.1+ = 25. Never hardcode this: read javaVersion.majorVersion from the piston-meta per-version JSON.
- PAPERMC v2 API IS DEAD. https://api.papermc.io/v2/projects/paper returns HTTP 410 Gone (verified). The only working API is Fill v3 at https://fill.papermc.io/v3 (plus GraphQL at https://fill.papermc.io/graphql).
- Paper Fill v3 shapes (verified live): GET /v3/projects -> {"projects":[{"project":{"id","name"},"versions":{family:[versions]}}]}; GET /v3/projects/paper -> {"project":{"id":"paper","name":"Paper"},"versions":{"26.2":["26.2","26.2-rc-2"],"1.21":[...]}}; GET /v3/projects/paper/versions/26.2/builds/latest -> {"id":121,"time":"2026-08-29T11:32:25Z","channel":"STABLE","commits":[{"sha","time","message"}],"downloads":{"server:default":{"name":"paper-26.2-121.jar","checksums":{"sha256":"0de30..."},"size":64522205,"url":"https://fill-data.papermc.io/v1/objects/<sha256>/paper-26.2-121.jar"}}}. Note the download key is the literal string "server:default" and CDN URLs are content-addressed by sha256. Channels: STABLE/ALPHA/BETA/RECOMMENDED. Projects include paper and folia.
- Paper docs mandate a non-generic User-Agent with a contact URL/email (example format: 'mc-image-helper/1.39.11 (https://github.com/itzg/docker-minecraft-server)'). I verified it is NOT currently enforced (no-UA and 'curl/8.0' both returned 200) — comply anyway, enforcement can land at any time.
- MINECRAFT SERVER MANAGEMENT PROTOCOL exists as of 25w35a / 1.21.9 and is the single most important architectural finding. JSON-RPC 2.0 over WebSocket. server.properties keys: management-server-enabled (bool, default false), management-server-host (string, default localhost), management-server-port (int), management-server-secret (string, 40-char alphanumeric, auto-generated if blank), management-server-tls-enabled (bool, DEFAULT TRUE), management-server-tls-keystore (PKCS12 path), management-server-tls-keystore-password, status-heartbeat-interval (int, default 0 = off). TLS keystore password can also come from env MINECRAFT_MANAGEMENT_TLS_KEYSTORE_PASSWORD.
- Management protocol auth: either HTTP header 'Authorization: Bearer <secret>' or 'Sec-WebSocket-Protocol: minecraft-v1,<secret>'. Methods (exact strings): minecraft:server/status, minecraft:server/save, minecraft:server/stop, minecraft:server/system_message, minecraft:players, minecraft:players/kick, minecraft:allowlist (+ /set /add /remove /clear), minecraft:operators (+ /set /add /remove /clear), minecraft:bans (+...), minecraft:ip_bans (+...), minecraft:gamerules and minecraft:gamerules/update, minecraft:serversettings/{autosave,difficulty,max_players,motd,game_mode,view_distance,status_heartbeat_interval,...}. Notifications: minecraft:notification/server/{started,stopping,saving,saved,status,activity}, minecraft:notification/players/{joined,left}, minecraft:notification/{operators,allowlist,bans,ip_bans}/{added,removed}, minecraft:notification/gamerules/updated. Calling rpc.discover returns the full JSON schema — gvm can codegen its Go client from it. Paper supports the protocol too.
- server.properties KEYS WERE REMOVED. In 25w35a: pvp, allow-nether, spawn-monsters, enable-command-block were deleted and replaced by game rules pvp, allowEnteringNetherUsingPortals, spawnMonsters, enableCommandBlocks (all default true). Earlier, spawn-animals and spawn-npcs were removed in 1.21.2 (24w33a). Writing these keys on a 26.x server is a silent no-op. Also noted on the wiki: white-list default is slated to flip to true in 26.3 (verify at release).
- Vanilla acquisition chain: https://launchermeta.mojang.com/mc/game/version_manifest_v2.json -> {"latest":{"release","snapshot"},"versions":[{"id","type","url","time","releaseTime","sha1","complianceLevel"}]} -> fetch versions[].url (https://piston-meta.mojang.com/v1/packages/<sha1>/<id>.json) -> downloads.server = {sha1,size,url} pointing at https://piston-data.mojang.com/v1/objects/<sha1>/server.jar. Also carries javaVersion.{component,majorVersion}.
- Fabric (meta.fabricmc.net, v2 only; no v3): /v2/versions/game, /v2/versions/loader, /v2/versions/loader/{game}, /v2/versions/installer, /v2/versions/loader/{game}/{loader}/profile/json, /v2/versions/loader/{game}/{loader}/server/json, and the server launcher jar at /v2/versions/loader/{game}/{loader}/{installer}/server/jar. Verified: GET /v2/versions/loader/26.2/0.19.5/1.1.2/server/jar -> 200, application/java-archive, 181838 bytes. Loader entries: {"loader":{"separator","build","maven","version","stable"},"intermediary":{"maven","version","stable"}}. Installer entries: {"url","maven","version","stable"}. FABRIC_META_BASE_URL env var overrides the meta host (mirroring).
- NeoForge: https://maven.neoforged.net/releases/net/neoforged/neoforge/maven-metadata.xml. Version scheme now tracks MC directly: MC 26.2 -> 26.2.0.<build> (e.g. 26.2.0.76), MC 26.1.2 -> 26.1.2.<build> (e.g. 26.1.2.103); the 1.21.x era used 21.1.<build>. Installer verified: https://maven.neoforged.net/releases/net/neoforged/neoforge/26.2.0.76/neoforge-26.2.0.76-installer.jar -> 200, 3604163 bytes.
- Forge: use https://files.minecraftforge.net/net/minecraftforge/forge/promotions_slim.json — top-level keys are "homepage" and "promos"; promos keys are "<mcver>-latest" and "<mcver>-recommended". Live values: "26.2-latest":"65.1.3", "26.2-recommended":"65.1.0", "1.21.11-latest":"61.2.1", "1.21.11-recommended":"61.2.0". Installer verified: https://maven.minecraftforge.net/net/minecraftforge/forge/26.2-65.1.3/forge-26.2-65.1.3-installer.jar -> 200, 7858189 bytes. Forge is still actively shipping for 26.x.
- MODERN FORGE/NEOFORGE PRODUCE NO FAT JAR. `java -jar <installer>.jar --installServer` writes: run.sh, run.bat, user_jvm_args.txt, libraries/ (~hundreds of MB), mods/, and libraries/net/neoforged/neoforge/<ver>/unix_args.txt (+ win_args.txt). run.sh is literally one line: `java @user_jvm_args.txt @libraries/net/neoforged/neoforge/<ver>/unix_args.txt "$@"`. The argfile path embeds the loader version, so it changes on every loader update.
- ServerStarterJar restores a stable `java -jar server.jar` entrypoint for Forge 1.17+ and all NeoForge. Verified: https://github.com/NeoForged/serverstarterjar/releases/latest/download/server.jar -> 200, 25891 bytes. Usage: `java @user_jvm_args.txt -jar server.jar`; flags `--installer <version>`, `--installer <installer-url>`, `--installer-force`. Drop it next to run.sh.
- Purpur (api.purpurmc.org/v2, verified): GET /v2/purpur -> {"project":"purpur","metadata":{"current":"26.2"},"versions":[...]}; GET /v2/purpur/26.2 -> {"project","version","builds":{"latest":"2632","all":[...]}}; download at /v2/purpur/<version>/<build>/download or /v2/purpur/<version>/latest/download (verified 200, 67412118 bytes). Build numbers are STRINGS, not ints — unlike Paper's int build ids.
- Quilt is alive: https://meta.quiltmc.org/v3/versions/loader/26.2 returns 200 (724KB JSON). Loader still in beta since 2022; mod ecosystem publishes to Fabric first. Treat as a low-priority variant, not a v1 target.
- Modrinth API v2 (api.modrinth.com/v2) is current; v3 exists but is experimental. Rate limit 300 req/min per IP, headers X-Ratelimit-Limit / X-Ratelimit-Remaining / X-Ratelimit-Reset (verified live). Unique User-Agent required. Auth header is `Authorization: <token>` (raw PAT, no 'Bearer' prefix).
- Modrinth version object fields (verified live from /v2/project/fabric-api/version): game_versions[], loaders[], environment (string), id, project_id, author_id, featured, name, version_number, changelog, changelog_url, date_published, downloads, version_type (release|beta|alpha), status, requested_status, files[], dependencies[]. files[] = {id, hashes{sha512,sha1}, url, filename, primary, size, file_type}. dependencies[] = {version_id, project_id, file_name, dependency_type} with dependency_type in {required, optional, incompatible, embedded}.
- Modrinth now returns BOTH legacy and new environment metadata on v2. Project objects carry client_side and server_side ('required'|'optional'|'unsupported') AND environment (array). Version objects carry environment as a single string; observed value: 'client_or_server_prefers_both'. Full value set: client_and_server, client_only, client_only_server_optional, singleplayer_only, server_only, server_only_client_optional, dedicated_server_only, client_or_server, client_or_server_prefers_both, unknown.
- Modrinth hash lookup works and is the adoption primitive: GET /v2/version_file/{hash}?algorithm=sha1 -> 200 (verified with fabric-api's sha1 5307b87f8336f27e8f963263ac514f4bc6175d90). Bulk variants: POST /v2/version_files and POST /v2/version_files/update. Hashing an existing mods/ directory reconstructs a manifest from an unmanaged server.
- Modrinth search: GET /v2/search?facets=[["project_type:mod"],["versions:26.2"],["categories:fabric"]]&index=<relevance|downloads|follows|newest|updated>&offset=&limit= (limit max 100). Inner array = OR, outer array = AND. Response: {hits, offset, limit, total_hits}; hit fields include project_id, slug, title, description, categories, versions[], downloads, follows, latest_version, environment, client_side, server_side, license, date_created, date_modified.
- Hangar (Paper/Velocity plugin repo) base is https://hangar.papermc.io/api/v1. Verified GET /api/v1/projects?limit=1 -> {"pagination":{"count":3763,"limit":1,"offset":0},"result":[{id,name,namespace{owner,slug},stats,category,description,lastUpdated,visibility,settings,supportedPlatforms,mainPageContent,memberNames,avatarUrl,createdAt,publishedAt}]}. Versions at /api/v1/projects/{author}/{slug}/versions; downloads at /api/v1/projects/{author}/{slug}/versions/{name}/{platform}/download with platform in {PAPER,VELOCITY,WATERFALL}. Auth: POST /api/v1/authenticate?apiKey=<key> -> JWT, then `Authorization: HangarAuth <jwt>`. The raw OpenAPI is NOT at /api/v1/openapi.json (404); the SPA lives at /api-docs.
- Spiget (api.spiget.org/v2) mirrors SpigotMC: GET /v2/resources/{id} returns an `external` boolean and file{type,size,url}; GET /v2/resources/{id}/download 302s to cdn.spiget.org for locally-hosted files, or to a third-party URL when external is true.
- CurseForge v1 (api.curseforge.com): x-api-key header; gameId 432 = Minecraft. /v1/mods/search, /v1/mods/{modId}/files, /v1/mods/{modId}/files/{fileId}/download-url. API keys are approved per-project by Overwolf.
- Minecraft RCON wire format: int32 LE length (of the remainder, excluding itself), int32 request id, int32 type, NUL-terminated body, plus one pad NUL byte. Types: 3 = SERVERDATA_AUTH, 2 = SERVERDATA_EXECCOMMAND (and the auth-response type), 0 = SERVERDATA_RESPONSE_VALUE. Auth success = a type-2 packet echoing your request id; auth failure = request id -1. Max client->server payload 1446 bytes (1460 total); max server->client payload 4096 bytes (4110 total). Responses fragment at 4096 bytes with NO end-of-response marker. Use ISO-8859-1, not ASCII — bodies contain 0xA7 section signs for color codes.
- Default ports for a single instance: 25565 game (TCP; also UDP for query when enable-query=true, query.port default 25565), 25575 rcon (rcon.port), 25585 management server. Multi-instance needs a distinct triple per instance; only the game port belongs in ufw — rcon and management must bind 127.0.0.1 and stay firewalled.
- eula.txt is generated on first boot and the server then exits. Contents: a comment line '#By changing the setting below to TRUE you are indicating your agreement to our EULA (https://aka.ms/MinecraftEULA).', a timestamp comment line, then 'eula=false'. Paper additionally honors the system property -Dcom.mojang.eula.agree=true to skip the check.
- ops.json is a JSON array of {"uuid": hyphenated-uuid, "name": string, "level": 1-4, "bypassesPlayerLimit": bool}. whitelist.json is an array of {"uuid", "name"}. banned-players.json / banned-ips.json also exist. All of these are REWRITTEN by the server from in-memory state on shutdown.
- Paper CLI arguments (verified from docs): --nogui; -W/--universe/--world-container/--world-dir (default '.'); -P/--plugins (default 'plugins'); -c/--config (default server.properties); -b/--bukkit-settings (bukkit.yml); -S/--spigot-settings (spigot.yml); --paper-dir/--paper-settings-directory (default 'config'); -p/--port/--server-port; -h/--host/--server-ip; -s/--max-players/--size; -w/--level-name/--world; --forceUpgrade; --eraseCache; --serverId; --nojline; --noconsole. CLI args OVERRIDE server.properties.
- Paper system properties worth knowing: com.mojang.eula.agree=true, Paper.WorkerThreadCount (chunk loading threads), Paper.MaxViewDistance (default 32), paper.playerconnection.keepalive (default 30s), disable.watchdog, paper.disableOldApiSupport.
- itzg/mc-server-runner is the reference supervisor design and is exactly the pattern gvm should reimplement natively in Go. Flags: -stop-command (default 'stop'), -stop-duration, -stop-server-announce-delay, -detach-stdin, -named-pipe, -remote-console, -shell, -bootstrap, -debug, plus a websocket console. It traps SIGTERM and translates it into writing the stop command to the child's stdin, then waits.
- Adoptium is required for Java 25 on Debian 12 and Ubuntu 22.04 (neither ships openjdk-25). Exact steps: apt install -y wget apt-transport-https gpg; wget -qO - https://packages.adoptium.net/artifactory/api/gpg/key/public | gpg --dearmor > /usr/share/keyrings/adoptium.gpg; echo "deb [signed-by=/usr/share/keyrings/adoptium.gpg] https://packages.adoptium.net/artifactory/deb $(lsb_release -cs) main" > /etc/apt/sources.list.d/adoptium.list; apt update; apt install -y temurin-25-jdk. Installs to /usr/lib/jvm/temurin-25-jdk-amd64.

## Pitfalls

- MODRINTH FILTERS SILENTLY IGNORE NON-JSON PARAMETERS. I measured this directly. `?loaders=["fabric"]&game_versions=["26.2"]` (URL-encoded JSON arrays) returned HTTP 200 with 12 versions, all game_versions==['26.2'] and loaders==['fabric']. `?loaders=fabric&game_versions=26.2` ALSO returned HTTP 200 — with 1190 versions spanning 26.3-pre-2, 1.21.1, etc. The filters were dropped with no error. A naive Go client using url.Values.Set("loaders","fabric") will happily install a snapshot mod onto a release server. Always marshal to JSON then URL-encode, and always re-verify the returned version's game_versions/loaders before writing the jar to disk.
- NEOFORGE maven-metadata.xml <latest> AND <release> ARE WRONG. Verified live: release/latest both say 26.1.2.103 while 26.2.0.76 exists in the same <versions> list. The list is in publish order, not version order, because backports to older MC branches get published after newer-branch builds. Same problem on Forge's maven-metadata.xml (last entries were 1.21-51.0.33 while 26.2-65.1.3 is current). Parse the full <versions> list, filter by the MC-version prefix you actually want, and sort numerically yourself. For Forge, prefer promotions_slim.json entirely.
- api.papermc.io/v2 returns 410 Gone. Any code, docs, blog snippet, or Pterodactyl egg written before mid-2026 that hits api.papermc.io will hard-fail. v2 stopped receiving builds 2025-12-31 and was disabled 2026-07-01.
- PAPERCLIP AND THE FABRIC SERVER LAUNCHER BOTH REQUIRE OUTBOUND INTERNET ON FIRST BOOT. Paper ships a binary patch, not a complete server: on first run it downloads the matching vanilla jar from Mojang, sha256-verifies it, applies the patch, and populates cache/, versions/ and libraries/. The Fabric server launcher jar (182KB) likewise downloads loader + vanilla server at first run. A firewall-locked-down VPS or an air-gapped rebuild will hang or fail at boot with a confusing error. gvm must do this warm-up as an explicit, observable install step — never on the first user-visible start — and must archive the resulting tree.
- TURNING ON THE MANAGEMENT SERVER CAN BRICK BOOT. management-server-tls-enabled defaults to TRUE, and a TLS-enabled management server with no PKCS12 keystore configured refuses to let the server start at all. For a localhost-only control channel set management-server-tls-enabled=false AND management-server-host=127.0.0.1. Also: the wiki's properties table lists management-server-port default as 0 while the 25w35a changelog says 25585 — the two sources disagree, so ALWAYS write the port explicitly rather than relying on a default.
- WRITING pvp / allow-nether / spawn-monsters / enable-command-block TO server.properties ON 26.x IS A SILENT NO-OP. They were deleted in 25w35a. The values now live as game rules (pvp, allowEnteringNetherUsingPortals, spawnMonsters, enableCommandBlocks) and can only be set at runtime via `/gamerule` over RCON or minecraft:gamerules/update over the management protocol — or by editing level.dat while stopped. A gvm config template that still emits pvp=false will look like it worked and won't.
- THE SERVER REWRITES ITS OWN CONFIG AND STATE FILES ON SHUTDOWN. server.properties is regenerated (comments dropped, values re-escaped in Java .properties style — ':', '=', '#', '!' and leading whitespace get backslash-escaped, backslashes doubled, non-Latin-1 chars \u-escaped). ops.json, whitelist.json, banned-players.json and banned-ips.json are all rewritten from in-memory state. This is the exact same trap as the Project Zomboid .ini in the existing scripts: any edit gvm makes to a live server's files is silently discarded at stop. Rule: mutate these only while stopped, or go through RCON/management while running, then re-read after stop.
- RCON HAS NO END-OF-RESPONSE MARKER AND FRAGMENTS AT 4096 BYTES. Long output (`list` on a busy server, `/forge tps`, plugin list dumps) arrives split across packets with nothing marking the last one. 'Read until a packet shorter than 4096' is the common heuristic and is NOT reliable (an exactly-4096-byte final packet deadlocks you). The correct fix: after sending the real command, immediately send a second SERVERDATA_EXECCOMMAND with a distinct sentinel request id, then read until that sentinel's response arrives; everything before it belongs to the first command. Also decode as ISO-8859-1 — 0xA7 section signs in colored output will corrupt a strict UTF-8/ASCII decoder.
- RCON RUNS COMMANDS ON THE MAIN SERVER THREAD. A slow or heavy command issued by gvm's health-check loop directly costs TPS. Poll with the server-list-ping protocol instead; reserve RCON for actual mutations.
- SIGKILL CORRUPTS WORLDS — the same lesson as the PZ scripts, for the same reason. Killing mid-region-file-write produces corrupted chunks. systemd's TimeoutStopSec defaults to 90s; a large modpack routinely needs longer to save and unload, so the default silently escalates to SIGKILL on a slow shutdown. Set TimeoutStopSec=300+ per instance.
- THE screen + systemd ExecStop PATTERN IS BROKEN BY DEFAULT. `ExecStop=screen -p 0 -S mc -X eval 'stuff "stop\015"'` returns immediately, so systemd considers stopping done and kills the whole cgroup — including the JVM that just received 'stop' and hasn't finished saving. Workarounds are KillMode=none (deprecated, leaks processes) or KillSignal=SIGCONT. Neither is good. Have gvm be the supervisor and own the child's stdin instead of shelling out to screen.
- save-off WITHOUT A GUARANTEED save-on IS A SILENT DATA-LOSS BOMB. If the backup panics, times out, or the gvm process dies between `save-off` and `save-on`, the server keeps running and stops persisting to disk indefinitely — everything since the freeze is lost at the next crash. Any implementation needs a deferred restore plus an independent watchdog that re-issues save-on unconditionally after a deadline.
- `save-all` IS ASYNCHRONOUS. You must use `save-all flush` (or the management protocol's minecraft:server/save plus the minecraft:notification/server/saved notification) to know the write actually completed before you start copying files.
- DIMENSION FOLDER LAYOUT DIFFERS BY PLATFORM. Vanilla/Fabric/Forge/NeoForge keep the Nether and End inside <level-name>/DIM-1 and <level-name>/DIM1. Paper/Spigot/Purpur split them into sibling directories <level-name>_nether and <level-name>_the_end. A backup routine that only archives <level-name>/ loses the Nether and End on Paper. Forge/NeoForge additionally keep per-world mod configs in <level-name>/serverconfig/.
- SPIGET's `external` FLAG MUST BE CHECKED BEFORE DOWNLOADING. When external is true, /v2/resources/{id}/download redirects to an arbitrary third-party site and you get an HTML landing page or a Cloudflare challenge, not a jar — written straight into plugins/ it produces a confusing startup failure. Premium SpigotMC resources cannot be fetched at all. Always verify the downloaded bytes start with the ZIP magic PK\x03\x04 and contain plugin.yml or paper-plugin.yml before accepting.
- CURSEFORGE: allowModDistribution=false BREAKS AUTOMATION. Many authors opt out of third-party distribution; for those files /v1/mods/{modId}/files/{fileId}/download-url returns null and there is no API path to the jar. Modpack manifests referencing such files cannot be fully resolved. This is the single biggest risk to the CurseForge modpack target — design the resolver to report 'N files require manual placement' with names and links rather than failing opaquely.
- MODRINTH dependency_type="embedded" MUST NOT BE DOWNLOADED. Embedded dependencies are already jarred inside the parent mod; fetching them separately produces duplicate-mod crashes at load. Only `required` is a hard fetch; `incompatible` must abort the plan. A dependency with version_id set pins an exact version, one with only project_id set means 'resolve newest matching loader + game_version yourself'. There is no per-version transitive-closure endpoint — /v2/project/{id}/dependencies is project-level only, so gvm must BFS.
- environment=client_only / singleplayer_only MODS INSTALLED ON A DEDICATED SERVER CRASH IT. Filter on the version's environment field; only install client_and_server, server_only, server_only_client_optional, dedicated_server_only, client_or_server, client_or_server_prefers_both. Fall back to legacy server_side != 'unsupported' when environment is 'unknown'. This is the Minecraft analogue of the PZ workshop-ID-vs-Mod-ID distinction: two overlapping metadata systems where trusting the wrong one breaks the server.
- THE FORGE/NEOFORGE ARGFILE PATH EMBEDS THE LOADER VERSION (libraries/net/neoforged/neoforge/<ver>/unix_args.txt). Any launch command gvm caches becomes stale the moment the loader is updated, and run.sh is regenerated by the installer. Either re-read run.sh after every install, or standardize on ServerStarterJar so the invocation stays `java @user_jvm_args.txt -jar server.jar` forever.
- NEVER MIX AIKAR'S G1GC FLAGS WITH ZGC. Aikar's set is G1-specific tuning (G1NewSizePercent, G1HeapRegionSize, InitiatingHeapOccupancyPercent, MaxGCPauseMillis...). Passing them alongside -XX:+UseZGC produces either JVM warnings or, with -XX:+UnlockExperimentalVMOptions in the mix, silently ignored flags and a config that looks tuned but isn't. Aikar's flags remain valid on Java 21 and below; on Java 25 (i.e. all of 26.x) the modern recommendation is Generational ZGC with a much smaller flag set.
- -XX:+AlwaysPreTouch COMMITS THE ENTIRE HEAP AT STARTUP. With N instances on one VPS this multiplies: 4 servers x -Xms6G with AlwaysPreTouch = 24GB of RSS reserved before a single player joins. It is in both Aikar's and the ZGC recommendation sets. On a multi-instance box either drop it, or size heaps against total RAM rather than per-instance comfort.
- JLINE ANSI ESCAPES POLLUTE CAPTURED STDOUT. Paper/vanilla use JLine for the interactive console; captured stdout can contain ANSI color sequences that wreck log parsing and dashboard rendering. Pass --nojline (Paper) and/or set TERM=dumb in the unit's Environment=. Note the server also writes its own logs/latest.log with colors already stripped and rotates it itself — don't double-manage rotation.
- LOG-LINE READINESS DETECTION IS THE WEAKEST SIGNAL. `Done (X.XXXs)! For help, type "help"` on a [Server thread/INFO] line is the canonical marker, but Forge/NeoForge print a long ModLauncher preamble first, modpacks routinely reconfigure the log4j pattern, and some packs wrap the line in color codes. Use it only as a secondary confirmation. Also watch for `FAILED TO BIND TO PORT!` — the definitive multi-instance port-collision failure — and `Starting Minecraft server on *:<port>` to confirm binding.
- update-alternatives IS GLOBAL AND WILL BREAK MULTI-INSTANCE. A box running a 1.20.4 instance (Java 17), a 1.21.8 instance (Java 21) and a 26.2 instance (Java 25) needs three JDKs simultaneously. Pin an absolute interpreter path per instance (e.g. /usr/lib/jvm/temurin-25-jdk-amd64/bin/java) in the instance record; never depend on `java` from PATH.

## Recommendations

- Model a Minecraft instance as (platform, mc_version, loader_version, build_id) and give every platform a small Go interface with two methods: Resolve(channel) -> pinned artifact set with checksums, and Materialize(dir) -> on-disk server ready to launch. Paper/Purpur/vanilla resolve to a single checksummed jar; Fabric resolves to a launcher jar; Forge/NeoForge resolve to an installer jar plus a post-install step. This keeps the branchy acquisition logic out of the supervisor entirely.
- Make the Minecraft Server Management Protocol the PRIMARY control channel and RCON the fallback. Detect capability at install time: 26.x and 1.21.9+ get the management server (management-server-enabled=true, management-server-host=127.0.0.1, management-server-tls-enabled=false, explicit management-server-port, gvm-generated 40-char alphanumeric management-server-secret stored in Postgres). Everything older falls back to RCON. Generate the Go client from `rpc.discover` rather than hand-writing method structs — the method surface is still growing.
- Use minecraft:notification/server/started as the authoritative readiness signal, and TCP server-list-ping as the platform-independent health probe. The ping handshake (next_state=1) works against every platform with no credentials, returns players.online / players.max / version.name for the dashboard, and costs the server almost nothing — unlike an RCON `list`, which runs on the main thread. Define readiness as: TCP ping succeeds AND (mgmt 'started' notification OR log 'Done (' seen) within a deadline, with the log line as tiebreaker only.
- Replace screen entirely with gvm-as-supervisor. Ship `gvm run --instance <name>` as a Type=simple systemd ExecStart; it holds the child's stdin pipe, tees stdout to both the dashboard's log ring buffer and Postgres, traps SIGTERM, and runs the shutdown ladder: (1) minecraft:server/stop over the management socket, (2) RCON `stop`, (3) write `stop\n` to stdin, (4) SIGTERM, (5) SIGKILL only after TimeoutStopSec. This preserves the hard-won PZ lesson (clean shutdown via injected command, never SIGKILL) while dropping screen's systemd incompatibility. itzg/mc-server-runner is the working reference for the exact semantics.
- Set TimeoutStopSec=300 (not the 90s default) on every Minecraft unit, and make it configurable per instance — heavy modpacks legitimately need minutes to unload and save. Use one unix user and one systemd template unit (gvm@<instance>.service) per instance, mirroring the existing PZ_USER pattern.
- For backups, implement the freeze as an explicit state machine with a mandatory unfreeze, not a shell pipeline. Modern path (26.x): set minecraft:serversettings/autosave=false -> call minecraft:server/save -> await the minecraft:notification/server/saved notification -> snapshot -> autosave=true. Legacy path: `save-off` -> `save-all flush` -> snapshot -> `save-on`. Guard both with a deferred restore AND an independent timer goroutine that re-issues the unfreeze unconditionally after N seconds regardless of what the backup goroutine is doing.
- Make the freeze window milliseconds, not minutes: between freeze and unfreeze do only `cp --reflink=auto` (XFS/btrfs) or a btrfs/zfs snapshot, then unfreeze immediately and tar/compress from the frozen copy afterwards. On a plain ext4 VPS, fall back to freeze-tar-unfreeze but surface the resulting stall duration in the dashboard so operators can see the cost.
- Back up the full set, not just the world dir: <level-name>/ plus <level-name>_nether/ and <level-name>_the_end/ on Paper-family, server.properties, ops.json, whitelist.json, banned-players.json, banned-ips.json, usercache.json, mods/, plugins/ and their config trees, config/ (Paper) and <level-name>/serverconfig/ (Forge/NeoForge). Detect the layout from the platform, don't guess.
- Store the pinned artifact identity, not just a version string. For Paper store {version, build id (int), sha256}; for Purpur {version, build (string), url}; for Fabric {game, loader, installer}; for NeoForge/Forge {mc, loader version, installer sha}. Verify every download against its published checksum before it touches the instance directory. Every API here publishes one (Mojang sha1, Paper sha256, Modrinth sha512+sha1) — use it, and make an unverified download a hard failure.
- Standardize Forge/NeoForge on ServerStarterJar so the launch command is a stable `java @user_jvm_args.txt -jar server.jar nogui` across loader upgrades, and write JVM tuning exclusively into user_jvm_args.txt. This makes Forge/NeoForge look identical to Paper from the supervisor's point of view and removes the version-embedded argfile path as a source of breakage.
- Pick the JDK from data, not from a table: read javaVersion.majorVersion out of the piston-meta per-version JSON for the target MC version, then map to an absolute Temurin path installed from the Adoptium apt repo. Keep a small ensure_jdk(major) routine that is idempotent and records the absolute java path on the instance row. Ship the Adoptium repo setup in gvm's bootstrap alongside the existing ufw/SSH-port-autodetect logic.
- Select GC flags by Java major version, not by copy-paste. Java <=21: Aikar's G1GC set. Java 25 (all of 26.x): -Xms=-Xmx, -XX:+UseZGC, -XX:+UseCompactObjectHeaders, -XX:+UseStringDeduplication, -XX:TrimNativeHeapInterval=5000, and -XX:+UseTransparentHugePages where the host permits. Treat AlwaysPreTouch as a per-host policy toggle because of its multi-instance RSS cost. Store the resolved flag string on the instance so a JVM upgrade is a visible, reviewable diff rather than a silent behavior change.
- Build the mod/plugin layer around content hashes, because that is what makes the adoption path work. Modrinth's /v2/version_file/{sha1} and the bulk POST /v2/version_files turn any existing unmanaged mods/ or plugins/ directory into a reconstructed manifest. Anything that doesn't resolve gets recorded as a 'vendored' file, checksummed and preserved verbatim. This is the direct Minecraft analogue of reconciling PZ workshop IDs against internal Mod IDs, and it is what lets gvm adopt a running server without touching its content.
- Rank the plugin/mod sources by automatability and be explicit about it in the UI: Modrinth (full metadata, hashes, dependency graph, environment flags) > Hangar (Paper-native, but many versions are external links) > Spiget (must check `external`, Cloudflare-prone, no premium) > CurseForge (needs an approved key, allowModDistribution can block downloads outright). Implement Modrinth first and completely; make the others pluggable sources behind the same interface.
- Write server.properties through a real Java .properties codec (correct escaping of ':', '=', '#', '!', leading whitespace, backslashes) and treat the file as server-owned: gvm's Postgres row is the desired state, the file is a projection, and gvm re-reads it after every clean stop to pick up server-side rewrites. Same rule for ops.json/whitelist.json — mutate via the management protocol or RCON while running, via the file only while stopped.
- Expose the four ex-properties (pvp, allowEnteringNetherUsingPortals, spawnMonsters, enableCommandBlocks) in the gvm config schema as GAME RULES with a version-gated writer: on <25w35a write server.properties keys, on 26.x apply them via minecraft:gamerules/update at post-start. Otherwise operators will set pvp=false in the dashboard and watch it do nothing.
- Allocate a port block per instance (game, rcon, management, optional query) from a Postgres-backed allocator and write all of them into server.properties explicitly. Extend the existing firewall.sh logic so ufw opens ONLY the game port (TCP, plus UDP when query is on) per instance, and add an assertion that rcon.port and management-server-port are bound to 127.0.0.1 — a misconfigured RCON on a public interface is a plaintext-password remote-console.
- Split installation into explicit phases with the network warm-up made visible: resolve -> download+verify -> materialize (run the Forge/NeoForge installer, or first-boot Paperclip/Fabric to populate libraries/ and cache/) -> configure -> archive-the-clean-tree -> start. Reuse the PZ rebuild.sh 'archive before destroy' discipline: never delete an instance tree without tarring it first, and keep the world directories out of the destroy set entirely so a rebuild is a binary swap, not a world reset.
- Treat Quilt as a config-only variant of the Fabric code path (meta.quiltmc.org/v3 has the same shape) and do not build it out for v1; the mod ecosystem publishes to Fabric first and the loader is still in beta after four years. Folia is available through the same Paper Fill v3 API and is a cheap addition if regionized threading is ever wanted.
- Pin an explicit User-Agent of the form 'gvm/<version> (+<contact-url-or-email>)' on every outbound API call — Paper and Modrinth both require it by policy, Paper does not currently enforce it, and being on the wrong side of that when enforcement lands would break every install at once. Respect Modrinth's X-Ratelimit-Remaining header with a token-bucket limiter shared across the whole gvm process, since 300 req/min is easy to exhaust when resolving a 200-mod pack's dependency graph.

## Detail

## Executive summary — three findings that should change the architecture

1. **The Minecraft world moved while the knowledge base wasn't looking.** Version strings are now `YY.DROP[.HOTFIX]` (current release **26.2**, snapshot **26.3-pre-2**), **Java 25** is required from 26.1 onward, and four `server.properties` keys were deleted and turned into game rules. Any version handling written against the `1.x.y` assumption is already wrong.
2. **`api.papermc.io/v2` is HTTP 410 Gone.** Verified. The only working Paper API is **Fill v3** at `https://fill.papermc.io/v3`.
3. **There is now an official server control API.** The **Minecraft Server Management Protocol** (JSON-RPC 2.0 over WebSocket, added 25w35a / 1.21.9) replaces most of what gvm would otherwise do with RCON and log scraping — including a reliable `server/started` notification and a proper save/autosave control for backups. This should be gvm's primary control channel, with RCON as the compatibility fallback.

---

## 1. Obtaining server binaries programmatically

### Vanilla
```
GET https://launchermeta.mojang.com/mc/game/version_manifest_v2.json
  -> {"latest":{"release":"26.2","snapshot":"26.3-pre-2"},
      "versions":[{"id","type","url","time","releaseTime","sha1","complianceLevel"}]}
GET <versions[].url>            # https://piston-meta.mojang.com/v1/packages/<sha1>/<id>.json
  -> downloads.server = {"sha1","size","url"}   # https://piston-data.mojang.com/v1/objects/<sha1>/server.jar
  -> javaVersion = {"component":"java-runtime-epsilon","majorVersion":25}
```
`javaVersion.majorVersion` is the authoritative Java requirement — read it, don't hardcode a table.

### Paper / Folia — Fill v3 (verified live)
```
GET https://fill.papermc.io/v3/projects
GET https://fill.papermc.io/v3/projects/paper
GET https://fill.papermc.io/v3/projects/paper/versions/26.2/builds
GET https://fill.papermc.io/v3/projects/paper/versions/26.2/builds/latest
```
Build object:
```json
{ "id": 121, "time": "2026-08-29T11:32:25Z", "channel": "STABLE",
  "commits": [{"sha","time","message"}],
  "downloads": { "server:default": {
      "name": "paper-26.2-121.jar",
      "checksums": {"sha256": "0de30efb024bc8b83c9c7d507d11802897ad8056b6110ec09fe1a91d126ccb54"},
      "size": 64522205,
      "url": "https://fill-data.papermc.io/v1/objects/<sha256>/paper-26.2-121.jar" }}}
```
Download key is the literal string `"server:default"`. `build.id` is an **int**. Channels: `STABLE`/`ALPHA`/`BETA`/`RECOMMENDED`. GraphQL alternative at `https://fill.papermc.io/graphql`.

### Purpur (verified live)
```
GET https://api.purpurmc.org/v2/purpur          -> {"project","metadata":{"current":"26.2"},"versions":[...]}
GET https://api.purpurmc.org/v2/purpur/26.2     -> {"builds":{"latest":"2632","all":[...]}}
GET https://api.purpurmc.org/v2/purpur/26.2/latest/download   # 200, 67,412,118 bytes
GET https://api.purpurmc.org/v2/purpur/<ver>/<build>/download
```
Build numbers are **strings** here, unlike Paper's ints.

### Fabric — `meta.fabricmc.net`, v2 only
```
/v2/versions/game
/v2/versions/loader                       -> [{"loader":{"separator","build","maven","version","stable"},
                                               "intermediary":{"maven","version","stable"}}]
/v2/versions/loader/{game}
/v2/versions/installer                    -> [{"url","maven","version","stable"}]
/v2/versions/loader/{game}/{loader}/server/json
/v2/versions/loader/{game}/{loader}/{installer}/server/jar     # <- the server launcher
```
Verified: `/v2/versions/loader/26.2/0.19.5/1.1.2/server/jar` → 200, `application/java-archive`, 181,838 bytes. That 182 KB jar is a **thin launcher** — on first run it downloads Fabric Loader and the vanilla `server.jar` into `libraries/` and `.fabric/`. `FABRIC_META_BASE_URL` redirects it to a mirror. When changing loader versions, delete `.fabric/` or you get classloading errors.

### NeoForge
`https://maven.neoforged.net/releases/net/neoforged/neoforge/maven-metadata.xml`. Version scheme now mirrors MC: MC `26.2` → `26.2.0.<build>` (live: `26.2.0.76`); MC `26.1.2` → `26.1.2.<build>` (live: `26.1.2.103`); the 1.21.x era used `21.1.<build>`.

Installer verified: `.../neoforge/26.2.0.76/neoforge-26.2.0.76-installer.jar` → 200, 3,604,163 bytes.

### Forge
Use `https://files.minecraftforge.net/net/minecraftforge/forge/promotions_slim.json` — `{"homepage": ..., "promos": {"<mc>-latest": "<forge>", "<mc>-recommended": "<forge>"}}`. Live: `"26.2-latest":"65.1.3"`, `"26.2-recommended":"65.1.0"`, `"1.21.11-latest":"61.2.1"`. Forge is still shipping for 26.x.

Installer verified: `https://maven.minecraftforge.net/net/minecraftforge/forge/26.2-65.1.3/forge-26.2-65.1.3-installer.jar` → 200, 7,858,189 bytes.

### The modern Forge/NeoForge installer flow — no fat jar
```
java -jar <installer>.jar --installServer
```
produces:
```
run.sh                          # ONE LINE, see below
run.bat
user_jvm_args.txt               # where YOUR JVM flags go
mods/
libraries/                      # hundreds of MB
libraries/net/neoforged/neoforge/<ver>/unix_args.txt
libraries/net/neoforged/neoforge/<ver>/win_args.txt
```
`run.sh` is:
```sh
java @user_jvm_args.txt @libraries/net/neoforged/neoforge/<ver>/unix_args.txt "$@"
```
Java `@argfile` syntax — the classpath and module args live in the file. Append `nogui` via `"$@"`. The argfile path embeds the loader version, so it changes on every loader update.

**ServerStarterJar** removes this problem: `https://github.com/NeoForged/serverstarterjar/releases/latest/download/server.jar` (verified 200, 25,891 bytes) restores `java @user_jvm_args.txt -jar server.jar` for Forge 1.17+ and all NeoForge. Flags: `--installer <version|url>`, `--installer-force`. **Recommended** — it makes Forge/NeoForge look identical to Paper from the supervisor's point of view.

---

## 2. Java runtime

| Minecraft | Java |
|---|---|
| < 1.17 | 8 |
| 1.17 | 16 |
| 1.18 – 1.20.4 | 17 |
| 1.20.5 – 1.21.11 | 21 |
| **26.1+** | **25** |

Confirmed from primary source (piston-meta `javaVersion.majorVersion: 25`, `component: java-runtime-epsilon`). Debian 12 and Ubuntu 22.04 ship no `openjdk-25`, so use Adoptium:
```bash
apt install -y wget apt-transport-https gpg
wget -qO - https://packages.adoptium.net/artifactory/api/gpg/key/public \
  | gpg --dearmor > /usr/share/keyrings/adoptium.gpg
echo "deb [signed-by=/usr/share/keyrings/adoptium.gpg] https://packages.adoptium.net/artifactory/deb $(lsb_release -cs) main" \
  > /etc/apt/sources.list.d/adoptium.list
apt update && apt install -y temurin-25-jdk    # -> /usr/lib/jvm/temurin-25-jdk-amd64
```
Multi-instance needs several JDKs coexisting. Pin an absolute `java` path per instance; `update-alternatives` is global and will break you.

### GC flags
**Aikar's flags are G1GC tuning and are still correct on Java 21 and below.** On Java 25 (all of 26.x) the modern recommendation is Generational ZGC:
```
-Xms<N>M -Xmx<N>M -XX:+UseZGC -XX:+UseCompactObjectHeaders \
-XX:+UseStringDeduplication -XX:+AlwaysPreTouch -XX:TrimNativeHeapInterval=5000
```
(`-XX:+UseTransparentHugePages` on Linux with root; `-XX:-ZUncommit` if you can't set `Xms == Xmx`.) Generational ZGC went production-ready in Java 23; compact object headers are available from 25.

*Uncertain / verify before shipping:* the canonical Aikar flag string (`-XX:+UseG1GC -XX:+ParallelRefProcEnabled -XX:MaxGCPauseMillis=200 -XX:G1NewSizePercent=30 …`) and its >12 GB variant should be re-read from `https://mcflags.emc.gs` rather than trusted from memory. Also note the guidance is contested: some sources put the G1→ZGC crossover at ~12 GB heap, others at 32 GB. Below ~2 GB, G1 wins.

Never mix the two sets. `-XX:+AlwaysPreTouch` commits the whole heap at startup — on an N-instance VPS that multiplies.

---

## 3. Control plane

### Minecraft Server Management Protocol (preferred, 1.21.9 / 26.x+)
`server.properties`:
```properties
management-server-enabled=true
management-server-host=127.0.0.1
management-server-port=25585
management-server-secret=<40 alphanumeric chars>     # auto-generated if blank
management-server-tls-enabled=false                  # DEFAULT IS TRUE
management-server-tls-keystore=                      # PKCS12
management-server-tls-keystore-password=             # or env MINECRAFT_MANAGEMENT_TLS_KEYSTORE_PASSWORD
status-heartbeat-interval=0
```
Transport `ws://host:port` (or `wss://`), JSON-RPC 2.0. Auth by either `Authorization: Bearer <secret>` or `Sec-WebSocket-Protocol: minecraft-v1,<secret>`.

Methods:
```
minecraft:server/status  |  /save  |  /stop  |  /system_message
minecraft:players  |  minecraft:players/kick
minecraft:allowlist   [ /set /add /remove /clear ]
minecraft:operators   [ /set /add /remove /clear ]
minecraft:bans        [ /set /add /remove /clear ]
minecraft:ip_bans     [ /set /add /remove /clear ]
minecraft:gamerules   |  minecraft:gamerules/update
minecraft:serversettings/{autosave,difficulty,max_players,motd,game_mode,view_distance,status_heartbeat_interval,...}
rpc.discover          -> full JSON schema (codegen your Go client from this)
```
Notifications:
```
minecraft:notification/server/{started,stopping,saving,saved,status,activity}
minecraft:notification/players/{joined,left}
minecraft:notification/{operators,allowlist,bans,ip_bans}/{added,removed}
minecraft:notification/gamerules/updated
```
Paper supports it. A legacy-support plugin exists covering 1.8.8–1.21.9 for Bukkit/Spigot/Paper.

**Caution:** `management-server-tls-enabled` defaults to `true`, and TLS-enabled with no keystore **prevents the server from booting at all**. Also, the wiki's properties table lists `management-server-port` default as `0` while the 25w35a changelog says `25585` — the sources disagree, so always write it explicitly.

### RCON (fallback)
```properties
enable-rcon=true
rcon.port=25575
rcon.password=<required, non-empty>
broadcast-rcon-to-ops=false      # default true; set false so gvm automation doesn't spam ops
```
Wire format: `int32 LE length` (of the remainder, excluding itself) · `int32 request id` · `int32 type` · NUL-terminated body · one pad NUL. Types: `3` = AUTH, `2` = EXECCOMMAND (also the auth-response type), `0` = RESPONSE_VALUE. Auth success = a **type-2** packet echoing your request id; failure = request id **-1**. Max C→S payload 1446 (1460 total); max S→C payload 4096 (4110 total).

Quirks vs Valve Source RCON:
- **No end-of-response marker.** Responses fragment at 4096 bytes with nothing marking the last packet. "Read until < 4096" deadlocks on an exactly-4096-byte tail. Correct approach: send the real command, immediately send a second EXECCOMMAND with a distinct sentinel request id, read until the sentinel's response arrives.
- Decode as **ISO-8859-1**, not ASCII/UTF-8 — bodies carry `0xA7` section signs for color codes.
- Commands execute **on the main server thread**; a chatty health-check loop costs TPS.
- Password is plaintext, no encryption. Bind 127.0.0.1 only.

### Graceful shutdown
Ladder, in order: `minecraft:server/stop` → RCON `stop` → write `stop\n` to stdin → SIGTERM → SIGKILL (only after the timeout). Vanilla and Paper *do* install a SIGTERM shutdown hook, but it races systemd's `TimeoutStopSec` (**default 90 s** — too short for a big modpack). Set 300+.

**Do not use the screen+systemd pattern.** `ExecStop=screen -p 0 -S mc -X eval 'stuff "stop\015"'` returns immediately, so systemd kills the whole cgroup while the JVM is still saving. Workarounds (`KillMode=none`, `KillSignal=SIGCONT`) are both bad. Instead make gvm the supervisor: `Type=simple`, `ExecStart=/usr/local/bin/gvm run --instance <n>`, gvm owns the child's stdin and pipes, traps SIGTERM, runs the ladder. This is exactly `itzg/mc-server-runner`'s design (`-stop-command`, `-stop-duration`, `-stop-server-announce-delay`, `-detach-stdin`, `-named-pipe`, `-remote-console`) — reimplement it natively in Go rather than shelling out.

This preserves the PZ lesson (inject the clean-shutdown command, never SIGKILL) while dropping screen entirely, and gives you structured per-instance stdout for the dashboard for free.

### Backup flush
Legacy: `save-off` → `save-all flush` → snapshot → `save-on`. Note `save-all` alone is **asynchronous**; the `flush` argument is what makes it block.

Modern: `minecraft:serversettings/autosave=false` → `minecraft:server/save` → await `minecraft:notification/server/saved` → snapshot → `autosave=true`. No polling.

Two hard requirements: (a) a deferred restore **plus** an independent watchdog timer that re-issues the unfreeze unconditionally — dying between save-off and save-on leaves a running server that silently stops persisting; (b) keep the window to milliseconds via `cp --reflink=auto` or a btrfs/zfs snapshot, then tar from the frozen copy afterwards.

### Readiness detection
Ranked by reliability:
1. `minecraft:notification/server/started` (management protocol) — authoritative.
2. **Server-list-ping** on the game port (protocol 1.7+): handshake with `next_state=1`, status request, parse JSON for `players.online` / `players.max` / `version.name`. Platform-independent, credential-free, cheap, and doubles as the dashboard's player-count source. Go libraries exist (`xrjr/mcutils`, `iverly/go-mcping`).
3. Log line `Done (X.XXXs)! For help, type "help"` on `[Server thread/INFO]`. Weakest: Forge/NeoForge print a ModLauncher preamble first, modpacks reconfigure the log4j pattern, and color codes can wrap the line. Also watch `Starting Minecraft server on *:<port>` (bind confirmed) and `FAILED TO BIND TO PORT!` (the definitive multi-instance collision failure).

Define readiness as *TCP ping succeeds AND (mgmt notification OR log Done) within a deadline*.

---

## 4. Configuration files

### eula.txt
Generated on first boot, then the server exits:
```
#By changing the setting below to TRUE you are indicating your agreement to our EULA (https://aka.ms/MinecraftEULA).
#<Day Mon DD HH:MM:SS TZ YYYY>
eula=false
```
Paper also honours `-Dcom.mojang.eula.agree=true`.

### server.properties — keys that matter
Networking/instance: `server-port` (25565), `server-ip`, `query.port` (25565), `enable-query`, `rcon.port` (25575), `enable-rcon`, `rcon.password`, `broadcast-rcon-to-ops`, `management-server-*`.
World: `level-name` (world), `level-seed`, `level-type` (`minecraft:normal`), `difficulty` (easy), `hardcore`, `gamemode`, `spawn-protection` (16).
Perf: `view-distance` (10, 3–32), `simulation-distance` (10, 3–32), `max-tick-time` (60000; `-1` disables the watchdog — often needed for heavy modpacks), `sync-chunk-writes` (true — keep it), `max-players` (20).
Access: `online-mode` (true), `white-list`, `enforce-whitelist`, `allow-flight` (many mods need `true` to avoid false-positive kicks), `motd`.
New in 26.2: `chat-spam-threshold-seconds`, `command-spam-threshold-seconds` (both default 10).

**Removed** — writing these is a silent no-op:

| Removed key | Version | Replacement game rule |
|---|---|---|
| `pvp` | 25w35a | `pvp` |
| `allow-nether` | 25w35a | `allowEnteringNetherUsingPortals` |
| `spawn-monsters` | 25w35a | `spawnMonsters` |
| `enable-command-block` | 25w35a | `enableCommandBlocks` |
| `spawn-animals` | 1.21.2 (24w33a) | — |
| `spawn-npcs` | 1.21.2 (24w33a) | — |

The wiki also flags `white-list` as slated to default to `true` in **26.3** — version-dependent, verify at release.

Format is Java `.properties`: ISO-8859-1, with `:` `=` `#` `!` and leading whitespace backslash-escaped and backslashes doubled on rewrite. **The server regenerates the file on shutdown**, dropping comments and re-escaping — exactly the trap the PZ `.ini` presents. Use a real `.properties` codec, treat Postgres as the desired state and the file as a projection, and re-read after every clean stop.

### ops.json / whitelist.json
```json
[{"uuid":"550e8400-e29b-41d4-a716-446655440000","name":"PlayerName","level":4,"bypassesPlayerLimit":true}]
[{"uuid":"...","name":"PlayerName"}]
```
Op levels: 1 bypass spawn protection · 2 `/gamemode` `/tp` `/give` + command blocks · 3 `/ban` `/kick` `/op` · 4 `/stop` `/save-all`. `banned-players.json` and `banned-ips.json` exist alongside. All are **rewritten from in-memory state at shutdown** — edit them only while stopped, otherwise go through the management protocol or RCON.

UUIDs on an online-mode server must be real: `https://api.mojang.com/users/profiles/minecraft/<name>` returns `{"id": <undashed>, "name": ...}`; insert the hyphens yourself. Offline-mode UUIDs are UUIDv3 of `"OfflinePlayer:<name>"`.

### Paper CLI (useful for multi-instance isolation)
`--nogui` · `-W/--universe/--world-container/--world-dir` (default `.`) · `-P/--plugins` (default `plugins`) · `-c/--config` (default `server.properties`) · `-b/--bukkit-settings` · `-S/--spigot-settings` · `--paper-dir/--paper-settings-directory` (default `config`) · `-p/--port` · `-h/--host` · `-s/--max-players` · `-w/--level-name` · `--forceUpgrade` · `--eraseCache` · `--serverId` · `--nojline` · `--noconsole`. **CLI args override server.properties.** Use `--nojline` (and `TERM=dumb`) so captured stdout has no ANSI escapes.

Note Fabric/Forge/NeoForge have **no** `--world-container` equivalent — worlds stay in the server directory there.

---

## 5. Mod and plugin sources

### Modrinth v2 — the primary source
Base `https://api.modrinth.com/v2`. Unique User-Agent required. **300 req/min per IP**, verified headers `X-Ratelimit-Limit` / `X-Ratelimit-Remaining` / `X-Ratelimit-Reset`. Auth header is `Authorization: <token>` — a **raw PAT, no `Bearer` prefix**.

```
GET /v2/project/{id|slug}
GET /v2/project/{id|slug}/version?loaders=["fabric"]&game_versions=["26.2"]   (URL-encoded)
GET /v2/version/{id}
GET /v2/version_file/{hash}?algorithm=sha1        # verified 200
POST /v2/version_files                            # bulk hash -> version
POST /v2/version_files/update                     # bulk update check
GET /v2/search?facets=[["project_type:mod"],["versions:26.2"]]&index=downloads&limit=100
```

Version object (verified live):
```
game_versions[], loaders[], environment, id, project_id, author_id, featured,
name, version_number, changelog, changelog_url, date_published, downloads,
version_type (release|beta|alpha), status, requested_status, files[], dependencies[]

files[]        = {id, hashes{sha512,sha1}, url, filename, primary, size, file_type}
dependencies[] = {version_id, project_id, file_name, dependency_type}
                 dependency_type ∈ {required, optional, incompatible, embedded}
```

**The silent-filter bug I measured.** Same endpoint, two encodings, both HTTP 200:

| Query | Results | Actual filtering |
|---|---|---|
| `?loaders=["fabric"]&game_versions=["26.2"]` (URL-encoded JSON) | 12 | correct — all `26.2`/`fabric` |
| `?loaders=fabric&game_versions=26.2` | **1190** | **none — filters dropped silently** |

The unfiltered set contained `26.3-pre-2` and `1.21.1` entries. A Go client using `url.Values.Set("loaders","fabric")` will cheerfully install a snapshot mod onto a release server, with no error anywhere. Marshal to JSON, then URL-encode, then re-verify the returned `game_versions`/`loaders` before writing anything to disk.

**Environment metadata is dual now.** Project objects carry *both* the legacy `client_side` / `server_side` (`required`|`optional`|`unsupported`) *and* the new `environment` array. Version objects carry `environment` as a single string (observed: `client_or_server_prefers_both`). Value set: `client_and_server`, `client_only`, `client_only_server_optional`, `singleplayer_only`, `server_only`, `server_only_client_optional`, `dedicated_server_only`, `client_or_server`, `client_or_server_prefers_both`, `unknown`.

For a dedicated server, install when `environment` is one of `client_and_server`, `server_only`, `server_only_client_optional`, `dedicated_server_only`, `client_or_server`, `client_or_server_prefers_both`; skip `client_only` / `singleplayer_only`; on `unknown` fall back to `server_side != "unsupported"`. **This is structurally the same trap as PZ workshop-IDs vs internal Mod IDs** — two overlapping metadata systems, and trusting the wrong one breaks the server.

Dependency rules: only `required` is a hard fetch. `embedded` must **not** be downloaded (already inside the parent jar; fetching it causes duplicate-mod crashes). `incompatible` aborts the plan. `version_id` set = exact pin; only `project_id` set = resolve newest matching loader+game_version. There is no per-version transitive-closure endpoint (`/v2/project/{id}/dependencies` is project-level), so gvm must BFS.

### Hangar (Paper/Velocity plugins)
```
GET  /api/v1/projects?limit=&offset=
     -> {"pagination":{"count":3763,"limit":1,"offset":0},"result":[{id,name,namespace{owner,slug},stats,
         category,description,lastUpdated,visibility,settings,supportedPlatforms,memberNames,avatarUrl,...}]}
GET  /api/v1/projects/{author}/{slug}/versions
GET  /api/v1/projects/{author}/{slug}/versions/{name}/{platform}/download   # platform ∈ PAPER|VELOCITY|WATERFALL
POST /api/v1/authenticate?apiKey=<key>   -> JWT, then  Authorization: HangarAuth <jwt>
```
Anonymous reads work but are rate-limited harder. Many versions are **external** links (GitHub releases) rather than hosted files. The raw OpenAPI is *not* at `/api/v1/openapi.json` (404); `/api-docs` is an SPA.

### Spiget (SpigotMC mirror) — best-effort only
`GET /v2/resources/{id}` returns an **`external` boolean** and `file{type,size,url}`; `/v2/resources/{id}/download` 302s to `cdn.spiget.org` for locally-hosted files or to a third-party URL when `external` is true. Check `external` *first* — otherwise you write an HTML landing page or a Cloudflare challenge into `plugins/`. Premium resources are unfetchable. Always verify ZIP magic `PK\x03\x04` and the presence of `plugin.yml`/`paper-plugin.yml`.

### CurseForge v1 (relevant to target 7b)
`x-api-key` header; `gameId=432` is Minecraft. `/v1/mods/search`, `/v1/mods/{modId}/files`, `/v1/mods/{modId}/files/{fileId}/download-url`. Keys are approved per-project by Overwolf. **The blocker: authors can set `allowModDistribution: false`, in which case `download-url` returns null and there is no API path to the jar.** Modpack manifests referencing those files can't be fully automated — design the resolver to report "N files require manual placement" with names and links, not an opaque failure.

---

## 6. Multi-instance and ports

Per instance: **25565** game (TCP; UDP too when `enable-query=true`, `query.port` default 25565) · **25575** RCON · **25585** management. Allocate a block per instance from a Postgres-backed allocator and write every port explicitly. Extend the existing `firewall.sh` so ufw opens **only** the game port; assert that RCON and the management server bind `127.0.0.1`. An RCON port on a public interface is a plaintext-password remote console.

Suggested install phases, mirroring the PZ `rebuild.sh` discipline: **resolve → download+verify checksum → materialize (run the Forge/NeoForge installer, or first-boot Paperclip/Fabric so `libraries/`+`cache/` populate) → configure → archive the clean tree → start.** The materialize phase needs outbound internet and must be explicit and observable — Paperclip downloads and sha256-verifies the vanilla jar from Mojang on first run, and the Fabric launcher does the same for loader+server. Never let that happen on a user's first "start" click. And never destroy an instance tree without tarring it first, with world directories excluded from the destroy set so a rebuild is a binary swap rather than a world reset.

## 7. Adoption path for an existing unmanaged server

Modrinth's hash lookup is the primitive: sha1 every jar in `mods/` (or `plugins/`) and `POST /v2/version_files` to reconstruct a manifest. Whatever doesn't resolve gets recorded as a checksummed "vendored" file preserved verbatim. Combined with reading the existing `server.properties`, `ops.json` and `whitelist.json` **while the server is stopped**, this lets gvm adopt a live server without touching its content — the same shape as the PZ world-preserving import requirement.

## Sources

- https://launchermeta.mojang.com/mc/game/version_manifest_v2.json
- https://piston-meta.mojang.com/v1/packages/168cea25a2a1cbbcd1a55ea40bf184b7c66a29f3/26.3-pre-2.json
- https://www.minecraft.net/en-us/article/minecraft-new-version-numbering-system
- https://minecraft.wiki/w/Java_Edition_26.1
- https://docs.papermc.io/misc/downloads-service/
- https://fill.papermc.io/v3/projects/paper
- https://fill.papermc.io/v3/projects/paper/versions/26.2/builds
- https://docs.papermc.io/paper/reference/cli-arguments/
- https://docs.papermc.io/paper/reference/system-properties/
- https://docs.papermc.io/paper/reference/vanilla-data-files/
- https://github.com/PaperMC/Paperclip
- https://api.purpurmc.org/v2/purpur
- https://api.purpurmc.org/v2/purpur/26.2
- https://purpurmc.org/docs/purpur/
- https://meta.fabricmc.net/
- https://meta.fabricmc.net/v2/versions/installer
- https://meta.fabricmc.net/v2/versions/loader/26.2
- https://wiki.fabricmc.net/install
- https://github.com/itzg/docker-minecraft-server/blob/master/docs/types-and-platforms/server-types/fabric.md
- https://meta.quiltmc.org/v3/versions/loader
- https://quiltmc.org/en/about/faq/
- https://maven.neoforged.net/releases/net/neoforged/neoforge/maven-metadata.xml
- https://docs.neoforged.net/user/docs/server/
- https://github.com/neoforged/ServerStarterJar
- https://files.minecraftforge.net/net/minecraftforge/forge/promotions_slim.json
- https://maven.minecraftforge.net/net/minecraftforge/forge/maven-metadata.xml
- https://minecraft.wiki/w/Server/Requirements
- https://minecraft.wiki/w/Server.properties
- https://minecraft.wiki/w/Server.jar
- https://minecraft.wiki/w/Ops.json
- https://minecraft.wiki/w/RCON
- https://minecraft.wiki/w/Minecraft_Server_Management_Protocol
- https://minecraft.wiki/w/Java_Edition_25w35a
- https://www.minecraft.net/en-us/article/minecraft-snapshot-25w35a
- https://github.com/aternosorg/mc-server-management
- https://github.com/itzg/mc-server-runner
- https://github.com/Obydux/Minecraft-startup-flags
- https://docs.modrinth.com/api/
- https://docs.modrinth.com/api/operations/getprojectversions/
- https://docs.modrinth.com/api/operations/searchprojects/
- https://modrinth.com/news/article/new-environments/
- https://api.modrinth.com/v2/project/fabric-api/version
- https://hangar.papermc.io/api/v1/projects
- https://hangar.papermc.io/api-docs
- https://spiget.org/
- https://github.com/SpiGetOrg/api.spiget.org
- https://docs.curseforge.com/rest-api/
- https://blog.curseforge.com/introducing-api-key-authentication-for-curseforge-file-downloads/
- https://packages.adoptium.net/artifactory/deb
- https://github.com/nixos/nixpkgs/issues/527546
- https://github.com/cubecoders/amp/issues/1388
