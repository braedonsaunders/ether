package capability

// SetBeforeConsumeLock exposes the package-local barrier to external tests only.
func SetBeforeConsumeLock(t *Tickets, hook func()) { t.beforeConsumeLock = hook }
