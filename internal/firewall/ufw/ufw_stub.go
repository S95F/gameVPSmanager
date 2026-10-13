//go:build !linux

package ufw

import (
	"context"
	"errors"

	"github.com/S95F/gameVPSmanager/internal/firewall"
)

var errNotLinux = errors.New("ufw backend requires Linux")

// Backend is a no-op stub on non-Linux platforms.
type Backend struct{}

func (Backend) Name() string              { return "ufw" }
func (Backend) Available() (bool, string) { return false, errNotLinux.Error() }

func (Backend) Status(_ context.Context) (firewall.RuleSet, bool, error) {
	return nil, false, errNotLinux
}

func (Backend) Apply(_ context.Context, _ *firewall.Plan) error {
	return errNotLinux
}

func (Backend) Enable(_ context.Context) error {
	return errNotLinux
}

func (Backend) Disable(_ context.Context) error {
	return errNotLinux
}
