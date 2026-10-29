package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// Opt-in because it trains two real models; normal scoped tests stay quick.
func TestTrainedRankerFixtureIsActiveAndDeterministic(t *testing.T) {
	if os.Getenv("ETHER_PARITY_TRAIN_RANKER") != "1" {
		t.Skip("requires the bounded parity ranker training experiment")
	}
	ctx := context.Background()
	read := func(dir string) ([]byte, string) {
		t.Helper()
		if err := seed(ctx, dir); err != nil {
			t.Fatal(err)
		}
		proof, err := os.ReadFile(filepath.Join(dir, "ranker-training.json"))
		if err != nil {
			t.Fatal(err)
		}
		var p struct {
			Insight          core.RankerInsight            `json:"insight"`
			HoldoutPositives int                           `json:"holdout_positives"`
			HoldoutNegatives int                           `json:"holdout_negatives"`
			HoldoutViewers   map[string]rankerHoldoutClass `json:"holdout_viewers"`
		}
		if err = json.Unmarshal(proof, &p); err != nil {
			t.Fatal(err)
		}
		if !p.Insight.Active || p.Insight.Examples < 2000 || p.Insight.Positives < 30 || p.Insight.AUC < 0.55 || p.Insight.AUC < p.Insight.BaselineAUC+0.02 || p.HoldoutPositives < 5 || p.HoldoutNegatives < 20 {
			t.Fatalf("ranker gate not independently satisfied: %+v", p)
		}
		for _, name := range []string{"owner", "adult", "teen", "child", "friend"} {
			if counts := p.HoldoutViewers[name]; counts.Positives == 0 || counts.Negatives == 0 {
				t.Fatalf("%s missing held-out class: %+v", name, counts)
			}
		}
		db, err := store.Open(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var model string
		if err = db.QueryRowContext(ctx, `SELECT model FROM personalize_ranker WHERE id=1`).Scan(&model); err != nil {
			t.Fatal(err)
		}
		return proof, model
	}
	a, am := read(t.TempDir())
	b, bm := read(t.TempDir())
	if !bytes.Equal(a, b) || am != bm {
		t.Fatal("real API-trained ranker proof or model differs between identical seeds")
	}
}
