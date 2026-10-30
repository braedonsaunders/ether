package app

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestExperienceHubRowsKeepsSemanticData(t *testing.T) {
	rows := experience.HubRows([]core.Hub{{ID: "home.continue", Context: "hub.home.continue", Items: []core.Title{{Key: core.MovieKey(1)}}}, {ID: "home.watchlist", Context: "hub.private", Title: "Saved"}})
	if len(rows) != 2 || rows[0].Purpose != frontend.RowContinue || len(rows[0].Entries) != 1 || rows[0].Entries[0].Title.Key != core.MovieKey(1) || rows[1].Purpose != frontend.RowWatchlist {
		t.Fatalf("unexpected rows: %+v", rows)
	}
}

func TestExperienceRowSourceAppendsAdditionalRows(t *testing.T) {
	source := experience.CandidateRowSource{
		Load: func(context.Context, frontend.RowRequest) ([]experience.RankedCandidate, error) {
			return []experience.RankedCandidate{{ID: "home.continue", Style: "hero", Purpose: frontend.RowContinue}}, nil
		},
		Additional: func(context.Context, frontend.RowRequest) ([]frontend.Row, error) {
			return []frontend.Row{{ID: "playlists", Purpose: core.RowPurpose("playlist")}}, nil
		},
	}
	rows, err := source.Rows(context.Background(), frontend.RowRequest{})
	if err != nil || len(rows) != 2 || rows[0].ID != "home.continue" || rows[0].Layout.Style != "hero" || rows[1].ID != "playlists" {
		t.Fatalf("rows = %+v, error = %v", rows, err)
	}
}
