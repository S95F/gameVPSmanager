package curseforge

import "strings"

// ExcludeSpec is the itzg modpack file filter schema.
//
// Rules (in precedence order):
//  1. forceIncludes WINS over excludes — an entry in both always appears.
//  2. excludes removes a file unless forceIncludes covers it.
//  3. includes, when non-empty, acts as an allowlist — only matching files are
//     kept (subject to rule 1).
//
// Pattern matching is case-insensitive substring (CurseForge pack filenames are
// not fully portable between OS; case-folding avoids false negatives on paths
// that were renamed for casing only).
type ExcludeSpec struct {
	// ForceIncludes lists filenames that are always kept, regardless of
	// excludes. Case-insensitive.
	ForceIncludes []string `json:"forceIncludes,omitempty"`
	// Excludes lists filenames to remove from the pack. Case-insensitive.
	Excludes []string `json:"exclude,omitempty"`
	// Includes lists filenames to keep. When non-empty, only matching files
	// are kept (allowlist). Empty means "keep everything".
	Includes []string `json:"include,omitempty"`
}

// Keep reports whether a file should be included given this spec.
//
// filename is the bare filename (not a path). The three-rule evaluation
// follows exactly the precedence comment above.
func (s ExcludeSpec) Keep(filename string) bool {
	lower := strings.ToLower(filename)

	// Rule 1: forceIncludes wins.
	for _, pat := range s.ForceIncludes {
		if strings.Contains(lower, strings.ToLower(pat)) {
			return true
		}
	}

	// Rule 2: excludes removes a file.
	for _, pat := range s.Excludes {
		if strings.Contains(lower, strings.ToLower(pat)) {
			return false
		}
	}

	// Rule 3: includes allowlist (only when non-empty).
	if len(s.Includes) > 0 {
		for _, pat := range s.Includes {
			if strings.Contains(lower, strings.ToLower(pat)) {
				return true
			}
		}
		return false
	}

	return true
}
