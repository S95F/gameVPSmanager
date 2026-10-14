package minecraft

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/S95F/gameVPSmanager/internal/provider"
)

const forgeMetadataURL = "https://maven.minecraftforge.net/net/minecraftforge/forge/maven-metadata.xml"
const forgeBaseURL = "https://maven.minecraftforge.net/net/minecraftforge/forge"

// latestForgeFor returns the latest Forge version for the given mcVersion.
// Forge versions follow the pattern "<mcVersion>-<forgeVersion>"
// e.g. "1.21.1-52.0.20".
func latestForgeFor(versions []string, mcVersion string) (string, error) {
	prefix := mcVersion + "-"
	var matching []string
	for _, v := range versions {
		if strings.HasPrefix(v, prefix) {
			matching = append(matching, v)
		}
	}
	if len(matching) == 0 {
		return "", fmt.Errorf("no Forge versions for Minecraft %s", mcVersion)
	}
	sort.Strings(matching)
	return matching[len(matching)-1], nil
}

// forgeResolver plans a Forge installer download.
type forgeResolver struct{ client *http.Client }

func (f *forgeResolver) plan(ctx context.Context, c *provider.Ctx, in provider.Intent) (*provider.Plan, error) {
	mc := mcSpec(c)

	pistonVer, err := fetchPistonVersion(ctx, f.client, mc.MCVersion)
	if err != nil {
		return nil, err
	}

	// Parse the full Maven versions list (never trust <latest>/<release>).
	allVersions, err := fetchMavenVersions(ctx, f.client, forgeMetadataURL)
	if err != nil {
		return nil, fmt.Errorf("Forge Maven metadata: %w", err)
	}

	// Use manifest-declared version or find the latest.
	forgeVersion := mc.Loader.Version
	if forgeVersion == "" || forgeVersion == "latest" {
		forgeVersion, err = latestForgeFor(allVersions, mc.MCVersion)
		if err != nil {
			return nil, err
		}
	} else {
		found := false
		for _, v := range allVersions {
			if v == forgeVersion {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("Forge version %q not found in Maven", forgeVersion)
		}
	}

	installerJar := fmt.Sprintf("forge-%s-installer.jar", forgeVersion)
	dlURL := fmt.Sprintf("%s/%s/%s", forgeBaseURL, forgeVersion, installerJar)

	art := provider.Artifact{
		Digest:  provider.Digest("sha1:0000000000000000000000000000000000000000"),
		Source:  dlURL,
		DestRel: installerJar,
	}
	versionID := fmt.Sprintf("forge-%s", forgeVersion)
	return &provider.Plan{
		Intent:    in,
		Reason:    fmt.Sprintf("install Forge %s for Minecraft %s", forgeVersion, mc.MCVersion),
		Artifacts: []provider.Artifact{art},
		Steps:     []string{"download " + installerJar},
		Target:    provider.Version{ID: versionID, Label: "Forge " + forgeVersion},
		Opaque:    &opaque{JavaMajor: pistonVer.JavaVersion.MajorVersion},
	}, nil
}
