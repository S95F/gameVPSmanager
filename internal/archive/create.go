package archive

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/pgzip"

	"github.com/S95F/gameVPSmanager/internal/version"
)

// DefaultLevel is pgzip's compression level for a world archive.
//
// Level 5 rather than 6 because on a 4-vCPU VPS it cuts a 2.4 GB Project
// Zomboid archive from roughly four minutes to seventy seconds for a few
// percent of size, and that number IS the adoption downtime window (13.1).
const DefaultLevel = 5

// ArchiveDirMode is /var/backups/gvm/<name> (D-44).
//
// install.sh chowns the backup directory to the game user; that is the one
// thing in it deliberately not ported. A compromised mod must not be able to
// read every world the server has ever had.
const ArchiveDirMode fs.FileMode = 0o700

// ArchiveFileMode matches the directory's intent, and matches backup.sh's own
// `chmod 600` on the tarball.
const ArchiveFileMode fs.FileMode = 0o600

// Source is one base directory and the member-path prefix its contents are
// stored under.
//
// Include, IncludeOptional and Exclude are relative to Base, slash-separated.
// They are NOT rendered templates: the caller (the backup job) renders the
// manifest's backup.include against the instance's paths and hands the results
// down already relative, so this package has no template grammar in it and can
// be tested against a temp directory on any platform.
type Source struct {
	// Prefix is the member-path prefix, "data/" style, ending in "/". Empty
	// means members are stored at the top level, which only a
	// single-root archive should do.
	Prefix string
	// Base is the absolute directory the relative paths are resolved against.
	Base string
	// Include lists paths that must exist. A directory is taken recursively.
	// Empty Include and IncludeOptional means the whole of Base.
	Include []string
	// IncludeOptional lists paths whose absence is a note, not an error
	// (section 8.2's include_optional).
	IncludeOptional []string
	// Exclude removes paths. A pattern ending in "/**" drops everything under
	// a directory but keeps the directory; any other pattern is matched with
	// path.Match against the whole relative path and against each of its
	// ancestor directories, so "steamapps/workshop" drops the subtree.
	Exclude []string
}

// Options is everything Create needs. Nothing here is read from a database:
// an archive must be producible on a box whose Postgres is down.
type Options struct {
	// Dir is the destination directory, normally /var/backups/gvm/<name>.
	Dir string
	// Instance, Class and Reason become both the filename and the manifest.
	Instance string
	Class    Class
	Reason   string

	Sources []Source

	Creator     Creator
	Subject     Subject
	SpecYAML    string
	ContentLock json.RawMessage
	Fingerprint *Fingerprint

	SecretsIncluded bool
	SecretsRedacted []string

	// Level is the pgzip level; zero means DefaultLevel.
	Level int
	// FileMode is the archive's mode; zero means ArchiveFileMode.
	FileMode fs.FileMode
	// Now is the injectable clock.
	Now func() time.Time
	// OnNote receives non-fatal observations: a missing optional include, a
	// skipped socket. They belong in the job log, not in an error.
	OnNote func(string)
}

