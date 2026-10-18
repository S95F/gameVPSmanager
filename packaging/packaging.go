// Package packaging holds the unit files, slices and tmpfiles gvm installs.
//
// They are embedded rather than merely shipped, for two reasons that both
// matter more than the twenty kilobytes.
//
// The first is that they can then be TESTED. A systemd unit is configuration
// in the sense that nothing checks it until the night it is wrong: a
// TimeoutStartSec on the wrong unit, a timer pointing at a service that is not
// installed, a concrete template instance that silently dropped half the
// hardening it was meant to inherit. packaging_test.go reads these files and
// asserts the things sections 7.5 and 13.4 promise, so a packaging mistake
// fails `go test` on the development machine rather than at 05:03 on a VPS.
//
// The second is that `gvm doctor` will need to compare what is installed on a
// host against what this build expects, and a doctor that read the files from
// /usr/lib would be comparing the host with itself.
package packaging

import "embed"

// FS is every file this build installs, at the path it is installed under
// within its destination directory.
//
// systemd's files go to /usr/lib/systemd/system, the tmpfiles fragment to
// /usr/lib/tmpfiles.d and the sysusers fragment to /usr/lib/sysusers.d; the
// mapping is the installer's business (internal/hostinit's one table) and
// deliberately not encoded here, because a second place that knows where a
// unit belongs is a second place to be wrong about it.
//
//go:embed systemd tmpfiles sysusers.d
var FS embed.FS
