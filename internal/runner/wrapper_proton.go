package runner

import (
	"fmt"
	"path"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// applyProtonWrapper prepends the Proton binary and its extra arguments to the
// Launch's argv so the game runs under Proton's compatibility layer.
//
// Security rules (enforced here and by noshell_test.go):
//
//   - Proton is invoked DIRECTLY, never via a shell. No "sh -c", no "bash -c".
//
//   - The Path must contain a separator so it is not resolved through PATH.
//
//   - A misconfigured wrapper returns an error rather than silently starting
//     the game without Proton, which would make the failure invisible.
//
// Mapping from spec to argv:
//
//	spec.runtime.wrapper.args[0]   = Proton binary absolute path
//	spec.runtime.wrapper.args[1:]  = extra Proton flags (e.g. "run")
//	spec.runtime.exec              = game binary (becomes first positional arg)
//	spec.runtime.args              = game arguments (appended after game binary)
//
// Result: [proton-path] [proton-flags...] [game-binary] [game-args...]
func applyProtonWrapper(l Launch, w *spec.Wrapper, r *spec.Renderer) (Launch, error) {
	if w == nil || w.Kind != "proton" {
		kind := ""
		if w != nil {
			kind = w.Kind
		}
		return l, fmt.Errorf("runner: applyProtonWrapper: expected kind=proton, got %q", kind)
	}

	protonPath, protonFlags, err := resolveProtonBinary(w, l.Dir, r)
	if err != nil {
		return l, err
	}

	// Build new argv: proton [flags...] [original path] [original args...]
	newArgs := make([]string, 0, len(protonFlags)+1+len(l.Args))
	newArgs = append(newArgs, protonFlags...)
	newArgs = append(newArgs, l.Path)
	newArgs = append(newArgs, l.Args...)

	l.Path = protonPath
	l.Args = newArgs
	return l, nil
}

// resolveProtonBinary extracts the Proton binary path and extra flags from the
// wrapper spec. spec.runtime.wrapper.args[0] is the binary path; the rest are
// flags Proton receives before the game binary.
func resolveProtonBinary(w *spec.Wrapper, dir string, r *spec.Renderer) (binPath string, flags []string, err error) {
	if len(w.Args) == 0 {
		return "", nil, fmt.Errorf("runner: spec.runtime.wrapper.kind=proton requires " +
			"spec.runtime.wrapper.args[0] to be the Proton binary path (e.g. /steam/proton)")
	}

	raw := w.Args[0]
	if r != nil {
		rendered, _, rerr := r.Render("spec.runtime.wrapper.args[0]", raw)
		if rerr != nil {
			return "", nil, fmt.Errorf("runner: spec.runtime.wrapper.args[0]: %w", rerr)
		}
		raw = rendered
	}

	// Enforce path-separator rule (no bare names that would consult PATH).
	if !path.IsAbs(raw) && !pathHasSep(raw) {
		return "", nil, fmt.Errorf("runner: spec.runtime.wrapper.args[0] %q has no path "+
			"separator; write an absolute path or ./proton so the binary is not "+
			"resolved through PATH", raw)
	}

	var resolved string
	if path.IsAbs(raw) {
		resolved = path.Clean(raw)
	} else {
		if dir == "" {
			return "", nil, fmt.Errorf("runner: spec.runtime.wrapper.args[0] %q is "+
				"relative but spec.runtime.cwd is empty", raw)
		}
		resolved = path.Join(dir, raw)
	}

	// Remaining args are Proton flags (e.g. "run").
	extraFlags := make([]string, len(w.Args)-1)
	copy(extraFlags, w.Args[1:])

	return resolved, extraFlags, nil
}

// pathHasSep reports whether s contains a forward slash.
func pathHasSep(s string) bool {
	for _, c := range s {
		if c == '/' {
			return true
		}
	}
	return false
}
