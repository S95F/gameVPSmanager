package runner

import (
	"io"

	"github.com/S95F/gameVPSmanager/internal/console"
)

// pumpBufSize is one read from the pty master. 32 KiB is comfortably larger
// than a pty's kernel buffer, so a busy console is drained in one syscall
// rather than four -- and a pump that is not draining is a game that is
// blocked in write(2), which is a game that has stopped saving.
const pumpBufSize = 32 << 10

// pump drains src into the ingest until the stream ends.
//
// The loop order is the whole trap. The bytes from a read are handed to the
// ingest BEFORE the error is examined, because the last read of a dying
// process usually returns data AND the error together: `n > 0, err = EIO` is
// the crash tail and the hangup arriving in one call. Checking err first
// throws away the thirty lines that say why the server died.
func pump(src io.Reader, in *console.Ingest) error {
	return pumpWith(src, in, endOfStream)
}

// pumpWith is pump with an injectable end-of-stream predicate, so the loop's
// behaviour on a hangup is asserted on the development machine where the
// hangup in question (EIO from a pty master) cannot be produced.
func pumpWith(src io.Reader, in *console.Ingest, eos func(error) bool) error {
	buf := make([]byte, pumpBufSize)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			// Ingest.Write always reports the whole buffer consumed and never
			// returns a log error; a failing log file is read back through
			// RawErr and reported in status.json, because a broken log must
			// not stop the console.
			_, _ = in.Write(buf[:n])
		}
		if err != nil {
			if eos(err) {
				// Flush publishes whatever the child wrote without a trailing
				// newline. A crash message is very often exactly that.
				in.Flush()
				return nil
			}
			return err
		}
	}
}
