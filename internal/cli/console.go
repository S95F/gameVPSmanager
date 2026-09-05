// `gvm console` — attach to a running instance's console socket.
//
// The one thing this command must never be ambiguous about is DETACHING.
// The operator this product is being built for has spent years in `screen -r
// pzserver`, where the way out is Ctrl-A D and Ctrl-C kills the server. Here
// the way out is Ctrl-D, and it leaves the server running. A user who believes
// detaching stopped their server does something worse next — reboots the box,
// or starts a second copy on the same world — so the banner says it on the way
// in and the notice says it again on the way out, both times naming the server
// and the command that would actually stop it.
//
// The client is line-oriented on purpose (section 11.4). The PTY belongs to the
// GAME and is shared by every viewer, so there is no resize op and no raw mode:
// a per-client TIOCSWINSZ on a shared terminal would reflow another operator's
// screen. Whole lines in, whole lines out, and `:` for the handful of commands
// that are addressed to gvm rather than to the game.

package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/socket"
	"github.com/S95F/gameVPSmanager/internal/version"
)

// DefaultBacklog is how much history a fresh attach prints. It is a screenful
// of context rather than the whole ring: an operator attaching to find out what
// just happened wants the last minute, and 20 000 lines scrolling past is the
// same as no lines at all.
const DefaultBacklog = 200

// consoleOptions is one `gvm console` invocation, fully parsed.
type consoleOptions struct {
	// Name is the instance, empty when --socket named a path directly. The
	// screen bridge takes that path (runner.ScreenCompat.Argv renders `gvm
	// console --socket <path> --no-color`), because it runs as the instance
	// user and must not depend on being able to read /etc/gvm.
	Name   string
	Socket string

	Backlog int
	Follow  bool
	// ANSI covers colour AND the in-place rewrite of a superseded line. They
	// are one flag because they are one question — may this client emit escape
	// sequences — and a caller piping the console into a file wants neither.
	ANSI bool
	// Times prefixes each line with its timestamp. Off by default: the game's
	// own output usually carries one already, and two timestamps per line push
	// the text that matters off the right of an 80-column terminal.
	Times bool
	// Actor is an attribution CLAIM. Empty is the normal case and the better
	// one: the socket resolves `root_cli` or `unix:<account>` from credentials
	// the kernel stamped, and a claim is honoured only for the tiers that
	// legitimately speak for someone else (section 11.4).
	Actor string
}

// parseConsoleArgs is separated from runConsole so the whole argument surface
// is tested on the development machine, where the socket it names cannot exist.
func parseConsoleArgs(env *Env, args []string) (consoleOptions, error) {
	var opt consoleOptions
	var noColor, noFollow bool

	fs := flag.NewFlagSet("gvm console", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, "Usage: gvm console <instance>\n"+
			"   or: gvm console --socket <path>\n\n"+
			"Ctrl-D or :detach leaves the console. The server keeps running.\n\n")
		fs.PrintDefaults()
	}
	fs.StringVar(&opt.Socket, "socket", "", "console socket path, instead of an instance name")
	fs.IntVar(&opt.Backlog, "backlog", DefaultBacklog, "lines of history to print before following")
	fs.BoolVar(&opt.Times, "timestamps", false, "prefix every line with its timestamp")
	fs.StringVar(&opt.Actor, "actor", "", "attribution recorded for lines you type (default: resolved from the connection)")
	fs.BoolVar(&noColor, "no-color", false, "emit no escape sequences at all")
	fs.BoolVar(&noFollow, "no-follow", false, "print the backlog and exit instead of attaching")

	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return consoleOptions{}, flagErr(err)
	}
	// The two negatives are turned into positives here, so nothing downstream
	// has to reason about `!noColor`.
	opt.ANSI, opt.Follow = !noColor, !noFollow

	switch {
	case len(pos) > 1:
		fs.Usage()
		return consoleOptions{}, fmt.Errorf("name one instance, not %d", len(pos))
	case len(pos) == 1 && opt.Socket != "":
		return consoleOptions{}, errors.New("name an instance or --socket, not both")
	case len(pos) == 1:
		opt.Name = pos[0]
		if err := instance.ValidateName(opt.Name); err != nil {
			return consoleOptions{}, err
		}
		opt.Socket = instance.PathsFor(opt.Name).ConsoleSock
	case opt.Socket == "":
		fs.Usage()
		return consoleOptions{}, errors.New("name an instance")
	}

	if opt.Backlog < 0 {
		return consoleOptions{}, fmt.Errorf("--backlog %d is negative", opt.Backlog)
	}
	if opt.Actor != "" {
		if _, ok := socket.SanitizeActor(opt.Actor); !ok {
			// The value is not echoed back: an actor claim carrying a CR would
			// reproduce the forged line one row below the message complaining
			// about it.
			return consoleOptions{}, errors.New("--actor is not a usable attribution string")
		}
	}
	return opt, nil
}

