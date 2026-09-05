#!/usr/bin/env bash
# =============================================================
# Reads mods.txt, finds the matching downloaded workshop folders,
# extracts each mod's internal Mod ID from its mod.info, and writes
# both WorkshopItems= and Mods= into the server ini in the same order.
#
# Run it after adding or removing anything in mods.txt:
#   sudo ./sync-mods.sh          write the lists
#   sudo ./sync-mods.sh --dry    show what it would write
# =============================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"

DRY=0
[ "${1:-}" = "--dry" ] && DRY=1

require_root
load_config
[ -f "$SERVER_INI" ] || die "$SERVER_INI not found. Run install.sh first."

# --- locate a workshop item's folder across the possible cache roots ---
find_item_dir() {
  local id="$1" root
  while read -r root; do
    [ -d "$root/$id" ] && { printf '%s\n' "$root/$id"; return 0; }
  done < <(workshop_roots)
  return 1
}

# --- how strongly does this mod.info look like a Build 42 build? ---
# 3 = explicit (lives in a 42/ folder, or declares versionMin 42+)
# 1 = B42 folder layout (ships common/ or 42/ media dirs)
# 0 = no evidence either way
b42_score() {
  local info="$1" moddir
  moddir="$(dirname "$info")"
  if [ "$(basename "$moddir")" = "42" ]; then echo 3; return; fi
  if grep -qiE '^[[:space:]]*versionMin[[:space:]]*=[[:space:]]*4[2-9]' "$info"; then echo 3; return; fi
  if [ -d "$moddir/42" ] || [ -d "$moddir/common" ]; then echo 1; return; fi
  echo 0
}

mod_id_from_info() {
  grep -m1 -iE '^[[:space:]]*id[[:space:]]*=' "$1" \
    | cut -d= -f2- | tr -d '\r' | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//'
}

RESOLVED=()
MISSING=()
AMBIGUOUS=()

printf '\n%-12s  %-9s  %s\n' "WORKSHOP" "STATUS" "MOD ID(S)"
printf '%s\n' "--------------------------------------------------------------"

while read -r wid; do
  [ -z "$wid" ] && continue
  if ! item_dir="$(find_item_dir "$wid")"; then
    MISSING+=("$wid")
    printf '%-12s  %-9s  %s\n' "$wid" "MISSING" "not downloaded yet"
    continue
  fi

  entries=(); chosen=(); dropped=(); best=-1
  while IFS= read -r info; do
    mid="$(mod_id_from_info "$info")"
    [ -z "$mid" ] && continue
    entries+=("$(b42_score "$info"):$mid")
  done < <(find "$item_dir" -maxdepth 5 -name 'mod.info' -type f | sort)

  if [ "${#entries[@]}" -eq 0 ]; then
    MISSING+=("$wid")
    printf '%-12s  %-9s  %s\n' "$wid" "EMPTY" "no mod.info found"
    continue
  fi

  # Keep only the best-scoring candidates: a dual-build item resolves to its
  # B42 variant, while an item holding two real B42 mods keeps both.
  for e in "${entries[@]}"; do
    [ "${e%%:*}" -gt "$best" ] && best="${e%%:*}"
  done
  for e in "${entries[@]}"; do
    if [ "${e%%:*}" -eq "$best" ]; then chosen+=("${e#*:}"); else dropped+=("${e#*:}"); fi
  done
  [ "$best" -eq 0 ] && AMBIGUOUS+=("$wid")

  # de-duplicate, keep order
  for mid in "${chosen[@]}"; do
    seen=0
    for existing in ${RESOLVED[@]+"${RESOLVED[@]}"}; do
      [ "$existing" = "$mid" ] && seen=1 && break
    done
    [ "$seen" -eq 0 ] && RESOLVED+=("$mid")
  done

  status="ok"
  [ "${#chosen[@]}" -gt 1 ] && status="multi"
  [ "$best" -eq 0 ] && status="check"
  note=""
  [ "${#dropped[@]}" -gt 0 ] && note="   (skipped non-B42: $(printf '%s ' "${dropped[@]}"))"
  printf '%-12s  %-9s  %s%s\n' "$wid" "$status" "$(printf '%s ' "${chosen[@]}")" "$note"
done < <(read_mod_ids)

echo

[ "${#RESOLVED[@]}" -eq 0 ] && die "No mod IDs resolved. Has the server booted once with WorkshopItems set?"

WORKSHOP_LINE="$(read_mod_ids | paste -sd';' -)"
MODS_LINE="$(printf '%s;' "${RESOLVED[@]}" | sed 's/;$//')"

echo "WorkshopItems=$WORKSHOP_LINE"
echo "Mods=$MODS_LINE"
echo

if [ "${#MISSING[@]}" -gt 0 ]; then
  warn "Not downloaded: ${MISSING[*]}"
  warn "Steam sometimes stalls. Restart the server once and rerun this script;"
  warn "if one stays stuck, delete its folder from the workshop cache and restart."
fi
if [ "${#AMBIGUOUS[@]}" -gt 0 ]; then
  warn "No B42 marker found in: ${AMBIGUOUS[*]}  (rows marked 'check')"
  warn "Every mod ID inside those items was enabled. If one turns out to be a B41 variant, drop it from Mods= by hand."
fi

if [ "$DRY" -eq 1 ]; then
  log "Dry run - nothing written."
  exit 0
fi

set_ini WorkshopItems "$WORKSHOP_LINE" "$SERVER_INI"
set_ini Mods          "$MODS_LINE"     "$SERVER_INI"
chown "$PZ_USER:$PZ_USER" "$SERVER_INI"
log "Wrote ${#RESOLVED[@]} mod IDs to $SERVER_INI"

if systemctl is-active --quiet zomboid.service; then
  warn "Server is running - restart it to apply: sudo systemctl restart zomboid"
fi
