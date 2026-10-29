package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/personalize"
	"github.com/braedonsaunders/ether/internal/state"
)

// trainRankerFixture generates dated Home serves and actual viewer progress
// through the real personalization and state APIs. Its deterministic viewing
// preference favours less-popular permitted films, independently of position.
// The production training gate is never changed: an inactive or non-lifting
// model is a hard fixture failure.
func trainRankerFixture(ctx context.Context, dir string, db *sql.DB, engine *personalize.Engine, st *state.State, viewers []core.User, clock *atomic.Int64) error {
	for daysAgo := 44; daysAgo >= 3; daysAgo-- {
		at := fixtureNow.AddDate(0, 0, -daysAgo)
		clock.Store(at.UnixNano())
		for _, viewer := range viewers {
			if err := engine.Refresh(ctx, &viewer); err != nil {
				return fmt.Errorf("train viewer %s: %w", viewer.Username, err)
			}
			hc := core.HubContext{Device: core.DeviceWeb, Daypart: core.DaypartAfternoon, Now: at, ClientID: fmt.Sprintf("ranker-%d-%d", viewer.ID, daysAgo)}
			hubs, err := engine.HomeHubsFor(ctx, &viewer, hc, 20)
			if err != nil {
				return err
			}
			engine.RecordImpressions(&viewer, hc, hubs)
			engine.Flush(ctx)
			var chosen *core.Title
			for row, h := range hubs {
				if row >= 8 {
					break
				}
				if h.ID == "home.continue" || h.ID == "continueWatching" || strings.Contains(h.ID, ".inprogress") || strings.Contains(h.ID, "upnext") {
					continue
				}
				for pos, item := range h.Items {
					if pos >= 12 {
						break
					}
					if item.Type != core.TypeMovie || item.Key <= core.MovieKey(10012) || item.Adult || catalog.RatingExceeds(item.ContentRating, viewer.MaxContentRating) {
						continue
					}
					if chosen == nil || item.Popularity < chosen.Popularity || (item.Popularity == chosen.Popularity && item.Key < chosen.Key) {
						v := item
						chosen = &v
					}
				}
			}
			if chosen == nil {
				return fmt.Errorf("ranker training has no eligible logged film for %s on %s", viewer.Username, at.Format("2006-01-02"))
			}
			if err := st.Progress(ctx, viewer.ID, chosen.Key, 5*time.Minute, 90*time.Minute, "playing"); err != nil {
				return err
			}
			if err := st.Progress(ctx, viewer.ID, chosen.Key, 80*time.Minute, 90*time.Minute, "stopped"); err != nil {
				return err
			}
			deadline := time.NewTimer(10 * time.Second)
			for {
				var n int
				err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personalize_plays WHERE user_id=? AND item_key=? AND started_at=? AND hub_id<>'' AND kind<>'' AND max_fraction>=0.8`, viewer.ID, chosen.Key, at.UnixMilli()).Scan(&n)
				if err != nil {
					deadline.Stop()
					return err
				}
				if n > 0 {
					deadline.Stop()
					break
				}
				select {
				case <-ctx.Done():
					deadline.Stop()
					return ctx.Err()
				case <-deadline.C:
					return fmt.Errorf("ranker training play not recorded for %s on %s", viewer.Username, at.Format("2006-01-02"))
				case <-time.After(10 * time.Millisecond):
				}
			}
			// Each synthetic viewer resets this training-only title after
			// watching, so the finite catalogue can generate repeated serves.
			// The attributed play and ranker label remain in real state.
			if err := st.Unscrobble(ctx, viewer.ID, chosen.Key); err != nil {
				return err
			}
		}
		if daysAgo%5 == 0 || daysAgo == 3 {
			var serves, labels int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personalize_serves`).Scan(&serves); err != nil {
				return err
			}
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personalize_serve_items WHERE label=1`).Scan(&labels); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "RANKER_TRAIN date=%s serves=%d positive_labels=%d\n", at.Format("2006-01-02"), serves, labels)
		}
	}
	clock.Store(fixtureNow.UnixNano())
	insight, err := engine.TrainRanker(ctx)
	if err != nil {
		return fmt.Errorf("train real ranker: %w", err)
	}
	holdoutPositive, holdoutNegative, examples, err := rankerFixtureHoldout(ctx, db)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "RANKER_RESULT active=%t examples=%d positives=%d auc=%.6f baseline=%.6f holdout_positives=%d holdout_negatives=%d\n", insight.Active, insight.Examples, insight.Positives, insight.AUC, insight.BaselineAUC, holdoutPositive, holdoutNegative)
	if !insight.Active || insight.TrainedAt == nil || insight.Examples < 2000 || insight.Positives < 30 || insight.AUC < 0.55 || insight.AUC < insight.BaselineAUC+0.02 {
		return fmt.Errorf("ranker did not pass production activation gate: active=%t examples=%d positives=%d auc=%.6f baseline=%.6f", insight.Active, insight.Examples, insight.Positives, insight.AUC, insight.BaselineAUC)
	}
	if examples != insight.Examples || holdoutPositive < 5 || holdoutNegative < 20 {
		return fmt.Errorf("ranker holdout evidence mismatch: rows=%d model=%d positives=%d negatives=%d", examples, insight.Examples, holdoutPositive, holdoutNegative)
	}
	proof := struct {
		Description      string             `json:"description"`
		Insight          core.RankerInsight `json:"insight"`
		HoldoutPositives int                `json:"holdout_positives"`
		HoldoutNegatives int                `json:"holdout_negatives"`
	}{"Deterministic synthetic viewers prefer less-popular titles and reset them after watching; real Home/progress APIs, production training and chronological holdout gate", insight, holdoutPositive, holdoutNegative}
	b, err := json.MarshalIndent(proof, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "ranker-training.json"), append(b, '\n'), 0600)
}

// Reproduce only the production chronological split and label counts from
// the real serve log, independently of the model's reported AUC and Active.
func rankerFixtureHoldout(ctx context.Context, db *sql.DB) (int, int, int, error) {
	rows, err := db.QueryContext(ctx, `SELECT i.label,i.fv,length(i.features),COALESCE(p.abandoned,0)
		FROM personalize_serve_items i JOIN personalize_serves s ON s.id=i.serve_id
		LEFT JOIN personalize_plays p ON p.id=i.play_id AND i.label=1
		WHERE i.label IN (0,1)
		ORDER BY s.first_at DESC,i.serve_id DESC,i.row_pos,i.item_pos LIMIT 150000`)
	if err != nil {
		return 0, 0, 0, err
	}
	defer rows.Close()
	var labels []bool
	for rows.Next() {
		var label, version, size, abandoned int
		if err := rows.Scan(&label, &version, &size, &abandoned); err != nil {
			return 0, 0, 0, err
		}
		if !((version == 1 && size == 68) || (version == 2 && size == 80)) {
			return 0, 0, 0, fmt.Errorf("invalid training feature snapshot version=%d size=%d", version, size)
		}
		labels = append(labels, label == 1 && abandoned == 0)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, err
	}
	// The SQL order is newest first. The last 20% after reversal is the
	// chronological holdout, hence the first N-split entries here.
	n := len(labels)
	split := int(float64(n) * 0.8)
	positive := 0
	for _, v := range labels[:n-split] {
		if v {
			positive++
		}
	}
	return positive, n - split - positive, n, nil
}
