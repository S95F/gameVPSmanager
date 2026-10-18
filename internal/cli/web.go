// `gvm web` — the dashboard and API (Phase 48 item 5).
//
// Runs as User=gvm (gvm-web.service): no master key, no host access, Postgres
// as gvm_web only, with everything privileged reached through gvmd's closed
// verb set over the admin socket. The default listener is the unix socket the
// proxy fronts (D-06); edge.listen puts it on a loopback TCP port instead —
// e.g. behind a serverGitUpdater-managed Caddy proxying to 127.0.0.1:<port>.

package cli

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/S95F/gameVPSmanager/internal/gvmd"
	"github.com/S95F/gameVPSmanager/internal/spec"
	"github.com/S95F/gameVPSmanager/internal/store"
	"github.com/S95F/gameVPSmanager/internal/web"
	"github.com/S95F/gameVPSmanager/internal/webstore"
)

// webDSN is the peer-auth DSN for the web tier's own Postgres role.
const webDSN = "postgres://gvm_web@/gvm?host=/var/run/postgresql"

func runWeb(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("gvm web", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	configPath := fs.String("config", "/etc/gvm/gvm.yaml", "host config with the edge settings")
	listen := fs.String("listen", "", "listener override: a unix socket path or a loopback host:port")
	dsn := fs.String("dsn", "", "postgres DSN (default: unix socket peer auth, gvm_web)")
	gvmdSock := fs.String("gvmd", AdminSocket, "gvmd's admin socket")
	dev := fs.Bool("dev", false, "development over plain http://localhost: drop the Secure cookie attribute")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}

	data, err := os.ReadFile(*configPath)
	if err != nil {
		return fmt.Errorf("web: read %s: %w", *configPath, err)
	}
	host, diags := spec.LoadHost(*configPath, data)
	if diags.HasErrors() {
		return fmt.Errorf("web: %s does not validate:\n%s", *configPath, diags.String())
	}

	addr := *listen
	if addr == "" {
		addr = host.Spec.Edge.Listen
	}
	if addr == "" {
		addr = host.Spec.Edge.Socket.Path
	}
	// A TCP listener must be loopback (rule 7 / item 7): the proxy is the one
	// public face, and a dashboard on 0.0.0.0 would bypass its TLS entirely.
	if !strings.HasPrefix(addr, "/") {
		if err := requireLoopback(addr); err != nil {
			return fmt.Errorf("web: %w", err)
		}
	}

	webDSNv := *dsn
	if webDSNv == "" {
		webDSNv = webDSN
	}
	st, err := store.Open(ctx, webDSNv)
	if err != nil {
		return fmt.Errorf("web: open store: %w", err)
	}
	defer st.Close()

	client := &gvmd.Client{Path: *gvmdSock}
	ws := webstore.New(st, client)

	srv, err := web.New(web.Config{
		Addr:            addr,
		SessionQuerier:  ws,
		TokenQuerier:    ws,
		Identity:        ws,
		Authorizer:      ws,
		InstanceQuerier: webstore.Instances{Client: client},
		Dispatcher:      webstore.Dispatcher{Client: client},
		Console:         webstore.Console{},
		TrustedOrigins:  host.Spec.Edge.TrustedOrigins,
		InsecureCookies: *dev,
	})
	if err != nil {
		return err
	}
	if *dev {
		fmt.Fprintln(env.Stderr, "web: --dev drops the Secure cookie attribute; never run a reachable dashboard this way")
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(env.Stdout, "gvm web: serving %s\n", addr)
	return srv.ListenAndServe(ctx)
}

// requireLoopback refuses a TCP dashboard listener that is not loopback.
func requireLoopback(addr string) error {
	hostPart, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listen %q is neither a unix socket path nor host:port: %v", addr, err)
	}
	ip := net.ParseIP(hostPart)
	if hostPart == "localhost" || (ip != nil && ip.IsLoopback()) {
		return nil
	}
	return fmt.Errorf("listen %q is not loopback; the dashboard is reached through the reverse proxy, "+
		"never directly (set edge.listen to 127.0.0.1:<port>, or use the unix socket)", addr)
}
