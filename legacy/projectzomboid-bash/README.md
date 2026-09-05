# Project Zomboid B42 server — VPS installer

Turns a fresh Debian 12 / Ubuntu 22.04 or 24.04 VPS into a modded Build 42
dedicated server, with the 18 mods from the linked Steam collection and a
locked-down firewall.

## Files

| File | Purpose |
| --- | --- |
| `config.env` | Every tunable. Edit this first. |
| `mods.txt` | Workshop IDs, in load order. Managed on the server by `rebuild.sh` from the collection — treat any local copy as a mirror: pull it down after rebuilds, never push it up. |
| `sandbox.txt` | Per-key difficulty overrides on top of `SANDBOX_PRESET`. |
| `install.sh` | Full install: packages, SteamCMD, server, config, mods, systemd, firewall. |
| `firewall.sh` | UFW rules + fail2ban. Standalone — safe to rerun. |
| `sync-mods.sh` | Resolves downloaded workshop items to Mod IDs and writes `Mods=`. |
| `update.sh` | Clean stop → backup → Steam update → start → re-sync mods. Also what the nightly timer runs. |
| `backup.sh` | Tarball of the save + configs, with retention. |
| `nightly.sh` | Installs the nightly timer; plain restart vs full update per `config.env`. Standalone — safe to rerun. |
| `rebuild.sh` | Pull the mod list from the Steam collection, archive + delete the world, regenerate fresh. |
| `lib/common.sh` | Shared helpers. |
| `systemd/*.template` | Service and nightly-restart timer, filled in at install time. |

## Install

```bash
scp -r pz-server root@your-vps:/root/
ssh root@your-vps
cd /root/pz-server
nano config.env          # at minimum: PUBLIC_NAME, JVM_XMX, MAX_PLAYERS
chmod +x *.sh
sudo ./install.sh
```

Expect 20–45 minutes. The two long pauses are the ~5GB Steam download and the
first world generation.

## Sizing

Build 42 multiplayer is memory-hungry — roughly 6GB before a single mod loads,
and this list adds vehicles and UI frameworks on top. **8GB is the floor, 10–16GB
is comfortable.** Native memory lives *outside* the Java heap, so leave the box
about 3GB above `JVM_XMX`:

| Box RAM | `JVM_XMX` |
| --- | --- |
| 8 GB | `5g` |
| 10 GB | `7g` |
| 16 GB | `12g` |

Too small a heap doesn't crash outright — it shows up as a server that hitches
for everyone every few seconds.

## Firewall

`firewall.sh` sets default-deny inbound, default-allow outbound, and opens
exactly three things:

| Port | Proto | Why |
| --- | --- | --- |
| your SSH port | TCP | rate-limited via `ufw limit` (6 connections / 30s per source) |
| 16261 | UDP | server browser, handshake, direct connect |
| 16262 | UDP | data channel after the handshake |

RCON (27015/TCP) is deliberately **not** exposed. Reach it through a tunnel:

```bash
ssh -L 27015:127.0.0.1:27015 user@your-vps
```

Or set `ADMIN_IP` in `config.env` to allow a single source address.

The script detects your SSH port from the live session before enabling UFW, so
a non-standard port won't lock you out. Still — open a second SSH session and
confirm you can get in before you close the first one.

## Day-to-day

```bash
systemctl status zomboid
journalctl -u zomboid -f
sudo -u pzserver script -q /dev/null -c 'screen -r pzserver'   # live console, Ctrl-A then D to detach
sudo systemctl restart zomboid
sudo ./backup.sh
sudo ./update.sh
```

From the live console you can run admin commands directly (`save`, `quit`,
`players`, `adduser "name" "password"`, `setaccesslevel name admin`).

With `UPDATE_ON_RESTART=true` (the default) the nightly `RESTART_TIME` cycle is
a full `update.sh`, not just a restart: clean stop, backup, Steam binary
update, boot (which re-downloads updated workshop mods), then a Mod-ID re-sync
— with one extra restart only if a mod's IDs actually changed. A failed Steam
download still boots the server on the old binaries and retries the next
night. Tune `RESTART_DAILY` / `RESTART_TIME` / `RESTART_TZ` /
`UPDATE_ON_RESTART` in `config.env` and apply with `sudo ./nightly.sh`; the
job's output lands in `journalctl -u zomboid-restart`.

**Never `kill -9` the server.** `systemctl stop` issues `save` then `quit` and
waits up to 180s; killing it mid-write is the standard way to corrupt a world.

## Difficulty

