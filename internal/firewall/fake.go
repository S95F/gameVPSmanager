package firewall

import "context"

// FakeBackend is a test double. It records Apply calls and serves a fixed
// Status. Tests can inspect Applied and drive lockout scenarios.
type FakeBackend struct {
	Live    RuleSet
	Active  bool
	Applied []*Plan
	Enabled bool

	ApplyErr  error
	EnableErr error
}

func (f *FakeBackend) Name() string              { return "fake" }
func (f *FakeBackend) Available() (bool, string) { return true, "" }

func (f *FakeBackend) Status(_ context.Context) (RuleSet, bool, error) {
	return f.Live, f.Active, nil
}

func (f *FakeBackend) Apply(_ context.Context, p *Plan) error {
	if f.ApplyErr != nil {
		return f.ApplyErr
	}
	f.Applied = append(f.Applied, p)
	return nil
}

func (f *FakeBackend) Enable(_ context.Context) error {
	if f.EnableErr != nil {
		return f.EnableErr
	}
	f.Enabled = true
	return nil
}

func (f *FakeBackend) Disable(_ context.Context) error {
	f.Enabled = false
	return nil
}
