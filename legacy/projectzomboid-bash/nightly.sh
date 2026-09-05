#!/usr/bin/env bash
# =============================================================
# Installs (or removes) the systemd timer for the nightly cycle.
# Driven by config.env:
#   RESTART_DAILY=true       arm the timer at RESTART_TIME
#   UPDATE_ON_RESTART=true   nightly cycle runs the full update.sh
#                            (backup, Steam update, mod re-sync)
#                            instead of a plain restart
# Standalone - safe to rerun after any config.env change:
#   sudo ./nightly.sh
# =============================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"

require_root
load_config

if [ "${RESTART_DAILY:-true}" != "true" ]; then
  log "RESTART_DAILY=false - removing the nightly timer."
  systemctl disable --now zomboid-restart.timer >/dev/null 2>&1 || true
  rm -f /etc/systemd/system/zomboid-restart.timer /etc/systemd/system/zomboid-restart.service
  systemctl daemon-reload
  exit 0
fi

# Empty RESTART_TZ = the server's own clock; an IANA name (America/Denver)
# pins the wall-clock time through DST changes.
ON_CALENDAR="*-*-* $RESTART_TIME:00${RESTART_TZ:+ $RESTART_TZ}"
systemd-analyze calendar "$ON_CALENDAR" >/dev/null 2>&1 \
  || die "systemd rejects 'OnCalendar=$ON_CALENDAR' - check RESTART_TIME/RESTART_TZ in config.env."
sed -e "s|__ON_CALENDAR__|$ON_CALENDAR|g" \
    "$SCRIPT_DIR/systemd/zomboid-restart.timer.template" > /etc/systemd/system/zomboid-restart.timer

if [ "${UPDATE_ON_RESTART:-false}" = "true" ]; then
  log "Nightly cycle at $RESTART_TIME ${RESTART_TZ:-server time}: full update (backup, Steam binaries, workshop mods, mod IDs)."
  # bash explicitly, so a lost +x bit after an scp from Windows can't break the job
  cat > /etc/systemd/system/zomboid-restart.service <<EOF
[Unit]
Description=Nightly update + restart of the Project Zomboid server

[Service]
Type=oneshot
ExecStart=/usr/bin/env bash $SCRIPT_DIR/update.sh
EOF
else
  log "Nightly cycle at $RESTART_TIME ${RESTART_TZ:-server time}: plain restart."
  cat > /etc/systemd/system/zomboid-restart.service <<'EOF'
[Unit]
Description=Nightly restart of the Project Zomboid server

[Service]
Type=oneshot
ExecStart=/bin/systemctl restart zomboid.service
EOF
fi

systemctl daemon-reload
systemctl enable zomboid-restart.timer >/dev/null
# enable --now is not enough: a changed OnCalendar on a live timer only
# recomputes the next elapse on restart.
systemctl restart zomboid-restart.timer
systemctl list-timers zomboid-restart.timer --no-pager | sed -n '1,2p'
log "Installed. The job logs to: journalctl -u zomboid-restart"
