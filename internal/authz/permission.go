package authz

// Permission is a named capability.
type Permission string

// All permissions in the system. These are the canonical strings that appear
// in authz_permissions and in audit_log.params when an action is denied.
const (
	// Instance lifecycle
	PermInstanceView    Permission = "instance.view"
	PermInstanceCreate  Permission = "instance.create"
	PermInstanceApply   Permission = "instance.apply"
	PermInstanceStart   Permission = "instance.start"
	PermInstanceStop    Permission = "instance.stop"
	PermInstanceRestart Permission = "instance.restart"
	PermInstanceDestroy Permission = "instance.destroy"
	PermInstanceConsole Permission = "instance.console"
	PermInstanceControl Permission = "instance.control"
	PermInstancePorts   Permission = "instance.ports"
	PermInstanceLogs    Permission = "instance.logs"

	// Content management
	PermContentView Permission = "content.view"
	PermContentEdit Permission = "content.edit"
	PermContentSync Permission = "content.sync"

	// Backup / restore
	PermBackupCreate  Permission = "backup.create"
	PermBackupView    Permission = "backup.view"
	PermBackupRestore Permission = "backup.restore"
	PermBackupPrune   Permission = "backup.prune"

	// Jobs
	PermJobView Permission = "job.view"
	PermJobRun  Permission = "job.run"

	// Rebuild / update
	PermInstanceUpdate  Permission = "instance.update"
	PermInstanceRebuild Permission = "instance.rebuild"
	PermInstanceInstall Permission = "instance.install"

	// Adoption
	PermAdoptDiscover Permission = "adopt.discover"
	PermAdoptCutover  Permission = "adopt.cutover"
	PermAdoptRollback Permission = "adopt.rollback"
	PermAdoptFinalize Permission = "adopt.finalize"

	// Game manifests
	PermManifestView    Permission = "manifest.view"
	PermManifestPublish Permission = "manifest.publish"
	PermManifestApprove Permission = "manifest.approve"

	// Users and grants
	PermUserView   Permission = "user.view"
	PermUserCreate Permission = "user.create"
	PermUserEdit   Permission = "user.edit"
	PermUserDelete Permission = "user.delete"
	PermGrantEdit  Permission = "grant.edit"

	// Tokens
	PermTokenCreate Permission = "token.create"
	PermTokenRevoke Permission = "token.revoke"
	PermTokenView   Permission = "token.view"

	// Secrets
	PermSecretView   Permission = "secret.view"
	PermSecretRotate Permission = "secret.rotate"

	// Host / infrastructure
	PermDoctorRun     Permission = "doctor.run"
	PermFirewallView  Permission = "firewall.view"
	PermFirewallEdit  Permission = "firewall.edit"
	PermEdgeView      Permission = "edge.view"
	PermEdgeEdit      Permission = "edge.edit"
	PermDBMigrate     Permission = "db.migrate"
	PermDBReplaySpool Permission = "db.replay_spool"
	PermAuditView     Permission = "audit.view"
)
