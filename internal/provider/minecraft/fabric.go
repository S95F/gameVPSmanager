package minecraft

import (
	"context"
	"fmt"
	"net/http"

	"github.com/S95F/gameVPSmanager/internal/provider"
)

const fabricMetaBase = "https://meta.fabricmc.net/v2"

type fabricLoaderEntry struct {
	Loader struct {
		Version string `json:"version"`
		Stable  bool   `json:"stable"`
	} `json:"loader"`
	Intermediary struct {
		Version string `json:"version"`
	} `json:"intermediary"`
	LauncherMeta struct {
		Version int `json:"version"`
	} `json:"launcherMeta"`
}

type fabricInstallerEntry struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	Maven   string `json:"maven"`
	Stable  bool   `json:"stable"`
}

// fabricResolver plans a Fabric server download.
// Fabric's preferred server-side approach is the server launcher JAR.
type fabricResolver struct{ client *http.Client }

func (f *fabricResolver) plan(ctx context.Context, c *provider.Ctx, in provider.Intent) (*provider.Plan, error) {
	mc := mcSpec(c)

	pistonVer, err := fetchPistonVersion(ctx, f.client, mc.MCVersion)
	if err != nil {
		return nil, err
	}

	// Get the latest stable loader version for this MC version.
	loadersURL := fmt.Sprintf("%s/versions/loader/%s", fabricMetaBase, mc.MCVersion)
	var loaders []fabricLoaderEntry
	if err := getJSON(ctx, f.client, loadersURL, &loaders); err != nil {
		return nil, fmt.Errorf("fabric meta loaders: %w", err)
	}
	if len(loaders) == 0 {
		return nil, fmt.Errorf("no Fabric loaders for Minecraft %s", mc.MCVersion)
	}
	var loaderVersion string
	for _, l := range loaders {
		if l.Loader.Stable {
			loaderVersion = l.Loader.Version
			break
		}
	}
	if loaderVersion == "" {
		loaderVersion = loaders[0].Loader.Version
	}

	// Get the latest stable installer.
	var installers []fabricInstallerEntry
	if err := getJSON(ctx, f.client, fabricMetaBase+"/versions/installer", &installers); err != nil {
		return nil, fmt.Errorf("fabric meta installers: %w", err)
	}
	if len(installers) == 0 {
		return nil, fmt.Errorf("no Fabric installers found")
	}
	var installer fabricInstallerEntry
	for _, ins := range installers {
		if ins.Stable {
			installer = ins
			break
		}
	}
	if installer.URL == "" {
		installer = installers[0]
	}

	// The server launcher download URL.
	dlURL := fmt.Sprintf("%s/versions/loader/%s/%s/%s/server/jar",
		fabricMetaBase, mc.MCVersion, loaderVersion, installer.Version)

	versionID := fmt.Sprintf("fabric-%s-%s", mc.MCVersion, loaderVersion)
	art := provider.Artifact{
		Digest:  provider.Digest("sha256:0000000000000000000000000000000000000000000000000000000000000000"),
		Source:  dlURL,
		DestRel: "fabric-server-launch.jar",
	}
	_ = art // will be populated with real digest in production
	return &provider.Plan{
		Intent:    in,
		Reason:    fmt.Sprintf("install Fabric %s (loader %s)", mc.MCVersion, loaderVersion),
		Artifacts: []provider.Artifact{art},
		Steps:     []string{"download fabric-server-launch.jar"},
		Target:    provider.Version{ID: versionID, Label: "Fabric " + mc.MCVersion},
		Opaque:    &opaque{JavaMajor: pistonVer.JavaVersion.MajorVersion},
	}, nil
}
