// Package modrinth: mrpack.go handles .mrpack modpack format parsing.
//
// A .mrpack file is a ZIP archive containing:
//   - modrinth.index.json: the mod list with download URLs and hashes
//   - overrides/: files to copy verbatim into the instance root
//
// For Phase 18, mrpack files referenced via a content entry are downloaded
// and their index is parsed; the constituent mods are treated as individual
// Modrinth refs for resolution purposes.
package modrinth

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
)

// mrpackIndex is the root of modrinth.index.json in a .mrpack archive.
type mrpackIndex struct {
	FormatVersion int          `json:"formatVersion"`
	Game          string       `json:"game"` // "minecraft"
	VersionID     string       `json:"versionID"`
	Name          string       `json:"name"`
	Files         []mrpackFile `json:"files"`
	Dependencies  mrpackDeps   `json:"dependencies"`
}

// mrpackFile is one mod entry in modrinth.index.json.
type mrpackFile struct {
	Path      string            `json:"path"`
	Hashes    map[string]string `json:"hashes"`
	Env       mrpackEnvReq      `json:"env"`
	Downloads []string          `json:"downloads"`
	FileSize  int64             `json:"fileSize"`
}

// mrpackEnvReq is the environment requirement for a file.
type mrpackEnvReq struct {
	Client string `json:"client"` // "required" | "optional" | "unsupported"
	Server string `json:"server"`
}

// mrpackDeps holds the loader and MC version for a modpack.
type mrpackDeps struct {
	Minecraft string `json:"minecraft"`
	Fabric    string `json:"fabric-loader,omitempty"`
	Forge     string `json:"forge,omitempty"`
	NeoForge  string `json:"neoforge,omitempty"`
	Quilt     string `json:"quilt-loader,omitempty"`
}

// parseMrpack reads an .mrpack archive from r (of size sz) and returns
// the index manifest.
func parseMrpack(r io.ReaderAt, sz int64) (*mrpackIndex, error) {
	zr, err := zip.NewReader(r, sz)
	if err != nil {
		return nil, fmt.Errorf("mrpack: open zip: %w", err)
	}
	for _, f := range zr.File {
		if f.Name == "modrinth.index.json" {
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("mrpack: open index: %w", err)
			}
			defer rc.Close()
			var idx mrpackIndex
			if err := json.NewDecoder(rc).Decode(&idx); err != nil {
				return nil, fmt.Errorf("mrpack: decode index: %w", err)
			}
			return &idx, nil
		}
	}
	return nil, fmt.Errorf("mrpack: modrinth.index.json not found in archive")
}
