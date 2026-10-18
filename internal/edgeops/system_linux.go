//go:build linux

package edgeops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// HostSystem is the real thing. Every program it runs is named by absolute
// path with argv — no shell, ever (rule 2).
type HostSystem struct{}

func (HostSystem) ReadFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotExist
	}
	return b, err
}

func (HostSystem) WriteFile(path string, data []byte, mode uint32) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".gvm-edge-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), os.FileMode(mode)); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (HostSystem) Rename(oldPath, newPath string) error { return os.Rename(oldPath, newPath) }

func (HostSystem) Remove(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Validate runs caddy's own linter AS CADDY: as root it would create
// /root/.local/share/caddy and a stray ACME account (§17.2).
func (HostSystem) Validate(ctx context.Context, configPath string) (string, error) {
	cmd := exec.CommandContext(ctx, "/usr/sbin/runuser", "-u", "caddy", "--",
		"/usr/bin/caddy", "validate", "--config", configPath, "--adapter", "caddyfile")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (HostSystem) ReloadCaddy(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "/usr/bin/systemctl", "reload", "caddy")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl reload caddy: %v: %s", err, out)
	}
	return nil
}

// AdminGet is the READ-ONLY admin-API client (D-36: the socket, never
// localhost:2019; gvm never writes through it).
func (HostSystem) AdminGet(ctx context.Context, socketPath, requestPath string) ([]byte, error) {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: 5 * time.Second,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://caddy-admin"+requestPath, nil)
	if err != nil {
		return nil, err
	}
	// The rendered global block sets `enforce_origin`; over the unix socket
	// the allowed origin is the socket's own address.
	req.Host = socketPath
	req.Header.Set("Origin", socketPath)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("admin API %s: %s: %.200s", requestPath, resp.Status, body)
	}
	return body, nil
}
