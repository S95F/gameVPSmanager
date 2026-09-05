#!/usr/bin/env bash
# =============================================================
# Rebuild the world with the current Steam collection's mod list.
#
# Pulls the item list from the collection (COLLECTION_ID in config.env),
# updates mods.txt (new items appended, delisted items dropped, your
# ordering kept), downloads everything, then archives AND deletes the
# old world so a fresh one generates with the full mod list active.
#
# Player accounts, admin account, passwords and server settings survive.
# Characters and the map do not - that is the point.
#
#   sudo ./rebuild.sh              fetch list + rebuild (asks to confirm)
#   sudo ./rebuild.sh --keep-world fetch list + apply mods, world untouched
#   sudo ./rebuild.sh --dry        show what would change, touch nothing
#   sudo ./rebuild.sh --no-fetch   use mods.txt as-is (Steam down)
#   sudo ./rebuild.sh --yes        no confirmation prompt (automation)
# =============================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"

DRY=0; FETCH=1; ASSUME_YES=0; KEEP_WORLD=0
for arg in "$@"; do
  case "$arg" in
    --dry)        DRY=1 ;;
    --no-fetch)   FETCH=0 ;;
    --yes)        ASSUME_YES=1 ;;
    --keep-world) KEEP_WORLD=1 ;;
    -h|--help)    sed -n '2,18p' "$0"; exit 0 ;;
    *)            die "Unknown option: $arg" ;;
  esac
done

require_root
load_config
[ -n "${SERVER_NAME:-}" ]    || die "SERVER_NAME is empty in config.env."
[ -f "$SERVER_INI" ]         || die "$SERVER_INI not found. Run install.sh first."
MODS_FILE="$SCRIPT_DIR/mods.txt"
WORLD_DIR="$DATA_DIR/Saves/Multiplayer/$SERVER_NAME"

