package modrinth

// client.go provides Modrinth API HTTP utilities used by source.go.
// The actual HTTP client logic lives in getJSON (source.go).
// This file exports the API base URL as a testable variable.

// modrinthAPIBaseVar allows tests to override the API base URL.
var modrinthAPIBaseVar = modrinthAPIBase
