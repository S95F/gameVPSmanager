package cli

import "errors"

// userError is the shape every operator-facing error is meant to take: what
// happened, then what to do next. It renders as "<Msg>. <Hint>", and Main
// prefixes it with "gvm <verb>: ", so the whole sentence reads
//
//	gvm create: --game is required. `gvm game list` shows what this box can run
//
// Ref is a pointer for a developer reading a bug report — a help topic, a
// doctor id — shown ONLY under GVM_DEBUG=1 (renderError appends it there). The
// operator at 2am reads Msg and Hint; the reference they cannot act on stays
// out of their way until someone is actually debugging. Design references
// (D-nn, section x.y) never go in Msg or Hint — the lint
// (TestNoDesignRefsInOperatorText) enforces that — so Ref is the one channel a
// developer-facing pointer may travel, and only under the debug flag.
type userError struct {
	Msg  string
	Hint string
	Ref  string
}

func (e *userError) Error() string {
	if e.Hint == "" {
		return e.Msg
	}
	return e.Msg + ". " + e.Hint
}

// renderError is how Main turns an error into the line it prints. It is the one
// place GVM_DEBUG adds a userError's Ref, so the flag works for a userError
// wrapped in any of the exit-code carriers (usageErr, refused, checked) too.
func renderError(env *Env, err error) string {
	msg := err.Error()
	var ue *userError
	if errors.As(err, &ue) && ue.Ref != "" && env.Getenv("GVM_DEBUG") != "" {
		msg += " [" + ue.Ref + "]"
	}
	return msg
}
