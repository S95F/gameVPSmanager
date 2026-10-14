package curseforge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// jsonReader wraps a JSON-encoded byte slice in an io.Reader.
func jsonReader(b []byte) io.Reader { return bytes.NewReader(b) }

// cfAPIBase is the CurseForge v1 API root.
const cfAPIBase = "https://api.curseforge.com/v1"

// Client is the CurseForge HTTP client. The x-api-key is supplied by the
// operator at runtime; it is never embedded in this package.
type Client struct {
	http   *http.Client
	apiKey string
}

// NewClient returns a Client with the given operator-supplied API key.
// nil hc uses http.DefaultClient.
func NewClient(apiKey string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{http: hc, apiKey: apiKey}
}

// getFile fetches the metadata for one file from the CurseForge API.
func (c *Client) getFile(ctx context.Context, projectID, fileID int) (*cfFile, error) {
	u := fmt.Sprintf("%s/mods/%d/files/%d", cfAPIBase, projectID, fileID)
	var resp cfResponse[cfFile]
	if err := c.getJSON(ctx, u, &resp); err != nil {
		return nil, fmt.Errorf("curseforge: get file %d/%d: %w", projectID, fileID, err)
	}
	return &resp.Data, nil
}

// getLatestFile fetches the latest file for a project, optionally filtered by
// game version string.
func (c *Client) getLatestFile(ctx context.Context, projectID int, gameVersion string) (*cfFile, error) {
	mod, err := c.getMod(ctx, projectID)
	if err != nil {
		return nil, err
	}
	var best *cfFileIndex
	for i := range mod.LatestFilesIndexes {
		fi := &mod.LatestFilesIndexes[i]
		if gameVersion != "" && fi.GameVersion != gameVersion {
			continue
		}
		if best == nil || fi.FileID > best.FileID {
			best = fi
		}
	}
	if best == nil {
		return nil, fmt.Errorf("curseforge: no file for project %d (game_version=%q)", projectID, gameVersion)
	}
	return c.getFile(ctx, projectID, best.FileID)
}

// getMod fetches project metadata.
func (c *Client) getMod(ctx context.Context, projectID int) (*cfMod, error) {
	u := fmt.Sprintf("%s/mods/%d", cfAPIBase, projectID)
	var resp cfResponse[cfMod]
	if err := c.getJSON(ctx, u, &resp); err != nil {
		return nil, fmt.Errorf("curseforge: get mod %d: %w", projectID, err)
	}
	return &resp.Data, nil
}

// fingerprintSearch searches for files by murmur2 fingerprint.
func (c *Client) fingerprintSearch(ctx context.Context, fingerprints []uint32) (*cfFingerprintResult, error) {
	u := fmt.Sprintf("%s/fingerprints", cfAPIBase)
	body := struct {
		Fingerprints []uint32 `json:"fingerprints"`
	}{Fingerprints: fingerprints}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u,
		jsonReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "gvm/"+time.Now().Format("2006"))
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("curseforge: fingerprint search: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("curseforge: fingerprint search: HTTP %d", resp.StatusCode)
	}
	var out cfResponse[cfFingerprintResult]
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("curseforge: fingerprint search: %w", err)
	}
	return &out.Data, nil
}

// getJSON fetches url and decodes the JSON response into v.
func (c *Client) getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "gvm/"+time.Now().Format("2006"))
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, u)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
