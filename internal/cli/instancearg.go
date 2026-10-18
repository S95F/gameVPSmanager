package cli

import (
	"errors"
	"flag"
	"fmt"
	"strings"
)

// The instance is the subject of almost every gvm command, and the convention
// is that it is the first positional: `gvm stop pz-main`, not `gvm stop
// --instance pz-main`. But a script that builds commands programmatically, or
// an operator who learned the flag on another tool, reaches for `--instance` or
// `--name`, and a CLI that rejected them would be pedantic about a thing it
// understood perfectly well.
//
// So --instance and --name are universal, hidden aliases for the first
// positional. They are NOT registered flags: a registered flag would show in
// every verb's --help (they are meant to be invisible) and would collide with
// the verbs that already use --name or --instance for something else (token
// create's --name, backup's --instance). Instead parseWithInstance lifts them
// out of the argument list before the flag set ever sees it, and feeds the
// value in as the positional the verb was going to read anyway.

// parseWithInstance is parseInterspersed plus the --instance/--name aliases. A
// verb whose single positional is an instance calls this instead of
// parseInterspersed; nothing else about the verb changes, and the alias never
// appears in its help.
func parseWithInstance(fs *flag.FlagSet, args []string) ([]string, error) {
	rest, alias, err := extractInstanceAlias(args)
	if err != nil {
		return nil, err
	}
	pos, err := parseInterspersed(fs, rest)
	if err != nil {
		return nil, err
	}
	if alias != "" {
		if len(pos) > 0 {
			return nil, fmt.Errorf("name the instance once: as the argument %q or with "+
				"--instance/--name, not both", pos[0])
		}
		pos = []string{alias}
	}
	return pos, nil
}

// extractInstanceAlias pulls any --instance/--name value out of args, returning
// the remaining args and the alias. It understands one or two dashes and the
// `=value` form. Repeats take the last value; --instance and --name given with
// different values are a contradiction, because they are two names for one
// thing.
func extractInstanceAlias(args []string) (rest []string, alias string, err error) {
	seen := map[string]string{}
	for i := 0; i < len(args); i++ {
		name, val, hasEq, ok := matchInstanceFlag(args[i])
		if !ok {
			rest = append(rest, args[i])
			continue
		}
		if hasEq {
			seen[name] = val
			continue
		}
		if i+1 >= len(args) {
			return nil, "", fmt.Errorf("--%s needs a value", name)
		}
		seen[name] = args[i+1]
		i++
	}
	inst, hasInst := seen["instance"]
	nm, hasName := seen["name"]
	switch {
	case hasInst && hasName && inst != nm:
		return nil, "", errors.New("--instance and --name were both given and differ; " +
			"they are two names for the same instance")
	case hasInst:
		return rest, inst, nil
	case hasName:
		return rest, nm, nil
	}
	return rest, "", nil
}

// matchInstanceFlag reports whether a token is `--instance`/`--name` (one or
// two dashes), and splits an `=value` if present.
func matchInstanceFlag(arg string) (name, val string, hasEq, ok bool) {
	s := arg
	switch {
	case strings.HasPrefix(s, "--"):
		s = s[2:]
	case strings.HasPrefix(s, "-"):
		s = s[1:]
	default:
		return "", "", false, false
	}
	key, v, eq := strings.Cut(s, "=")
	if key != "instance" && key != "name" {
		return "", "", false, false
	}
	return key, v, eq, true
}
