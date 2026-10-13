//go:build !linux

package sshport

// ListenersFor is not available off Linux.
func (osEnv) ListenersFor([]string) ([]int, error) { return nil, nil }

// ReadSocketUnitPorts is not available off Linux.
func (osEnv) ReadSocketUnitPorts() ([]int, error) { return nil, nil }
