// Package none is the provider for an exec-only game: one whose binaries are
// already on the box and whose manifest declares no installer.
//
// The difference from `adopted` is who owns the files and why gvm leaves them
// alone. An adopted install is a running world that predates gvm and must not
// be touched. A `none` install is a managed instance of a game that simply has
// no install step gvm can drive -- an operator-supplied build, a vendored
// server jar, a binary delivered by the distribution -- so there is nothing to
// fetch, and `gvm install` has nothing to do rather than being forbidden from
// doing it.
//
// Both therefore plan nothing, and both say so differently, because the
// operator's next move differs: promote the instance, or put the files there.
package none

import (
	"context"
	"fmt"

	"github.com/S95F/gameVPSmanager/internal/provider"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Provider is stateless.
type Provider struct{}

// New returns the driver.
func New() Provider { return Provider{} }

func (Provider) Kind() string { return spec.ProviderNone }

func init() { provider.Register(New()) }

// Inspect reports whether the operator's files are there.
//
// A manifest with no provider block may still declare a sentinel's shape in
// future; today there is none to read, so the verdict is "cannot tell" rather
// than "installed", which is what SentinelResult.OK says for an undeclared
// sentinel. Presence of SOME file is reported separately and is not the same
// claim.
func (Provider) Inspect(ctx context.Context, c *provider.Ctx) (*provider.Inspection, error) {
	present, err := provider.Populated(c.Paths.Install)
	if err != nil {
		return nil, err
	}
	return &provider.Inspection{
		Present:  present,
		Sentinel: provider.SentinelResult{Declared: false},
		Integrity: provider.IntegrityResult{
			Checked: false,
			Reason: "this game declares no installer, so gvm has no manifest of expected " +
				"bytes to check the tree against",
		},
	}, nil
}

// Plan is always empty: there is nothing to fetch.
func (Provider) Plan(ctx context.Context, c *provider.Ctx, in provider.Intent) (*provider.Plan, error) {
	return provider.EmptyPlan(in, fmt.Sprintf(
		"%s declares provider kind none: it has no installer, so there is nothing for gvm to "+
			"fetch. The binaries under %s are the operator's to place and to update.",
		c.Name, c.Paths.Install.Path())), nil
}

// Apply does nothing, and refuses anything.
func (Provider) Apply(ctx context.Context, c *provider.Ctx, p *provider.Plan, pr provider.Progress) (*provider.Applied, error) {
	return provider.ApplyEmptyPlan(c, p, fmt.Sprintf(
		"provider kind none has no way to write: nothing in %s describes where these bytes "+
			"would come from", c.Name))
}

// Observe is the every-30s version.
func (Provider) Observe(ctx context.Context, c *provider.Ctx) (*provider.Observation, error) {
	present, err := provider.Populated(c.Paths.Install)
	if err != nil {
		return nil, err
	}
	detail := c.Paths.Install.Path() + " holds the operator-supplied binaries"
	if !present {
		detail = c.Paths.Install.Path() + " is empty, and this game has no installer: put the " +
			"server files there"
	}
	return &provider.Observation{
		Healthy: present,
		Detail:  detail,
		ReadAt:  c.Now(),
	}, nil
}
