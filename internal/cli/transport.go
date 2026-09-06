// Section 3.3's transport ladder, in one function.
//
// The rule it encodes is four lines long and the fourth is the important one:
// a verb that cannot be carried is an ERROR naming the two ways to fix it, not
// a quiet fallback to some other path. A CLI that silently downgraded from
// "talk to the API" to "do it locally as whoever you are" would be a CLI that
// did something different depending on the environment it inherited.
//
// The one thing this file exists to make structural is the socket that is NOT
// here. /run/gvm/admin.sock is 0660 root:gvm, and gvm-web is in group gvm
// because it has to read /etc/gvm. If the CLI dialled that socket for its
// root-local verbs, then every member of group gvm -- which is to say the web
// tier's own account, and anything that compromises it -- would hold the
// complete root verb surface by running `gvm stop`. That is precisely the
// escalation the two-daemon split of section 3.2 exists to prevent, so the
// root CLI calls hostops IN-PROCESS: it is already root, and a socket hop
// would add a boundary that only weakens the one above it.
package cli

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/secrets"
)

// AdminSocket is gvmd's socket. It is declared here to be NAMED in a refusal
// and nowhere else; TestTheCLINeverNamesTheAdminSocket is what keeps it that
// way, by failing on any other string literal in the package that mentions it.
const AdminSocket = "/run/gvm/admin.sock"

// The two environment variables that select the remote transport.
const (
	EnvURL   = "GVM_URL"
	EnvToken = "GVM_TOKEN"
)

// Mode is which of the three transports carries a verb.
type Mode string

const (
	// ModeLocalRoot calls internal/hostops in this process, as root.
	ModeLocalRoot Mode = "local-root"
	// ModeRemote is HTTPS to a gvm-web with a bearer token.
	ModeRemote Mode = "remote"
	// ModeConsole is a direct AF_UNIX connection to one instance's console
	// socket. It is not a host operation and does not go through gvmd at all.
	ModeConsole Mode = "console"
)

// Route is how a verb is ALLOWED to be carried, declared once per verb in the
// `routes` table below.
//
// It is declared rather than inferred inside Resolve because the question "is
// this a host op" has exactly one right answer per verb, and an answer derived
// from the name would be wrong the first time somebody adds a verb that does
// not look privileged.
type Route uint8

const (
	// RouteLocal needs no transport at all: it reads this binary, or files
	// the caller can already read, and works as any user on any platform.
	RouteLocal Route = iota
	// RouteHostOp changes or inspects the host. Root in-process, or remote.
	RouteHostOp
	// RouteConsole talks to one instance's console socket.
	RouteConsole
	// RouteRole is a role systemd invokes, never an operator verb. It brings
	// its own preconditions and Resolve is not consulted for it.
	RouteRole
)

// Transport is the resolved answer for one invocation.
type Transport struct {
	Mode Mode
	Verb string

	// URL is the base URL for ModeRemote, already parsed and rejected if it
	// is not https (or an explicit http://localhost, for a developer running
	// the dashboard behind nothing).
	URL string

	// Token is the bearer credential. It is a secrets.Value so that a
	// Transport reaching fmt, slog or an error string cannot print it: the
	// place a token is most likely to leak is the diagnostic printed when the
	// transport fails.
	Token secrets.Value
}

// ErrNoTransport is "this verb cannot be carried from here".
var ErrNoTransport = errors.New("gvm: no transport for this command")

// ErrRemoteUnbuilt is the honest answer for a resolved remote transport in a
// build whose API client does not exist yet.
//
// It is a distinct sentinel from ErrNoTransport because the operator's next
// move differs completely: one means "become root or set GVM_URL", the other
// means "this gvm cannot do that yet, and no amount of configuration will
// change it in this build".
var ErrRemoteUnbuilt = errors.New("gvm: the remote transport is not built in this release")

