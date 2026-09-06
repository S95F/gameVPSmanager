//go:build !linux

package reconcile

import "context"

// CheckCalendar off Linux reports that it did not ask, and does not pretend.
//
// The pure half still runs, so a schedule with a newline in it is refused on
// the development machine exactly as it is on the target -- that check is
// about unit-file syntax and is not systemd's to make. The grammar itself is
// systemd's, there is no systemd here, and answering "valid" would move a
// broken nightly from `gvm apply` on the box to the first night it did not
// fire.
func (SystemdCalendar) CheckCalendar(_ context.Context, expr string) error {
	if err := ValidateCalendar(expr); err != nil {
		return err
	}
	return ErrCalendarUnchecked
}
