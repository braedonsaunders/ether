package debrid

import (
	"errors"
	"github.com/braedonsaunders/ether/internal/core"
	"testing"
)

func TestChooseFileRejectsSameNumberedReboot(t *testing.T) {
	target := &core.ReleaseTarget{Season: 2, Episode: 2, ShowYear: 2005, EpisodeTitle: "Stone and Sky"}
	files := []File{{ID: "wrong-year", Path: "Space.Pilot.2023.S02E02.Light.1080p.mkv", Size: 2000}, {ID: "wrong-title", Path: "Space.Pilot.S02E02.Light.1080p.mkv", Size: 1500}, {ID: "right", Path: "Space.Pilot.2005.S02E02.Stone.and.Sky.1080p.mkv", Size: 1000}}
	got, err := ChooseFile(files, target)
	if err != nil || got.ID != "right" {
		t.Fatalf("wrong episode file: %+v %v", got, err)
	}
	if _, err := ChooseFile(files[:2], target); err == nil || errors.Is(err, core.ErrReleaseRejected) {
		t.Fatalf("identity mismatch accepted or permanently poisoned other series: %v", err)
	}
}

func TestReportedSeriesIDsPermitAlternateEpisodeNamesButNotWrongFiles(t *testing.T) {
	target := &core.ReleaseTarget{Season: 2, Episode: 2, ShowYear: 2005, EpisodeTitle: "Stone and Sky", ExpectedIDs: core.IDs{TMDB: 123}, ReportedIDs: core.IDs{TMDB: 123}}
	files := []File{
		{ID: "wrong-episode", Path: "Pilote.Spatial.S02E03.1080p.mkv", Size: 4000},
		{ID: "wrong-year", Path: "Pilote.Spatial.2023.S02E02.1080p.mkv", Size: 3000},
		{ID: "right", Path: "Pilote.Spatial.S02E02.Pierre.et.Ciel.1080p.mkv", Size: 2000},
	}
	got, err := ChooseFile(files, target)
	if err != nil || got.ID != "right" {
		t.Fatalf("alternate episode title: %+v %v", got, err)
	}
	target.ReportedIDs.TMDB = 456
	if _, err := ChooseFile(files, target); err == nil {
		t.Fatal("conflicting series ID was accepted")
	}
}

func TestChooseFileInEpisodeRangePackPicksExactEpisode(t *testing.T) {
	files := []File{
		{ID: "e43", Path: "Running Man Episode 1-129 English Subbed 720P/[iSUBS]Running.Man.E43.720P.avi", Size: 2_748_515_170},
		{ID: "e02", Path: "Running Man Episode 1-129 English Subbed 720P/[iSUBS]Running.Man.E02.720P.avi", Size: 1_560_000_000},
		{ID: "e01", Path: "Running Man Episode 1-129 English Subbed 720P/[iSUBS]Running.Man.E01.720P.avi", Size: 1_491_294_208},
		{ID: "e129", Path: "Running Man Episode 1-129 English Subbed 720P/[iSUBS]Running.Man.E129.720P.avi", Size: 1_600_000_000},
	}
	t1 := &core.ReleaseTarget{Season: 1, Episode: 1, ShowYear: 2010, EpisodeTitle: "Times Square", ExpectedIDs: core.IDs{TMDB: 33238}, ReportedIDs: core.IDs{TMDB: 33238}}
	got, err := ChooseFile(files, t1)
	if err != nil || got.ID != "e01" {
		t.Fatalf("want e01 for S01E01, got %+v err=%v", got, err)
	}
	if chosenFileMatchesTarget(files[0].Path, t1) {
		t.Fatal("E43 file matched S01E01 target")
	}
	if !chosenFileMatchesTarget(files[2].Path, t1) {
		t.Fatal("E01 file rejected for S01E01 target")
	}
	t129 := &core.ReleaseTarget{Season: 1, Episode: 129, ShowYear: 2010, ExpectedIDs: core.IDs{TMDB: 33238}, ReportedIDs: core.IDs{TMDB: 33238}}
	got129, err := ChooseFile(files, t129)
	if err != nil || got129.ID != "e129" {
		t.Fatalf("want e129 for S01E129, got %+v err=%v", got129, err)
	}
}
