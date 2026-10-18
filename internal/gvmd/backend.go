package gvmd

// The Backend seams. Each verb handler calls exactly one of these, so the
// daemon's orchestration is tested with recorders and no host, the same way
// hostinit tests its System.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/lifecycle"
)

// Dispatch is an acknowledged lifecycle dispatch: the unit action was
// accepted by systemd (not necessarily finished — progress is observable
// through status and the console stream).
type Dispatch struct {
	OpID string
}

// ErrLeaseHeld is returned by a Lifecycle implementation when a job holds the
// instance. Its text is the lease line shown to the operator.
type ErrLeaseHeld struct{ Line string }

func (e *ErrLeaseHeld) Error() string { return "lease held: " + e.Line }

// ErrTOTPNotEnrolled is returned by VerifyTOTP for an account with no secret.
var ErrTOTPNotEnrolled = errors.New("gvmd: totp not enrolled")

// ErrDegraded is returned when Postgres is unreachable or the schema is
// behind the binary; auth verbs are refused while it holds (rule 8).
var ErrDegraded = errors.New("gvmd: datastore unavailable")

// Backend is everything the verb handlers may do. Every field must be set;
// NewServer refuses a partial backend so a verb can never dangle.
type Backend struct {
	// Statuses sweeps every instance, or one when instance != "".
	Statuses func(ctx context.Context, instance string) ([]lifecycle.Status, error)

	// Start, Stop and Restart dispatch the unit action for one instance,
	// refusing with *ErrLeaseHeld while a job holds it.
	Start   func(ctx context.Context, instance string) error
	Stop    func(ctx context.Context, instance string) error
	Restart func(ctx context.Context, instance string) error

	// ConsoleSend writes one line to the instance's console socket.
	ConsoleSend func(ctx context.Context, instance, line string) error

	// BackupNow starts the instance's backup job unit.
	BackupNow func(ctx context.Context, instance string) error

	// VerifyTOTP checks a code for an email: ErrTOTPNotEnrolled,
	// identity.ErrInvalidTOTP, ErrDegraded or nil.
	VerifyTOTP func(ctx context.Context, email, code string) error
}

func (b Backend) validate() error {
	missing := []string{}
	if b.Statuses == nil {
		missing = append(missing, "Statuses")
	}
	if b.Start == nil {
		missing = append(missing, "Start")
	}
	if b.Stop == nil {
		missing = append(missing, "Stop")
	}
	if b.Restart == nil {
		missing = append(missing, "Restart")
	}
	if b.ConsoleSend == nil {
		missing = append(missing, "ConsoleSend")
	}
	if b.BackupNow == nil {
		missing = append(missing, "BackupNow")
	}
	if b.VerifyTOTP == nil {
		missing = append(missing, "VerifyTOTP")
	}
	if len(missing) > 0 {
		return fmt.Errorf("gvmd: backend is missing %s", strings.Join(missing, ", "))
	}
	return nil
}
