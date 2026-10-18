//go:build !linux

package edgeops

import (
	"context"

	"github.com/S95F/gameVPSmanager/internal/platform"
)

// HostSystem refuses off Linux; the orchestration above it still compiles and
// tests everywhere through the System seam.
type HostSystem struct{}

func (HostSystem) ReadFile(string) ([]byte, error)        { return nil, platform.ErrNotSupported }
func (HostSystem) WriteFile(string, []byte, uint32) error { return platform.ErrNotSupported }
func (HostSystem) Rename(string, string) error            { return platform.ErrNotSupported }
func (HostSystem) Remove(string) error                    { return platform.ErrNotSupported }
func (HostSystem) ReloadCaddy(context.Context) error      { return platform.ErrNotSupported }
func (HostSystem) Validate(context.Context, string) (string, error) {
	return "", platform.ErrNotSupported
}
func (HostSystem) AdminGet(context.Context, string, string) ([]byte, error) {
	return nil, platform.ErrNotSupported
}