# ------------------------------------------------------------------
# 1. Pull the item list from the Steam collection
# ------------------------------------------------------------------
ADDED=(); DROPPED=()
if [ "$FETCH" -eq 1 ]; then
  [ -n "${COLLECTION_ID:-}" ] || die "COLLECTION_ID not set in config.env (use --no-fetch to skip)."
  COLL_URL="https://steamcommunity.com/sharedfiles/filedetails/?id=$COLLECTION_ID"
  log "Fetching collection $COLLECTION_ID ..."
  HTML="$(mktemp)"; trap 'rm -f "$HTML"' EXIT
  curl -fsSL --retry 3 --retry-delay 5 \
       -A "Mozilla/5.0 (X11; Linux x86_64)" "$COLL_URL" -o "$HTML" \
    || die "Could not fetch the collection page. Steam may be rate-limiting, or the collection is private/removed. Retry later, or run with --no-fetch to use mods.txt as-is."

  # Each entry renders as: <div class="collectionItem" id="sharedfile_<ID>">
  # with its <div class="workshopItemTitle">Title</div> shortly after.
  COLL_TSV="$(awk '
    /class="collectionItem"/ {
      if (match($0, /sharedfile_[0-9]+/)) id = substr($0, RSTART + 11, RLENGTH - 11)
    }
    /workshopItemTitle/ && id != "" {
      line = $0
      sub(/.*workshopItemTitle">/, "", line); sub(/<.*/, "", line)
      print id "\t" line
      id = ""
    }
  ' "$HTML" | sed 's/&amp;/\&/g; s/&#39;/'"'"'/g; s/&quot;/"/g')"

  COLL_IDS="$(printf '%s\n' "$COLL_TSV" | cut -f1 | grep -vx "$COLLECTION_ID" || true)"
  COUNT="$(printf '%s\n' "$COLL_IDS" | grep -c . || true)"
  [ "$COUNT" -gt 0 ] || die "Fetched the page but found no items - Steam markup changed, or the collection is empty/removed. mods.txt was not touched."

  # Belt and braces: count ids straight from the raw HTML. If the id/title
  # pairing above missed any, refuse to rewrite mods.txt off a bad parse.
  RAW_COUNT="$(grep -oE 'id="sharedfile_[0-9]+"' "$HTML" | grep -oE '[0-9]+' \
                 | sort -u | grep -vx "$COLLECTION_ID" | grep -c . || true)"
  [ "$RAW_COUNT" -eq "$COUNT" ] \
    || die "Parsed $COUNT items but the page contains $RAW_COUNT - Steam markup changed. mods.txt was not touched (use --no-fetch to rebuild from it as-is)."
  log "Collection lists $COUNT items."

  # --- update mods.txt: keep known lines in place, drop delisted, append new ---
  NEW_FILE="$(mktemp)"
  while IFS= read -r line; do
    id="$(printf '%s\n' "$line" | sed -e 's/#.*//' -e 's/[[:space:]]//g')"
    if printf '%s\n' "$id" | grep -qE '^[0-9]+$'; then
      if printf '%s\n' "$COLL_IDS" | grep -qx "$id"; then
        printf '%s\n' "$line" >> "$NEW_FILE"
      else
        DROPPED+=("$id")
      fi
    else
      printf '%s\n' "$line" >> "$NEW_FILE"
    fi
  done < "$MODS_FILE"

  KNOWN="$(read_mod_ids)"
  while IFS="$(printf '\t')" read -r id title; do
    [ -z "$id" ] || [ "$id" = "$COLLECTION_ID" ] && continue
    printf '%s\n' "$KNOWN" | grep -qx "$id" && continue
    if [ "${#ADDED[@]}" -eq 0 ]; then
      printf '\n# --- Added from collection %s ---\n' "$(date +%Y-%m-%d)" >> "$NEW_FILE"
    fi
    printf '%-12s # %s\n' "$id" "$title" >> "$NEW_FILE"
    ADDED+=("$id")
  done <<< "$COLL_TSV"

  [ "${#ADDED[@]}" -gt 0 ]   && log "New from collection: ${ADDED[*]}"
  [ "${#DROPPED[@]}" -gt 0 ] && warn "No longer in collection, removing: ${DROPPED[*]}"
  [ "${#ADDED[@]}" -eq 0 ] && [ "${#DROPPED[@]}" -eq 0 ] && log "mods.txt already matches the collection."

  if [ "$DRY" -eq 0 ]; then
    cp -f "$MODS_FILE" "$MODS_FILE.bak"
    cat "$NEW_FILE" > "$MODS_FILE"
  fi
  rm -f "$NEW_FILE"
else
  log "Skipping collection fetch (--no-fetch); using mods.txt as-is."
fi

MOD_COUNT="$(read_mod_ids | wc -l)"
STAMP="$(date +%Y%m%d-%H%M%S)"
ARCHIVE="$BACKUP_DIR/world-pre-rebuild-${SERVER_NAME}-${STAMP}.tar.gz"

if [ "$DRY" -eq 1 ]; then
  [ -n "${SANDBOX_PRESET:-}" ] && log "Difficulty: would apply preset '$SANDBOX_PRESET' (+ sandbox.txt overrides)."
  [ -z "${SANDBOX_PRESET:-}" ] && [ -f "$SCRIPT_DIR/sandbox.txt" ] && log "Difficulty: would apply sandbox.txt overrides."
  if [ "$KEEP_WORLD" -eq 1 ]; then
    log "Dry run. Would apply $MOD_COUNT mods to '$SERVER_NAME' - world kept."
  else
    log "Dry run. Would rebuild '$SERVER_NAME' with $MOD_COUNT mods:"
    log "  archive world -> $ARCHIVE"
    log "  delete        -> $WORLD_DIR"
    log "  (accounts, passwords and server settings are kept)"
  fi
  exit 0
fi

# ------------------------------------------------------------------
# 2. Confirm - a full rebuild erases the map and everyone's characters
# ------------------------------------------------------------------
if [ "$KEEP_WORLD" -eq 0 ]; then
  warn "This rebuilds '$SERVER_NAME' with $MOD_COUNT mods."
  warn "The world is archived to $ARCHIVE and then DELETED."
  warn "Everyone starts over. Accounts, passwords and settings are kept."
  if [ "$ASSUME_YES" -eq 0 ]; then
    printf 'Type the server name (%s) to continue: ' "$SERVER_NAME"
    read -r REPLY
    [ "$REPLY" = "$SERVER_NAME" ] || die "Aborted - nothing changed except mods.txt (backup at mods.txt.bak)."
  fi
else
  log "Applying $MOD_COUNT mods to '$SERVER_NAME' - the world stays (brief downtime for the mod-download boot)."
fi

# ------------------------------------------------------------------
# 3. Stop the server (and the nightly timer, so it can't fire mid-rebuild)
# ------------------------------------------------------------------
log "Stopping server..."
systemctl stop zomboid-restart.timer 2>/dev/null || true
systemctl stop zomboid.service       2>/dev/null || true
as_pz "screen -S pzboot -X quit" >/dev/null 2>&1 || true

# ------------------------------------------------------------------
# 4. Download workshop content, resolve Mod IDs (world still intact)
# ------------------------------------------------------------------
set_ini WorkshopItems "$(read_mod_ids | paste -sd';' -)" "$SERVER_INI"
chown "$PZ_USER:$PZ_USER" "$SERVER_INI"

log "Boot #1: downloading workshop content (old world still in place)..."
pz_boot_and_wait "SERVER STARTED" 3600 \
  || die "Download boot failed - the world was NOT touched. Restart with: sudo systemctl start zomboid  - then check $DATA_DIR/server-console.txt and rerun."

NOT_DOWNLOADED=()
while read -r wid; do
  found=0
  while read -r root; do
    [ -d "$root/$wid" ] && { found=1; break; }
  done < <(workshop_roots)
  [ "$found" -eq 0 ] && NOT_DOWNLOADED+=("$wid")
done < <(read_mod_ids)
if [ "${#NOT_DOWNLOADED[@]}" -gt 0 ]; then
  die "Not downloaded yet: ${NOT_DOWNLOADED[*]} - the world was NOT touched. Steam stalls sometimes: rerun ./rebuild.sh (--no-fetch is fine). If one never arrives, check that the item is still up on the Workshop."
fi

"$SCRIPT_DIR/sync-mods.sh" || die "Mod ID resolution failed - the world was NOT touched. Fix the report above and rerun."

# ------------------------------------------------------------------
# 5. Archive, then delete the old world (skipped with --keep-world)
# ------------------------------------------------------------------
if [ "$KEEP_WORLD" -eq 1 ]; then
  apply_sandbox
  log "Keeping the world. Starting server with the updated mod list..."
  systemctl start zomboid.service
  if [ "${RESTART_DAILY:-false}" = "true" ]; then
    systemctl start zomboid-restart.timer 2>/dev/null || true
  fi
  log "Done. $MOD_COUNT mods enabled - watch the boot: journalctl -u zomboid -f"
  exit 0
fi

mkdir -p "$BACKUP_DIR"
MEMBERS=("Server")
[ -d "$WORLD_DIR" ] && MEMBERS+=("Saves/Multiplayer/$SERVER_NAME")
[ -d "$DATA_DIR/db" ] && MEMBERS+=("db")
log "Archiving old world -> $ARCHIVE"
tar -czf "$ARCHIVE" -C "$DATA_DIR" "${MEMBERS[@]}" \
  || die "Archive failed - NOT deleting the world. Check free disk space (df -h)."
chmod 600 "$ARCHIVE"
log "Archive size: $(du -h "$ARCHIVE" | cut -f1)  (kept until you delete it - not pruned by backup retention)"

log "Deleting old world..."
rm -rf "${DATA_DIR:?}/Saves/Multiplayer/${SERVER_NAME:?}"
[ -d "$DATA_DIR/Saves/Multiplayer/${SERVER_NAME}_player" ] \
  && rm -rf "${DATA_DIR:?}/Saves/Multiplayer/${SERVER_NAME:?}_player"

# ------------------------------------------------------------------
# 6. Fresh world generates on boot, with every mod and the difficulty
#    settings from config.env/sandbox.txt active
# ------------------------------------------------------------------
apply_sandbox
log "Starting server - the new world generates now (several minutes)..."
systemctl start zomboid.service
if [ "${RESTART_DAILY:-false}" = "true" ]; then
  systemctl start zomboid-restart.timer 2>/dev/null || true
fi

cat <<EOF

$(log "Rebuild underway. $MOD_COUNT mods enabled.")

  Old world   : $ARCHIVE
  Watch boot  : journalctl -u zomboid -f
  Ready when  : grep -F "SERVER STARTED" $DATA_DIR/server-console.txt
  Restore old : sudo systemctl stop zomboid
                sudo tar -xzf $ARCHIVE -C $DATA_DIR
                sudo chown -R $PZ_USER:$PZ_USER $DATA_DIR && sudo systemctl start zomboid
EOF
