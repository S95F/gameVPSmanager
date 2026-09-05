#!/usr/bin/env bash
# =============================================================
# Archives the world save plus the server configs.
# Safe to run while the server is up, but a save first is better:
#   sudo ./backup.sh
# =============================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"

require_root
load_config

mkdir -p "$BACKUP_DIR"
STAMP="$(date +%Y%m%d-%H%M%S)"
ARCHIVE="$BACKUP_DIR/pz-${SERVER_NAME}-${STAMP}.tar.gz"

# If the server is live, flush the world to disk first.
if systemctl is-active --quiet zomboid.service; then
  log "Server is running - issuing /save first."
  as_pz "screen -p 0 -S pzserver -X eval 'stuff \"save\\015\"'" 2>/dev/null || true
  sleep 15
fi

log "Creating $ARCHIVE ..."
tar -czf "$ARCHIVE" \
  -C "$DATA_DIR" \
  "Server" \
  "Saves/Multiplayer/${SERVER_NAME}" \
  2>/dev/null || die "tar failed - check that $DATA_DIR/Saves/Multiplayer/${SERVER_NAME} exists."

chmod 600 "$ARCHIVE"
log "Backup size: $(du -h "$ARCHIVE" | cut -f1)"

# Retention
COUNT="$(find "$BACKUP_DIR" -maxdepth 1 -name "pz-${SERVER_NAME}-*.tar.gz" | wc -l)"
if [ "$COUNT" -gt "$BACKUP_KEEP" ]; then
  find "$BACKUP_DIR" -maxdepth 1 -name "pz-${SERVER_NAME}-*.tar.gz" -printf '%T@ %p\n' \
    | sort -n | head -n "$((COUNT - BACKUP_KEEP))" | cut -d' ' -f2- \
    | while read -r old; do log "Pruning $(basename "$old")"; rm -f "$old"; done
fi
