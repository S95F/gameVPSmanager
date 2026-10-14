// Package curseforge implements the CurseForge content source (section 12.8).
//
// Security rules:
//
//   - The x-api-key is operator-supplied. No key is ever embedded in this
//     package.
//
//   - downloadUrl is used VERBATIM from the API response. It is never
//     constructed from a project slug, file id, or any other field.
//     nosynth_test.go enforces this structurally.
//
//   - A blocked mod (downloadUrl empty) enters ResolutionBlocked and never
//     aborts the plan. The operator drops the JAR into the drop-box and
//     gvm content resolve-blocked rescans it.
package curseforge

// cfResponse wraps every CurseForge API call in the standard envelope.
type cfResponse[T any] struct {
	Data T `json:"data"`
}

// cfFile is the CurseForge file object returned by the API.
//
// downloadUrl is the ONLY source of a CDN URL in this package. It may be
// empty when the mod author opts out of API distribution.
type cfFile struct {
	ID           int         `json:"id"`
	GameID       int         `json:"gameId"`
	ModID        int         `json:"modId"`
	FileName     string      `json:"fileName"`
	FileDate     string      `json:"fileDate"`
	FileLength   int64       `json:"fileLength"`
	ReleaseType  int         `json:"releaseType"` // 1=release, 2=beta, 3=alpha
	FileStatus   int         `json:"fileStatus"`
	Hashes       []cfHash    `json:"hashes"`
	DownloadURL  string      `json:"downloadUrl"` // empty == blocked
	IsAvailable  bool        `json:"isAvailable"`
	Fingerprint  uint32      `json:"fileFingerprint"` // murmur2
	Dependencies []cfFileDep `json:"dependencies"`
	GameVersions []string    `json:"gameVersions"`
	Modules      []cfModule  `json:"modules"`
	IsServerPack bool        `json:"isServerPack"`
}

// cfHash is a single file hash as returned by the API.
type cfHash struct {
	Value string `json:"value"`
	Algo  int    `json:"algo"` // 1=SHA1, 2=MD5
}

// cfFileDep is a dependency entry on a CurseForge file.
type cfFileDep struct {
	ModID        int `json:"modId"`
	RelationType int `json:"relationType"` // 1=embedded,2=optional,3=required,4=tool,5=incompatible,6=include
}

// cfModule is a named entry (subdirectory/jar) inside a CurseForge file.
type cfModule struct {
	Name        string `json:"name"`
	Fingerprint uint32 `json:"fingerprint"`
}

// cfMod is the CurseForge project/mod object.
type cfMod struct {
	ID    int    `json:"id"`
	Slug  string `json:"slug"`
	Name  string `json:"name"`
	Links struct {
		WebsiteURL string `json:"websiteUrl"`
	} `json:"links"`
	// LatestFilesIndexes is used to find the most recent file for a given
	// game version without fetching all files.
	LatestFilesIndexes []cfFileIndex `json:"latestFilesIndexes"`
}

// cfFileIndex is a lightweight summary of one file for a given game version.
type cfFileIndex struct {
	GameVersion     string `json:"gameVersion"`
	FileID          int    `json:"fileId"`
	FileName        string `json:"fileName"`
	ReleaseType     int    `json:"releaseType"`
	GameVersionType int    `json:"gameVersionTypeId"`
	ModLoader       int    `json:"modLoader"` // 0=any, 1=Forge, 2=Cauldron, 3=LiteLoader, 4=Fabric, 5=Quilt, 6=NeoForge
}

// cfFingerprintResult is returned by the /fingerprints endpoint.
type cfFingerprintResult struct {
	IsCacheBuilt          bool                 `json:"isCacheBuilt"`
	ExactMatches          []cfFingerprintMatch `json:"exactMatches"`
	ExactFingerprints     []uint32             `json:"exactFingerprints"`
	InstalledFingerprints []uint32             `json:"installedFingerprints"`
	UnmatchedFingerprints []uint32             `json:"unmatchedFingerprints"`
}

// cfFingerprintMatch is a fingerprint match entry.
type cfFingerprintMatch struct {
	ID   int    `json:"id"`
	File cfFile `json:"file"`
}

// depRelationEmbedded is a CurseForge file dependency type that means the
// sub-file is bundled inside the parent. It must NEVER be downloaded separately
// (identical to Modrinth's "embedded" rule).
const depRelationEmbedded = 1

// depRelationIncompatible is a file dependency type that means two mods
// conflict. Resolving a file with an incompatible dep aborts the plan.
const depRelationIncompatible = 5
