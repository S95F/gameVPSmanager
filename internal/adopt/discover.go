package adopt

// DiscoverResult is the complete read-only picture of a legacy server.
type DiscoverResult struct {
	Game       string // "projectzomboid"
	UnitName   string // "zomboid.service"
	InstallDir string // WorkingDirectory from unit
	DataDir    string // DATA_DIR from config.env
	ServerName string // SERVER_NAME from config.env
	User       string
	Group      string

	// Launch
	UsesScreen bool   // ExecStart contains screen(1)
	ExecStart  string // raw (continuation-folded) ExecStart value

	// Ports (from .ini; never manifest defaults)
	Ports map[string]int

	// JVM heap sizes (from config.env)
	JVMXmx string
	JVMXms string

	// Content counts
	WorkshopItemCount int
	ModCount          int
	AmbiguousMods     []AmbiguousMod

	// Process state
	PID     int    // 0 = not running
	PIDType string // "jvm" | "screen" | "unknown" | ""

	Findings []DiscoverFinding
}

// AmbiguousMod is a workshop item whose download directory contains more than
// one mod subfolder — gvm cannot auto-select the activation name.
type AmbiguousMod struct {
	WorkshopID string
	Candidates []string
}

// DiscoverFinding is a single observation produced during discovery.
type DiscoverFinding struct {
	Code     string // e.g. "ADOPT-001"
	Severity string // "error" | "warning" | "note"
	Message  string
}

const (
	severityError   = "error"
	severityWarning = "warning"
	severityNote    = "note"
)
