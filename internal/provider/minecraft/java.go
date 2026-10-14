package minecraft

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/S95F/gameVPSmanager/internal/provider"
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

// fetchSHA1File downloads the Maven-convention <artifact>.sha1 sidecar file and
// returns its hex string. Maven repositories publish these adjacent to every
// artifact, so a single 40-char GET provides the verified digest at plan time
// without downloading the full JAR.
func fetchSHA1File(ctx context.Context, client *http.Client, artifactURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, artifactURL+".sha1", nil)
	if err != nil {
		return "", fmt.Errorf("sha1 sidecar: prepare: %w", err)
	}
	req.Header.Set("User-Agent", "gvm/"+time.Now().Format("2006"))
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("sha1 sidecar: fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("sha1 sidecar: HTTP %d from %s", resp.StatusCode, artifactURL+".sha1")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 128))
	if err != nil {
		return "", fmt.Errorf("sha1 sidecar: read: %w", err)
	}
	// Maven sha1 files contain the hex, optionally followed by two spaces and a filename.
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return "", fmt.Errorf("sha1 sidecar: empty response from %s", artifactURL+".sha1")
	}
	return strings.ToLower(fields[0]), nil
}

// fetchURLDigest streams artURL to completion, computing its SHA256 without
// retaining the bytes. This is the plan-time call for providers whose upstream
// does not publish a hash sidecar (e.g. Fabric, Purpur). Apply downloads the
// same URL again through fetchToBlobs and the blob store verifies the hash.
func fetchURLDigest(ctx context.Context, client *http.Client, artURL string) (provider.Digest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, artURL, nil)
	if err != nil {
		return "", fmt.Errorf("plan digest: prepare: %w", err)
	}
	req.Header.Set("User-Agent", "gvm/"+time.Now().Format("2006"))
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("plan digest: fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("plan digest: HTTP %d from %s", resp.StatusCode, artURL)
	}
	h := sha256.New()
	if _, err := io.Copy(h, resp.Body); err != nil {
		return "", fmt.Errorf("plan digest: hash: %w", err)
	}
	return provider.NewDigest(provider.AlgoSHA256, h.Sum(nil)), nil
}
