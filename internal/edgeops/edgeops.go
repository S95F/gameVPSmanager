// Package edgeops applies the rendered Caddy configuration: render →
// validate → stage → swap → reload → verify, with rollback on failure
// (ARCHITECTURE §17.2). It owns only spec.caddy.config_path — gvm never
// writes /etc/caddy/Caddyfile (D-37) and never writes through the admin API
// (the file is the single source of truth).
//
// The host effects live behind System, so the whole sequence — including both
// rollback paths — is driven and tested with a recorder and no Caddy.
package edgeops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/caddyconf"
	"github.com/S95F/gameVPSmanager/internal/edge"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// System is the set of host effects Apply performs.
type System interface {
	// ReadFile returns a file's bytes; a missing file is (nil, ErrNotExist).
	ReadFile(path string) ([]byte, error)
	// WriteFile writes atomically (temp + rename in the same directory).
	WriteFile(path string, data []byte, mode uint32) error
	// Rename moves new over old atomically.
	Rename(oldPath, newPath string) error
	// Remove deletes a staged file; missing is not an error.
	Remove(path string) error
	// Validate runs `caddy validate` on the staged file AS THE CADDY USER —
	// as root it would create /root/.local/share/caddy and a stray ACME
	// account (§17.2). The returned output is caddy's own words.
	Validate(ctx context.Context, configPath string) (output string, err error)
	// ReloadCaddy is `systemctl reload caddy`: graceful, so live SSE streams
	// ride stream_close_delay instead of dying.
	ReloadCaddy(ctx context.Context) error
	// AdminGet performs a read-only GET over Caddy's admin unix socket.
	// gvm never writes through the admin API.
	AdminGet(ctx context.Context, socketPath, requestPath string) ([]byte, error)
}

// ErrNotExist is what ReadFile returns for a missing file.
var ErrNotExist = errors.New("edgeops: file does not exist")

// Result is what one Apply did.
type Result struct {
	Changed    bool
	RolledBack bool
	// Log is the step-by-step account, printed verbatim to the operator.
	Log []string
}

// verifyWindow bounds the post-reload config check (§17.2 step 6: hard).
const verifyWindow = 10 * time.Second

// Apply renders plan and converges the live Caddy on it.
func Apply(ctx context.Context, sys System, host *spec.HostConfig, plan *edge.Plan) (Result, error) {
	var res Result
	say := func(format string, args ...any) {
		res.Log = append(res.Log, fmt.Sprintf(format, args...))
	}

	switch plan.Mode {
	case "caddy_snippet":
		return applySnippet(sys, host, plan)
	case "caddy_managed":
		// fall through
	default:
		return res, fmt.Errorf("edgeops: edge.mode %s has no Caddy config to apply", plan.Mode)
	}

	configPath := host.Spec.Edge.Caddy.ConfigPath
	adminSock := strings.TrimPrefix(host.Spec.Edge.Caddy.Admin, "unix/")

	var rendered bytes.Buffer
	if err := caddyconf.Render(&rendered, host, plan); err != nil {
		return res, err
	}

	current, err := sys.ReadFile(configPath)
	if err != nil && !errors.Is(err, ErrNotExist) {
		return res, fmt.Errorf("edgeops: read %s: %w", configPath, err)
	}
	hadCurrent := err == nil
	if hadCurrent && bytes.Equal(current, rendered.Bytes()) {
		say("%s already matches the plan; nothing to do", configPath)
		return res, nil
	}

	// 1–2. Stage next to the live file (same filesystem, so the rename is
	// atomic) and validate the STAGED file, as caddy.
	staged := configPath + ".new"
	if err := sys.WriteFile(staged, rendered.Bytes(), 0o640); err != nil {
		return res, fmt.Errorf("edgeops: stage %s: %w", staged, err)
	}
	say("staged   %s", staged)
	if out, err := sys.Validate(ctx, staged); err != nil {
		_ = sys.Remove(staged)
		say("validate FAILED; nothing changed")
		return res, fmt.Errorf("edgeops: the staged config does not validate; the live config is untouched:\n%s", out)
	}
	say("validate ok (as caddy)")

	// 3–4. Keep a rollback copy, then swap.
	backup := configPath + ".bak"
	if hadCurrent {
		if err := sys.WriteFile(backup, current, 0o640); err != nil {
			_ = sys.Remove(staged)
			return res, fmt.Errorf("edgeops: write %s: %w", backup, err)
		}
		say("backup   %s", backup)
	}
	if err := sys.Rename(staged, configPath); err != nil {
		_ = sys.Remove(staged)
		return res, fmt.Errorf("edgeops: swap %s: %w", configPath, err)
	}
	res.Changed = true
	say("swapped  %s", configPath)

	rollback := func(why string) {
		if !hadCurrent {
			say("rollback: no previous config existed; %s is left for inspection", configPath)
			return
		}
		if err := sys.Rename(backup, configPath); err != nil {
			say("rollback FAILED restoring %s: %v", backup, err)
			return
		}
		if err := sys.ReloadCaddy(ctx); err != nil {
			say("rollback reload FAILED: %v", err)
			return
		}
		res.RolledBack = true
		say("rolled back: the previous config is live again (%s)", why)
	}

	// 5. Reload.
	if err := sys.ReloadCaddy(ctx); err != nil {
		rollback("reload failed")
		return res, fmt.Errorf("edgeops: systemctl reload caddy: %w", err)
	}
	say("reload   caddy")

	// 6. Verify the config LOADED (hard): poll the read-only admin API for a
	// non-empty http servers map.
	if err := verifyLoaded(ctx, sys, adminSock); err != nil {
		rollback("the reloaded config did not come up")
		return res, fmt.Errorf("edgeops: %w", err)
	}
	say("verify   config live (admin API reports http servers)")

	// Step 8, certificate status, is deliberately NOT here: rolling back a
	// good config because ACME is slow would be strictly worse than waiting
	// (§17.2). `gvm edge plan` and the browser tell that story.
	return res, nil
}

