package capability

import "database/sql"

// SetBeforeConsumeLock exposes the package-local barrier to external tests only.
func SetBeforeConsumeLock(t *Tickets, hook func()) { t.beforeConsumeLock = hook }

// SetBeforeLegacyCleanup is available to external tests only.
func SetBeforeLegacyCleanup(t *Tickets, hook func(*sql.Tx)) { t.beforeLegacyCleanup = hook }
