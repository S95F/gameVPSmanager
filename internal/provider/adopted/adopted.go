// Package adopted is the provider for a game whose install gvm did not
// perform and will not perform.
//
// It exists so that an adopted instance HAS a provider -- so Inspect, Observe
// and the lifecycle's call graph are one path rather than one path plus a nil
// check at every site -- and not so that such an install can be reinstalled.
// Plan is always empty and always non-destructive, which makes "refuses
// app_update and validate at the type level" (section 10) a fact about the
// data rather than a promise: there is no field in an empty plan that could
// carry an app_update, and Apply refuses any plan that carries one.
//
// It is NOT the same thing as `managed_install: false`. That flag is on the
// INSTANCE and applies to every provider: an adopted Project Zomboid server
// runs the `steamcmd` driver, because the manifest describes the game while
// the flag describes who owns the files. This package is for a manifest whose
// game has no installer gvm can drive at all.
package adopted

import (
	"context"
	"fmt"

	"github.com/S95F/gameVPSmanager/internal/provider"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Provider is stateless. Everything it is allowed to see arrives in the Ctx.
type Provider struct{}

// New returns the driver.
func New() Provider { return Provider{} }

func (Provider) Kind() string { return spec.ProviderAdopted }

func init() { provider.Register(New()) }

// Inspect reports what is on disk without judging it.
//
// Present is "there is something in the install tree", not "this is
// installed": this driver has no sentinel and no record of what it wrote,
// because it wrote nothing. Claiming a sentinel verdict it cannot support
// would make an adopted install look either broken or verified, and it is
// neither -- it is somebody else's, and working.
//
// HandEdits and Unmanaged stay empty for the same reason. Every file here is
// unmanaged, and answering "all 48,000 of them" is noise that would bury the
// one case the fields exist to surface.
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
			Reason: "this install predates gvm: there is no record of what gvm wrote, so there " +
				"is nothing to compare the files against",
		},
	}, nil
}

// Plan is always empty and always non-destructive, for every intent.
//
// Including verify. A read-only integrity check still needs a record of the
// expected bytes, and this driver has none; reporting "verified" off the
// absence of evidence is the failure mode D-32 exists to prevent.
func (Provider) Plan(ctx context.Context, c *provider.Ctx, in provider.Intent) (*provider.Plan, error) {
	return provider.EmptyPlan(in, fmt.Sprintf(
		"%s is an adopted install: gvm never materialises, updates or validates its files. "+
			"That is what adoption means, and taking over its files is not available in this "+
			"build.", c.Name)), nil
}

// Apply does nothing, and refuses anything.
func (Provider) Apply(ctx context.Context, c *provider.Ctx, p *provider.Plan, pr provider.Progress) (*provider.Applied, error) {
	return provider.ApplyEmptyPlan(c, p, fmt.Sprintf(
		"the adopted driver cannot write: %s's files are not gvm's to replace", c.Name))
}

// Observe is the every-30s version. It reports no version, because it has no
// way to learn one that would not be a guess about somebody else's installer.
func (Provider) Observe(ctx context.Context, c *provider.Ctx) (*provider.Observation, error) {
	present, err := provider.Populated(c.Paths.Install)
	if err != nil {
		return nil, err
	}
	detail := c.Paths.Install.Path() + " holds files gvm did not install"
	if !present {
		detail = c.Paths.Install.Path() + " is empty, and this instance is adopted: gvm will " +
			"not fill it"
	}
	return &provider.Observation{
		Healthy: present,
		Detail:  detail,
		ReadAt:  c.Now(),
	}, nil
}