// Create writes one archive and returns an UNVERIFIED receipt.
//
// VerifiedAt is deliberately zero: the bytes have been written but not read
// back, and section 13.2 makes verification a separate, explicit step that
// runs before every destructive action. A Create that returned a verified Ref
// would be asserting something it has not checked, which is the exact failure
// the type exists to prevent. Callers that want both in one call use
// CreateAndVerify.
//
// The write is two-pass over the source tree. Pass one stats and hashes; pass
// two writes the tar and re-hashes as the bytes go by, comparing. The second
// hash is nearly free (the data is being read anyway) and it turns the one
// race this design has -- a file changing between the digest and the write,
// which the freeze lease of D-31 normally prevents -- from a silently corrupt
// archive into a named error.
func Create(ctx context.Context, o Options) (ref *Ref, err error) {
	now := clockOf(o.Now)
	createdAt := now().UTC().Truncate(time.Second)

	if err := o.validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(o.Dir, ArchiveDirMode); err != nil {
		return nil, fmt.Errorf("archive: create %s: %w", o.Dir, err)
	}

	staged, err := scanSources(ctx, o)
	if err != nil {
		return nil, err
	}

	members := make([]Member, len(staged))
	for i := range staged {
		members[i] = staged[i].Member
	}
	table, err := json.MarshalIndent(memberFile{
		Schema:  SchemaVersion,
		Count:   len(members),
		Members: members,
	}, "", " ")
	if err != nil {
		return nil, fmt.Errorf("archive: marshal member table: %w", err)
	}
	table = append(table, '\n')

	man := &Manifest{
		Schema:     SchemaVersion,
		GVMVersion: version.Version,
		GVMCommit:  version.Commit,
		CreatedAt:  createdAt,
		CreatedBy:  o.Creator,
		Instance:   o.Subject,
		Class:      o.Class,
		Reason:     o.Reason,
		SpecYAML:   o.SpecYAML,
		// The path SET is stripped here on purpose: 8412 relative paths do
		// not fit in the head budget, and the check that needs them
		// (Fingerprint.Contains) is a live pre/post comparison during a
		// cutover, never a read-back from an archive.
		WorldFingerprint:        o.Fingerprint.summary(),
		ContentLock:             o.ContentLock,
		Roots:                   rootsOf(o.Sources),
		MemberTable:             &MemberTable{Path: MembersMember, Count: len(members), Bytes: int64(len(table)), SHA256: hexSHA256(table)},
		SecretsIncluded:         o.SecretsIncluded,
		SecretsRedactedFromSpec: o.SecretsRedacted,
	}
	if man.Instance.Name == "" {
		man.Instance.Name = o.Instance
	}
	if err := man.Validate(); err != nil {
		return nil, err
	}
	head, err := man.marshalHead()
	if err != nil {
		return nil, err
	}

	final := filepath.Join(o.Dir, Filename(o.Instance, o.Class, createdAt))
	// The dot prefix keeps a temp file that outlives a crash out of every
	// *.tar.gz glob retention, the index and the operator use, so a partial
	// archive can never be mistaken for a backup. Retention would otherwise
	// count it towards keep:N and prune a good one to make room.
	tmpf, err := os.CreateTemp(o.Dir, "."+filepath.Base(final)+".tmp*")
	if err != nil {
		return nil, fmt.Errorf("archive: create temp in %s: %w", o.Dir, err)
	}
	tmp := tmpf.Name()
	defer func() {
		if err != nil {
			tmpf.Close()
			os.Remove(tmp)
		}
	}()

	whole := sha256.New()
	written, err := writeArchive(ctx, io.MultiWriter(tmpf, whole), head, table, staged, o.level(), createdAt)
	if err != nil {
		return nil, err
	}
	if err = tmpf.Sync(); err != nil {
		return nil, fmt.Errorf("archive: fsync %s: %w", tmp, err)
	}
	if err = tmpf.Close(); err != nil {
		return nil, fmt.Errorf("archive: close %s: %w", tmp, err)
	}
	if err = os.Chmod(tmp, o.fileMode()); err != nil {
		return nil, fmt.Errorf("archive: chmod %s: %w", tmp, err)
	}

	// The 8 KB promise is asserted here, against the file that was actually
	// produced, rather than assumed from the head budget. If the compressor
	// were ever to stop flushing after the first member, DB-free restore
	// would break silently and only at 2am; this makes it break at create.
	if err = assertPeekable(tmp, createdAt, o.Instance); err != nil {
		return nil, err
	}

	if err = promoteNew(tmp, final, o); err != nil {
		return nil, err
	}
	syncDir(o.Dir)

	return &Ref{
		Path:      final,
		SHA256:    hex.EncodeToString(whole.Sum(nil)),
		Bytes:     written,
		Instance:  o.Instance,
		Class:     o.Class,
		Reason:    o.Reason,
		CreatedAt: createdAt,
	}, nil
}

// CreateAndVerify writes an archive and reads it back, returning a receipt
// that may authorize a destroy.
//
// It exists so the ordering of section 13.6 -- archive, verify, only then
// delete -- is one call rather than a sequence a caller can get half right.
func CreateAndVerify(ctx context.Context, o Options, v VerifyOptions) (*Ref, *Report, error) {
	ref, err := Create(ctx, o)
	if err != nil {
		return nil, nil, err
	}
	if v.Now == nil {
		v.Now = o.Now
	}
	rep, err := VerifyRef(ctx, ref, v)
	if err != nil {
		return ref, rep, err
	}
	return ref, rep, nil
}

func (o Options) level() int {
	if o.Level == 0 {
		return DefaultLevel
	}
	return o.Level
}