func runConsole(ctx context.Context, env *Env, args []string) error {
	opt, err := parseConsoleArgs(env, args)
	if err != nil {
		return err
	}
	if err := requireLinuxHost("gvm console"); err != nil {
		return err
	}
	c, err := socket.Dial(ctx, opt.Socket)
	if err != nil {
		return dialError(opt.Name, opt.Socket, err)
	}
	defer c.Close()
	return attachConsole(ctx, env, c, opt)
}

// clientID is what the hello frame carries. It names the binary and its build
// so a runner's journal can say which client sent a line.
func clientID() string { return "gvm-cli/" + version.Version }

// consoleUI renders one attached session.
//
// Everything it needs is a field and the transport is a *socket.Client rather
// than a path, so the whole session — banner, rendering, the local `:` commands
// and the detach notice — runs over a net.Pipe against the real server, in a
// test, on Windows.
type consoleUI struct {
	c    *socket.Client
	opt  consoleOptions
	in   io.Reader
	peer console.PeerInfo

	// mu guards the terminal. Two goroutines write to it -- the reader
	// rendering the game's output, and the input loop answering a `:` command
	// -- and an unsynchronised terminal is not merely a data race: half a game
	// line inside the detach notice is a message an operator reads as something
	// other than what happened. Everything with a Locked suffix assumes it is
	// already held.
	mu  sync.Mutex
	out io.Writer
	// lastSeq is the seq of the line printed most recently, so a superseded
	// line (Ring.Supersede republishes the SAME seq) is rewritten in place
	// instead of scrolling a near-identical copy past the operator.
	lastSeq  uint64
	openLine bool
}

func attachConsole(ctx context.Context, env *Env, c *socket.Client, opt consoleOptions) error {
	ui := &consoleUI{c: c, opt: opt, out: env.Stdout, in: env.Stdin}

	peer, err := c.Hello(clientID())
	if err != nil {
		return err
	}
	ui.peer = peer
	ui.banner()

	// The reader starts BEFORE the tail is asked for, and that ordering is not
	// cosmetic. A backlog of a few hundred long lines is bigger than a socket's
	// send buffer, so a client that wrote its next request while the server was
	// still writing history would have both ends blocked on a write nobody is
	// draining. Hello is safe to do synchronously above because it is one small
	// frame each way; everything after it is streamed.
	backlogDone := make(chan struct{})
	events := make(chan error, 1)
	go func() { events <- ui.readEvents(sync.OnceFunc(func() { close(backlogDone) })) }()

	if err := c.Tail(0, opt.Backlog, opt.Follow); err != nil {
		return ui.end(events, err)
	}
	// A ping is the end-of-backlog marker. The server answers ops in order on
	// one goroutine, so the pong cannot overtake the `lines` frames the tail
	// produced; without it a follower cannot tell "here is your history, now
	// you are live" from "the server has said nothing yet", and --no-follow
	// would have no idea when to stop reading.
	if err := c.Request(console.Request{Op: console.OpPing}); err != nil {
		return ui.end(events, err)
	}

	select {
	case <-backlogDone:
	case err := <-events:
		ui.serverGone(err)
		return nil
	case <-ctx.Done():
		err := ui.end(events, nil)
		ui.detached()
		return err
	}

	if !opt.Follow {
		return ui.end(events, nil)
	}
	ui.note("following. Ctrl-D or :detach to leave; the server keeps running.")
	return ui.follow(ctx, events)
}

// end closes the connection and waits for the reader, so no goroutine outlives
// the session holding a socket open.
func (u *consoleUI) end(events <-chan error, err error) error {
	u.c.Close()
	<-events
	u.mu.Lock()
	u.closeOpenLineLocked()
	u.mu.Unlock()
	return err
}

