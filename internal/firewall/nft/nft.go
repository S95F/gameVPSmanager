// Package nft is a v1.1 stub. nftables support is not yet implemented;
// Available() returns false with a reason explaining the upgrade path.
package nft

import (
	"context"
	"errors"

	"github.com/S95F/gameVPSmanager/internal/firewall"
)

// ErrUnsupported is returned by any operation on the nft backend until
// nftables support is implemented.
var ErrUnsupported = errors.New("nft: nftables backend is not yet implemented; use backend: ufw or backend: none")

// Backend is the nft firewall backend stub.
type Backend struct{}

func (Backend) Name() string              { return "nft" }
func (Backend) Available() (bool, string) { return false, ErrUnsupported.Error() }

func (Backend) Status(_ context.Context) (firewall.RuleSet, bool, error) {
	return nil, false, ErrUnsupported
}

func (Backend) Apply(_ context.Context, _ *firewall.Plan) error {
	return ErrUnsupported
}

func (Backend) Enable(_ context.Context) error {
	return ErrUnsupported
}

func (Backend) Disable(_ context.Context) error {
	return ErrUnsupported
}