func (o Options) fileMode() fs.FileMode {
	if o.FileMode == 0 {
		return ArchiveFileMode
	}
	return o.FileMode
}

func (o Options) note(format string, args ...any) {
	if o.OnNote != nil {
		o.OnNote(fmt.Sprintf(format, args...))
	}
}

func (o Options) validate() error {
	var errs []error
	if strings.TrimSpace(o.Instance) == "" {
		errs = append(errs, errors.New("archive: no instance name; an archive that cannot say "+
			"what it is a backup of is not a backup"))
	}
	if !o.Class.Valid() {
		errs = append(errs, fmt.Errorf("archive: class %q is neither %q nor %q", o.Class, ClassRoutine, ClassPreserve))
	}
	if strings.TrimSpace(o.Reason) == "" {
		errs = append(errs, errors.New("archive: no reason; the audit log and the manifest both "+
			"record why this archive exists, and \"unknown\" is not an answer at 2am"))
	}
	if strings.TrimSpace(o.Dir) == "" {
		errs = append(errs, errors.New("archive: no destination directory"))
	}
	if len(o.Sources) == 0 {
		errs = append(errs, errors.New("archive: no sources; an empty archive would satisfy "+
			"archive-before-destroy while protecting nothing"))
	}
	seen := map[string]bool{}
	for i, s := range o.Sources {
		if s.Base == "" || !filepath.IsAbs(s.Base) {
			errs = append(errs, fmt.Errorf("archive: source %d base %q is not absolute", i, s.Base))
		}
		if s.Prefix != "" && !strings.HasSuffix(s.Prefix, "/") {
			errs = append(errs, fmt.Errorf("archive: source %d prefix %q must end in \"/\"", i, s.Prefix))
		}
		if strings.HasPrefix(s.Prefix, MetaPrefix) {
			errs = append(errs, fmt.Errorf("archive: source %d prefix %q is inside the reserved "+
				"%s namespace", i, s.Prefix, MetaPrefix))
		}
		if seen[s.Prefix] {
			errs = append(errs, fmt.Errorf("archive: prefix %q is used by two sources, so a "+
				"member path would not say which base it came from", s.Prefix))
		}
		seen[s.Prefix] = true
	}
	if len(o.Sources) > 1 && seen[""] {
		errs = append(errs, errors.New("archive: a multi-root archive cannot have an unprefixed "+
			"source; restore would not know where its members belong"))
	}
	return errors.Join(errs...)
}

func rootsOf(sources []Source) []Root {
	out := make([]Root, 0, len(sources))
	for _, s := range sources {
		out = append(out, Root{Prefix: s.Prefix, Base: filepath.ToSlash(s.Base)})
	}
	return out
}

// staging pairs a manifest member with the file on disk it came from.
type staging struct {
	Member
	abs string
}

