package app

import "time"

// Match the historical playback.Manager Options default for the isolated
// source and its remote frontend. Standalone construction keeps its default.
const specialistPlaybackIdleTimeout = 2 * time.Minute
