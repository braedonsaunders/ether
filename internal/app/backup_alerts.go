package app

import "log/slog"

// backupAlerts turns scheduled backup results into one alert per failure
// streak. An alert that could not be queued is tried again on the next
// failed attempt (backup.Run backs those off); a success ends the streak.
type backupAlerts struct {
	log     *slog.Logger
	notify  func(error) error // nil: log only
	failing bool              // an alert for the current streak was queued
}

func (b *backupAlerts) observe(err error) {
	if err == nil {
		if b.failing {
			b.log.Info("scheduled backup succeeded again")
		}
		b.failing = false
		return
	}
	b.log.Warn("scheduled backup", "err", err)
	if b.failing || b.notify == nil {
		return
	}
	if e := b.notify(err); e != nil {
		b.log.Warn("backup failure notification", "err", e)
		return
	}
	b.failing = true
}
