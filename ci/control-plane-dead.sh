#!/usr/bin/env bash
# ci/control-plane-dead.sh — verify that game units have no hard dependency on
# the control-plane services (Postgres, gvmd, gvm-web).
#
# Architecture invariant: a running game server must not be stopped or prevented
# from starting because the database or daemon is unavailable.  Any unit that
# carries Requires=postgresql.service, Wants=postgresql.service, or
# After=postgresql.service violates this invariant (section 14.6).
#
# Run: bash ci/control-plane-dead.sh [unit-dir]
# Returns: 0 = all clear, 1 = violations found.

set -euo pipefail

UNIT_DIR="${1:-internal/cli/testdata}"
CONTROL_PLANE_SERVICES=(
    "postgresql.service"
    "gvmd.service"
    "gvm-web.service"
    "gvm-db-migrate.service"
)
DISALLOWED_KEYWORDS=("Requires=" "BindsTo=")

violations=0

while IFS= read -r -d '' unit; do
    unit_name="$(basename "$unit")"
    # Only check instance units, not the control-plane units themselves.
    case "$unit_name" in
        gvm@*.service|gvm@.service)
            ;;
        *)
            continue
            ;;
    esac

    for svc in "${CONTROL_PLANE_SERVICES[@]}"; do
        for kw in "${DISALLOWED_KEYWORDS[@]}"; do
            if grep -q "${kw}.*${svc}\|${svc}.*${kw}" "$unit" 2>/dev/null; then
                echo "FAIL: $unit: ${kw}${svc} — game units must not hard-depend on control-plane services"
                violations=$((violations + 1))
            fi
        done
    done
done < <(find "$UNIT_DIR" -name "*.service" -print0)

# Also check template unit files under internal/hostops/ and testdata.
while IFS= read -r -d '' unit; do
    for svc in "${CONTROL_PLANE_SERVICES[@]}"; do
        for kw in "${DISALLOWED_KEYWORDS[@]}"; do
            if grep -q "${kw}.*${svc}\|${svc}.*${kw}" "$unit" 2>/dev/null; then
                echo "FAIL: $unit: ${kw}${svc} — game units must not hard-depend on control-plane services"
                violations=$((violations + 1))
            fi
        done
    done
done < <(find "internal" -name "*.service" -print0 2>/dev/null)

if [ "$violations" -eq 0 ]; then
    echo "OK: no control-plane dependencies in game units"
    exit 0
else
    echo "FAIL: $violations violation(s) found"
    exit 1
fi
