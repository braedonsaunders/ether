//go:build !plexparity

package app

import "github.com/braedonsaunders/ether/internal/core"

func parityIndexer(v core.Indexer) core.Indexer            { return v }
func parityHealth(v core.HealthChecker) core.HealthChecker { return v }
func parityEngine(v core.Engine, _ string) core.Engine     { return v }
