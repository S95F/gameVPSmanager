#!/usr/bin/env bash
# =============================================================
# Stops the server cleanly, backs up, updates the binaries from Steam,
# re-resolves mod IDs, and starts back up.
#   sudo ./update.sh
# Also runs unattended every night when UPDATE_ON_RESTART=true (see
# nightly.sh), so no failure here may leave the server stopped.
# =============================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"

require_root
load_config

WAS_RUNNING=0
if systemctl is-active --quiet zomboid.service; then
  WAS_RUNNING=1
  log "Stopping server (waits for a clean save)..."
  systemctl stop zomboid.service
fi

"$SCRIPT_DIR/backup.sh" || warn "Backup failed - continuing anyway."

# Cap the download so a Steam outage can't hang the nightly job, and keep
# going if it fails - an outdated server beats one left stopped.
log "Updating server binaries..."
as_pz "timeout 1800 '$STEAMCMD_DIR/steamcmd.sh' +force_install_dir '$INSTALL_DIR' \
    +login anonymous +app_update $PZ_APPID validate +quit" \
  || warn "Steam update failed or timed out - starting on the existing binaries."

log "Starting server..."
systemctl start zomboid.service

# Workshop items are refreshed by the server on boot; give it time, then
# re-resolve mod IDs in case a mod changed its internal ID or added a build.
log "Waiting 180s for workshop downloads before re-checking mod IDs..."
sleep 180
LISTS_BEFORE="$(get_ini WorkshopItems "$SERVER_INI");$(get_ini Mods "$SERVER_INI")"
"$SCRIPT_DIR/sync-mods.sh" || warn "Mod ID resolution reported problems."
LISTS_AFTER="$(get_ini WorkshopItems "$SERVER_INI");$(get_ini Mods "$SERVER_INI")"

# The server read the ini at boot, so a changed list needs one more cycle.
if [ "$LISTS_AFTER" != "$LISTS_BEFORE" ]; then
  log "Mod lists changed - restarting to load them..."
  systemctl restart zomboid.service
fi

[ "$WAS_RUNNING" -eq 1 ] || log "Note: the server was stopped before this update; it is running now."
log "Update finished. Watch the boot: journalctl -u zomboid -f"
