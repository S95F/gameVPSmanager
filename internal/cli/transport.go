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
	"strings"

	"github.com/S95F/gameVPSmanager/internal/avail"
	"github.com/S95F/gameVPSmanager/internal/secrets"
)

// AdminSocket is gvmd's socket. It is declared here, and named in no refusal
// an operator reads, because the rationale now lives in this comment and in the
// embedded `transport` help topic rather than in a transport error: that socket
// is 0660 root:gvm, so a CLI that dialled it would hand the whole root verb
// surface to every member of group gvm — the internet-facing web tier's own
// account among them — which is exactly the escalation the two-daemon split of
// section 3.2 exists to prevent. The root CLI therefore calls hostops
// in-process and never dials this path. TestTheCLINeverNamesTheAdminSocket
// keeps the literal to this one file: a `socket.Dial(AdminSocket)` cannot be
// written without first writing the string, and the test fails if it appears
// anywhere else.
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
// Built on avail.New so it matches avail.ErrUnavailable and exits 3. It stays a
// distinct sentinel from ErrNoTransport because the operator's next move
// differs completely: ErrNoTransport means "become root or set GVM_URL", this
// one means "no amount of configuration changes it in this build".
var ErrRemoteUnbuilt = avail.New("this gvm build has no API client; run it as root on the host")

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
		// There is no API client in this build, so a remote target cannot be
		// carried by any verb. Fail at once with the one honest sentence, rather
		// than resolving to ModeRemote and refusing a step later — and never
		// advise `gvm token create`, a command for a daemon this build does not
		// ship (U10). The admin.sock rationale is a code comment now (on the
		// AdminSocket const) and the embedded `transport` help topic, so no
		// refusal names a path that is not here.
		msg := "this gvm build has no API client; unset " + EnvURL +
			" / drop --remote and run it as root on the host"
		if (verb == "backup" || verb == "restore") && !strings.Contains(target, "://") {
			msg += ". If you meant the off-box backup target, that is spec.backup.remote " +
				"in the instance YAML, not --remote"
		}
		return Transport{}, fmt.Errorf("%w: %s", ErrRemoteUnbuilt, msg)
	}

	if env.Geteuid() == 0 {
		switch route {
		case RouteHostOp:
			return Transport{Mode: ModeLocalRoot, Verb: verb}, nil
		case RouteConsole:
			return Transport{Mode: ModeConsole, Verb: verb}, nil
		}
	}

	// Rule 4, reworded: the API client is not in this build, so the only fix
	// offered is becoming root. No --remote / GVM_URL / token advice is printed,
	// because every one of those names a path this host does not have (U10).
	switch route {
	case RouteConsole:
		return Transport{}, fmt.Errorf("%w: `gvm %s` needs the console socket, which is 0660 "+
			"inside a 2750 directory. Run it as root: sudo gvm %s", ErrNoTransport, verb, verb)
	default:
		return Transport{}, fmt.Errorf("%w: `gvm %s` is a host operation. Run it as root: "+
			"sudo gvm %s", ErrNoTransport, verb, verb)
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
	"version":    RouteLocal,
	"completion": RouteLocal,
	"help":       RouteLocal,
	"game":       RouteLocal,

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
	"mods":     RouteHostOp,
	"job":      RouteHostOp,
	"backup":   RouteHostOp,
	"restore":  RouteHostOp,
	"update":   RouteHostOp,
	"rebuild":  RouteHostOp,
	"adopt":    RouteHostOp,
	"firewall": RouteHostOp,
	"doctor":   RouteHostOp,
	"user":     RouteHostOp,
	"token":    RouteHostOp,
	"secret":   RouteHostOp,
	"audit":    RouteHostOp,
	"db":       RouteHostOp,
}

func routeOf(verb string) Route { return routes[verb] }
