package minecraft

import (
	"context"
	"fmt"
	"net/http"

	"github.com/S95F/gameVPSmanager/internal/provider"
	"github.com/S95F/gameVPSmanager/internal/spec"
)

// loaderResolver routes a plan/apply to the loader-specific logic.
type loaderResolver interface {
	plan(ctx context.Context, c *provider.Ctx, in provider.Intent) (*provider.Plan, error)
}

// resolverFor returns the right loaderResolver for the manifest's loader.kind.
func resolverFor(client *http.Client, mc *spec.MinecraftSpec) (loaderResolver, error) {
	switch mc.Loader.Kind {
	case "vanilla":
		return &vanillaResolver{client: client}, nil
	case "paper":
		return &paperResolver{client: client}, nil
	case "purpur":
		return &purpurResolver{client: client}, nil
	case "fabric":
		return &fabricResolver{client: client}, nil
	case "neoforge":
		return &neoforgeResolver{client: client}, nil
	case "forge":
		return &forgeResolver{client: client}, nil
	default:
		return nil, fmt.Errorf("minecraft provider: unknown loader kind %q", mc.Loader.Kind)
	}
}

// vanillaResolver plans a vanilla Minecraft server download.
type vanillaResolver struct{ client *http.Client }

func (v *vanillaResolver) plan(ctx context.Context, c *provider.Ctx, in provider.Intent) (*provider.Plan, error) {
	mc := mcSpec(c)
	ver, err := fetchPistonVersion(ctx, v.client, mc.MCVersion)
	if err != nil {
		return nil, err
	}
	dl := ver.Downloads.Server
	if dl.URL == "" {
		return nil, fmt.Errorf("no server download for Minecraft %s", ver.ID)
	}
	digest := provider.Digest("sha1:" + dl.SHA1)
	art := provider.Artifact{
		Digest:  digest,
		Size:    dl.Size,
		Source:  dl.URL,
		DestRel: "server.jar",
	}
	reason := fmt.Sprintf("install vanilla Minecraft %s server.jar", ver.ID)
	if in == provider.IntentUpdate {
		reason = fmt.Sprintf("update to Minecraft %s", ver.ID)
	}
	return &provider.Plan{
		Intent:    in,
		Reason:    reason,
		Artifacts: []provider.Artifact{art},
		Bytes:     dl.Size,
		Steps:     []string{"download server.jar"},
		Target:    provider.Version{ID: ver.ID, Label: "vanilla " + ver.ID},
		Opaque:    &opaque{JavaMajor: ver.JavaVersion.MajorVersion},
	}, nil
}