// scanSources is pass one: walk, stat and hash.
func scanSources(ctx context.Context, o Options) ([]staging, error) {
	var out []staging
	seen := map[string]bool{}

	for _, s := range o.Sources {
		found, err := scanSource(ctx, o, s, seen)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	if len(out) == 0 {
		return nil, errors.New("archive: the sources matched no files; a backup that archives " +
			"nothing must fail loudly rather than leave a valid, empty tarball behind")
	}
	// Lexical order puts every parent directory before its children (a string
	// is always less than itself plus anything), which is what makes a
	// sequential extraction able to create directories before it needs them.
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func scanSource(ctx context.Context, o Options, s Source, seen map[string]bool) ([]staging, error) {
	var out []staging

	type entry struct {
		rel      string
		optional bool
	}
	var roots []entry
	for _, r := range s.Include {
		roots = append(roots, entry{rel: cleanRel(r)})
	}
	for _, r := range s.IncludeOptional {
		roots = append(roots, entry{rel: cleanRel(r), optional: true})
	}
	if len(roots) == 0 {
		roots = append(roots, entry{rel: ""})
	}

	add := func(rel string, fi fs.FileInfo, link string) error {
		if rel == "." {
			rel = ""
		}
		member := s.Prefix + rel
		if rel == "" {
			// The base directory itself has no member: its mode belongs to
			// the destination, which restore must not silently reassign.
			return nil
		}
		if seen[member] {
			return nil
		}
		if strings.HasPrefix(member, MetaPrefix) {
			return fmt.Errorf("archive: %s would be stored as %q, inside the reserved %s "+
				"namespace; a source file cannot be allowed to impersonate the manifest",
				filepath.Join(s.Base, filepath.FromSlash(rel)), member, MetaPrefix)
		}
		st, err := stageOne(ctx, member, filepath.Join(s.Base, filepath.FromSlash(rel)), fi, link)
		if err != nil {
			return err
		}
		if st == nil {
			o.note("skipped %s: not a regular file, directory or symlink", member)
			return nil
		}
		seen[member] = true
		out = append(out, *st)
		return nil
	}

	for _, r := range roots {
		abs := s.Base
		if r.rel != "" {
			abs = filepath.Join(s.Base, filepath.FromSlash(r.rel))
		}
		fi, err := os.Lstat(abs)
		if errors.Is(err, fs.ErrNotExist) {
			if r.optional {
				o.note("optional include %q is absent under %s", r.rel, s.Base)
				continue
			}
			return nil, fmt.Errorf("archive: include %q does not exist under %s", r.rel, s.Base)
		}
		if err != nil {
			return nil, fmt.Errorf("archive: stat %s: %w", abs, err)
		}
		// Ancestor directories of an include root are stored so their modes
		// survive the round trip; tar -C $DATA_DIR "Saves/Multiplayer/x"
		// carried them implicitly and lost their permissions doing it.
		for _, anc := range ancestors(r.rel) {
			if excluded(s.Exclude, anc) {
				continue
			}
			afi, err := os.Lstat(filepath.Join(s.Base, filepath.FromSlash(anc)))
			if err != nil {
				continue
			}
			if err := add(anc, afi, ""); err != nil {
				return nil, err
			}
		}
		if !fi.IsDir() {
			if excluded(s.Exclude, r.rel) {
				continue
			}
			if err := add(r.rel, fi, ""); err != nil {
				return nil, err
			}
			continue
		}
		walkRoot := abs
		err = filepath.WalkDir(walkRoot, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return fmt.Errorf("archive: walk %s: %w", p, err)
			}
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			rel, rerr := filepath.Rel(s.Base, p)
			if rerr != nil {
				return fmt.Errorf("archive: relativise %s: %w", p, rerr)
			}
			slash := filepath.ToSlash(rel)
			if slash == "." {
				slash = ""
			}
			if slash != "" && excluded(s.Exclude, slash) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			fi, ferr := d.Info()
			if ferr != nil {
				return fmt.Errorf("archive: stat %s: %w", p, ferr)
			}
			link := ""
			if fi.Mode()&fs.ModeSymlink != 0 {
				if link, ferr = os.Readlink(p); ferr != nil {
					return fmt.Errorf("archive: readlink %s: %w", p, ferr)
				}
			}
			return add(slash, fi, link)
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// stageOne stats and hashes a single entry. A nil result means "not a kind of
// file an archive can carry", which is a note rather than an error: a unix
// socket in a game's data directory is normal and is not data.
func stageOne(ctx context.Context, member, abs string, fi fs.FileInfo, link string) (*staging, error) {
	uid, gid := fileOwner(fi)
	m := Member{
		Path:  member,
		Mode:  octalMode(uint32(fi.Mode().Perm())),
		UID:   uid,
		GID:   gid,
		MTime: fi.ModTime().UTC().Truncate(time.Second),
	}
	switch {
	case fi.IsDir():
		m.Kind = KindDir
	case fi.Mode()&fs.ModeSymlink != 0:
		m.Kind = KindSymlink
		m.Link = filepath.ToSlash(link)
	case fi.Mode().IsRegular():
		m.Kind = KindFile
		m.Size = fi.Size()
		sum, err := hashFile(ctx, abs)
		if err != nil {
			return nil, err
		}
		m.SHA256 = sum
	default:
		return nil, nil
	}
	return &staging{Member: m, abs: abs}, nil
}

// writeArchive is pass two. It returns the number of compressed bytes written.
func writeArchive(ctx context.Context, w io.Writer, head, table []byte, staged []staging, level int, at time.Time) (int64, error) {
	counted := &countingWriter{w: w}
	zw, err := pgzip.NewWriterLevel(counted, level)
	if err != nil {
		return 0, fmt.Errorf("archive: pgzip level %d: %w", level, err)
	}
	tw := tar.NewWriter(zw)

	if err := writeMeta(tw, ManifestMember, head, at); err != nil {
		return 0, err
	}
	// Flushing HERE is what makes PeekManifest possible. Without it the
	// deflate stream buffers the manifest together with the first megabytes
	// of world data, and the first 8 KB of the file decompress to nothing.
	if err := tw.Flush(); err != nil {
		return 0, fmt.Errorf("archive: flush manifest member: %w", err)
	}
	if err := zw.Flush(); err != nil {
		return 0, fmt.Errorf("archive: flush compressor after the manifest: %w", err)
	}
	if err := writeMeta(tw, MembersMember, table, at); err != nil {
		return 0, err
	}

	for _, st := range staged {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if err := writeMember(tw, st); err != nil {
			return 0, err
		}
	}
	if err := tw.Close(); err != nil {
		return 0, fmt.Errorf("archive: close tar: %w", err)
	}
	if err := zw.Close(); err != nil {
		return 0, fmt.Errorf("archive: close compressor: %w", err)
	}
	return counted.n, nil
}

func writeMeta(tw *tar.Writer, name string, body []byte, at time.Time) error {
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Size:     int64(len(body)),
		Mode:     0o600,
		ModTime:  at,
		Format:   tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("archive: header %s: %w", name, err)
	}
	if _, err := tw.Write(body); err != nil {
		return fmt.Errorf("archive: write %s: %w", name, err)
	}
	return nil
}

func writeMember(tw *tar.Writer, st staging) error {
	hdr := &tar.Header{
		Name:    st.Path,
		Mode:    int64(st.Perm()),
		Uid:     st.UID,
		Gid:     st.GID,
		ModTime: st.MTime,
		// PAX rather than USTAR: a workshop mod's path routinely exceeds
		// USTAR's 100-byte name field, and truncating a member name is a
		// silently wrong restore.
		Format: tar.FormatPAX,
	}
	switch st.Kind {
	case KindDir:
		hdr.Typeflag = tar.TypeDir
		hdr.Name = st.Path + "/"
	case KindSymlink:
		hdr.Typeflag = tar.TypeSymlink
		hdr.Linkname = st.Link
	default:
		hdr.Typeflag = tar.TypeReg
		hdr.Size = st.Size
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("archive: header %s: %w", st.Path, err)
	}
	if st.Kind != KindFile {
		return nil
	}

	f, err := os.Open(st.abs)
	if err != nil {
		return fmt.Errorf("archive: open %s: %w", st.abs, err)
	}
	defer f.Close()

	h := sha256.New()
	n, err := io.CopyN(io.MultiWriter(tw, h), f, st.Size)
	switch {
	case errors.Is(err, io.EOF) || (err == nil && n != st.Size):
		return fmt.Errorf("%w: %s was %d bytes when it was hashed and %d when it was written",
			ErrDrifted, st.abs, st.Size, n)
	case err != nil:
		return fmt.Errorf("archive: copy %s: %w", st.abs, err)
	}
	// The re-hash is nearly free -- the bytes are streaming past anyway --
	// and it is the only thing standing between a file that was edited
	// mid-backup and an archive that verifies against a digest of the old
	// contents while holding the new ones.
	if got := hex.EncodeToString(h.Sum(nil)); got != st.SHA256 {
		return fmt.Errorf("%w: %s hashed %s during the scan and %s while being written; "+
			"the freeze lease (D-31) is what normally prevents this",
			ErrDrifted, st.abs, st.SHA256, got)
	}
	return nil
}

// assertPeekable re-reads the head of the finished file.
func assertPeekable(path string, at time.Time, instance string) error {
	m, err := PeekManifestFile(path)
	if err != nil {
		return fmt.Errorf("archive: the archive just written is not peekable, so DB-free "+
			"restore would not work on it: %w", err)
	}
	if !m.CreatedAt.Equal(at) || m.Instance.Name != instance {
		return fmt.Errorf("%w: the head read back from %s describes %q at %s, not %q at %s",
			ErrCorrupt, path, m.Instance.Name, m.CreatedAt, instance, at)
	}
	return nil
}

// cleanRel normalises a caller's relative path into the slash form member
// paths use.
func cleanRel(p string) string {
	p = filepath.ToSlash(strings.TrimSpace(p))
	p = strings.Trim(p, "/")
	if p == "" || p == "." {
		return ""
	}
	return path.Clean(p)
}

// ancestors lists the directories above rel, outermost first.
func ancestors(rel string) []string {
	if rel == "" {
		return nil
	}
	parts := strings.Split(rel, "/")
	out := make([]string, 0, len(parts)-1)
	for i := 1; i < len(parts); i++ {
		out = append(out, strings.Join(parts[:i], "/"))
	}
	return out
}

// excluded applies a source's exclude patterns to one relative path.
//
// "steamapps/workshop/**" keeps the directory and drops its contents, which is
// what the manifests mean by it: the difference between a 2.4 GB nightly and
// an 8 GB one (section 13.2). Any other pattern is matched against the whole
// path and against every ancestor, so naming a directory drops its subtree.
func excluded(patterns []string, rel string) bool {
	if rel == "" {
		return false
	}
	for _, raw := range patterns {
		p := cleanRel(strings.TrimSuffix(cleanRel(raw), "/**"))
		if p == "" {
			continue
		}
		deep := strings.HasSuffix(cleanRel(raw), "/**")
		if deep {
			if strings.HasPrefix(rel, p+"/") {
				return true
			}
			continue
		}
		if matchPathOrAncestor(p, rel) {
			return true
		}
	}
	return false
}

func matchPathOrAncestor(pattern, rel string) bool {
	if ok, err := path.Match(pattern, rel); err == nil && ok {
		return true
	}
	for _, anc := range ancestors(rel) {
		if ok, err := path.Match(pattern, anc); err == nil && ok {
			return true
		}
	}
	return false
}

func hashFile(ctx context.Context, abs string) (string, error) {
	f, err := os.Open(abs)
	if err != nil {
		return "", fmt.Errorf("archive: open %s: %w", abs, err)
	}
	defer f.Close()
	h := sha256.New()
	if err := copyCtx(ctx, h, f); err != nil {
		return "", fmt.Errorf("archive: read %s: %w", abs, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyCtx copies in chunks so a cancelled context stops a 2.4 GB read rather
// than being noticed after it.
func copyCtx(ctx context.Context, dst io.Writer, src io.Reader) error {
	buf := make([]byte, 256<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// syncDir makes a rename durable. The fsync failing is not worth reporting:
// FlushFileBuffers has no meaning for a directory handle on Windows and always
// fails there, and by the time it runs the rename has already succeeded.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}

// promoteNew moves the finished temp file into place without being able to
// replace anything.
//
// os.Rename replaces silently on both Linux (rename(2)) and Windows
// (MoveFileEx with MOVEFILE_REPLACE_EXISTING), and Filename() has one-second
// resolution with no nonce -- so two successful creates of the same instance
// and class in one second would leave one archive where the operator asked for
// two, with nothing recorded. os.Link is the primitive that refuses instead:
// it fails with EEXIST rather than clobbering, so the check and the act are one
// operation and there is no window between them.
//
// A backup directory on a filesystem without hard links falls back to a stat
// and a rename. That is a check-then-act, but the alternative is a deployment
// where backups simply stop working, and the window it leaves is the one the
// link path closes for everybody else.
func promoteNew(tmp, final string, o Options) error {
	switch err := os.Link(tmp, final); {
	case err == nil:
		// The temp file is now a second name for the same inode. Removing it
		// leaves the archive; a crash between the two leaves only a
		// dot-prefixed .tmp file, which is invisible to every *.tar.gz glob.
		//
		// A failure to unlink it is a NOTE and not an error: the archive is
		// written, verified-shaped and in place, and failing here would discard
		// a good backup over a leftover file no glob can see. It still gets
		// said out loud, because that leftover costs a full archive of disk.
		if rerr := os.Remove(tmp); rerr != nil {
			o.note("wrote %s but could not remove the temporary %s: %v; it is a second name for "+
				"the same file and costs no extra disk until the archive is pruned", final, tmp, rerr)
		}
		return nil
	case errors.Is(err, fs.ErrExist):
		return fmt.Errorf("%w: %s. An archive of this instance and class was already written this "+
			"second; gvm will not replace it", ErrArchiveExists, final)
	}

	if _, err := os.Lstat(final); err == nil {
		return fmt.Errorf("%w: %s. An archive of this instance and class was already written this "+
			"second; gvm will not replace it", ErrArchiveExists, final)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("archive: checking %s before promoting the new archive: %w", final, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("archive: rename %s -> %s: %w", tmp, final, err)
	}
	return nil
}
