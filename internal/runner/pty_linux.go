//go:build linux

package runner

import (
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// openPTY allocates a pty pair and configures the slave's line discipline for
// a game server.
//
// Both ends are opened O_NOCTTY. Without it, a process that is a session
// leader with no controlling terminal acquires the first tty it opens as its
// ctty -- so the RUNNER would end up owning the game's terminal, and a hangup
// on it would deliver SIGHUP to the supervisor instead of to the game.
func openPTY(rows, cols uint16) (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("runner: open /dev/ptmx: %w", err)
	}
	defer func() {
		if err != nil {
			master.Close()
		}
	}()

	// TIOCSPTLCK(0) unlocks the slave; it is locked at allocation precisely so
	// that nothing can open it in the window before the master is configured.
	if err = withFD(master, func(fd int) error {
		return unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0)
	}); err != nil {
		return nil, nil, fmt.Errorf("runner: unlock pty: %w", err)
	}

	var n int
	if err = withFD(master, func(fd int) error {
		var e error
		n, e = unix.IoctlGetInt(fd, unix.TIOCGPTN)
		return e
	}); err != nil {
		return nil, nil, fmt.Errorf("runner: pty number: %w", err)
	}

	name := "/dev/pts/" + strconv.Itoa(n)
	slave, err = os.OpenFile(name, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("runner: open %s: %w", name, err)
	}
	if err = configureTTY(slave, rows, cols); err != nil {
		slave.Close()
		return nil, nil, err
	}
	return master, slave, nil
}

// configureTTY sets the line discipline the game will see.
//
// Each flag here is a failure that has happened to somebody:
//
//   - ECHO and friends OFF. The tty echoes what is written to the master back
//     out of it, so with echo on, every command an operator types appears
//     TWICE in the console: once as the attributed actor echo the runner
//     appends itself, and once as if the game had printed it. The second copy
//     is unattributed, which makes the console log lie about who typed what.
//   - ISIG OFF. With it on, a 0x03 byte anywhere in an injected line sends
//     SIGINT to the game. Exactly one function in gvm is permitted to signal
//     the game, and it is the shutdown ladder; a stray control byte in a
//     broadcast message must not be able to stop a server.
//   - IXON/IXOFF OFF. A 0x13 (XOFF) byte would suspend the game's output
//     until an XON arrived, and a game whose stdout is blocked eventually
//     blocks in write(2) and stops saving.
//   - ICANON and ICRNL ON. Canonical mode is what makes the slave deliver
//     whole lines, which is what a game's stdin reader expects, and ICRNL is
//     what makes a manifest's `line_ending: cr` still terminate one.
//   - OPOST OFF. Output post-processing would insert a CR before every LF the
//     game writes, so the raw log -- the complete record the ring is allowed
//     to evict from -- would contain bytes the game never produced.
func configureTTY(slave *os.File, rows, cols uint16) error {
	return withFD(slave, func(fd int) error {
		t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
		if err != nil {
			return fmt.Errorf("runner: tcgetattr: %w", err)
		}
		t.Lflag &^= unix.ECHO | unix.ECHOE | unix.ECHOK | unix.ECHONL | unix.ECHOCTL | unix.ECHOPRT | unix.ECHOKE
		t.Lflag &^= unix.ISIG
		t.Lflag |= unix.ICANON
		t.Iflag &^= unix.IXON | unix.IXOFF | unix.IXANY
		t.Iflag |= unix.ICRNL
		t.Oflag &^= unix.OPOST
		if err := unix.IoctlSetTermios(fd, unix.TCSETS, t); err != nil {
			return fmt.Errorf("runner: tcsetattr: %w", err)
		}
		// The window size is set once, at spawn, and never renegotiated. The
		// pty is the game's terminal and every viewer shares it, so honouring
		// one client's resize would reflow a console somebody else is reading
		// -- and some games format their output to the width they saw at
		// startup, which would then be wrong for everyone.
		ws := &unix.Winsize{Row: rows, Col: cols}
		if err := unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, ws); err != nil {
			return fmt.Errorf("runner: set window size: %w", err)
		}
		return nil
	})
}

// withFD runs fn with the file's descriptor, keeping the file registered with
// the runtime poller.
//
// os.File.Fd() would be shorter and is wrong here: it puts the descriptor into
// blocking mode and removes it from the poller for good, so a later Close
// could no longer interrupt a blocked read on the pty master.
func withFD(f *os.File, fn func(fd int) error) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var inner error
	if err := rc.Control(func(fd uintptr) { inner = fn(int(fd)) }); err != nil {
		return err
	}
	return inner
}
