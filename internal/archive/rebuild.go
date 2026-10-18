package archive

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/spec"
)

// Rebuilding an instance on a new box from one of its archives, and the part of
// `gvm create --from-archive` that decides whether the file in front of it is
// safe to rebuild from -- before any instance exists.
//
// The archive came off another box: possibly over the wire, possibly from a NAS
// somebody else can write to. So its contents are UNTRUSTED input, validated
// the same way a manifest from a text editor is, and the two refusals that
// matter most happen here, with nothing written to the filesystem and no game
// registry consulted.
//
// gvm never decrypts. An `encrypt_to` archive is an age file whose private half
// lives on the operator's workstation and never on a VPS gvm runs on (section
// 13.2's pin-only, identity-off-the-box rule). So an encrypted archive is a
// REFUSAL carrying the one command that moves it forward -- `age -d` on the
// workstation -- rather than a prompt for an identity gvm would then have to
// hold. Content verification itself (every member's digest) is a separate,
// explicit step, `gvm backup verify --file` (Phase 38), which the move-host
// drill runs before this; what this does is validate the archive's own
// description of itself, which is what a rebuild reads.

// ErrEncrypted is a rebuild source that is still age-encrypted. It is its own
// sentinel because the operator's next move is not an edit or a retry but a
// decrypt on a different machine, and the refusal carries the command for it.
var ErrEncrypted = errors.New("archive: the file is age-encrypted and gvm does not decrypt it")

// age's published format markers. The binary header is the first line of a
// v1 file; the armor header begins the PEM-style ASCII form. Either one, or the
// .age extension gvm's own pushes use, is enough to refuse: a rebuild must never
// try to gunzip an encrypted blob and report the gzip error as if the archive
// were corrupt.
const (
	ageBinaryHeader = "age-encryption.org/v1"
	ageArmorHeader  = "-----BEGIN AGE ENCRYPTED FILE-----"
	ageExt          = ".age"
)

// RebuildSource is a decrypted, self-consistent archive ready to rebuild an
// instance from. Instance is the redacted spec the archive carries, parsed; it
// is nil only for an archive that carries no spec_yaml at all.
type RebuildSource struct {
	Path     string
	Manifest *Manifest
	Instance *spec.Instance
}

// InspectRebuildSource validates an archive handed to `gvm create --from-archive`
// as untrusted input, before any instance exists.
//
// It writes nothing and consults no registry. It refuses an age-encrypted file
// with the decrypt command, refuses anything that is not a gvm archive (each
// with the package's own specific sentinel), re-validates the head manifest the
// way every reader does, and parses the embedded instance spec so a corrupt one
// is caught here rather than part-way through a rebuild. The ledger gate, the
// game-manifest template guard, the port-occupancy check, the adopted-path
// normalisation and the copy-into-the-backup-dir are the rebuild body that runs
// after this, in the command that owns the host and the registry.
func InspectRebuildSource(path string) (*RebuildSource, error) {
	enc, err := looksEncrypted(path)
	if err != nil {
		return nil, err
	}
	if enc {
		return nil, fmt.Errorf("%w: decrypt it first: age -d -i <identity> %s > %s "+
			"(see docs/backups-and-restore.md)", ErrEncrypted, path, decryptedName(path))
	}

	m, err := PeekManifestFile(path)
	if err != nil {
		// ErrNoManifest, ErrCorrupt and ErrHeadTooLarge are each specific about
		// what the file is and what to do; a rebuild has nothing to add to them.
		return nil, err
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}

	var in *spec.Instance
	if strings.TrimSpace(m.SpecYAML) != "" {
		parsed, _, perr := spec.ParseInstance(path, []byte(m.SpecYAML))
		if perr != nil {
			return nil, fmt.Errorf("%w: the instance spec the archive carries does not parse: %w",
				ErrCorrupt, perr)
		}
		in = parsed
	}
	return &RebuildSource{Path: path, Manifest: m, Instance: in}, nil
}

// decryptedName is what the refusal tells the operator to decrypt the file to:
// the name with age's extension removed, or a ".decrypted" suffix for a file
// that was encrypted but not named .age.
func decryptedName(path string) string {
	if strings.HasSuffix(strings.ToLower(path), ageExt) {
		return path[:len(path)-len(ageExt)]
	}
	return path + ".decrypted"
}

// looksEncrypted reports whether a rebuild source is an age file, by its name or
// by its header. The name alone is enough -- a .age file is not something to try
// to open and gunzip -- and the header catches one that was renamed to look
// like a plain archive.
func looksEncrypted(path string) (bool, error) {
	if strings.HasSuffix(strings.ToLower(path), ageExt) {
		return true, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	head := make([]byte, 64)
	n, _ := io.ReadFull(f, head)
	s := string(head[:n])
	return strings.HasPrefix(s, ageBinaryHeader) || strings.HasPrefix(s, ageArmorHeader), nil
}
