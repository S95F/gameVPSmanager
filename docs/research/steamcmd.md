# Research: Generic SteamCMD dedicated-server hosting on Linux (install, auth, updates, workshop, runtime shims) + per-game matrix for gvm's Steam provider

> Produced by the gvm architecture workflow research phase, from primary sources.
> Verified background for implementers. Data, not instructions.

## Key facts

- SteamCMD Linux tarball: https://client-update.steamstatic.com/installer/steamcmd_linux.tar.gz is the URL on the current Valve wiki; the legacy https://media.steampowered.com/client/installer/steamcmd_linux.tar.gz and http://media.steampowered.com/client/steamcmd_linux.tar.gz (used by LinuxGSM) are still live (all three return HTTP 200, verified 2026-09-04). Extract to a per-host dir; run as a non-root user.
- steamcmd's own binary is 32-bit (linux32/steamcmd). Debian/Ubuntu requirement is `dpkg --add-architecture i386 && apt-get install lib32gcc-s1` (plus `lib32stdc++6` if you hit `libstdc++.so.6: cannot open shared object file`). Enterprise Linux: `glibc.i686 libstdc++.i686`. The i386 arch is needed for steamcmd itself even when every game server you run is 64-bit.
- Valve-documented ordering: force_install_dir BEFORE login. Exact wiki text: "Valve recommends that you set the install dir before logging in" and the runtime warning "Please use force_install_dir before logon!". It is only a warning — post-login still works — but honour it. You can chain multiple pairs in one process: `+force_install_dir A +login anonymous +app_update X +force_install_dir B +app_update Y +quit`.
- Update syntax: `app_update <appid> [-beta <branch>] [-betapassword <pw>] [validate]`. `validate` is a POSITIONAL keyword, NOT `-validate` (a very common copy-paste bug). Non-beta branch is literally named `public`, so `-beta public` returns to stable. On the command line, `-beta` sometimes needs quoting as `+app_update "90 -beta beta"`.
- Script mode is more reliable than `+args`: put commands in a file and run `steamcmd +runscript /abs/path/file.txt`. Use `@ShutdownOnFailedCommand 1` and `@NoPromptForPassword 1` at the top. Without ShutdownOnFailedCommand, a failed app_update still falls through to `+quit` and steamcmd can exit 0.
- Exit codes are NOT documented by Valve (open request: ValveSoftware/steam-for-linux#4341, never answered). LinuxGSM's production approach is to tee stdout to a log and grep for `Error!` plus hex state codes, and only use $? as a secondary signal. gvm must do the same.
- SteamCMD hex state codes (LinuxGSM-Docs/steamcmd/errors.md): 0x202=disk quota/space, 0x212=insufficient disk space, 0x402=connection/update did not complete, 0x602=update required but not completed, 0x606=disk write failure (usually permissions), 0x626=missing update files, 0x6A6=corrupt update files, 0x10E=HLDS flake (retry), 0x2/0x206 unknown, 0x6=no connection to content servers. In-progress states: 0x3 reconfiguring, 0x5 validating, 0x11 preallocating, 0x61 downloading, 0x101 committing.
- Deterministic install verification (better than exit codes): parse `<install_dir>/steamapps/appmanifest_<appid>.acf`. Check StateFlags==4 (fully installed), and that BytesDownloaded==BytesToDownload AND BytesStaged==BytesToStage. LinuxGSM treats a mismatch on either pair as a half-completed update and forces a re-run. Also guard against >1 or 0 appmanifest files for the same appid.
- Update detection without downloading: local build = `buildid` key in appmanifest_<appid>.acf; remote build = `steamcmd +login <user> +app_info_request <appid> +app_info_update 1 +app_info_print <appid> +quit`, then read branches.<branch>.buildid. CRITICAL GOTCHA: you must delete every cached `appinfo.vdf` under $HOME first (`find $HOME -name appinfo.vdf -delete`) or app_info_print returns stale buildids. LinuxGSM does exactly this.
- Anonymous login (`login anonymous`) covers the large majority of dedicated servers. Confirmed credentialed-login servers among the popular set: DayZ 223350 (account must OWN DayZ), Terraria 105600 (game purchase required), Arma 3 233780, Killing Floor 215350/215360, Starbound 211820, Natural Selection 2 4940. Everything else in the gvm target list is anonymous.
- Steam Guard automation: after one successful interactive login, steamcmd caches a refresh token in `<steamcmd_dir>/config/config.vdf` plus sentry file(s) `ssfn<digits>` in the steam root. Subsequent `+login <user>` (no password) works. `+set_steam_guard_code <code>` exists for feeding a code non-interactively. Mobile-authenticator TOTP can also be passed as a third positional: `+login <user> <pass> <5charcode>`. LinuxGSM detects the failure string `Two-factor code mismatch`.
- steamclient.so shim: many servers call SteamAPI_Init and dlopen `$HOME/.steam/sdk64/steamclient.so` (32-bit servers: `sdk32`). SteamCMD does not place it there. Fix: `mkdir -p ~/.steam/sdk64 && ln -s <steamcmd>/linux64/steamclient.so ~/.steam/sdk64/steamclient.so` (and `<steamcmd>/linux32/steamclient.so -> ~/.steam/sdk32/`). Alternative source: `steamcmd +@sSteamCmdForcePlatformType linux +login anonymous +app_update 1007 +quit` (Steamworks SDK Redist) yields linux64/steamclient.so.
- SteamAppId shim: several servers are installed under the SERVER appid but must announce the CLIENT/game appid at runtime. Valheim's shipped start_server.sh does `export SteamAppId=892970` (server app is 896660); Core Keeper needs `SteamAppId=1621690` (server app 1963720); V Rising ships a `steam_appid.txt` containing 1604030 (server app 1829350). Same scripts also do `export LD_LIBRARY_PATH=./linux64:$LD_LIBRARY_PATH`. gvm must model {install_appid, runtime_appid, ld_library_path_extra} per game.
- Workshop content lives under `<force_install_dir>/steamapps/workshop/content/<GAME_appid>/<publishedfileid>` — note GAME appid, not server appid (PZ: server 380870, workshop under 108600). `workshop_download_item <appid> <publishedfileid> [validate]` is the direct path; it only works anonymously if the developer ticked Steamworks > app > Workshop > "Enable anonymous game servers to download workshop items". Steamworks docs: "If the user is not subscribed to the item (e.g. a Game Server using anonymous login), the workshop item will be downloaded and cached temporarily."
- The "1MB workshop limit" is a misremembering: the documented 1MB cap is on the workshop item PREVIEW IMAGE (ISteamUGC::SetItemPreview — "must be under 1MB"). There is no 1MB cap on workshop downloads. The real anonymous constraints are (a) app opt-in, and (b) practical flakiness/timeouts on multi-GB items, which is why the PZ suite lets the server itself fetch items via WorkshopItems= instead of calling workshop_download_item.
- Workshop metadata without any API key (verified live 2026-09-04): `POST https://api.steampowered.com/ISteamRemoteStorage/GetPublishedFileDetails/v1/` with form fields `itemcount=N` and `publishedfileids[0]=...`. Returns per item: `publishedfileid`, `result` (1=ok, 9=not found), `consumer_app_id`, `creator_app_id`, `file_size`, `time_created`, `time_updated`, `title`, `banned`, `visibility`, and `tags[]` (for PZ these include "Build 41"/"Build 42"). Companion keyless endpoint for collections: `POST .../ISteamRemoteStorage/GetCollectionDetails/v1/` with `collectioncount` + `publishedfileids[0]`.
- Source RCON canonical ports (Valve SRCDS wiki): 27015 TCP/UDP game + RCON (`-port`), 27020 UDP SourceTV (`+tv_port`), 27005 UDP client (`-clientport`), 26900 UDP outbound Steam port (`-sport`).
- CS2: the dedicated server and client merged into appid 730; appid 740 (old CS:GO DS) is dead. ~60-65 GB download, glibc >= 2.31 (so Ubuntu 20.04+/Debian 11+), min 2 GB RAM. Launch via `game/cs2.sh -dedicated`, never `game/bin/linuxsteamrt64/cs2` directly. GSLT required for public listing: `+sv_setsteamaccount <token>`, generated at http://steamcommunity.com/dev/managegameservers with game id 730.
- Cross-platform install for Windows-only servers on Linux: `steamcmd +@sSteamCmdForcePlatformType windows +force_install_dir ... +login anonymous +app_update <id> validate +quit` (values: windows|macos|linux). Needed for V Rising 1829350, Enshrouded 2278520, ARK: Survival Ascended 2430930, then run under Wine/Proton.
- HOME is load-bearing: steamcmd writes `$HOME/.steam` and `$XDG_DATA_HOME/Steam` (~/.local/share/Steam). systemd units must set `Environment=HOME=...`. LinuxGSM pre-creates ~/.steam/root and ~/.steam/steam as symlinks to ~/.local/share/Steam to work around ValveSoftware/steam-for-linux#6976 (steamcmd inconsistently uses ~/.steam vs ~/Steam).
- ulimit: SteamCMD and several servers (notably ARK) need a raised file-descriptor limit; `ulimit -n 2048` minimum for steamcmd, ARK wants ~100000. The existing PZ unit already sets LimitNOFILE=100000 — keep that as a per-game tunable.

## Pitfalls

- Trusting steamcmd's exit code. It self-updates and re-execs on many runs, and with `+`-style args it will run `+quit` after a failed `+app_update` and can still exit 0. Always tee stdout, grep for `Error!` / `0x[0-9A-F]{3}`, AND verify the appmanifest afterwards.
- Writing `-validate` instead of `validate`. `app_update 294420 -validate` silently does no validation (it is parsed as an unknown option), so a corrupted install looks like it validated.
- Stale `appinfo.vdf` making update checks return the old buildid forever. Your nightly job will report 'no update' indefinitely. Delete appinfo.vdf under HOME before every app_info_print.
- Running two steamcmd processes concurrently under the same HOME. They fight over the same ~/.steam / ~/.local/share/Steam state and over appmanifest files. For N instances, either serialize steamcmd through a mutex in gvm, or give each instance its own HOME. Never share one force_install_dir between two instances.
- Assuming the Valve wiki's Dedicated Servers List is correct. It is demonstrably stale: it lists Project Zomboid's DS as appid 108600 / anonymous=No / 'Game Purchase Required' (actually 380870, anonymous works — that is what the existing install.sh does), lists 7 Days to Die Linux as anonymous=No (it works), lists Squad Windows as 4103240 (typo for 403240), and still lists CS:GO 740. Treat it as a hint, not a source of truth; verify per game.
- Forgetting the steamclient.so symlink. Failure mode is a server that starts, prints `[S_API FAIL] SteamAPI_Init() failed` or `Failed to load module '/home/x/.steam/sdk64/steamclient.so'`, then either exits or runs invisibly to the server browser. Bite-sized to fix, painful to diagnose from a dashboard.
- Forgetting the SteamAppId env shim (Valheim 892970, Core Keeper 1621690). The server simply will not start, with no useful error.
- Treating 'has RCON' as universal. Valheim, Satisfactory, Enshrouded, Core Keeper, 7 Days to Die (telnet only), vanilla Terraria and DayZ (BattlEye, not Source) all need a non-RCON control path. If gvm's stop/save/announce plane assumes Source RCON, half the catalogue is unmanageable.
- SIGTERM-kills a Valheim world. systemd's default KillSignal=SIGTERM does NOT trigger Valheim's save; you must set `KillSignal=SIGINT`. This is the exact same class of bug the PZ scripts already avoid with the screen-stuffed save/quit + KillSignal=SIGCONT trick — generalize it into a per-game 'shutdown recipe' rather than one hardcoded behaviour.
- Anonymous workshop downloads that 'succeed' but produce nothing, on apps that never opted into anonymous UGC. Check for the item directory + non-empty content after `workshop_download_item`, do not trust the printed 'Success'.
- CS2 needs ~65 GB and a GSLT per instance; multi-instance CS2 on a small VPS will fill the disk. GSLTs are per-server and get revoked if the account is flagged — surface that in the dashboard.
- DayZ and Terraria require credentialed SteamCMD login. That drags Steam Guard, refresh-token expiry, and 'a user can only be logged in once at any time (counting both graphical client and SteamCMD logins)' into a headless VPS. Refresh tokens do expire and automation silently starts failing months later.
- Games installed under a server appid but reporting a different appid at runtime will 404 in the server browser if the shim is wrong — but will otherwise look healthy in `systemctl status`. Health checks must be protocol-level (A2S query / RCON auth / HTTP API), not just 'process is alive'.
- ARK: Survival Ascended, V Rising and Enshrouded have no native Linux server. Any 'generic Steam provider' that assumes a Linux depot will download nothing (or error `Invalid platform`) for these. Model a `platform_override: windows` + `runtime: proton|wine` capability from day one or you will retrofit it badly.
- Directory-name capitalization: the CS2 wiki notes 'On some Linux distributions the path cannot contain any capital letters' for force_install_dir. Generate instance paths lowercase.

## Recommendations

- Model a game as a declarative spec, not code: {install_appid, runtime_appid/steam_appid_shim, platform (linux|windows+proton), anonymous_login (bool), branch, launch: {cwd, argv, env{LD_LIBRARY_PATH,SteamAppId}}, ports[{name,proto,default,arg}], control_plane (source_rcon|telnet|websocket_rcon|battleye|http_api|stdin_only), shutdown_recipe, workshop {game_appid, mechanism}, ram_floor_mb}. That single struct covers every row of the table below and keeps the Go code generic.
- Implement the SteamCMD driver as: write a runscript file with `@ShutdownOnFailedCommand 1` + `@NoPromptForPassword 1`, exec `steamcmd +runscript <abs path>`, stream stdout into both a ring buffer and a log file, classify by regex on `Error!` + hex codes, then ALWAYS post-verify via appmanifest (StateFlags, BytesDownloaded==BytesToDownload, BytesStaged==BytesToStage). Retry up to N times with backoff on 0x402/0x602/0x606/0x626/0x6A6/0x10E; hard-fail immediately on 0x202/0x212 (disk), `No subscription`, `Invalid platform`, `Two-factor code mismatch`, `Password check for AppId`.
- Serialize all steamcmd invocations per VPS through one mutex/queue (a `steam_jobs` table in Postgres with an advisory lock works well and gives the dashboard a live job feed). N concurrent instances updating at once is the #1 way to corrupt an install.
- Build the update checker on buildid comparison, not on a nightly blind `app_update`. Store `installed_buildid` per instance in Postgres; poll remote buildid via app_info_print (after deleting appinfo.vdf); only stop the server when they differ. This turns the PZ nightly.sh timer into a cheap check with a rare, deliberate restart — big win for a multi-instance box.
- Generalize the PZ shutdown knowledge into a per-game `shutdown_recipe` enum: (a) rcon_command_sequence (PZ: `save` then `quit`; CS2/GMod/L4D2: `quit`), (b) stdin_stuff via screen/tmux/pty (Valheim console, Enshrouded, Core Keeper), (c) signal (Valheim: SIGINT), (d) http_api (Satisfactory shutdown endpoint), (e) telnet (7DTD: `shutdown`). Keep the existing 'never SIGKILL before TimeoutStopSec' invariant globally, with per-game TimeoutStopSec.
- Prefer a Go pty/pipe supervisor over `screen` for the generic case. gvm as a long-running supervisor can own the child's stdin/stdout directly (giving you stdin control + log capture + clean shutdown for the no-RCON games), while still emitting a systemd unit per instance for boot persistence. Keep an opt-in screen/tmux mode for the PZ migration path so the live server's existing operator muscle memory (`screen -r pzserver`) keeps working.
- For workshop, use ISteamRemoteStorage/GetPublishedFileDetails/v1 (keyless) as the metadata source of truth: `time_updated` gives you real mod-update detection, `file_size` gives you disk pre-flight, `consumer_app_id` validates the item belongs to the game, and `tags` resolves the PZ Build 41 vs Build 42 ambiguity that sync-mods.sh currently guesses at with mod.info heuristics. Keep the mod.info scan as a fallback and cross-check the two.
- Keep the two-list concept from sync-mods.sh as a first-class generic idea: `acquisition_ids` (numeric workshop/publishedfile IDs) vs `activation_ids` (game-internal mod IDs). PZ needs it (WorkshopItems= vs Mods=), DayZ needs it (-mod=@Name symlinks), ARK needs it, and CurseForge Minecraft will need it too (project id vs jar filename). Design the schema with both columns now.
- Do not put SteamCMD credentials in config.env-equivalents. Store credentialed accounts in Postgres encrypted (or better, keep only the steamcmd `config/config.vdf` + `ssfn*` on disk with 0600 and a dedicated Steam account per VPS), gate the 'add Steam account' action behind the highest RBAC role, and never render the password back to the dashboard. Surface 'Steam login expired' as an explicit alert state because refresh tokens do expire silently.
- Give the firewall module a port model driven by the spec's ports[] array (name/proto/default), not a hardcoded list, and keep the SSH-port autodetection from firewall.sh. Default-deny the admin planes: RCON, 7DTD telnet 8081, Rust 28016, Palworld 25575/8212, TShock 7878 and Satisfactory 7777/TCP should bind to 127.0.0.1 or be firewalled to the Caddy host only — Palworld's own docs say the API is 'not designed to be exposed directly to the Internet'.
- Health-check protocol-level, per control plane: A2S_INFO on the query port for Source-family and Steam-networked games, RCON auth handshake where available, HTTP GET on the API for Satisfactory/TShock/Palworld-REST, log-marker scraping (the `pz_boot_and_wait` pattern) as the universal fallback for stdin-only games. Store last_seen_players/last_healthy in Postgres for the dashboard.
- Write the SteamCMD driver against an interface with a fake implementation so it unit-tests on Windows; put anything touching syscall/unix (setuid, prctl, pty, cgroups) behind `//go:build linux` and keep the spec parser, buildid diffing, appmanifest (.acf/VDF) parsing and workshop API client fully cross-platform. .acf is Valve KeyValues text — write a small VDF parser rather than grepping.
- For the live-PZ adoption path: import by reading the existing config.env + <servername>.ini + appmanifest_380870.acf (to seed installed_buildid) and registering the existing paths as an instance with `managed_install=false` first. Do not run `app_update ... validate` on adoption — validate overwrites customized files, and the Valve wiki explicitly warns about this. Take a backup.sh-equivalent archive before the first gvm-initiated action, exactly like rebuild.sh does.

## Detail

## 1. SteamCMD on Debian 12 / Ubuntu 22.04 / 24.04

**Install (manual, what gvm should do — the distro package pulls in `multiverse`/`non-free` and an EULA prompt):**

```bash
dpkg --add-architecture i386
apt-get update
apt-get install -y lib32gcc-s1            # + lib32stdc++6 if libstdc++.so.6 errors
# UTF-8 locale: steamcmd misbehaves without one (existing install.sh already does this)
mkdir -p /opt/gvm/steamcmd && cd /opt/gvm/steamcmd
curl -sqL "https://client-update.steamstatic.com/installer/steamcmd_linux.tar.gz" | tar zxvf -
```

All three known tarball URLs return 200 as of 2026-09-04: `client-update.steamstatic.com/installer/steamcmd_linux.tar.gz` (current wiki), `media.steampowered.com/client/installer/steamcmd_linux.tar.gz` (what the existing `install.sh` uses), `media.steampowered.com/client/steamcmd_linux.tar.gz` (LinuxGSM). Pin one, keep a fallback.

i386 is required because **steamcmd itself is a 32-bit binary** (`linux32/steamcmd`), independent of whether the game server is 64-bit. Only a handful of *servers* still need 32-bit libs at runtime (GoldSrc/Source-1: `srcds_run`, `hlds_run`, Garry's Mod, L4D2, Sven Co-op — the wiki's Sven row spells out `libc6:i386 zlib1g:i386 libssl1.1:i386 libstdc++6:i386`).

`ulimit -n`: steamcmd raises it itself but fails on locked-down hosts; ARK wants ~100000. Set `LimitNOFILE` per game in the unit.

**HOME is load-bearing.** steamcmd writes `$HOME/.steam` and `$XDG_DATA_HOME/Steam` (`~/.local/share/Steam`). ValveSoftware/steam-for-linux#6976 documents it inconsistently using `~/.steam` vs `~/Steam`; LinuxGSM's fix is to pre-create `~/.local/share/Steam` and symlink `~/.steam/root` and `~/.steam/steam` to it. Copy that. systemd units must carry `Environment=HOME=...` (the existing `zomboid.service.template` already does).

## 2. Login: anonymous vs credentialed, and Steam Guard

`login anonymous` covers the overwhelming majority. Servers in scope that **require a real account**: DayZ 223350 (account must own DayZ), Terraria 105600 (game purchase required), plus Arma 3 233780, Killing Floor 215350/215360, Starbound 211820, Natural Selection 2 4940. Wrong-account symptom: `ERROR! Failed to install app 'xxxxxx' (No subscription)`.

Also note the wiki's constraint: *"A user can only be logged in once at any time (counting both graphical client as well as SteamCMD logins)"* — a shared personal account will get bounced.

**Steam Guard mechanics for automation:**
- First interactive login writes a **refresh token into `<steam root>/config/config.vdf`** and one or more **sentry files `ssfn<digits>`** in the steam root. After that, `+login <user>` with no password works headlessly.
- `+set_steam_guard_code <code>` feeds a code non-interactively; the mobile-authenticator TOTP can also be given as a third positional arg to `+login user pass CODE`.
- `@NoPromptForPassword 1` prevents an interactive hang.
- Failure string to detect: `Two-factor code mismatch` (LinuxGSM greps exactly this).
- **Tokens expire.** Long-lived VPS automation using a credentialed account will silently break months later ("Cached Credentials Not Found" class of errors). gvm needs a `steam_account.status` field and a loud dashboard alert, plus a documented re-auth flow (operator runs one interactive steamcmd login on the box).
- Never check `config.vdf` / `ssfn*` into anything; chmod 0600, owned by the service user.

## 3. app_update, validate, branches

```
app_update <appid> [-beta <betaname>] [-betapassword <password>] [validate]
```

- `validate` is **positional, not a flag**. `-validate` is silently ignored — a very common bug in copy-pasted guides.
- Validation **overwrites customized files** ("if you customize mapcycle.txt, this file will be overwritten to the server default"). Files not part of the install are untouched. So: validate on install, on suspected corruption, and after a partial-download detection — not on every nightly update.
- Non-beta branch name is literally `public`: `app_update 90 -beta public` gets you off a beta.
- Command-line quoting quirk: `+app_update "90 -beta beta"` (or `"+app_update 90 -beta beta"`).
- Cross-platform: `+@sSteamCmdForcePlatformType windows|macos|linux`.
- GoldSrc (appid 90) is a special case needing `app_set_config 90 mod <name>` first and often multiple runs.

**Prefer runscript over `+args`:**

```
@ShutdownOnFailedCommand 1
@NoPromptForPassword 1
force_install_dir /srv/gvm/instances/pz-main
login anonymous
app_update 380870 validate
quit
```
`steamcmd +runscript /abs/path/script.txt` — the wiki notes relative script paths often fail; always pass an absolute path.

## 4. Detecting a failed or partial download — reliably

There is **no official exit-code table**. ValveSoftware/steam-for-linux#4341 is an explicit request for one and was never answered; the only concrete data point there is exit code 8 alongside state `0x602`.

**Layer 1 — parse stdout.** LinuxGSM's `fn_dl_steamcmd` tees output to a log and greps. Its exact classification:

| Match | Meaning | Action |
|---|---|---|
| `0x202`, `0x212` | not enough disk space/quota | hard fail |
| `Invalid platform` | wrong `sSteamCmdForcePlatformType`/arch | hard fail |
| `No subscription` | account lacks the license | hard fail |
| `Two-factor code mismatch` | Steam Guard | hard fail |
| `Password check for AppId` | wrong `-betapassword` | hard fail |
| `0x402`, `0x602` | update required but not completed (network) | retry |
| `0x606` | disk write failure (usually permissions) | retry |
| `0x626` | missing update files | retry |
| `0x6A6` | corrupt update files | retry |
| `0x10E` | HLDS flake | retry |
| `Error!` + none of the above | unknown | retry, then fail |

LinuxGSM retries up to 10 times. Non-failure in-progress states you may see mid-stream: `0x3` reconfiguring, `0x5` validating, `0x11` preallocating, `0x61` downloading, `0x101` committing. `0x6` = no connection to content servers.

**Layer 2 — the appmanifest (this is the authoritative check).** `<force_install_dir>/steamapps/appmanifest_<appid>.acf` is Valve KeyValues text:

- `StateFlags` — `4` = fully installed. `1026` = update started + update required (nothing usable on disk). `1062` = update started + update required + fully installed + missing files (corrupt). Treat anything != 4 as not-installed.
- `BytesDownloaded` vs `BytesToDownload`, and `BytesStaged` vs `BytesToStage` — LinuxGSM forces a re-download whenever either pair mismatches. This is what catches "steamcmd said Success but the install is half a gigabyte short".
- `buildid` — the installed build.
- Guard for **0 or >1** matching appmanifest files (a real, recurring failure mode; LinuxGSM deletes and re-downloads).

**Layer 3 — spec-declared sentinel file.** Same idea as the existing `install.sh` line `[ -x "$INSTALL_DIR/start-server.sh" ] || die`. Every game spec should name a required post-install file; check it exists and is executable.

**Update detection without downloading:**

```bash
find "$HOME" -name appinfo.vdf -delete      # MANDATORY or you get stale data
steamcmd +login anonymous +app_info_request $APPID +login anonymous \
  +app_info_update 1 +app_info_print $APPID +quit
# extract branches.<branch>.buildid, compare to appmanifest buildid
```
LinuxGSM literally does the double `+login` and the `appinfo.vdf` delete; both are workarounds for real bugs.

## 5. The force_install_dir ordering gotcha

Valve wiki: *"Valve recommends that you set the install dir before logging in."* Modern steamcmd prints `Please use force_install_dir before logon!` when you get it wrong. It is a **warning**, not an error — post-login still installs — but there are field reports (steam-for-linux#8709, #10609) of it silently installing into the default `steamapps/common` path. Always emit `+force_install_dir` first.

Note the existing `install.sh` and `update.sh` already do this correctly (`+force_install_dir ... +login anonymous +app_update ...`) — preserve that ordering when porting.

Multiple installs in one process are legal and cheaper than N steamcmd launches:
```
+force_install_dir /srv/a +login anonymous +app_update 380870 validate \
+force_install_dir /srv/b +app_update 896660 validate +quit
```

## 6. Workshop content acquisition

Two mechanisms, and the choice is per-game:

**(a) In-game / server-side acquisition** — the server itself subscribes and downloads. This is what PZ does (`WorkshopItems=` in the ini → server downloads on boot), what ARK does (`-AutoManagedMods`), and what Garry's Mod does (`+host_workshop_collection <id>` with `-authkey <steam web api key>`). It is more reliable for large content and does not need anonymous-UGC opt-in, but you cannot easily pre-stage it or know when it finished — hence the existing `pz_boot_and_wait "SERVER STARTED" 3600` boot-and-scrape pattern. **Keep that pattern; it generalizes to any game whose mod fetch is server-driven.**

**(b) `workshop_download_item <game_appid> <publishedfileid> [validate]`** — direct. Lands in `<force_install_dir>/steamapps/workshop/content/<game_appid>/<id>`. Constraints:
- Anonymous only works if the developer ticked **Steamworks → app → Technical Tools → Edit Steamworks Settings → Workshop → "Enable anonymous game servers to download workshop items"**. Many apps never did (Barotrauma #7707 is the canonical bug report).
- Steamworks docs on server-side UGC: *"If the user is not subscribed to the item (e.g. a Game Server using anonymous login), the workshop item will be downloaded and cached temporarily."* — "temporarily" means don't treat that cache as durable state.
- Large items (~>1GB) time out and stall in practice; the existing `sync-mods.sh` warning ("Steam sometimes stalls… delete its folder and restart") is accurate and should be preserved as retry logic.

**The "1MB / anonymous limit" is a conflation.** The documented 1MB cap is on the **workshop preview image** (`ISteamUGC::SetItemPreview`: the preview *"must be under 1MB"*). There is no 1MB download cap. The real anonymous limits are the opt-in flag and the temporary-cache semantics.

**Workshop appid ≠ server appid.** PZ: server 380870, workshop under `steamapps/workshop/content/108600`. The existing `lib/common.sh` already encodes both `PZ_APPID` and `PZ_GAME_APPID` and probes two possible workshop roots — generalize to `{install_appid, workshop_appid, workshop_roots[]}`.

**Keyless Steam Web API for workshop metadata** (verified live, 2026-09-04):

```
POST https://api.steampowered.com/ISteamRemoteStorage/GetPublishedFileDetails/v1/
     itemcount=1&publishedfileids[0]=2822286426
```
returns `{"response":{"result":1,"publishedfiledetails":[{ "publishedfileid","result","creator","creator_app_id","consumer_app_id","file_size","hcontent_file","preview_url","title","description","time_created","time_updated","visibility","banned","ban_reason","subscriptions","favorited","views","tags":[{"tag":"Build 41"},…] }]}}`

`result:1` = ok, `result:9` = not found. `time_updated` is the mod-update signal (no steamcmd needed). Collections: `POST .../ISteamRemoteStorage/GetCollectionDetails/v1/` with `collectioncount` + `publishedfileids[0]` (also keyless).

**Direct upgrade for the PZ path:** `sync-mods.sh` currently infers Build 42 from mod.info heuristics (`versionMin`, a `42/` or `common/` dir) and admits ambiguity ("check" rows). The `tags[]` array from GetPublishedFileDetails carries explicit `Build 41` / `Build 42` tags. Use the API as the primary signal, mod.info as the cross-check, and only fall back to the heuristic when both are silent.

## 7. steamclient.so, SteamAppId, LD_LIBRARY_PATH

Three separate shims, needed in different combinations:

**(i) steamclient.so location.** Servers that call `SteamAPI_Init` dlopen `$HOME/.steam/sdk64/steamclient.so` (32-bit: `sdk32`). SteamCMD never puts it there. The CS2 wiki's own troubleshooting section:
```bash
mkdir -p ~/.steam/sdk64
ln -s /path/to/steamcmd/linux64/steamclient.so ~/.steam/sdk64/steamclient.so
# 32-bit servers (GMod, L4D2, srcds/hlds):
mkdir -p ~/.steam/sdk32
ln -s /path/to/steamcmd/linux32/steamclient.so ~/.steam/sdk32/steamclient.so
```
Alternative clean source: `steamcmd +@sSteamCmdForcePlatformType linux +login anonymous +app_update 1007 +quit` (Steamworks SDK Redist) → `steamapps/common/Steamworks SDK Redist/linux64/steamclient.so`.

Failure signature to detect in logs: `[S_API FAIL] SteamAPI_Init() failed; unable to locate a running instance of Steam, or a local steamclient.dll` / `Failed to load module '…/.steam/sdk64/steamclient.so'`.

**(ii) SteamAppId env / steam_appid.txt.** Installed under the *server* appid, must announce the *game* appid at runtime:

| Game | install appid | runtime appid | how |
|---|---|---|---|
| Valheim | 896660 | 892970 | `export SteamAppId=892970` in `start_server.sh` |
| Core Keeper | 1963720 | 1621690 | `SteamAppId=1621690` env |
| V Rising | 1829350 | 1604030 | shipped `steam_appid.txt` |

Generic fallback when a server reports "no appID found": drop a `steam_appid.txt` containing the appid next to the binary.

**(iii) LD_LIBRARY_PATH.** Unity servers ship bundled libs. Valheim's `start_server.sh` is the canonical shape:
```bash
export templdpath=$LD_LIBRARY_PATH
export LD_LIBRARY_PATH=./linux64:$LD_LIBRARY_PATH
export SteamAppId=892970
./valheim_server.x86_64 -name "..." -port 2456 -nographics -batchmode ...
export LD_LIBRARY_PATH=$templdpath
```
Core Keeper needs the same (`<base>/linux64`). Model as `env: {LD_LIBRARY_PATH_PREPEND: ["./linux64"]}` in the spec; note this must be resolved relative to `cwd`, so `cwd` is part of the spec too.

**(iv) Headless display.** Core Keeper historically needs `xvfb-run -a ./CoreKeeperServer.x86_64 -batchmode -nographics`. Add an optional `wrapper: xvfb` to the spec.

## 8. Per-game matrix

App IDs and executables below are cross-checked against LinuxGSM `lgsm/config-default/config-lgsm/<game>/_default.cfg` (machine-readable, actively maintained) plus each game's primary docs. **RAM floor** = "will boot and hold a small group"; realistic production is the parenthesized figure.

| Game | Server AppID | Anon login | Native Linux | Default ports (proto) | RCON | RAM floor | Launch binary/script |
|---|---|---|---|---|---|---|---|
| **Project Zomboid** | **380870** (workshop under 108600) | **Yes** | Yes (glibc 2.15) | 16261/udp game, 16262/udp direct, 27015/tcp RCON (`RCONPort`) | **Source RCON** (`RCONPassword` must be set or it's disabled) | 4 GB B41 / **8 GB B42** (10 GB comfortable) | `./start-server.sh` (JVM wrapper) |
| **Valheim** | **896660** (needs `SteamAppId=892970`) | Yes | Yes | 2456/udp game, 2457/udp query (2456-2458 range) | **NONE** → stdin/console + **SIGINT** to save | 2 GB (4 GB) | `./valheim_server.x86_64` via `start_server.sh` |
| **7 Days to Die** | **294420** | **Yes** (wiki's Linux "No" is stale) | Yes (glibc 2.27) | 26900/tcp+udp game, 26901-26902/udp, **8081/tcp telnet**, 8080/tcp web dashboard | **No Source RCON** — Telnet console (`TelnetEnabled` default true, `TelnetPort` 8081); `WebDashboardEnabled` default false | 6-8 GB vanilla (16-32 GB overhaul mods) | `./7DaysToDieServer.x86_64` via `startserver.sh` |
| **Palworld** | **2394010** | Yes | Yes (glibc 2.17) | 8211/udp game, 27015/udp query, **25575/tcp RCON**, **8212/tcp REST API** | Source RCON — **officially deprecated**, "scheduled to stop functioning in an upcoming update"; use the REST API | 8 GB (16 GB; known leak, grows over days) | `./PalServer.sh` → `Pal/Binaries/Linux/PalServer-Linux-Shipping` |
| **Satisfactory** | **1690800** | Yes | Yes | 7777/udp game + **7777/tcp HTTPS API**, **8888/tcp reliable messaging** (1.1+). Legacy 15000 beacon / 15777 query no longer required | **NONE** → HTTPS API + Lightweight Query API | 8 GB (16 GB for big saves / 4+ players) | `./FactoryServer.sh` → `Engine/Binaries/Linux/FactoryServer-Linux-Shipping` |
| **ARK: Survival Evolved** | **376030** | Yes | Yes (glibc 2.17) | 7777/udp game, 7778/udp raw, 27015/udp query, **27020/tcp RCON** | **Source RCON** (`RCONEnabled`, `RCONPort`) | 6-8 GB (16 GB) | `ShooterGame/Binaries/Linux/ShooterGameServer` |
| **ARK: Survival Ascended** | **2430930** | Yes | **NO** — Windows depot only; `+@sSteamCmdForcePlatformType windows` + Wine/Proton (GE-Proton common) | 7777/udp, 27015/udp query, 27020/tcp RCON | Source RCON | **16 GB** (24-32 GB for 10+ players) | `ShooterGame/Binaries/Win64/ArkAscendedServer.exe` |
| **Counter-Strike 2** | **730** (740/CS:GO DS is dead) | Yes | Yes, **glibc ≥ 2.31** (Ubuntu 20.04+/Debian 11+) | 27015/udp+tcp game+RCON, 27020/udp SourceTV, 27005/udp client, 26900/udp out | **Source RCON** | 2 GB, **~65 GB disk** | `game/cs2.sh -dedicated` (never `game/bin/linuxsteamrt64/cs2`). GSLT required: `+sv_setsteamaccount <token>` (game id 730) |
| **Rust** | **258550** | Yes | Yes | 28015/udp game, **28016/tcp RCON**, 28017/udp query, 28082/tcp `app.port` (Rust+) | **WebSocket RCON** (`+rcon.web 1`) — *not* Source RCON in default config | 8 GB (12-16 GB; wiki says "12 GB free RAM" baseline) | `./RustDedicated` (`runds.sh` wrapper) |
| **Garry's Mod** | **4020** | Yes | Yes (32-bit engine — needs i386 libs + `~/.steam/sdk32` symlink) | 27015/udp+tcp, 27005/udp client, 27020/udp SourceTV | **Source RCON** | 2 GB (4 GB+ with addons) | `./srcds_run -game garrysmod`. `+host_workshop_collection <id>`, `-authkey <web api key>`, GSLT `+sv_setsteamaccount` (token created under appid **4000**) |
| **Terraria (TShock)** | vanilla **105600** — **credentialed login, game purchase required**; TShock itself is **not on Steam** (GitHub `Pryaxis/TShock` releases) | **No** (for 105600) | Yes | 7777/tcp game, **7878/tcp TShock REST API** (`RestApiEnabled`/`RestApiPort` in `tshock/config.json`, default off) | **NONE** (no Source RCON) → TShock REST API + stdin console | 1 GB (2 GB) | `TShock.Server` (v5.x, self-contained .NET) / vanilla `TerrariaServer` |
| **V Rising** | **1829350** (ships `steam_appid.txt` = 1604030) | Yes | **NO** — Windows only, `+@sSteamCmdForcePlatformType windows` + Wine/Proton | **9876/udp game, 9877/udp query** (shipped `ServerHostSettings.json` defaults; the official instructions doc shows 27015/27016 only as *example values*), **25575/tcp RCON** | **Source RCON**, limited command set (`help`, `announce`, `announcerestart`, `shutdown`, `name`, `description`, `password`, `version`, `time`). Password must be non-empty. Env overrides exist: `VR_RCON_ENABLED`, `VR_RCON_PORT`, `VR_RCON_PASSWORD`, `VR_RCON_BIND_ADDRESS` | 6-8 GB (12 GB+ for 40 players) | `VRisingServer.exe`. Config: `ServerHostSettings.json`, `ServerGameSettings.json`, `adminlist.txt`, `banlist.txt` |
| **Enshrouded** | **2278520** | Yes | **NO** — Windows only, Wine/Proton | 15636/udp game, 15637/udp query (both in `enshrouded_server.json`) | **NONE** → stdin/console | 4 GB (4 players) / 6 GB (8) / **8-16 GB (16 slots)** | `enshrouded_server.exe` |
| **Core Keeper** | **1963720** (needs `SteamAppId=1621690`) | Yes | Yes (may need `xvfb-run`; `LD_LIBRARY_PATH=./linux64`) | 27015/udp + 27016/udp (Steam relay). Primary join path is a printed **Game ID** (written to `GameID.txt` on Linux) needing no port-forward | **NONE** → stdin | 2-4 GB | `_launch.sh` → `CoreKeeperServer.x86_64 -batchmode -nographics` |
| **DayZ** | **223350** (experimental server: 1042420) | **No** — account must **own DayZ** | Linux `DayZServer` binary exists but stable-branch Linux support is patchy | 2302/udp game, 2303-2305/udp aux, 27016/udp Steam query (`steamQueryPort`), **2305/udp BattlEye RCon** (convention: gameport+3) | **BattlEye BERCon (UDP)** — *not* Source RCON. Keys: `RConPassword`, `RConPort`, `RestrictRCon` | 4-8 GB (8-16 GB modded) | `./DayZServer -config=serverDZ.cfg -mod=@A;@B -bepath=...` |
| **Left 4 Dead 2** | **222860** | Yes | Yes (32-bit srcds, glibc 2.3.6) | 27015/udp+tcp, 27005/udp client, 27020/udp SourceTV | **Source RCON** (`rcon_password`) | 1 GB per instance | `./srcds_run -game left4dead2` |
| **Squad** | **403240** (Windows table's "4103240" is a wiki typo) | Yes | Yes (glibc 2.17) | 7787/udp game, 7788/udp, 27165/udp query, 15000/udp beacon, **21114/tcp RCON** | **Source RCON** over **TCP** (clients defaulting to UDP fail). Empty password disables it. Config: `SquadGame/ServerConfig/{Server,Admins,Rcon,MapRotation}.cfg` | 8 GB (12-16 GB at 100 players) | `./SquadGameServer.sh Port=7787 QueryPort=27165` |
| **Insurgency: Sandstorm** | **581330** | Yes | Yes (glibc 2.17) | 27102/udp game, 27131/udp query, 27015/tcp RCON | **Source RCON** via `-Rcon -RconPassword=<pw> -RconListenPort=<port>`; GSLT via `-GSLTToken=` | 4-6 GB | `Insurgency/Binaries/Linux/InsurgencyServer-Linux-Shipping` |

### Games with NO RCON — these define the stdin/console control requirement

**Hard no-RCON (must have stdin/pty or signal control):** Valheim, Satisfactory, Enshrouded, Core Keeper, vanilla Terraria.
**No Source RCON but an alternative admin plane:** 7 Days to Die (raw **telnet** on 8081), Rust (**WebSocket** RCON on 28016), DayZ (**BattlEye BERCon**, UDP), Satisfactory (**HTTPS API** on 7777/tcp), Palworld (**REST API** on 8212/tcp; its Source RCON is deprecated and slated for removal), TShock (**REST API** on 7878/tcp).
**Clean Source RCON:** Project Zomboid, CS2, Garry's Mod, L4D2, ARK SE, ARK ASA, V Rising, Squad, Insurgency: Sandstorm.

That is roughly a 50/50 split — **a manager built only on Source RCON manages half the catalogue.** The control plane must be an interface with at least five implementations: `source_rcon`, `telnet`, `websocket_rcon`, `http_api`, `stdin` (+ `battleye` later).

### Shutdown recipes (generalize the PZ hard-won knowledge)

| Recipe | Games | Mechanism |
|---|---|---|
| RCON command sequence | PZ (`save`, wait, `quit`), CS2/GMod/L4D2 (`quit`), Squad, ARK (`SaveWorld` then `DoExit`), V Rising (`shutdown`) | connect, auth, send, wait for socket close |
| stdin stuffing | Valheim, Enshrouded, Core Keeper, Terraria (`exit`) | screen `stuff` today; a Go-owned pty is better |
| Signal | **Valheim: `KillSignal=SIGINT`** — SIGTERM/SIGKILL does *not* save and can corrupt the world | systemd `KillSignal=` |
| Telnet | 7 Days to Die (`shutdown` on 8081) | TCP line protocol |
| HTTP | Satisfactory (shutdown endpoint on the HTTPS API) | authenticated POST |

The existing `zomboid.service.template` trick — `KillSignal=SIGCONT` so systemd waits while `ExecStop=` stuffs `save`/`quit`, with `SendSIGKILL=yes` + `TimeoutStopSec=180` as the backstop — is exactly the right shape. Make `KillSignal`, `TimeoutStopSec` and the ExecStop recipe per-game spec fields rather than baking in the PZ values.

## 9. Multi-instance specifics

- **One steamcmd at a time per HOME.** Concurrent runs corrupt shared state under `~/.steam` / `~/.local/share/Steam` and race on appmanifest files. Serialize (Postgres advisory lock keyed on the VPS) or give each instance its own HOME.
- **Never share a `force_install_dir` between two instances** — they fight over the manifest. Either one install dir per instance (simple, disk-hungry: CS2 is 65 GB, ARK ~20 GB), or one shared read-only install + per-instance data dirs (what PZ already does with `-cachedir=$DATA_DIR`, and what CS2 Multiserver does). PZ, Source games, 7DTD, Valheim and Satisfactory all support a separate data/save dir; ARK and Rust are messier.
- **Port allocation** must come from the spec's `ports[]` array with a per-VPS allocator; several games need contiguous ranges (ARK 7777+7778, Squad 7787+7788, DayZ 2302-2305, Valheim 2456-2458).
- **Admin planes must not be world-exposed.** Palworld's own docs: the RCON/REST APIs *"are not designed to be exposed directly to the Internet… recommended that they be used within the LAN."* Same for 7DTD telnet 8081, TShock 7878, Rust 28016, Satisfactory 7777/tcp. Bind to 127.0.0.1 where the game allows, and let Caddy/gvm reach them locally; keep the existing `firewall.sh` SSH-autodetect behaviour and drive the allow-list from `ports[].public`.

## 10. Windows dev / Linux target

Keep the whole SteamCMD driver, `.acf`/VDF parser, buildid diffing, spec loader, workshop API client, RCON/telnet/HTTP clients and the Postgres layer **pure Go, no build tags** — they all test on Windows. Put behind `//go:build linux`: user/group creation, `setuid`/`setgid` on the child, pty allocation, cgroup/`LimitNOFILE` wiring, `unix.Statfs` for disk pre-flight, and systemd unit installation. Define a `Platform` interface with a Windows no-op implementation so `go test ./...` is green on the dev box.

## 11. PZ adoption path (non-destructive)

Read, don't write, on first contact: `config.env` → instance spec; `$DATA_DIR/Server/<name>.ini` → settings (`WorkshopItems`, `Mods`, `DefaultPort`, `UDPPort`, `RCONPort`, `RCONPassword`); `$INSTALL_DIR/steamapps/appmanifest_380870.acf` → `installed_buildid` and `StateFlags`; `mods.txt`/`sandbox.txt` → the acquisition list and sandbox overrides; `$DATA_DIR/Saves/Multiplayer/<name>/` → registered as the world, untouched. Register as `managed_install=false` so gvm will start/stop/backup/monitor but refuses `app_update … validate` (which would overwrite customized files) until the operator explicitly promotes the instance. Run the `backup.sh` equivalent before the first gvm-initiated write, matching what `rebuild.sh` already does (archive before destroy).

## Sources

- https://developer.valvesoftware.com/wiki/SteamCMD
- https://developer.valvesoftware.com/wiki/Dedicated_Servers_List
- https://developer.valvesoftware.com/wiki/Counter-Strike_2/Dedicated_Servers
- https://developer.valvesoftware.com/wiki/Source_Dedicated_Server
- https://github.com/ValveSoftware/steam-for-linux/issues/4341
- https://github.com/ValveSoftware/steam-for-linux/issues/6976
- https://github.com/ValveSoftware/steam-for-linux/issues/8298
- https://github.com/GameServerManagers/LinuxGSM-Docs/blob/master/steamcmd/errors.md
- https://raw.githubusercontent.com/GameServerManagers/LinuxGSM/master/lgsm/modules/core_steamcmd.sh
- https://raw.githubusercontent.com/GameServerManagers/LinuxGSM/master/lgsm/modules/core_dl.sh
- https://github.com/GameServerManagers/LinuxGSM/tree/master/lgsm/config-default/config-lgsm
- https://partner.steamgames.com/doc/features/workshop/implementation
- https://partner.steamgames.com/doc/api/ISteamUGC
- https://github.com/FakeFishGames/Barotrauma/issues/7707
- https://api.steampowered.com/ISteamRemoteStorage/GetPublishedFileDetails/v1/
- https://github.com/StunlockStudios/vrising-dedicated-server-instructions/blob/master/1.0.x/INSTRUCTIONS.md
- https://satisfactory.wiki.gg/wiki/Dedicated_servers
- https://wiki.facepunch.com/rust/Creating-a-server
- https://wiki.facepunch.com/gmod/Linux_Dedicated_Server_Hosting
- https://docs.palworldgame.com/api/rcon/
- https://docs.palworldgame.com/settings-and-operation/arguments/
- https://ark.wiki.gg/wiki/Dedicated_server_setup
- https://squad.fandom.com/wiki/Server_Installation
- https://squad.fandom.com/wiki/Server_Configuration
- https://7daystodie.fandom.com/wiki/Server:_serverconfig.xml
- https://core-keeper.fandom.com/wiki/Dedicated_server
- https://github.com/Pryaxis/TShock/wiki/
- https://tshock.readme.io/v4.3.22/reference/rest-api-endpoints
- https://feedback.bistudio.com/T179224
- https://github.com/lloesche/valheim-server-docker/issues/625
- https://github.com/game-ci/steam-deploy/blob/main/README.md
- https://steamdb.info/app/2430930/info/
