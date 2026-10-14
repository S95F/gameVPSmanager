#!/usr/bin/env bash
# ci/golden-units.sh — verify that the generated unit templates match their
# golden copies.  A drift between the template code and the golden files is a
# regression in `gvm apply`'s unit generation logic.
#
# Run: bash ci/golden-units.sh
# UPDATE_GOLDEN=1 bash ci/golden-units.sh   — regenerate golden files
# Returns: 0 = all clear, 1 = drift detected.

set -euo pipefail

GOLDEN_DIR="internal/cli/testdata/golden-units"
TEMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TEMP_DIR"' EXIT

# Regenerate the unit files from the current binary.
# This requires `gvm` to be built and in PATH or at ./gvm.
GVM_BIN="${GVM_BIN:-./gvm}"

if [ ! -x "$GVM_BIN" ]; then
    # Try building it.
    echo "Building gvm binary..."
    go build -o "$TEMP_DIR/gvm" ./cmd/gvm
    GVM_BIN="$TEMP_DIR/gvm"
fi

mkdir -p "$GOLDEN_DIR"

drifted=0
# For each golden file, compare with freshly generated output.
for golden in "$GOLDEN_DIR"/*.service "$GOLDEN_DIR"/*.conf 2>/dev/null; do
    [ -f "$golden" ] || continue
    name="$(basename "$golden")"

    # The golden files are named after the unit type.
    # We cannot fully regenerate without an instance spec, so this script
    # checks that the golden files have not drifted from themselves (i.e.,
    # that someone did not hand-edit them without running UPDATE_GOLDEN=1).
    # A more thorough check would integrate with the go tests.
    if [ "${UPDATE_GOLDEN:-0}" = "1" ]; then
        echo "SKIP: $name (UPDATE_GOLDEN mode: use go test with UPDATE_GOLDEN=1)"
        continue
    fi

    if ! git diff --quiet "$golden" 2>/dev/null; then
        echo "DRIFT: $name has uncommitted changes"
        drifted=$((drifted + 1))
    fi
done

# Also run the Go golden-file tests to catch template drift.
echo "Running Go golden-file tests..."
if ! go test -run TestGolden ./internal/cli/... -count=1; then
    echo "FAIL: Go golden-file tests failed"
    drifted=$((drifted + 1))
fi

if [ "$drifted" -eq 0 ]; then
    echo "OK: no unit template drift"
    exit 0
else
    echo "FAIL: $drifted drift(s) detected — run UPDATE_GOLDEN=1 make test to regenerate"
    exit 1
fi
