package curseforge

import (
	"encoding/json"
	"fmt"
	"io"
)

// PackManifest is the CurseForge modpack manifest.json format.
//
// A CurseForge .zip modpack contains a manifest.json at its root with this
// structure. The files list drives CurseForge acquisition; the overrides/
// directory provides files that go directly into the Minecraft root.
type PackManifest struct {
	Minecraft    PackMinecraft `json:"minecraft"`
	ManifestType string        `json:"manifestType"` // "minecraftModpack"
	Name         string        `json:"name"`
	Version      string        `json:"version"`
	Author       string        `json:"author"`
	Files        []PackFile    `json:"files"`
	Overrides    string        `json:"overrides"` // subdirectory name, typically "overrides"
}

// PackMinecraft is the Minecraft section of the manifest.
type PackMinecraft struct {
	Version    string          `json:"version"`
	ModLoaders []PackModLoader `json:"modLoaders"`
}

// PackModLoader is a single mod loader entry (e.g. "forge-47.2.0").
type PackModLoader struct {
	ID      string `json:"id"`
	Primary bool   `json:"primary"`
}

// PackFile is a single file entry in the modpack manifest.
type PackFile struct {
	ProjectID int  `json:"projectID"`
	FileID    int  `json:"fileID"`
	Required  bool `json:"required"`
}

// ParsePackManifest decodes a CurseForge manifest.json.
func ParsePackManifest(r io.Reader) (*PackManifest, error) {
	var m PackManifest
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return nil, fmt.Errorf("curseforge: parse manifest.json: %w", err)
	}
	if m.ManifestType != "minecraftModpack" {
		return nil, fmt.Errorf("curseforge: manifest.json: unexpected manifestType %q (want \"minecraftModpack\")",
			m.ManifestType)
	}
	return &m, nil
}

// Refs converts the manifest's file list to CurseForge acquisition ids in the
// form "projectID:fileID". Required files come before optional ones.
func (m *PackManifest) Refs() []string {
	out := make([]string, 0, len(m.Files))
	// Required first.
	for _, f := range m.Files {
		if f.Required {
			out = append(out, fmt.Sprintf("%d:%d", f.ProjectID, f.FileID))
		}
	}
	for _, f := range m.Files {
		if !f.Required {
			out = append(out, fmt.Sprintf("%d:%d", f.ProjectID, f.FileID))
		}
	}
	return out
}
