package minecraft

import (
	"context"
	"fmt"
	"net/http"

	"github.com/S95F/gameVPSmanager/internal/provider"
)

// Paper API base (v1 is gone, always use v2).
const paperAPIBase = "https://api.papermc.io/v2"

type paperBuildsResponse struct {
	ProjectID string `json:"project_id"`
	Version   string `json:"version"`
	Builds    []struct {
		Build     int    `json:"build"`
		Channel   string `json:"channel"` // "default" | "experimental"
		Downloads struct {
			Application struct {
				Name   string `json:"name"`
				SHA256 string `json:"sha256"`
			} `json:"application"`
		} `json:"downloads"`
	} `json:"builds"`
}

// paperResolver plans a Paper server download.
type paperResolver struct{ client *http.Client }

func (p *paperResolver) plan(ctx context.Context, c *provider.Ctx, in provider.Intent) (*provider.Plan, error) {
	mc := mcSpec(c)

	// First get the Java version from piston-meta.
	pistonVer, err := fetchPistonVersion(ctx, p.client, mc.MCVersion)
	if err != nil {
		return nil, err
	}

	// Fetch available builds from Paper API v2.
	buildsURL := fmt.Sprintf("%s/projects/paper/versions/%s/builds", paperAPIBase, mc.MCVersion)
	var resp paperBuildsResponse
	if err := getJSON(ctx, p.client, buildsURL, &resp); err != nil {
		return nil, fmt.Errorf("paper API: %w", err)
	}
	if len(resp.Builds) == 0 {
		return nil, fmt.Errorf("no Paper builds found for Minecraft %s", mc.MCVersion)
	}

	// Use the latest stable (default channel) build.
	var latestBuild struct {
		Build   int
		JarName string
		SHA256  string
	}
	for i := len(resp.Builds) - 1; i >= 0; i-- {
		b := resp.Builds[i]
		if b.Channel == "default" || b.Channel == "" {
			latestBuild.Build = b.Build
			latestBuild.JarName = b.Downloads.Application.Name
			latestBuild.SHA256 = b.Downloads.Application.SHA256
			break
		}
	}
	if latestBuild.Build == 0 {
		// Fall back to the last build if no "default" channel.
		b := resp.Builds[len(resp.Builds)-1]
		latestBuild.Build = b.Build
		latestBuild.JarName = b.Downloads.Application.Name
		latestBuild.SHA256 = b.Downloads.Application.SHA256
	}

	dlURL := fmt.Sprintf("%s/projects/paper/versions/%s/builds/%d/downloads/%s",
		paperAPIBase, mc.MCVersion, latestBuild.Build, latestBuild.JarName)

	digest := provider.Digest("sha256:" + latestBuild.SHA256)
	versionID := fmt.Sprintf("paper-%s-b%d", mc.MCVersion, latestBuild.Build)
	art := provider.Artifact{
		Digest:  digest,
		Source:  dlURL,
		DestRel: "paperclip.jar",
	}
	return &provider.Plan{
		Intent:    in,
		Reason:    fmt.Sprintf("install Paper %s (build %d)", mc.MCVersion, latestBuild.Build),
		Artifacts: []provider.Artifact{art},
		Steps:     []string{"download paperclip.jar"},
		Target:    provider.Version{ID: versionID, Label: "Paper " + mc.MCVersion},
		Opaque:    &opaque{JavaMajor: pistonVer.JavaVersion.MajorVersion},
	}, nil
}
