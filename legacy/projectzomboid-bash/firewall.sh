#!/usr/bin/env bash
# =============================================================
# Locks the VPS down to: SSH (rate-limited) + the two Project Zomboid
# UDP ports. Everything else inbound is dropped. Outbound stays open
# so SteamCMD and workshop downloads keep working.
#
#   sudo ./firewall.sh            apply rules
#   sudo ./firewall.sh --reset    wipe existing ufw rules first
# =============================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"

require_root
load_config

command -v ufw >/dev/null 2>&1 || { DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=600 install -y -qq ufw >/dev/null; }

# ------------------------------------------------------------------
# Work out the real SSH port before doing anything that could lock us out.
# Priority: config.env -> the port of this live SSH session -> sshd_config -> 22
# ------------------------------------------------------------------
detect_ssh_port() {
  [ -n "${SSH_PORT:-}" ] && { echo "$SSH_PORT"; return; }
  if [ -n "${SSH_CONNECTION:-}" ]; then
    local p; p="$(awk '{print $4}' <<<"$SSH_CONNECTION")"
    [[ "$p" =~ ^[0-9]+$ ]] && { echo "$p"; return; }
  fi
  local cfg
  cfg="$(grep -rhoiE '^[[:space:]]*Port[[:space:]]+[0-9]+' /etc/ssh/sshd_config /etc/ssh/sshd_config.d/*.conf 2>/dev/null \
        | grep -oE '[0-9]+' | head -1)"
  echo "${cfg:-22}"
}
SSHD_PORT="$(detect_ssh_port)"
log "SSH port detected as ${SSHD_PORT}."

if [ "${1:-}" = "--reset" ]; then
  warn "Resetting all existing ufw rules."
  ufw --force reset >/dev/null
fi

# ------------------------------------------------------------------
# Policy
# ------------------------------------------------------------------
ufw default deny incoming  >/dev/null
ufw default allow outgoing >/dev/null
ufw default deny routed    >/dev/null 2>&1 || true

# SSH: "limit" drops a source that opens 6+ connections in 30s - kills
# most brute-force noise without touching normal use.
ufw limit "${SSHD_PORT}/tcp" comment 'SSH (rate limited)' >/dev/null

# Project Zomboid. Both are UDP. 16261 is browser + handshake + direct
# connect; 16262 is the data channel after the handshake. Opening only
# one gives you a server people can see but never finish joining.
ufw allow "${GAME_PORT}/udp" comment 'Project Zomboid game port' >/dev/null
ufw allow "${UDP_PORT}/udp"  comment 'Project Zomboid data port'  >/dev/null

# RCON stays shut. If you set ADMIN_IP, only that address gets in.
# Otherwise reach it over an SSH tunnel:
#   ssh -L 27015:127.0.0.1:27015 user@your-vps
if [ -n "${ADMIN_IP:-}" ]; then
  ufw allow from "$ADMIN_IP" to any port "$RCON_PORT" proto tcp comment 'RCON (admin IP only)' >/dev/null
  log "RCON open to $ADMIN_IP only."
else
  ufw delete allow "${RCON_PORT}/tcp" >/dev/null 2>&1 || true
  log "RCON closed to the internet (use an SSH tunnel)."
fi

ufw logging low >/dev/null
ufw --force enable >/dev/null

# ------------------------------------------------------------------
# fail2ban for SSH
# ------------------------------------------------------------------
if [ "${INSTALL_FAIL2BAN:-false}" = "true" ]; then
  log "Installing fail2ban..."
  DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=600 install -y -qq fail2ban >/dev/null
  cat > /etc/fail2ban/jail.local <<EOF
[DEFAULT]
banaction = ufw
backend   = systemd
findtime  = 10m
bantime   = 1h
maxretry  = 5

[sshd]
enabled = true
port    = ${SSHD_PORT}
EOF
  systemctl enable fail2ban >/dev/null 2>&1 || true
  systemctl restart fail2ban || warn "fail2ban failed to start - check: systemctl status fail2ban"
fi

echo
ufw status verbose
echo
log "Firewall active. Open ports: ${SSHD_PORT}/tcp, ${GAME_PORT}/udp, ${UDP_PORT}/udp."
warn "Before closing this session, open a second SSH connection to confirm you can still get in."
