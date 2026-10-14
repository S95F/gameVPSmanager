package minecraft

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// pistonManifestURL is the Mojang version manifest endpoint.
// It is a var (not const) so tests can override it with an httptest.Server URL.
var pistonManifestURL = "https://piston-meta.mojang.com/mc/game/version_manifest_v2.json"

// pistonVersionManifest is a version_manifest_v2.json entry.
type pistonVersionManifest struct {
	Latest struct {
		Release  string `json:"release"`
		Snapshot string `json:"snapshot"`
	} `json:"latest"`
	Versions []struct {
		ID   string `json:"id"`
		Type string `json:"type"` // "release" | "snapshot"
		URL  string `json:"url"`
	} `json:"versions"`
}

// pistonVersion is one Minecraft version's metadata document.
type pistonVersion struct {
	ID          string `json:"id"`
	JavaVersion struct {
		MajorVersion int    `json:"majorVersion"`
		Component    string `json:"component"`
	} `json:"javaVersion"`
	Downloads struct {
		Server struct {
			URL  string `json:"url"`
			SHA1 string `json:"sha1"`
			Size int64  `json:"size"`
		} `json:"server"`
	} `json:"downloads"`
}

// fetchPistonVersion retrieves the version metadata for a specific MC version.
func fetchPistonVersion(ctx context.Context, client *http.Client, mcVersion string) (*pistonVersion, error) {
	var manifest pistonVersionManifest
	if err := getJSON(ctx, client, pistonManifestURL, &manifest); err != nil {
		return nil, fmt.Errorf("piston-meta manifest: %w", err)
	}

	// Resolve "latest-release" or "latest-snapshot" aliases.
	switch strings.ToLower(mcVersion) {
	case "latest", "latest-release":
		mcVersion = manifest.Latest.Release
	case "latest-snapshot":
		mcVersion = manifest.Latest.Snapshot
	}

	for _, v := range manifest.Versions {
		if v.ID == mcVersion {
			var ver pistonVersion
			if err := getJSON(ctx, client, v.URL, &ver); err != nil {
				return nil, fmt.Errorf("piston-meta version %s: %w", mcVersion, err)
			}
			return &ver, nil
		}
	}
	return nil, fmt.Errorf("minecraft version %q not found in piston-meta", mcVersion)
}

// javaMajorFromVersion returns the major Java version from a "1.8.0_302" or "21" style string.
// Used when piston-meta returns a javaVersion with majorVersion=0 (shouldn't happen, but safe).
func javaMajorFromVersion(s string) int {
	// "21" → 21, "1.8.0_302" → 8
	parts := strings.SplitN(s, ".", 3)
	if len(parts) == 0 {
		return 0
	}
	major, _ := strconv.Atoi(parts[0])
	if major == 1 && len(parts) > 1 {
		minor, _ := strconv.Atoi(parts[1])
		return minor
	}
	return major
}

// javaHomePaths returns candidate JVM home directories for the given major version,
// in order of preference. These are the well-known locations on Debian/Ubuntu/RHEL.
// We never exec java to check the version -- we derive it from the directory name.
func javaHomePaths(majorVersion int) []string {
	maj := strconv.Itoa(majorVersion)
	return []string{
		"/usr/lib/jvm/java-" + maj + "-openjdk-amd64",
		"/usr/lib/jvm/java-" + maj + "-openjdk",
		"/usr/lib/jvm/temurin-" + maj,
		"/usr/lib/jvm/java-" + maj + "-temurin-amd64",
		"/usr/lib/jvm/java-" + maj,
		"/usr/local/lib/jvm/java-" + maj,
	}
}

// javaVersionComponent is only here to make the piston JSON decoding type-safe.
var _ = json.Unmarshal
