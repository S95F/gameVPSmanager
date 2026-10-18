package webstore

// The live console feed: gvm-web sits DIRECTLY on each instance's console
// socket (D-09) — reads never go through gvmd, which is what keeps a hundred
// watching browsers off the root daemon. Writes (the send box) do go through
// gvmd's console.send verb, where they are role-checked and attributed.

import (
	"context"
	"errors"
	"fmt"

	"github.com/S95F/gameVPSmanager/internal/console"
	"github.com/S95F/gameVPSmanager/internal/gvmd"
	"github.com/S95F/gameVPSmanager/internal/instance"
	"github.com/S95F/gameVPSmanager/internal/socket"
	"github.com/S95F/gameVPSmanager/internal/web"
)

// Console implements web.ConsoleStreamer over the instances' own sockets.
type Console struct{}

// Stream dials the instance's console, replays the backlog (or resumes from
// `from`), and follows until ctx ends.
func (Console) Stream(ctx context.Context, instanceName string, from uint64, backlog int, emit func(console.Event)) error {
	paths := instance.PathsFor(instanceName)
	client, err := socket.Dial(ctx, paths.ConsoleSock)
	if err != nil {
		// No socket is a stopped server, not a failure: the page banners it.
		return fmt.Errorf("%w: %v", web.ErrConsoleDown, err)
	}
	defer client.Close()

	if _, err := client.Hello("gvm-web"); err != nil {
		return fmt.Errorf("%w: %v", web.ErrConsoleDown, err)
	}
	if err := client.Tail(from, backlog, true); err != nil {
		return fmt.Errorf("webstore: console tail %s: %w", instanceName, err)
	}

	// A follow legitimately blocks in Next for as long as the pane is open;
	// closing the client is how ctx cancellation unblocks it.
	go func() {
		<-ctx.Done()
		_ = client.Close()
	}()

	for {
		ev, err := client.Next()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// The runner going away mid-follow is the server stopping.
			return fmt.Errorf("%w: %v", web.ErrConsoleDown, err)
		}
		emit(ev)
	}
}

// ConsoleSend forwards one validated line through gvmd's console.send verb.
func (d Dispatcher) ConsoleSend(ctx context.Context, name, line, actor string) error {
	err := d.Client.ConsoleSend(ctx, name, line, actor)
	if errors.Is(err, gvmd.ErrGatewayDown) {
		return web.ErrControlUnavailable
	}
	return err
}