// follow runs the two loops an attached console is: events in, lines out.
//
// They are separate goroutines because the operator's stdin and the game's
// output are genuinely independent, and because socket.Client's contract is one
// reader and any number of writers. That is also why nothing in the input loop
// calls Send or Status, which read their own replies: while a follower is
// streaming, every request is fire-and-forget and its answer is rendered by the
// reader like any other event.
func (u *consoleUI) follow(ctx context.Context, events <-chan error) error {
	input := make(chan error, 1)
	go func() { input <- u.readInput() }()

	select {
	case err := <-events:
		// The server ended first. The input goroutine is left blocked on a read
		// that only process exit will unblock; that is deliberate, because the
		// alternative is a non-portable interruptible stdin for a command whose
		// next act is to return.
		u.serverGone(err)
		return nil
	case err := <-input:
		// The reader is stopped BEFORE the notice is printed. A game line
		// landing between "detached" and the command that would actually stop
		// the server is the one place a stray line changes what a message means.
		closeErr := u.end(events, err)
		u.detached()
		return closeErr
	case <-ctx.Done():
		// Ctrl-C is the other way an operator leaves, and it has to read
		// exactly like the intended one: in a screen session Ctrl-C killed the
		// server, and someone will press it here expecting the same.
		closeErr := u.end(events, nil)
		u.detached()
		return closeErr
	}
}

// readEvents renders every frame until the connection ends. onBacklogEnd is
// called once, on the pong that closes the history the attach asked for.
func (u *consoleUI) readEvents(onBacklogEnd func()) error {
	for {
		ev, err := u.c.Next()
		if err != nil {
			return err
		}
		if ev.Ev == console.EvPong && onBacklogEnd != nil {
			onBacklogEnd()
			onBacklogEnd = nil
			continue
		}
		u.render(ev)
	}
}

func (u *consoleUI) render(ev console.Event) {
	u.mu.Lock()
	defer u.mu.Unlock()
	switch ev.Ev {
	case console.EvLines:
		for _, l := range ev.Lines {
			u.lineLocked(l)
		}
	case console.EvLagged:
		// The count belongs to the batch, so this is the number of lines
		// dropped BEFORE the next one printed — not a running total the
		// operator has to subtract for themselves.
		u.noteLocked("%d lines dropped; this console could not keep up. Resuming at seq %d.",
			ev.Dropped, ev.ResumeFrom)
	case console.EvTruncated:
		u.noteLocked("history before seq %d has been evicted from the ring; "+
			"the whole of it is in console.log on the server.", ev.Oldest)
	case console.EvSent:
		// Nothing. The line is already on screen: the runner echoed it into the
		// ring as SourceActor, and a second confirmation would make every
		// command an operator types appear twice.
	case console.EvStatus:
		u.dumpJSONLocked("status", ev.Status)
	case console.EvCaps:
		u.dumpJSONLocked("caps", ev.Caps)
	case console.EvVerb:
		if out := strings.TrimRight(ev.Output, "\n"); out != "" {
			u.closeOpenLineLocked()
			for _, l := range strings.Split(out, "\n") {
				u.write(u.paint(colorDim, "   "+l) + "\n")
			}
		}
		u.noteLocked("verb ran on the %s channel.", ev.Channel)
	case console.EvShutdown:
		u.shutdown(ev)
	case console.EvError:
		u.noteLocked("refused (%s): %s", ev.Code, ev.Error)
	case console.EvPong:
	default:
		u.noteLocked("unrecognised event %q from the runner", string(ev.Ev))
	}
}

func (u *consoleUI) shutdown(ev console.Event) {
	switch ev.Phase {
	case console.PhaseStep:
		if ev.Send != "" {
			u.noteLocked("shutdown: %s rung, sending %q", ev.Rung, ev.Send)
			return
		}
		u.noteLocked("shutdown: %s rung", ev.Rung)
	case console.PhaseDone:
		u.noteLocked("the shutdown ladder has finished.")
		u.dumpJSONLocked("report", ev.Report)
	default:
		u.noteLocked("shutdown: %s", ev.Phase)
	}
}

