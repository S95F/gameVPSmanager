package spec

import (
	"fmt"
	"sort"
)

// ManifestRefs is what one scan of a manifest's templated fields found.
//
// Two callers need it and they must not disagree. `gvm run` derives the
// systemd credential ids it will read out of CREDENTIALS_DIRECTORY from
// Secrets; `gvm apply` derives the LoadCredential= lines it writes into the
// drop-in from the same set. A second implementation would eventually hand the
// runner an id the unit never loaded, and the symptom is an empty
// RCONPassword in a live config file rather than an error anywhere.
type ManifestRefs struct {
	// Secrets are TEMPLATE spellings (`rcon_password`), distinct and sorted.
	// The store's spelling (`rcon`) is secrets.FromTemplate's business; this
	// package cannot import internal/secrets without a cycle, and the two
	// namespaces are deliberately one function apart.
	Secrets []string

	// Hosts are the `.Host.*` names referenced, spelled as they appear
	// (".Host.RamMB"). They matter because two of them -- RamMB and IP4 --
	// are apply-time facts that the runner cannot supply, and rendering one
	// at runtime silently produces 0 or "".
	Hosts []string
}

// ScanManifestRefs walks every templated field in a manifest's PINNED bytes.
//
// It reads m.Raw rather than re-reading the file, so the references are
// collected from exactly the bytes that will build the argv and the unit --
// the same rule the pin check follows, and for the same reason.
func ScanManifestRefs(m *Manifest) (ManifestRefs, error) {
	var out ManifestRefs
	if m == nil {
		return out, fmt.Errorf("spec: no manifest to scan")
	}
	if len(m.Raw) == 0 {
		return out, fmt.Errorf("the pinned manifest carries no source bytes")
	}
	ix, err := IndexBytes(m.SourceRef, m.Raw)
	if err != nil {
		return out, err
	}
	seenSecret, seenHost := map[string]bool{}, map[string]bool{}
	for _, path := range TemplateFields(ix) {
		refs, terr := TemplateRefs(path, ix.Node(path).Value, SurfaceFor(path))
		if terr != nil {
			// The guard refused this file at load, so reaching here means the
			// pinned bytes are not the bytes that were validated. Refuse.
			return out, fmt.Errorf("%s: %s", path, terr.Msg)
		}
		for _, r := range refs {
			switch r.Root {
			case "Secret":
				if !seenSecret[r.Name] {
					seenSecret[r.Name] = true
					out.Secrets = append(out.Secrets, r.Name)
				}
			case "Host":
				if k := ".Host." + r.Name; !seenHost[k] {
					seenHost[k] = true
					out.Hosts = append(out.Hosts, k)
				}
			}
		}
	}
	sort.Strings(out.Secrets)
	sort.Strings(out.Hosts)
	return out, nil
}

// RuntimeUnknowable returns the `.Host.*` references only `gvm apply` can
// answer.
//
// `.Host.CPUs` is answerable anywhere, so it is not in the list; RamMB and IP4
// are host facts gathered at apply time and written into the instance YAML,
// and a runtime render of either would be a server sized for nothing or bound
// nowhere.
func (r ManifestRefs) RuntimeUnknowable() []string {
	var out []string
	for _, h := range r.Hosts {
		if h == ".Host.RamMB" || h == ".Host.IP4" {
			out = append(out, h)
		}
	}
	return out
}
