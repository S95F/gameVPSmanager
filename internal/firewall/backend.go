package firewall

import "context"

// Backend applies a Plan to the host firewall.
//
// Implementors: ufw.Backend, nft.Backend (v1.1 stub), NoneBackend, FakeBackend.
type Backend interface {
	// Name returns a human-readable identifier, e.g. "ufw".
	Name() string

	// Available reports whether the backend is present and usable.
	// nft returns false with a reason until nftables support is implemented.
	Available() (bool, string)

	// Status returns the current live rule set and whether it is active.
	// The bool is false when the firewall is disabled.
	Status(ctx context.Context) (RuleSet, bool, error)

	// Apply executes the steps in the plan. It is called only by `gvm firewall apply`
	// and the reconciler; it must never be called speculatively.
	Apply(ctx context.Context, plan *Plan) error

	// Enable enables the firewall backend (ufw enable). Only called by
	// `gvm firewall enable` after Guard.Check passes.
	Enable(ctx context.Context) error

	// Disable disables the firewall backend. Only for `gvm firewall disable`.
	Disable(ctx context.Context) error
}

// NoneBackend is the no-op backend used when spec.firewall.backend = "none".
// It reports every plan without applying anything.
type NoneBackend struct{}

func (NoneBackend) Name() string                                    { return "none" }
func (NoneBackend) Available() (bool, string)                       { return true, "" }
func (NoneBackend) Status(_ context.Context) (RuleSet, bool, error) { return nil, false, nil }
func (NoneBackend) Apply(_ context.Context, _ *Plan) error          { return nil }
func (NoneBackend) Enable(_ context.Context) error                  { return nil }
func (NoneBackend) Disable(_ context.Context) error                 { return nil }
