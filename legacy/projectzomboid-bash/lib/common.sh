#!/usr/bin/env bash
# Shared helpers. Sourced by every script in this directory.

PZ_APPID=380870          # Project Zomboid Dedicated Server
PZ_GAME_APPID=108600     # Project Zomboid (workshop content lives under this ID)

log()  { printf '\033[1;32m[pz]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[pz]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[pz]\033[0m %s\n' "$*" >&2; exit 1; }

require_root() {
  [ "$(id -u)" -eq 0 ] || die "Run this as root (sudo $0)."
}

load_config() {
  local dir="${SCRIPT_DIR:?SCRIPT_DIR must be set before load_config}"
  [ -f "$dir/config.env" ] || die "config.env not found next to the scripts."
  # shellcheck disable=SC1091
  . "$dir/config.env"
  PZ_HOME="$(dirname "$DATA_DIR")"
  SERVER_INI="$DATA_DIR/Server/${SERVER_NAME}.ini"
}

# Run a command as the server user with a proper login environment.
as_pz() {
  su -s /bin/bash -l "$PZ_USER" -c "$*"
}

gen_password() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -base64 18 | tr -d '/+=' | cut -c1-20
  else
    tr -dc 'A-Za-z0-9' </dev/urandom | head -c 20
  fi
}

# set_ini KEY VALUE FILE - replaces the line, or appends it if missing.
set_ini() {
  local key="$1" val="$2" file="$3" tmp
  tmp="$(mktemp)"
  awk -v k="$key" -v v="$val" '
    BEGIN { done = 0 }
    {
      if (index($0, k "=") == 1) { if (!done) { print k "=" v; done = 1 } }
      else { print }
    }
    END { if (!done) print k "=" v }
  ' "$file" > "$tmp"
  cat "$tmp" > "$file"          # preserves original ownership/permissions
  rm -f "$tmp"
}

get_ini() {
  local key="$1" file="$2"
  awk -v k="$key" 'index($0, k "=") == 1 { sub("^" k "=", ""); print; exit }' "$file"
}

# Apply difficulty: SANDBOX_PRESET (a preset shipped with the server itself,
# e.g. apocalypse/outbreak/rising/extinction) rewrites <name>_SandboxVars.lua,
# then sandbox.txt overrides individual keys on top. Needs the first boot to
# have happened (the file must exist when no preset is set).
apply_sandbox() {
  local sv="$DATA_DIR/Server/${SERVER_NAME}_SandboxVars.lua"
  local overrides="$SCRIPT_DIR/sandbox.txt"

  if [ -n "${SANDBOX_PRESET:-}" ]; then
    local pdir="$INSTALL_DIR/media/lua/shared/Sandbox" preset="" f base want
    want="$(printf '%s' "$SANDBOX_PRESET" | tr '[:upper:]' '[:lower:]')"
    for f in "$pdir"/*.lua; do
      base="$(basename "$f" .lua)"
      if [ "$(printf '%s' "$base" | tr '[:upper:]' '[:lower:]')" = "$want" ]; then
        preset="$f"; break
      fi
    done
    [ -n "$preset" ] || die "SANDBOX_PRESET='$SANDBOX_PRESET' not found. Available in $pdir: $(cd "$pdir" && ls *.lua | sed -e 's/\.lua$//' -e '/^SandboxVars$/d' | paste -sd', ' -)"
    head -1 "$preset" | grep -q '^return {' || die "Unexpected format in $preset - not a 'return {' preset."
    log "Applying sandbox preset '$(basename "$preset" .lua)'."
    {
      printf -- '-- Written by the pz scripts from preset %s + sandbox.txt.\n' "$(basename "$preset" .lua)"
      printf -- '-- Edit config.env / sandbox.txt instead of this file.\n'
      sed '1s/^return {/SandboxVars = {/' "$preset"
    } > "$sv"
  fi

  [ -f "$sv" ] || die "$sv not found - the server must boot once before sandbox settings exist."

  if [ -f "$overrides" ]; then
    local n=0 line key val sec leaf tmp
    while IFS= read -r line; do
      line="${line%%#*}"
      case "$line" in *=*) ;; *) continue ;; esac
      key="$(printf '%s' "${line%%=*}" | tr -d '[:space:]')"
      val="$(printf '%s' "${line#*=}" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"
      { [ -n "$key" ] && [ -n "$val" ]; } || continue
      sec=""; leaf="$key"
      case "$key" in *.*) sec="${key%%.*}"; leaf="${key#*.}" ;; esac

      tmp="$(mktemp)"
      if awk -v sec="$sec" -v key="$leaf" -v val="$val" '
        BEGIN { depth = 0; section = ""; found = 0; kre = "^[ \t]*" key "[ \t]*=" }
        {
          orig = $0; out = $0
          # never rewrite a "Name = {" section header line
          if (!found && orig !~ /=[ \t]*\{[ \t]*$/) {
            if ((sec == "" && depth == 1) || (sec != "" && depth == 2 && section == sec)) {
              if (match(orig, kre)) {
                indent = orig; sub(/[^ \t].*/, "", indent)
                out = indent key " = " val ","
                found = 1
              }
            }
          }
          print out
          c = orig; opens = gsub(/\{/, "", c)
          c = orig; closes = gsub(/\}/, "", c)
          if (depth == 1 && opens > closes && orig ~ /^[ \t]*[A-Za-z_][A-Za-z0-9_]*[ \t]*=[ \t]*\{/) {
            section = orig
            sub(/^[ \t]*/, "", section); sub(/[ \t]*=.*/, "", section)
          }
          depth += opens - closes
          if (depth <= 1) section = ""
        }
        END { exit found ? 0 : 3 }
      ' "$sv" > "$tmp"; then
        cat "$tmp" > "$sv"
        n=$((n + 1))
      else
        warn "sandbox.txt: '$key' not found in $(basename "$sv") - skipped (check spelling against the live file)."
      fi
      rm -f "$tmp"
    done < "$overrides"
    [ "$n" -gt 0 ] && log "Applied $n sandbox override(s) from sandbox.txt."
  fi

  chown "$PZ_USER:$PZ_USER" "$sv"
}

