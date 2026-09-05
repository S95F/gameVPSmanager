#!/usr/bin/env bash
# =============================================================
# Project Zomboid Build 42 dedicated server installer
# Target: fresh Debian 12 / Ubuntu 22.04 / 24.04 VPS, run as root
#
#   sudo ./install.sh                 full install + firewall
#   sudo ./install.sh --skip-firewall install only
#   sudo ./install.sh --skip-mods     no workshop mods
# =============================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"

SKIP_FIREWALL=0
SKIP_MODS=0
for arg in "$@"; do
  case "$arg" in
    --skip-firewall) SKIP_FIREWALL=1 ;;
    --skip-mods)     SKIP_MODS=1 ;;
    -h|--help)       sed -n '2,12p' "$0"; exit 0 ;;
    *)               die "Unknown option: $arg" ;;
  esac
done

require_root
load_config

command -v apt-get >/dev/null 2>&1 || die "This script targets Debian/Ubuntu (apt)."

# Passwords get passed through to the server on the command line, so keep them
# free of characters that break shell quoting.
case "${ADMIN_PASSWORD}${SERVER_PASSWORD}${RCON_PASSWORD}" in
  *[\'\"\\\$\`]*) die "Passwords in config.env must not contain quotes, backslashes, \$ or backticks." ;;
esac

# ------------------------------------------------------------------
# 0. Sanity checks
# ------------------------------------------------------------------
TOTAL_MB=$(awk '/MemTotal/ {print int($2/1024)}' /proc/meminfo)
log "Detected ${TOTAL_MB}MB RAM."
if [ "$TOTAL_MB" -lt 7500 ]; then
  warn "Build 42 wants ~8GB minimum (10GB comfortable) with a mod list this size."
  warn "On a smaller box, lower JVM_XMX in config.env and expect stutter or OOM kills."
fi
FREE_GB=$(df -BG --output=avail / | tail -1 | tr -dc '0-9')
[ "$FREE_GB" -lt 12 ] && warn "Only ${FREE_GB}GB free on / - the server plus mods needs ~10GB."

# ------------------------------------------------------------------
# 1. Packages
# ------------------------------------------------------------------
log "Installing packages..."
export DEBIAN_FRONTEND=noninteractive
# A fresh cloud image often runs unattended-upgrades on first boot and holds the
# dpkg lock for minutes. Wait for it rather than failing.
APT="apt-get -o DPkg::Lock::Timeout=600"

dpkg --add-architecture i386
$APT update -qq || die "apt update failed. Check /etc/apt/sources.list.d/ubuntu.sources and outbound networking."
$APT install -y -qq \
  curl ca-certificates tar gzip screen ufw sudo \
  lib32gcc-s1 lib32stdc++6 \
  locales procps >/dev/null

# SteamCMD only needs the 32-bit gcc/stdc++ libs to bootstrap. libsdl2 i386 is
# useful for some Steam titles but is not required here, and its package name
# moves between releases - so try it and carry on if it is not there.
$APT install -y -qq libsdl2-2.0-0:i386 >/dev/null 2>&1 \
  || warn "libsdl2 (i386) unavailable on this release - not needed for a headless PZ server, continuing."

# SteamCMD is unhappy without a UTF-8 locale.
if ! locale -a 2>/dev/null | grep -qi 'en_US.utf8'; then
  sed -i 's/^# *en_US.UTF-8/en_US.UTF-8/' /etc/locale.gen 2>/dev/null || true
  locale-gen en_US.UTF-8 >/dev/null 2>&1 || true
fi

# ------------------------------------------------------------------
# 2. Service user
# ------------------------------------------------------------------
if ! id "$PZ_USER" >/dev/null 2>&1; then
  log "Creating user $PZ_USER..."
  useradd -m -d "$PZ_HOME" -s /bin/bash "$PZ_USER"
else
  log "User $PZ_USER already exists."
fi
mkdir -p "$INSTALL_DIR" "$STEAMCMD_DIR" "$DATA_DIR" "$BACKUP_DIR"
chown -R "$PZ_USER:$PZ_USER" "$INSTALL_DIR" "$STEAMCMD_DIR" "$DATA_DIR" "$PZ_HOME"
chown "$PZ_USER:$PZ_USER" "$BACKUP_DIR"
chmod 750 "$BACKUP_DIR"

# ------------------------------------------------------------------
# 3. SteamCMD
# ------------------------------------------------------------------
if [ ! -x "$STEAMCMD_DIR/steamcmd.sh" ]; then
  log "Installing SteamCMD..."
  curl -sSL https://media.steampowered.com/client/installer/steamcmd_linux.tar.gz \
    | tar -xz -C "$STEAMCMD_DIR"
  chown -R "$PZ_USER:$PZ_USER" "$STEAMCMD_DIR"
else
  log "SteamCMD already present."
fi

# ------------------------------------------------------------------
# 4. Server binaries (app 380870, anonymous login - the server is free)
# ------------------------------------------------------------------
log "Downloading the dedicated server (several GB, be patient)..."
as_pz "'$STEAMCMD_DIR/steamcmd.sh' +force_install_dir '$INSTALL_DIR' \
    +login anonymous +app_update $PZ_APPID validate +quit"
[ -x "$INSTALL_DIR/start-server.sh" ] || die "start-server.sh missing - the Steam download failed."

# ------------------------------------------------------------------
# 5. Credentials
# ------------------------------------------------------------------
[ -z "$ADMIN_PASSWORD" ] && ADMIN_PASSWORD="$(gen_password)" && GENERATED_ADMIN=1
[ -z "$RCON_PASSWORD" ]  && RCON_PASSWORD="$(gen_password)"
umask 077
cat > /root/pz-credentials.txt <<EOF
Project Zomboid server credentials - generated $(date -Is)
admin account : admin
admin password: $ADMIN_PASSWORD
rcon password : $RCON_PASSWORD
join password : ${SERVER_PASSWORD:-(none)}
EOF
umask 022
log "Credentials written to /root/pz-credentials.txt (root-readable only)."

# ------------------------------------------------------------------
# 6. First boot - generates <servername>.ini and the admin account
# ------------------------------------------------------------------
if [ ! -f "$SERVER_INI" ]; then
  log "First boot: generating config and world. This is the slow part."
  pz_boot_and_wait "SERVER STARTED" 2400 "-adminpassword '$ADMIN_PASSWORD'" \
    || die "First boot failed. Check $DATA_DIR/server-console.txt"
else
  log "Config already exists at $SERVER_INI - keeping it."
fi
[ -f "$SERVER_INI" ] || die "Expected $SERVER_INI to exist after first boot."

# ------------------------------------------------------------------
# 7. Apply configuration
# ------------------------------------------------------------------
apply_sandbox
log "Writing server settings..."
set_ini PublicName        "$PUBLIC_NAME"      "$SERVER_INI"
set_ini PublicDescription "$PUBLIC_DESC"      "$SERVER_INI"
set_ini Public            "$PUBLIC"           "$SERVER_INI"
set_ini Open              "$OPEN"             "$SERVER_INI"
set_ini Password          "$SERVER_PASSWORD"  "$SERVER_INI"
set_ini MaxPlayers        "$MAX_PLAYERS"      "$SERVER_INI"
set_ini PVP               "$PVP"              "$SERVER_INI"
set_ini PauseEmpty        "$PAUSE_EMPTY"      "$SERVER_INI"
set_ini DefaultPort       "$GAME_PORT"        "$SERVER_INI"
set_ini UDPPort           "$UDP_PORT"         "$SERVER_INI"
set_ini RCONPort          "$RCON_PORT"        "$SERVER_INI"
set_ini RCONPassword      "$RCON_PASSWORD"    "$SERVER_INI"
set_ini UPnP              "false"             "$SERVER_INI"   # pointless on a VPS
chown "$PZ_USER:$PZ_USER" "$SERVER_INI"

# ------------------------------------------------------------------
# 8. Workshop mods
# ------------------------------------------------------------------
if [ "$SKIP_MODS" -eq 0 ]; then
  MOD_IDS="$(read_mod_ids | paste -sd';' -)"
  MOD_COUNT="$(read_mod_ids | wc -l)"
  log "Enabling $MOD_COUNT workshop items."
  set_ini WorkshopItems "$MOD_IDS" "$SERVER_INI"

  log "Second boot: downloading workshop content..."
  pz_boot_and_wait "SERVER STARTED" 3600 \
    || warn "Mod download boot did not finish cleanly - check the console log, then rerun ./sync-mods.sh"

  "$SCRIPT_DIR/sync-mods.sh" || warn "sync-mods.sh could not resolve every Mod ID - see its output."
else
  log "Skipping mods (--skip-mods)."
fi

# ------------------------------------------------------------------
# 9. systemd service
# ------------------------------------------------------------------
log "Installing systemd unit..."
sed -e "s|__PZ_USER__|$PZ_USER|g" \
    -e "s|__INSTALL_DIR__|$INSTALL_DIR|g" \
    -e "s|__DATA_DIR__|$DATA_DIR|g" \
    -e "s|__PZ_HOME__|$PZ_HOME|g" \
    -e "s|__SERVER_NAME__|$SERVER_NAME|g" \
    -e "s|__JVM_XMX__|$JVM_XMX|g" \
    -e "s|__JVM_XMS__|$JVM_XMS|g" \
    "$SCRIPT_DIR/systemd/zomboid.service.template" > /etc/systemd/system/zomboid.service

"$SCRIPT_DIR/nightly.sh"

systemctl daemon-reload
systemctl enable zomboid.service >/dev/null
systemctl restart zomboid.service
log "Service started."

# ------------------------------------------------------------------
# 10. Firewall
# ------------------------------------------------------------------
if [ "$SKIP_FIREWALL" -eq 0 ]; then
  "$SCRIPT_DIR/firewall.sh"
else
  warn "Firewall skipped. Run ./firewall.sh before exposing this box."
fi

# ------------------------------------------------------------------
# Summary
# ------------------------------------------------------------------
PUBLIC_IP="$(curl -s --max-time 5 https://api.ipify.org || echo '<your-vps-ip>')"
cat <<EOF

$(log 'Install complete.')

  Connect     : ${PUBLIC_IP}:${GAME_PORT}
  Admin user  : admin
  Admin pass  : ${ADMIN_PASSWORD}${GENERATED_ADMIN:+   (generated)}
  Config file : ${SERVER_INI}
  Console log : ${DATA_DIR}/server-console.txt
  Saves       : ${DATA_DIR}/Saves/Multiplayer/${SERVER_NAME}/

  Status      : systemctl status zomboid
  Live console: sudo -u ${PZ_USER} script -q /dev/null -c 'screen -r pzserver'   (detach: Ctrl-A then D)
  Logs        : journalctl -u zomboid -f
  Update      : sudo ./update.sh
  Backup      : sudo ./backup.sh

Credentials are also in /root/pz-credentials.txt
EOF
