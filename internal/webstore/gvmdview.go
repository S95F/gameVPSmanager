package webstore

// The gvmd-backed halves of the dashboard: instance pages come from the
// status verb (filesystem + systemd truth, never Postgres — rule 1), and the
// lifecycle buttons dispatch through the gateway (item 5).

import (
	"context"
	"errors"
	"fmt"

	"github.com/S95F/gameVPSmanager/internal/gvmd"
	"github.com/S95F/gameVPSmanager/internal/web"
)

// Instances implements web.InstanceQuerier over gvmd's status verb.
type Instances struct {
	Client *gvmd.Client
}

// ListInstances backs the instance list page.
func (q Instances) ListInstances(ctx context.Context) ([]web.InstanceSummary, error) {
	sts, err := q.Client.Statuses(ctx, "")
	if err != nil {
		return nil, viewErr(err)
	}
	out := make([]web.InstanceSummary, 0, len(sts))
	for _, st := range sts {
		out = append(out, web.InstanceSummary{
			Name:   st.Name,
			Game:   st.Game,
			Status: web.DashboardStatusText(st.Rule),
		})
	}
	return out, nil
}

// GetInstance backs the detail and console pages.
func (q Instances) GetInstance(ctx context.Context, name string) (*web.InstanceDetail, error) {
	sts, err := q.Client.Statuses(ctx, name)
	if err != nil {
		return nil, viewErr(err)
	}
	if len(sts) == 0 {
		return nil, fmt.Errorf("webstore: no instance %q", name)
	}
	st := sts[0]
	return &web.InstanceDetail{
		Name:   st.Name,
		Game:   st.Game,
		Status: web.DashboardStatusText(st.Rule),
	}, nil
}

// viewErr maps gateway failures onto the error web's handlers read as "serve
// the degraded page"; a not_found refusal stays an ordinary error (→ 404).
func viewErr(err error) error {
	var remote *gvmd.RemoteError
	switch {
	case errors.Is(err, gvmd.ErrGatewayDown):
		return fmt.Errorf("%w: %v", web.ErrControlUnavailable, err)
	case errors.As(err, &remote) && remote.Code == gvmd.CodeNotFound:
		return err
	case errors.As(err, &remote) && remote.Code == gvmd.CodeDegraded:
		return fmt.Errorf("%w: %v", web.ErrControlUnavailable, err)
	default:
		return err
	}
}

// Dispatcher implements web.Dispatcher over gvmd's lifecycle verbs.
type Dispatcher struct {
	Client *gvmd.Client
}

func (d Dispatcher) Start(ctx context.Context, name, actor string) (string, error) {
	return d.lifecycle(ctx, gvmd.VerbStart, name, actor)
}

func (d Dispatcher) Stop(ctx context.Context, name, actor string) (string, error) {
	return d.lifecycle(ctx, gvmd.VerbStop, name, actor)
}

func (d Dispatcher) Restart(ctx context.Context, name, actor string) (string, error) {
	return d.lifecycle(ctx, gvmd.VerbRestart, name, actor)
}

func (d Dispatcher) lifecycle(ctx context.Context, verb gvmd.Verb, name, actor string) (string, error) {
	opID, err := d.Client.Lifecycle(ctx, verb, name, actor)
	if err == nil {
		return opID, nil
	}
	var remote *gvmd.RemoteError
	switch {
	case errors.As(err, &remote) && remote.Code == gvmd.CodeLeaseHeld:
		return "", &web.LeaseHeldError{Line: remote.Lease}
	case errors.Is(err, gvmd.ErrGatewayDown):
		return "", web.ErrControlUnavailable
	default:
		return "", err
	}
}