# Reads mods.txt, prints workshop IDs one per line, comments stripped, order preserved.
read_mod_ids() {
  local file="${SCRIPT_DIR}/mods.txt"
  [ -f "$file" ] || die "mods.txt not found."
  sed -e 's/#.*//' -e 's/[[:space:]]//g' "$file" | grep -E '^[0-9]+$' || true
}

# Where the server drops downloaded workshop content. Varies by version, so check both.
workshop_roots() {
  printf '%s\n' \
    "$DATA_DIR/steamapps/workshop/content/$PZ_GAME_APPID" \
    "$INSTALL_DIR/steamapps/workshop/content/$PZ_GAME_APPID"
}

# Boot the server in a detached screen session, wait for a marker in the console
# log, then send "quit" so the world saves cleanly.
#   pz_boot_and_wait <marker> <timeout_seconds> [extra args...]
pz_boot_and_wait() {
  local marker="$1" timeout="$2"; shift 2
  local session="pzboot"
  local console="$DATA_DIR/server-console.txt"

  as_pz "screen -S $session -X quit" >/dev/null 2>&1 || true
  rm -f "$console"

  log "Booting server (waiting for: $marker) - this can take several minutes..."
  as_pz "cd '$INSTALL_DIR' && screen -dmS $session ./start-server.sh \
      -cachedir='$DATA_DIR' -servername '$SERVER_NAME' \
      -Xmx$JVM_XMX -Xms$JVM_XMS $*"

  local waited=0
  while [ "$waited" -lt "$timeout" ]; do
    if [ -f "$console" ] && grep -qF "$marker" "$console"; then
      log "Marker reached after ${waited}s. Shutting down cleanly."
      as_pz "screen -p 0 -S $session -X eval 'stuff \"quit\\015\"'" || true
      local drain=0
      while as_pz "screen -list" 2>/dev/null | grep -q "$session"; do
        sleep 3; drain=$((drain + 3))
        [ "$drain" -ge 120 ] && { warn "Server did not exit in 120s; forcing."; as_pz "screen -S $session -X quit" || true; break; }
      done
      return 0
    fi
    if ! as_pz "screen -list" 2>/dev/null | grep -q "$session"; then
      warn "Server process exited early. Last lines of the console log:"
      tail -n 30 "$console" 2>/dev/null >&2 || true
      return 1
    fi
    sleep 5; waited=$((waited + 5))
  done

  warn "Timed out after ${timeout}s waiting for '$marker'."
  as_pz "screen -S $session -X quit" || true
  return 1
}