MP difficulty lives in the sandbox settings file the server generates on first
boot (`~pzserver/Zomboid/Server/servertest_SandboxVars.lua`) — zombie
population and behaviour, loot rarity, XP, day length, utility shutoff. With
nothing configured you run B42's default, which **is** the `apocalypse` preset
(the game's own `SandboxVars.lua` literally requires it).

Two layers, both optional:

1. `SANDBOX_PRESET` in `config.env` — a preset name the server itself ships
   (`outbreak` < `apocalypse` < `rising` ≲ `extinction`, plus
   `sixmonthslater`). Applied straight from the server's own
   `media/lua/shared/Sandbox/` files, so it tracks game updates.
2. `sandbox.txt` — individual `Key = value` overrides applied on top (examples
   inside). Keys are validated against the live file; typos warn, never write.

Both are applied by `install.sh` and by `rebuild.sh` in either mode.
Population, loot and XP changes take effect on the next restart cycle —
`sudo ./rebuild.sh --keep-world` is enough. World-generation settings (start
date, and a fair judgment call on loot already spawned in explored chunks)
only really land on a full `sudo ./rebuild.sh`. The nuance: already-visited
chunks keep their spawned loot either way; a fresh world applies everything
everywhere.

The Steam collection is the source of truth. Two paths, depending on whether
the current world should survive:

**Fresh world (the usual move — new mods get their content everywhere):**

1. Add/remove items in the Steam collection.
2. `sudo ./rebuild.sh` — it pulls the collection into `mods.txt` (your ordering
   is kept, new items land at the bottom), downloads everything, resolves Mod
   IDs, **archives the old world** to `/var/backups/pzserver/world-pre-rebuild-*.tar.gz`
   (never pruned by backup retention), **deletes it**, and boots a fresh world
   with the full list active. Accounts, passwords and server settings survive;
   the map and characters don't. It asks for confirmation, and every
   destructive step happens only after the downloads are verified — any earlier
   failure leaves the old world untouched.
3. `sudo ./rebuild.sh --dry` previews; `--no-fetch` skips Steam and rebuilds
   from `mods.txt` as-is. If a new item is a library/framework, move its line
   near the top of `mods.txt` afterwards and rerun (`--no-fetch` is enough) —
   the script keeps manual ordering on later runs.
4. The restore command for the archive is printed at the end of every run.

**Keep the world (in-place add — fine for UI/QoL mods):**

`sudo ./rebuild.sh --keep-world` — the same collection fetch, downloads and
Mod-ID resolution, but the archive/delete steps are skipped: the current map
and characters stay. Costs one server restart cycle.

Fully manual fallback (no collection fetch): edit `mods.txt`, restart to
download, `sudo ./sync-mods.sh`, restart again.

`sync-mods.sh --dry` shows what it would write without touching the ini.

Removing a mod from a live world is the risky direction — back up first, and
expect stranded objects from anything that added items or vehicles.

## Mod IDs, and why they're resolved rather than hardcoded

The ini needs two different lists: `WorkshopItems` (numeric IDs from the
Workshop URLs) and `Mods` (each mod's internal string ID from its `mod.info`).
They are not interchangeable, and a wrong `Mods` entry fails silently — the item
downloads, the mod never enables. Rather than guessing internal IDs, the scripts
read them out of the downloaded folders. Both lists are semicolon-separated;
commas break the list with no error.

Where an item ships both a B41 and a B42 variant, `sync-mods.sh` prefers the B42
one and prints `multi` next to anything it couldn't disambiguate — check those
rows and prune `Mods=` by hand if a B41 ID slipped in.

## Notes on this mod list

All 18 are Build 42 items. Two are load-bearing dependencies and are ordered
first: **that DAMN Library** (KI5's vehicle framework, needed by the Hilux and
Trailers) and **NeatUI Framework** (needed by Neat Crafting and CleanUI). Most
of the rest are UI/QoL mods — they still belong in the server list, since that's
what tells joining clients what to download.

## Troubleshooting

**Players see the server but time out joining** — 16262/UDP isn't open.

**No mods in game** — check the boot console for three phases: workshop
acquisition, mod initialisation, Lua errors. An item missing from phase one is a
`WorkshopItems` problem; present in one but absent from two is a wrong Mod ID;
red Lua right after a list change is almost always load order.

**A workshop item won't download** — Steam stalls sometimes. Restart once; if it
stays stuck, delete that item's folder under
`~pzserver/Zomboid/steamapps/workshop/content/108600/<id>/` and restart.

**Server hitches every few seconds** — heap too small. Raise `JVM_XMX` if the
box has room, then `sudo ./install.sh` is *not* needed; just edit the unit via
`config.env` + re-run the install, or edit `/etc/systemd/system/zomboid.service`
directly and `systemctl daemon-reload`.
