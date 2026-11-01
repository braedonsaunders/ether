//go:build !plexparity && !scorecard

package app

import "github.com/braedonsaunders/ether/internal/core"

func parityIndexer(v core.Indexer, _ string) core.Indexer            { return v }
func parityHealth(v core.HealthChecker, _ string) core.HealthChecker { return v }
func parityEngine(v core.Engine, _ string) core.Engine               { return v }
