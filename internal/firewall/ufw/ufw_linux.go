package ufw

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/firewall"
)

// Backend is the live ufw firewall backend on Linux.
type Backend struct{}

func (Backend) Name() string { return "ufw" }

func (Backend) Available() (bool, string) {
	_, err := exec.LookPath("ufw")
	if err != nil {
		return false, "ufw not found in PATH"
	}
	return true, ""
}

func (b Backend) Status(ctx context.Context) (firewall.RuleSet, bool, error) {
	out, err := runUFW(ctx, "status", "numbered")
	if err != nil {
		return nil, false, err
	}
	res := Parse(bytes.NewReader(out))
	return res.Rules, res.Active, nil
}

func (b Backend) Apply(ctx context.Context, plan *firewall.Plan) error {
	for _, step := range plan.Steps {
		switch step.Kind {
		case firewall.StepAdd, firewall.StepAdopt:
			// For adoption: ufw re-adds the rule with the new comment.
			args := strings.Fields(step.Rule.UFWSpec())
			if _, err := runUFW(ctx, args...); err != nil {
				return fmt.Errorf("ufw add %v: %w", args, err)
			}
		case firewall.StepDelete:
			// ufw delete requires the rule spec, not a line number, to avoid
			// races with concurrent rule additions shifting indices.
			args := append([]string{"delete"}, strings.Fields(step.Rule.UFWSpec())...)
			if _, err := runUFW(ctx, args...); err != nil {
				return fmt.Errorf("ufw delete %v: %w", args, err)
			}
		case firewall.StepShadow:
			// Shadow is a warning — no action taken.
		}
	}
	return nil
}

func (Backend) Enable(ctx context.Context) error {
	_, err := runUFW(ctx, "--force", "enable")
	return err
}

func (Backend) Disable(ctx context.Context) error {
	_, err := runUFW(ctx, "--force", "disable")
	return err
}

func runUFW(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ufw", args...)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return out, fmt.Errorf("ufw %v: exit %d: %s", args, exitErr.ExitCode(), bytes.TrimSpace(exitErr.Stderr))
		}
		return out, fmt.Errorf("ufw %v: %w", args, err)
	}
	return out, nil
}