// Resolve applies section 3.3's ladder.
//
//  1. --remote URL or $GVM_URL set          -> ModeRemote
//  2. euid == 0 and the verb is a host op   -> ModeLocalRoot
//  3. euid == 0 and the verb is `console`   -> ModeConsole
//  4. otherwise                             -> error naming the two fixes
//
// remote is the value of the command's own --remote flag, empty when it was
// not given. It wins over the environment so that one invocation can be aimed
// somewhere else without unsetting a variable the shell will hand to the next
// one too.
func Resolve(env *Env, verb string, remote string) (Transport, error) {
	route := routeOf(verb)

	target := remote
	if target == "" {
		target = strings.TrimSpace(env.Getenv(EnvURL))
	}
	if target != "" {
		u, err := parseRemote(target)
		if err != nil {
			return Transport{}, err
		}
		tok := strings.TrimSpace(env.Getenv(EnvToken))
		if tok == "" {
			return Transport{}, fmt.Errorf("%w: %s names a remote gvm but no %s is set; "+
				"mint one with `gvm token create` on that host",
				ErrNoTransport, sourceOf(remote), EnvToken)
		}
		return Transport{Mode: ModeRemote, Verb: verb, URL: u, Token: secrets.New(tok)}, nil
	}

	if env.Geteuid() == 0 {
		switch route {
		case RouteHostOp:
			return Transport{Mode: ModeLocalRoot, Verb: verb}, nil
		case RouteConsole:
			return Transport{Mode: ModeConsole, Verb: verb}, nil
		}
	}

	// Rule 4. The message names both fixes because the two audiences for it
	// are different people: an operator on the box forgot `sudo`, and someone
	// on a laptop has not been given a token yet.
	switch route {
	case RouteConsole:
		return Transport{}, fmt.Errorf("%w: `gvm %s` needs the console socket, which is 0660 "+
			"inside a 2750 directory. Run it as root, or set %s and %s to reach the dashboard "+
			"(`gvm token create` mints one)", ErrNoTransport, verb, EnvURL, EnvToken)
	default:
		return Transport{}, fmt.Errorf("%w: `gvm %s` is a host operation. Run it as root "+
			"(sudo gvm %s), or point it at a dashboard with --remote / %s plus %s "+
			"(`gvm token create` mints one).\n"+
			"gvm deliberately does not dial %s: that socket is 0660 root:gvm, so a CLI that "+
			"used it would hand the whole root verb surface to every member of group gvm",
			ErrNoTransport, verb, verb, EnvURL, EnvToken, AdminSocket)
	}
}

// RequireLocal is what a command calls when it can only run in-process.
//
// Every verb this phase builds is local-root only. Saying so here, once, is
// better than each command inventing its own sentence for a transport it does
// not implement -- and it means the day the API client lands, the compiler
// finds every call site.
func (t Transport) RequireLocal() error {
	switch t.Mode {
	case ModeLocalRoot:
		return nil
	case ModeRemote:
		return fmt.Errorf("%w: `gvm %s` against %s needs the API client, which lands with the "+
			"web tier. Run it as root on the host itself", ErrRemoteUnbuilt, t.Verb, t.URL)
	default:
		return fmt.Errorf("%w: `gvm %s` resolved to the %s transport", ErrNoTransport, t.Verb, t.Mode)
	}
}

// sourceOf names where a remote URL came from, so the fix is unambiguous.
func sourceOf(flagValue string) string {
	if flagValue != "" {
		return "--remote"
	}
	return "$" + EnvURL
}

// parseRemote refuses a URL that would send a bearer token in clear.
//
// http:// to a loopback host is allowed because that is a developer with the
// dashboard on a unix socket and a local proxy, and refusing it would push
// them towards disabling verification somewhere worse. Everything else must be
// https: a token is a root-equivalent credential for whatever the actor can
// reach, and there is no useful "just this once".
func parseRemote(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %q is not a URL: %v", ErrNoTransport, raw, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%w: %q names no host; want https://gvm.example.com",
			ErrNoTransport, raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopback(u.Hostname()) {
			return "", fmt.Errorf("%w: %q is plain http to %s; the bearer token would cross "+
				"the network in clear. Use https", ErrNoTransport, raw, u.Hostname())
		}
	default:
		return "", fmt.Errorf("%w: %q has scheme %q; want https", ErrNoTransport, raw, u.Scheme)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "[::1]"
}

// routes classifies every dispatchable verb.
//
// It is a table of its own rather than a field on Command because a Command
// holds a func value and Resolve is reached from several of them, which Go's
// initialisation analysis reads as a cycle. The closure property the field
// would have given for free is asserted instead, in both directions, by
// TestEveryCommandDeclaresARoute: a verb missing from this map, or a route
// naming a verb that does not exist, fails the build's tests.
//
// The default for an unlisted verb is RouteLocal, which is the SAFE direction
// -- it needs no transport and reaches no host -- but a verb that reached it
// by omission would silently stop requiring root, which is why the test exists
// rather than a comment saying to remember.
var routes = map[string]Route{
	"version": RouteLocal,
	"help":    RouteLocal,
	"game":    RouteLocal,

	"gvmd":         RouteRole,
	"web":          RouteRole,
	"run":          RouteRole,
	"run-prepare":  RouteRole,
	"run-shutdown": RouteRole,
	"probe-write":  RouteRole,

	"console": RouteConsole,
	"control": RouteConsole,

	"create":   RouteHostOp,
	"install":  RouteHostOp,
	"apply":    RouteHostOp,
	"start":    RouteHostOp,
	"stop":     RouteHostOp,
	"restart":  RouteHostOp,
	"status":   RouteHostOp,
	"list":     RouteHostOp,
	"logs":     RouteHostOp,
	"ports":    RouteHostOp,
	"destroy":  RouteHostOp,
	"content":  RouteHostOp,
	"backup":   RouteHostOp,
	"restore":  RouteHostOp,
	"update":   RouteHostOp,
	"rebuild":  RouteHostOp,
	"adopt":    RouteHostOp,
	"firewall": RouteHostOp,
	"doctor":   RouteHostOp,
}

func routeOf(verb string) Route { return routes[verb] }
