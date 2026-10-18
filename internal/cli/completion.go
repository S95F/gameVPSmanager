// `gvm completion` — print a shell completion script, and nothing else.
//
// Rule 2: this command spawns nothing. It does not source the shell, run
// `compgen`, or shell out to generate the script — it assembles static text
// from the dispatch table and prints it. What the operator does with that text
// (source it, drop it in a completions directory) is their shell's business,
// not gvm's.
//
// The verb list is read from `commands`, so a verb added to the table appears
// in completion the day it is added; there is no second list to keep in step.
// Completion stops at the top-level verb on purpose: per-command flags and
// subcommands are not yet a single declarative table (that is a later Phase 22
// refactor), and a completion that guessed them would go stale silently. A
// correct verb completion that never lies is worth more than a fuller one that
// does.

package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// runCompletion prints the completion script for one shell.
func runCompletion(_ context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(env.Stderr, "usage: gvm completion bash|zsh|fish")
		return usageErr(fmt.Errorf("completion: name a shell"))
	}
	if isHelpFlag(args[0]) {
		fmt.Fprintln(env.Stdout, "Usage: gvm completion bash|zsh|fish")
		fmt.Fprintln(env.Stdout)
		fmt.Fprintln(env.Stdout, "Prints a shell completion script to stdout. It spawns nothing.")
		fmt.Fprintln(env.Stdout)
		fmt.Fprintln(env.Stdout, "  bash:  source <(gvm completion bash)")
		fmt.Fprintln(env.Stdout, "  zsh:   gvm completion zsh > \"${fpath[1]}/_gvm\"")
		fmt.Fprintln(env.Stdout, "  fish:  gvm completion fish > ~/.config/fish/completions/gvm.fish")
		return errHelpShown
	}
	shell := args[0]
	cmds := completionCommands()
	switch shell {
	case "bash":
		fmt.Fprint(env.Stdout, bashCompletion(cmds))
	case "zsh":
		fmt.Fprint(env.Stdout, zshCompletion(cmds))
	case "fish":
		fmt.Fprint(env.Stdout, fishCompletion(cmds))
	default:
		fmt.Fprintln(env.Stderr, "usage: gvm completion bash|zsh|fish")
		return usageErr(fmt.Errorf("completion: unknown shell %q; one of bash, zsh, fish", shell))
	}
	return nil
}

// completionCommands is the visible, dispatchable verbs, sorted, for a stable
// script. Hidden roles and aliases are left out: a human never types them, and
// a completion that offered `gvmd` or `run-shutdown` would only invite trouble.
func completionCommands() []Command {
	out := make([]Command, 0, len(commands))
	for _, c := range commands {
		if c.Hidden {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func bashCompletion(cmds []Command) string {
	names := make([]string, len(cmds))
	for i, c := range cmds {
		names[i] = c.Name
	}
	var b strings.Builder
	b.WriteString("# gvm bash completion. Source it: source <(gvm completion bash)\n")
	b.WriteString("_gvm() {\n")
	b.WriteString("    local cur=\"${COMP_WORDS[COMP_CWORD]}\"\n")
	b.WriteString("    local commands=\"" + strings.Join(names, " ") + "\"\n")
	b.WriteString("    if [ \"${COMP_CWORD}\" -eq 1 ]; then\n")
	b.WriteString("        COMPREPLY=( $(compgen -W \"${commands}\" -- \"${cur}\") )\n")
	b.WriteString("        return 0\n")
	b.WriteString("    fi\n")
	b.WriteString("    return 0\n")
	b.WriteString("}\n")
	b.WriteString("complete -F _gvm gvm\n")
	return b.String()
}

func zshCompletion(cmds []Command) string {
	var b strings.Builder
	b.WriteString("#compdef gvm\n")
	b.WriteString("# gvm zsh completion. Install it: gvm completion zsh > \"${fpath[1]}/_gvm\"\n")
	b.WriteString("_gvm() {\n")
	b.WriteString("    local -a commands\n")
	b.WriteString("    commands=(\n")
	for _, c := range cmds {
		b.WriteString("        '" + zshEscape(c.Name) + ":" + zshEscape(c.Summary) + "'\n")
	}
	b.WriteString("    )\n")
	b.WriteString("    if (( CURRENT == 2 )); then\n")
	b.WriteString("        _describe 'gvm command' commands\n")
	b.WriteString("    fi\n")
	b.WriteString("}\n")
	b.WriteString("compdef _gvm gvm\n")
	return b.String()
}

func fishCompletion(cmds []Command) string {
	var b strings.Builder
	b.WriteString("# gvm fish completion. Install it: gvm completion fish > ~/.config/fish/completions/gvm.fish\n")
	b.WriteString("complete -c gvm -f\n")
	for _, c := range cmds {
		b.WriteString("complete -c gvm -n __fish_use_subcommand -a " + c.Name +
			" -d '" + fishEscape(c.Summary) + "'\n")
	}
	return b.String()
}

// zshEscape makes a string safe inside a single-quoted `name:desc` pair: the
// colon is _describe's field separator, and a bare single quote closes the
// quoting. Both are backslash-escaped.
func zshEscape(s string) string {
	s = strings.ReplaceAll(s, `'`, `'\''`)
	s = strings.ReplaceAll(s, ":", `\:`)
	return s
}

// fishEscape makes a string safe inside the single-quoted description of a
// `complete` line.
func fishEscape(s string) string {
	return strings.ReplaceAll(s, `'`, `\'`)
}
