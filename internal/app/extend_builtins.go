package app

import "github.com/braedonsaunders/ether/internal/extend"

// registerBuiltinDescriptors registers every built-in descriptor. Each call
// lives in its own file, owned by one workstream (docs/extend/BUILD.md).
func (a *App) registerBuiltinDescriptors(r *extend.Registry) error {
	for _, register := range []func(*extend.Registry) error{
		a.registerRankingDescriptors,    // extend_ranking.go (WS1a)
		a.registerCollectionDescriptors, // extend_collections.go (WS1b)
		a.registerLiveTVDescriptors,     // extend_livetv.go (WS1b, WS3)
		a.registerNotifyDescriptors,     // extend_notify.go (WS1c)
		a.registerConsoleDescriptors,    // extend_console.go (WS1d)
		a.registerSettingsDescriptors,   // extend_settings.go (WS1f)
		a.registerSubtitleDescriptors,   // extend_subtitles.go (WS3)
		a.registerSourceDescriptors,     // extend_sources.go (WS9b)
	} {
		if err := register(r); err != nil {
			return err
		}
	}
	return nil
}
