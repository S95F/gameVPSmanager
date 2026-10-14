package minecraft

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/provider"
)

const neoforgeMetadataURL = "https://maven.neoforged.net/releases/net/neoforged/neoforge/maven-metadata.xml"
const neoforgeBaseURL = "https://maven.neoforged.net/releases/net/neoforged/neoforge"

// mavenMetadata is the structure of a Maven maven-metadata.xml file.
// We parse the full <versions> list and NEVER trust <latest> or <release>
// (those can lag or be missing from mirrors).
type mavenMetadata struct {
	XMLName  xml.Name `xml:"metadata"`
	Versions struct {
		Version []string `xml:"version"`
	} `xml:"versioning>versions"`
}

// fetchMavenVersions retrieves and parses the maven-metadata.xml at url.
func fetchMavenVersions(ctx context.Context, client *http.Client, url string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	var meta mavenMetadata
	if err := xml.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("maven-metadata.xml: %w", err)
	}
	return meta.Versions.Version, nil
}

// latestNeoForgeFor returns the latest NeoForge version compatible with mcVersion.
// NeoForge versions follow the pattern "<mcMajor>.<mcMinor>.<mcPatch>-<nfPatch>"
// e.g. "21.1.68" for Minecraft 1.21.1.
func latestNeoForgeFor(versions []string, mcVersion string) (string, error) {
	// For NeoForge, strip the "1." prefix from MC version to get the major.minor
	// e.g. "1.21.1" → "21.1"
	mcShort := strings.TrimPrefix(mcVersion, "1.")

	var matching []string
	for _, v := range versions {
		if strings.HasPrefix(v, mcShort+".") {
			matching = append(matching, v)
		}
	}
	if len(matching) == 0 {
		return "", fmt.Errorf("no NeoForge versions for Minecraft %s", mcVersion)
	}
	sort.Strings(matching)
	return matching[len(matching)-1], nil
}

// neoforgeResolver plans a NeoForge installer download.
type neoforgeResolver struct{ client *http.Client }

func (n *neoforgeResolver) plan(ctx context.Context, c *provider.Ctx, in provider.Intent) (*provider.Plan, error) {
	mc := mcSpec(c)

	pistonVer, err := fetchPistonVersion(ctx, n.client, mc.MCVersion)
	if err != nil {
		return nil, err
	}

	// Parse the full Maven versions list (never trust <latest>/<release>).
	allVersions, err := fetchMavenVersions(ctx, n.client, neoforgeMetadataURL)
	if err != nil {
		return nil, fmt.Errorf("NeoForge Maven metadata: %w", err)
	}

	// Use manifest-declared version or find the latest.
	nfVersion := mc.Loader.Version
	if nfVersion == "" || nfVersion == "latest" {
		nfVersion, err = latestNeoForgeFor(allVersions, mc.MCVersion)
		if err != nil {
			return nil, err
		}
	} else {
		// Validate the pinned version exists in the real list.
		found := false
		for _, v := range allVersions {
			if v == nfVersion {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("NeoForge version %q not found in Maven", nfVersion)
		}
	}

	installerJar := fmt.Sprintf("neoforge-%s-installer.jar", nfVersion)
	dlURL := fmt.Sprintf("%s/%s/%s", neoforgeBaseURL, nfVersion, installerJar)

	art := provider.Artifact{
		Digest:  provider.Digest("sha1:0000000000000000000000000000000000000000"),
		Source:  dlURL,
		DestRel: installerJar,
	}
	versionID := fmt.Sprintf("neoforge-%s", nfVersion)
	return &provider.Plan{
		Intent:    in,
		Reason:    fmt.Sprintf("install NeoForge %s for Minecraft %s", nfVersion, mc.MCVersion),
		Artifacts: []provider.Artifact{art},
		Steps:     []string{"download " + installerJar},
		Target:    provider.Version{ID: versionID, Label: "NeoForge " + nfVersion},
		Opaque:    &opaque{JavaMajor: pistonVer.JavaVersion.MajorVersion},
	}, nil
}