// lineLocked prints one console line.
func (u *consoleUI) lineLocked(l console.Line) {
	var b strings.Builder

	// A repeated seq is Ring.Supersede republishing a line in place: a progress
	// bar, or an idle-flushed partial that has now completed. Rewriting the row
	// is the difference between one moving line and a hundred stationary ones.
	if u.openLine {
		if u.opt.ANSI && l.Seq == u.lastSeq {
			b.WriteString("\r\x1b[K")
		} else {
			b.WriteString("\n")
		}
	}
	if u.opt.Times {
		b.WriteString(u.paint(colorDim, l.At.Format(time.TimeOnly)+" "))
	}
	switch l.Source {
	case console.SourceRunner:
		b.WriteString(u.paint(colorYellow, l.Text))
	case console.SourceActor:
		// Marked with '>' like an injected line in the shutdown report, so the
		// live console and the post-mortem agree about who typed what.
		b.WriteString(u.paint(colorCyan, actorPrefix(l.Actor)+"> "+l.Text))
	default:
		b.WriteString(l.Text)
	}
	if !l.Partial {
		b.WriteString("\n")
	}
	u.write(b.String())
	u.lastSeq, u.openLine = l.Seq, l.Partial
}

// actorPrefix shortens an attribution to what fits in front of a line. The full
// string is in the ring, in console.log and in the audit row; the console wants
// the name, not the address.
func actorPrefix(actor string) string {
	if actor == "" {
		return ""
	}
	if _, rest, ok := strings.Cut(actor, ":"); ok && rest != "" {
		actor = rest
	}
	if at, _, ok := strings.Cut(actor, "@"); ok && at != "" {
		actor = at
	}
	return actor
}

// readInput is the operator's half. It returns nil on EOF, which is Ctrl-D,
// which is a detach.
func (u *consoleUI) readInput() error {
	sc := bufio.NewScanner(u.in)
	sc.Buffer(make([]byte, 0, 4096), console.MaxRequestFrame)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.HasPrefix(line, ":") {
			if done := u.local(line); done {
				return nil
			}
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !u.peer.Write {
			u.note("this connection may read the console but not write to it.")
			continue
		}
		if err := u.c.Request(console.Request{
			Op: console.OpSend, Line: line, Actor: u.opt.Actor,
		}); err != nil {
			return err
		}
	}
	// A scanner error is the terminal going away, which is also a detach.
	return sc.Err()
}

// local handles a `:` command and reports whether the session should end.
func (u *consoleUI) local(line string) bool {
	cmd, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
	switch cmd {
	case ":detach", ":quit", ":q", ":exit", ":bye":
		return true
	case ":help", ":?":
		u.help()
	case ":status":
		u.request(console.Request{Op: console.OpStatus})
	case ":caps":
		u.request(console.Request{Op: console.OpCaps})
	case ":stop":
		// This is the same confusion taken from the other side: someone who has
		// learnt that `:detach` leaves will guess `:stop` means "stop
		// watching". There is deliberately no console spelling of the shutdown
		// ladder, so the guess cannot cost a world.
		u.note(":stop is not a console command — it would have to stop the SERVER.")
		u.note("to leave, press Ctrl-D or type :detach. To stop %s: gvm stop %s",
			u.c.Instance(), u.c.Instance())
	case ":verb":
		u.verb(strings.TrimSpace(rest))
	default:
		u.note("unknown console command %q; :help lists them.", cmd)
	}
	return false
}

func (u *consoleUI) verb(rest string) {
	if !u.peer.Verb {
		u.note("this connection may not run verbs; they are routed through gvmd (D-09).")
		return
	}
	name, argstr, _ := strings.Cut(rest, " ")
	if name == "" {
		u.note("usage: :verb <name> [Msg=…] [Player=…] [Reason=…] [Duration=…]")
		return
	}
	args, err := parseVerbArgs(strings.Fields(argstr))
	if err != nil {
		u.note("%v", err)
		return
	}
	u.request(console.Request{Op: console.OpVerb, Verb: name, Args: args, Actor: u.opt.Actor})
}

func (u *consoleUI) request(r console.Request) {
	if err := u.c.Request(r); err != nil {
		u.note("%v", err)
	}
}

func (u *consoleUI) help() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.noteLocked("console commands:")
	for _, l := range []string{
		":detach, :quit   leave the console. The server keeps running.",
		":status          print the runner's status.json",
		":caps            print what this instance can currently be asked to do",
		":verb <name> …   run a manifest verb (root only)",
		":help            this list",
		"anything else    is sent to the game as a console line",
	} {
		u.write(u.paint(colorDim, "   "+l) + "\n")
	}
}

func (u *consoleUI) banner() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.closeOpenLineLocked()
	u.write(u.paint(colorBold, fmt.Sprintf("── %s ── attached\n", u.c.Instance())))
	u.write(u.paint(colorDim, "   Ctrl-D or :detach leaves the console. The server keeps running.\n"))
	if !u.peer.Write {
		u.write(u.paint(colorDim, "   read-only: this connection may not write to the console.\n"))
	}
}

