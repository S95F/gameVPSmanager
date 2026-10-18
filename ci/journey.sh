#!/usr/bin/env bash
# ci/journey.sh — the end-to-end lifecycle test on a real systemd host.
#
# Every other CI job runs `go test` against fake HostEnvs plus static grep/AST
# gates. Nothing else starts a real gvm@.service, so the project's one rule —
# "kill the control plane, the game still runs" — and every phase whose
# Definition of done says "needs a live VM" are unverified by the unit suite.
# This job closes that gap on an ubuntu-latest runner (systemd + passwordless
# sudo).
#
# It needs no real game and no install engine: the fixture game is
# provider: none (ci/fixturegame.yaml), so gvm fetches nothing and the journey
# pre-places a tiny prebuilt binary (tools/fixturegame). The sequence is:
#
#   host init -> create -> apply -> place binary -> start -> assert ready
#   -> assert no control-plane dependency (and survive Postgres going down)
#   -> backup -> backup verify -> stop (assert clean, not a degraded SIGKILL)
#
# A reboot test needs a VM and is out of scope here; run it nightly or on a tag.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

say()  { printf '\n=== %s ===\n' "$*"; }
fail() { printf '\nJOURNEY FAIL: %s\n' "$*" >&2; exit 1; }

inst=fx-1
acct=gvm-fx-1
install_dir=/srv/gvm/fx-1/install

# ── 1. Build gvm and the fixture game (static, CGO-free) ─────────────────────
say "build gvm and the fixture game"
CGO_ENABLED=0 go build -o "$root/gvm" ./cmd/gvm
CGO_ENABLED=0 go build -o "$root/fixture" ./tools/fixturegame

# ── 2. Stand up the host — no --dashboard, so no Postgres/gvmd/gvm-web ───────
say "gvm host init"
sudo "$root/gvm" host init

# ── 3. Install the fixture manifest as an operator override ──────────────────
say "install the fixture manifest"
sudo install -D -m 0644 "$root/ci/fixturegame.yaml" /etc/gvm/games/fixturegame.yaml

# ── 4. Define and reconcile the instance ─────────────────────────────────────
say "gvm create $inst"
sudo gvm create "$inst" --game fixturegame --unapproved-override
say "gvm apply $inst"
sudo gvm apply "$inst"

# ── 5. Place the prebuilt binary where provider:none expects it ──────────────
say "place the fixture binary at $install_dir/fixture"
sudo install -o "$acct" -g "$acct" -m 0755 "$root/fixture" "$install_dir/fixture"

# ── 6. Start and wait for readiness ──────────────────────────────────────────
say "gvm start $inst"
sudo gvm start "$inst"

say "assert the instance reaches ready"
ready=
for _ in $(seq 1 30); do
  if sudo gvm status "$inst" --json | jq -e '.ready == true' >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 1
done
if [ -z "$ready" ]; then
  sudo gvm status "$inst" || true
  journalctl -u "gvm@$inst.service" --no-pager -n 50 || true
  fail "the instance never reached ready"
fi
echo "OK: $inst is ready"

# ── 7. The one rule: the game does not depend on the control plane ───────────
say "assert the live unit has no control-plane dependency"
deps="$(systemctl show "gvm@$inst.service" \
  --property=Requires --property=Requisite --property=BindsTo \
  --property=Wants --property=After 2>/dev/null || true)"
for svc in postgresql.service gvmd.service gvm-web.service gvm-db-migrate.service; do
  if printf '%s\n' "$deps" | grep -q "$svc"; then
    fail "gvm@$inst declares a dependency on $svc (games must survive the control plane)"
  fi
done
echo "OK: gvm@$inst has no control-plane dependency"

# ubuntu-latest ships Postgres; if it is up, take it down and confirm the game
# lives. That is the real, live form of ci/control-plane-dead.sh's static check.
if systemctl is-active --quiet postgresql 2>/dev/null; then
  say "stop postgresql and confirm the game survives"
  sudo systemctl stop postgresql
  sleep 2
  systemctl is-active --quiet "gvm@$inst.service" || fail "the game unit died when postgresql stopped"
  echo "OK: gvm@$inst survived postgresql going down"
fi

# ── 8. Back up the running world, then read it back ──────────────────────────
say "gvm backup $inst"
sudo gvm backup "$inst"
say "gvm backup verify $inst"
sudo gvm backup verify "$inst"
echo "OK: backup written and verified"

# ── 9. Stop, and assert the shutdown was clean ───────────────────────────────
say "gvm stop $inst"
stopout="$(sudo gvm stop "$inst" 2>&1)" || { printf '%s\n' "$stopout"; fail "gvm stop exited non-zero"; }
printf '%s\n' "$stopout"
if printf '%s\n' "$stopout" | grep -qi "degraded"; then
  fail "the stop was degraded (a SIGKILL was needed); the fixture should handle SIGINT"
fi
echo "OK: $inst stopped cleanly"

say "JOURNEY OK"
