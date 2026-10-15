// `gvm probe-write` — the inside half of the write-probe.
//
// It runs INSIDE the transient unit `gvm apply` starts, as the account the
// game runs as, under the same ProtectHome=, ProtectSystem= and
// ReadWritePaths= the generated drop-in will carry. Its whole job is to try
// the one thing the sandbox might silently forbid — creating a file in each
// directory the game must write — and to report the result per path.
//
// Three details are load-bearing:
//
//   - It reports NDJSON on stdout, one object per path, and keeps going after
//     a failure. One exit code cannot say which of six paths was refused, and
//     an operator who has to widen a sandbox wants to widen it once.
//   - It fsyncs before unlinking. A create that succeeds and a write that
//     reaches the filesystem are different questions on an overlay, on a full
//     volume, and on a read-only bind mount that only fails at writeback.
//   - It exits 78 (EX_CONFIG, section 7.8's "the sandbox blocks a required
//     write") when any path failed. That is the same code the runner uses for
//     the same condition, so a failure here and a failure at boot are the
//     same event with the same number.
//
// It is deliberately cross-platform. The syscalls it makes — create, sync,
// remove — exist everywhere, so the logic that decides what to report is
// exercised by the ordinary Windows test run rather than only on the target.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/S95F/gameVPSmanager/internal/reconcile"
)

// probeWriteFailed is the exit status when at least one path is unwritable.
//
// 78 is EX_CONFIG, which the instance template lists in
// RestartPreventExitStatus: a sandbox that blocks a required write is not
// something a restart can fix, and a restart loop would hide the error.
const probeWriteFailed = 78

func runProbeWrite(_ context.Context, env *Env, args []string) error {
	flags := flag.NewFlagSet("gvm probe-write", flag.ContinueOnError)
	flags.SetOutput(env.Stderr)
	flags.Usage = func() {
		fmt.Fprint(env.Stderr, "Usage: gvm probe-write <dir> [dir...]\n\n"+
			"Creates, fsyncs and removes a probe file in each directory and reports\n"+
			"one NDJSON object per path on stdout. Run inside the transient unit\n"+
			"`gvm apply` builds; not meant to be typed.\n")
	}
	pos, err := parseInterspersed(flags, args)
	if err != nil {
		return flagErr(err)
	}
	if len(pos) == 0 {
		flags.Usage()
		return errors.New("name at least one directory to probe")
	}

	results := probeWritePaths(pos, os.Getpid())
	if err := writeProbeResults(env.Stdout, results); err != nil {
		return err
	}
	for _, r := range results {
		if !r.OK {
			// The message on stderr is for the journal; the machine-readable
			// answer already went to stdout, which the parent is reading.
			fmt.Fprintf(env.Stderr, "gvm probe-write: %s: %s\n", r.Path, r.Error)
			return &ExitError{Code: probeWriteFailed}
		}
	}
	return nil
}

// probeWritePaths tries every path and never stops at the first failure.
//
// Reporting all of them is the point: the parent turns this list into one
// message naming every refused path and the directive responsible, and an
// operator fixes the sandbox once instead of once per apply.
func probeWritePaths(paths []string, pid int) []reconcile.ProbeResult {
	out := make([]reconcile.ProbeResult, 0, len(paths))
	for _, p := range paths {
		r := reconcile.ProbeResult{Path: p, OK: true}
		if err := probeOne(p, pid); err != nil {
			r.OK = false
			r.Error = err.Error()
		}
		out = append(out, r)
	}
	return out
}

// probeOne creates, syncs and removes one probe file.
//
// The order matters. Create answers "may I make a file here", Sync answers
// "does a write actually reach the filesystem" — which is a different question
// on an overlay, on a volume with no space, and on a mount that only refuses
// at writeback — and Remove is what keeps the probe from leaving litter in a
// world directory.
//
// A file left behind when Remove fails is reported rather than ignored: a
// dot-file appearing in an operator's save directory with no explanation is
// exactly the kind of thing that gets blamed on the game.
func probeOne(dir string, pid int) error {
	name := filepath.Join(dir, reconcile.ProbeFilePrefix+strconv.Itoa(pid))

	// O_EXCL, so a leftover from a previous probe is a reported failure rather
	// than a silent success against a file this run did not create.
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	writeErr := func() error {
		if _, err := f.Write([]byte("gvm write probe\n")); err != nil {
			return err
		}
		return f.Sync()
	}()
	closeErr := f.Close()
	rmErr := os.Remove(name)

	switch {
	case writeErr != nil:
		return writeErr
	case closeErr != nil:
		// A deferred write can fail at close on a full or read-only-at-
		// writeback filesystem, which is precisely the case a create-only
		// probe would call a success.
		return closeErr
	case rmErr != nil:
		return fmt.Errorf("wrote %s and could not remove it: %w", name, rmErr)
	}
	return nil
}

// writeProbeResults emits one JSON object per line.
//
// NDJSON rather than a single array because the parent reads it from a pipe
// while the child is still running, and because a truncated array is
// unparseable while a truncated NDJSON stream is a short one the parent can
// name the missing paths from.
func writeProbeResults(w io.Writer, results []reconcile.ProbeResult) error {
	enc := json.NewEncoder(w)
	for _, r := range results {
		if err := enc.Encode(r); err != nil {
			return fmt.Errorf("gvm probe-write: reporting %s: %w", r.Path, err)
		}
	}
	return nil
}
