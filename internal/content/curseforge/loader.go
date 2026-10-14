package curseforge

import "strings"

// LoaderKind names a Minecraft mod loader.
type LoaderKind string

const (
	LoaderUnknown  LoaderKind = ""
	LoaderForge    LoaderKind = "forge"
	LoaderNeoForge LoaderKind = "neoforge"
	LoaderFabric   LoaderKind = "fabric"
	LoaderQuilt    LoaderKind = "quilt"
)

// DetectLoader extracts the primary mod loader from a PackManifest.
//
// The manifest encodes the loader as "forge-47.2.0", "fabric-0.15.0", etc.
// DetectLoader returns the kind (forge, neoforge, fabric, quilt) and the full
// loader version string ("forge-47.2.0").
func DetectLoader(m *PackManifest) (kind LoaderKind, loaderID string) {
	if m == nil {
		return LoaderUnknown, ""
	}
	for _, ml := range m.Minecraft.ModLoaders {
		lower := strings.ToLower(ml.ID)
		switch {
		case strings.HasPrefix(lower, "neoforge-"):
			kind, loaderID = LoaderNeoForge, ml.ID
		case strings.HasPrefix(lower, "forge-"):
			if kind == LoaderUnknown {
				kind, loaderID = LoaderForge, ml.ID
			}
		case strings.HasPrefix(lower, "fabric-"):
			if kind == LoaderUnknown {
				kind, loaderID = LoaderFabric, ml.ID
			}
		case strings.HasPrefix(lower, "quilt-"):
			if kind == LoaderUnknown {
				kind, loaderID = LoaderQuilt, ml.ID
			}
		}
		if ml.Primary {
			// Primary loader wins immediately.
			return detectLoaderFromString(ml.ID)
		}
	}
	return kind, loaderID
}

func detectLoaderFromString(id string) (LoaderKind, string) {
	lower := strings.ToLower(id)
	switch {
	case strings.HasPrefix(lower, "neoforge-"):
		return LoaderNeoForge, id
	case strings.HasPrefix(lower, "forge-"):
		return LoaderForge, id
	case strings.HasPrefix(lower, "fabric-"):
		return LoaderFabric, id
	case strings.HasPrefix(lower, "quilt-"):
		return LoaderQuilt, id
	}
	return LoaderUnknown, id
}

// LoaderVersion extracts the version number from a loader id like
// "forge-47.2.0" → "47.2.0".
func LoaderVersion(loaderID string) string {
	if i := strings.Index(loaderID, "-"); i >= 0 {
		return loaderID[i+1:]
	}
	return loaderID
}