// detached is the notice the whole command exists to make unmissable.
func (u *consoleUI) detached() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.closeOpenLineLocked()
	name := u.c.Instance()
	u.write(u.paint(colorBold, fmt.Sprintf("── detached from %s. The server is STILL RUNNING. ──\n", name)))
	u.write(u.paint(colorDim, fmt.Sprintf("   to stop it: gvm stop %s\n", name)))
}

// serverGone is the opposite notice, and it has to be equally plain: a console
// that simply ended looks exactly like a detach, and an operator who reads a
// crash as a detach will not go looking for the crash.
func (u *consoleUI) serverGone(err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.closeOpenLineLocked()
	name := u.c.Instance()
	u.write(u.paint(colorBold, fmt.Sprintf("── %s: the console ended. The runner is gone. ──\n", name)))
	switch {
	case err == nil, errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, net.ErrClosed):
	default:
		u.write(u.paint(colorDim, fmt.Sprintf("   %v\n", err)))
	}
	u.write(u.paint(colorDim, fmt.Sprintf("   what happened is in the journal and in console.log: gvm status %s\n", name)))
}

func (u *consoleUI) dumpJSONLocked(what string, raw json.RawMessage) {
	u.closeOpenLineLocked()
	if len(raw) == 0 {
		u.noteLocked("%s: the runner sent nothing", what)
		return
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "   ", "  "); err != nil {
		// Not worth failing an attach for: the bytes are the answer even when
		// they are not the shape this client expected.
		u.write("   " + string(raw) + "\n")
		return
	}
	u.write("   " + buf.String() + "\n")
}

// note is gvm speaking, never the game. The prefix is the one the runner's own
// annotations carry, so the two read as one voice in a transcript.
func (u *consoleUI) note(format string, a ...any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.noteLocked(format, a...)
}

func (u *consoleUI) noteLocked(format string, a ...any) {
	u.closeOpenLineLocked()
	u.write(u.paint(colorDim, "-- gvm: "+fmt.Sprintf(format, a...)) + "\n")
}

// closeOpenLineLocked terminates a partial line before gvm writes over it, so a
// note never lands in the middle of a sentence the game had not finished.
func (u *consoleUI) closeOpenLineLocked() {
	if u.openLine {
		u.write("\n")
		u.openLine = false
	}
}

func (u *consoleUI) write(s string) {
	// A console that cannot write to its own terminal has nothing left to do,
	// and reporting the failure would need the same broken writer.
	_, _ = io.WriteString(u.out, s)
}

func (u *consoleUI) paint(c string, s string) string { return paint(u.opt.ANSI, c, s) }

// The palette is four entries because four is what this UI distinguishes: the
// game (uncoloured, because it is the content), gvm's own notes, an operator's
// injected line, and the two banners. Anything more would be decoration on a
// screen whose job is to be read at 2am.
const (
	colorBold   = "\x1b[1m"
	colorDim    = "\x1b[2m"
	colorCyan   = "\x1b[36m"
	colorYellow = "\x1b[33m"
	colorReset  = "\x1b[0m"
)

func paint(on bool, c, s string) string {
	if !on || c == "" {
		return s
	}
	// The reset goes before the newline a caller appended, never after it: a
	// colour left open across a line break bleeds into whatever the game prints
	// next, and on a wrapped terminal into the scrollback above it.
	if nl := strings.HasSuffix(s, "\n"); nl {
		return c + strings.TrimSuffix(s, "\n") + colorReset + "\n"
	}
	return c + s + colorReset
}

// dialError turns a refused connect into a sentence an operator can act on.
// ENOENT on this path means one specific thing — the runner is not up — and
// printing the raw dial error instead leaves every reader to translate it.
func dialError(name, path string, err error) error {
	switch {
	case errors.Is(err, os.ErrNotExist):
		if name != "" {
			return fmt.Errorf("%s is not running: there is no console socket at %s", name, path)
		}
		return fmt.Errorf("no console socket at %s", path)
	case errors.Is(err, os.ErrPermission):
		return fmt.Errorf("%w: the console socket is 0660 inside a 2750 directory; "+
			"attach as root or as a member of %s", err, socket.ConsoleGroup)
	}
	return err
}
