package state

import (
	"embed"

	"go.mau.fi/util/dbutil"
)

//go:embed upgrades/*.sql
var rawUpgrades embed.FS

// UpgradeTable is the versioned bridge-state schema. It is exported for offline contract tests and
// for operators that need to inspect the latest supported version without running an upgrade.
var UpgradeTable dbutil.UpgradeTable

func init() {
	UpgradeTable.RegisterFSPath(rawUpgrades, "upgrades")
}

const versionTableName = "bridge_version"