// applySnippet writes the site block for a Caddy the operator owns: no
// validate (their binary, their modules), no reload, no caddy.service touch —
// the one import line is theirs to add (§17.3).
func applySnippet(sys System, host *spec.HostConfig, plan *edge.Plan) (Result, error) {
	var res Result
	say := func(format string, args ...any) {
		res.Log = append(res.Log, fmt.Sprintf(format, args...))
	}
	var rendered bytes.Buffer
	if err := caddyconf.Render(&rendered, host, plan); err != nil {
		return res, err
	}
	path := SnippetPath
	current, err := sys.ReadFile(path)
	if err == nil && bytes.Equal(current, rendered.Bytes()) {
		say("%s already matches the plan; nothing to do", path)
		return res, nil
	}
	if err := sys.WriteFile(path, rendered.Bytes(), 0o640); err != nil {
		return res, fmt.Errorf("edgeops: write %s: %w", path, err)
	}
	res.Changed = true
	say("wrote    %s", path)
	say("import it from your Caddyfile:  import %s", path)
	say("then reload your Caddy yourself; gvm never touches caddy.service in snippet mode")
	return res, nil
}

// SnippetPath is where caddy_snippet mode renders the site block (§17.3).
const SnippetPath = "/etc/gvm/caddy/gvm.caddyfile"

// verifyLoaded polls GET /config/apps/http until it reports servers.
func verifyLoaded(ctx context.Context, sys System, adminSock string) error {
	deadline := time.Now().Add(verifyWindow)
	var last error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		body, err := sys.AdminGet(ctx, adminSock, "/config/apps/http")
		if err == nil && bytes.Contains(body, []byte(`"servers"`)) {
			return nil
		}
		if err == nil {
			last = fmt.Errorf("admin API answered with no http servers: %.200s", body)
		} else {
			last = err
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("the reloaded config did not report http servers within %s: %w", verifyWindow, last)
}

// ── CA export (§17.4's bare-IP path) ─────────────────────────────────────────

// ExportCA reads the internal CA's PUBLIC certificate over the admin socket
// and writes its PEM to w. The private key never leaves Caddy: anything
// smelling of one is refused outright.
func ExportCA(ctx context.Context, sys System, adminSock string, w io.Writer) error {
	body, err := sys.AdminGet(ctx, adminSock, "/pki/ca/local")
	if err != nil {
		return fmt.Errorf("edgeops: read the internal CA: %w (is Caddy running with tls internal?)", err)
	}
	pem := extractPEM(body)
	if pem == "" {
		return fmt.Errorf("edgeops: the admin API returned no certificate: %.200s", body)
	}
	if strings.Contains(pem, "PRIVATE KEY") {
		return errors.New("edgeops: refusing to export: the response carries key material")
	}
	_, err = io.WriteString(w, pem)
	return err
}

// extractPEM pulls the root certificate PEM out of the /pki/ca/local JSON
// ({"root_certificate":"-----BEGIN CERTIFICATE-----\n..."}), tolerating the
// raw PEM itself for older servers.
func extractPEM(body []byte) string {
	s := string(body)
	if strings.HasPrefix(strings.TrimSpace(s), "-----BEGIN CERTIFICATE-----") {
		return strings.TrimSpace(s) + "\n"
	}
	const key = `"root_certificate":"`
	i := strings.Index(s, key)
	if i < 0 {
		return ""
	}
	rest := s[i+len(key):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	pem := strings.ReplaceAll(rest[:j], `\n`, "\n")
	if !strings.HasPrefix(pem, "-----BEGIN CERTIFICATE-----") {
		return ""
	}
	if !strings.HasSuffix(pem, "\n") {
		pem += "\n"
	}
	return pem
}
