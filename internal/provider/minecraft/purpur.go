package minecraft

import (
	"context"
	"fmt"
	"net/http"

	"github.com/S95F/gameVPSmanager/internal/provider"
)

const purpurAPIBase = "https://api.purpurmc.org/v2"

type purpurVersionResponse struct {
	Project string `json:"project"`
	Version string `json:"version"`
	Builds  struct {
		All    []string `json:"all"`
		Latest string   `json:"latest"`
	} `json:"builds"`
}

type purpurBuildResponse struct {
	Project string `json:"project"`
	Version string `json:"version"`
	Build   string `json:"build"`
	MD5     string `json:"md5"`
}

// purpurResolver plans a Purpur server download.
type purpurResolver struct{ client *http.Client }

func (p *purpurResolver) plan(ctx context.Context, c *provider.Ctx, in provider.Intent) (*provider.Plan, error) {
	mc := mcSpec(c)

	pistonVer, err := fetchPistonVersion(ctx, p.client, mc.MCVersion)
	if err != nil {
		return nil, err
	}

	verURL := fmt.Sprintf("%s/purpur/%s", purpurAPIBase, mc.MCVersion)
	var verResp purpurVersionResponse
	if err := getJSON(ctx, p.client, verURL, &verResp); err != nil {
		return nil, fmt.Errorf("purpur API: %w", err)
	}
	if verResp.Builds.Latest == "" {
		return nil, fmt.Errorf("no Purpur builds for Minecraft %s", mc.MCVersion)
	}
	latestBuild := verResp.Builds.Latest

	buildURL := fmt.Sprintf("%s/purpur/%s/%s", purpurAPIBase, mc.MCVersion, latestBuild)
	var buildResp purpurBuildResponse
	if err := getJSON(ctx, p.client, buildURL, &buildResp); err != nil {
		return nil, fmt.Errorf("purpur build API: %w", err)
	}

	dlURL := fmt.Sprintf("%s/purpur/%s/%s/download", purpurAPIBase, mc.MCVersion, latestBuild)
	// The Purpur API v2 provides only MD5, which is not a supported blob-store
	// algorithm. Stream the JAR at plan time to compute the real SHA256.
	// Apply re-downloads via fetchToBlobs; the blob store verifies on Put.
	digest, err := fetchURLDigest(ctx, p.client, dlURL)
	if err != nil {
		return nil, fmt.Errorf("purpur: compute plan digest: %w", err)
	}
	versionID := fmt.Sprintf("purpur-%s-b%s", mc.MCVersion, latestBuild)
	art := provider.Artifact{
		Digest:  digest,
		Source:  dlURL,
		DestRel: "purpur.jar",
	}
	return &provider.Plan{
		Intent:    in,
		Reason:    fmt.Sprintf("install Purpur %s (build %s)", mc.MCVersion, latestBuild),
		Artifacts: []provider.Artifact{art},
		Steps:     []string{"download purpur.jar"},
		Target:    provider.Version{ID: versionID, Label: "Purpur " + mc.MCVersion},
		Opaque:    &opaque{JavaMajor: pistonVer.JavaVersion.MajorVersion},
	}, nil
}
