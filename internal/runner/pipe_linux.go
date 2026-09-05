//go:build linux

package runner

import (
	"errors"
	"fmt"
	"os"
)

// childPipes is console mode `pipe`: explicit os.Pipe pairs for games that
// misbehave on a TTY (section 11.1).
//
// The pairs are created here rather than with cmd.StdoutPipe() for one
// concrete reason: os/exec closes the descriptors it created itself inside
// Wait, and it starts a copier goroutine for them. Both are fatal here. The
// close races the pump, and the copier means Wait cannot return until the
// copy finishes -- which is how the naive `go io.Copy(ring, stdout);
// cmd.Wait()` truncates the crash tail of a process that died with output
// still in flight.
type childPipes struct {
	// out is the parent's read end; outW is the child's stdout AND stderr.
	// One pipe for both, because the console is one interleaved stream and a
	// game that writes its stack trace to stderr must have it land between the
	// stdout lines it belongs with.
	out  *os.File
	outW *os.File

	// in is the parent's write end; inR is the child's stdin.
	in  *os.File
	inR *os.File
}

func openPipes() (*childPipes, error) {
	out, outW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("runner: output pipe: %w", err)
	}
	inR, in, err := os.Pipe()
	if err != nil {
		out.Close()
		outW.Close()
		return nil, fmt.Errorf("runner: stdin pipe: %w", err)
	}
	return &childPipes{out: out, outW: outW, in: in, inR: inR}, nil
}

// closeChildEnds drops the parent's copies of the descriptors the child now
// owns. It MUST be called immediately after a successful start.
//
// While the parent still holds outW, the output pipe has a writer that never
// writes, so the read end never sees EOF: the pump would sit there forever
// after the game exited, and the runner would never learn the game was gone.
func (p *childPipes) closeChildEnds() error {
	err := errors.Join(p.outW.Close(), p.inR.Close())
	p.outW, p.inR = nil, nil
	return err
}

// closeParentEnds releases what the runner kept. The child's stdin write end
// is closed here and NOWHERE else: closing it hands the game EOF on stdin,
// which is how a supervisor kills a server by accident (section 11.1's verdict
// on the FIFO model).
func (p *childPipes) closeParentEnds() error {
	var errs []error
	if p.out != nil {
		errs = append(errs, p.out.Close())
		p.out = nil
	}
	if p.in != nil {
		errs = append(errs, p.in.Close())
		p.in = nil
	}
	return errors.Join(errs...)
}

// closeAll releases every descriptor, for a failed start.
func (p *childPipes) closeAll() error {
	var errs []error
	if p.outW != nil {
		errs = append(errs, p.outW.Close())
		p.outW = nil
	}
	if p.inR != nil {
		errs = append(errs, p.inR.Close())
		p.inR = nil
	}
	errs = append(errs, p.closeParentEnds())
	return errors.Join(errs...)
}
