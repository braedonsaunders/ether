package resolver

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// A 1080p SDR profile, a client that can play 2160p, and a 341-release
// pool must not offer or play a 2160p release unless the owner opts in.
// That is the shape reported in GitHub #205.
func TestProfileCeilingBlocksTallerVariants(t *testing.T) {
	releases := ceilingPool()
	if len(releases) != 341 {
		t.Fatalf("pool %d, want 341", len(releases))
	}
	caps := core.ClientCaps{
		Product: "Plex Web", MaxHeight: 2160,
		VideoCodecs: []string{"h264", "hevc"}, AudioCodecs: []string{"aac", "ac3", "eac3"},
	}
	cfg := testCfg()
	cfg.Quality.Rules = nil
	cfg.Quality.Default = "1080p"
	cfg.Quality.Remote = "1080p"
	svc := New(&fakeIndexer{releases: releases}, fakeHealth{}, nil, cfg, Options{})
	defer svc.Close()
	ctx := context.Background()
	title := matrixTitle()

	res, err := svc.Resolve(ctx, core.ResolveRequest{Title: title, Client: caps})
	if err != nil {
		t.Fatal(err)
	}
	if res.Release.Info.Height == 0 || res.Release.Info.Height > 1080 || res.Release.Info.HDR10 || res.Release.Info.DolbyVision {
		t.Fatalf("auto played above the ceiling: %+v %s", res.Release.Info, res.Release.Name)
	}
	vs, err := svc.Variants(ctx, core.ResolveRequest{Title: title, Client: caps}, maxVariants)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) == 0 || !vs[0].Auto || vs[0].Release.Info.Height > 1080 {
		t.Fatalf("auto variant %+v", vs)
	}
	for _, v := range vs {
		if v.Release.Info.Height > 1080 || v.Release.Info.HDR10 || v.Release.Info.DolbyVision {
			t.Fatalf("offered above the ceiling: %s %+v", v.Release.Name, v.Release.Info)
		}
	}
	for _, v := range vs {
		if strings.Contains(v.Release.ID, "huge") {
			t.Fatal("oversized 1080p offered")
		}
	}

	prefer, err := svc.Resolve(ctx, core.ResolveRequest{Title: title, Client: caps, Prefer: "web2160-0"})
	if err != nil || prefer.Release.ID == "web2160-0" || prefer.Release.Info.Height > 1080 {
		t.Fatalf("explicit 4K without opt-in res=%+v err=%v", prefer, err)
	}
	rows, _, err := svc.History(ctx, HistoryFilter{Limit: 5})
	if err != nil || len(rows) == 0 || rows[0].Chooser != chooserAuto {
		t.Fatalf("history %+v err=%v", rows, err)
	}
	dec, ok := svc.LatestDecision(title.Key)
	if !ok || dec.ProfileMaxHeight != 1080 || dec.ClientMaxHeight != 2160 || dec.EffectiveMaxHeight != 1080 || dec.AllowHDR {
		t.Fatalf("decision %+v ok=%v", dec, ok)
	}
	sawCeiling := false
	for _, c := range dec.Candidates {
		if c.Height > 1080 && c.Rejected && c.Reason == "above profile max height" {
			sawCeiling = true
		}
	}
	if !sawCeiling {
		t.Fatalf("decision omitted the 2160p rejection: %+v", dec.Candidates)
	}

	cfg.Resolver.OfferHigherVersions = true
	opted := New(&fakeIndexer{releases: releases}, fakeHealth{}, nil, cfg, Options{})
	defer opted.Close()
	if _, err := opted.Resolve(ctx, core.ResolveRequest{Title: title, Client: caps}); err != nil {
		t.Fatal(err)
	}
	optVS, err := opted.Variants(ctx, core.ResolveRequest{Title: title, Client: caps}, maxVariants)
	if err != nil || len(optVS) == 0 || !optVS[0].Auto || optVS[0].Release.Info.Height > 1080 {
		t.Fatalf("opt-in auto %+v err=%v", optVS, err)
	}
	taller, hdr := false, false
	for _, v := range optVS {
		if v.Release.Info.Height > 1080 && !v.Release.Info.HDR10 && !v.Release.Info.DolbyVision {
			taller = true
		}
		if v.Release.Info.HDR10 || v.Release.Info.DolbyVision {
			hdr = true
		}
	}
	if !taller || hdr {
		t.Fatalf("opt-in variants taller=%v hdr=%v %+v", taller, hdr, optVS)
	}
	picked, err := opted.Resolve(ctx, core.ResolveRequest{Title: title, Client: caps, Prefer: "web2160-0"})
	if err != nil || picked.Release.ID != "web2160-0" {
		t.Fatalf("opt-in prefer res=%+v err=%v", picked, err)
	}
	optRows, _, err := opted.History(ctx, HistoryFilter{Limit: 5})
	if err != nil || len(optRows) == 0 || optRows[0].Chooser != chooserViewer || !strings.Contains(optRows[0].ChosenVersion, "2160p") {
		t.Fatalf("viewer history %+v err=%v", optRows, err)
	}
}

func ceilingPool() []core.Release {
	var out []core.Release
	add := func(id, name string, sizeMB int64) {
		out = append(out, rel(id, name, sizeMB))
	}
	for i := 0; i < 180; i++ {
		name := fmt.Sprintf("The.Signal.1999.2160p.NF.WEB-DL.DDP5.1.H.265-G%d", i)
		if i == 0 {
			name = "The.Signal.1999.2160p.NF.WEB-DL.DDP5.1.H.265-HHWEB"
		}
		add(fmt.Sprintf("web2160-%d", i), name, 8000)
	}
	for i := 0; i < 40; i++ {
		add(fmt.Sprintf("bluray2160-%d", i), fmt.Sprintf("The.Signal.1999.2160p.BluRay.HEVC.DTS-HD.MA.5.1-B%d", i), 20000)
	}
	add("dv2160", "The.Signal.1999.2160p.WEB-DL.DV.HDR.H.265-GRP", 12000)
	for i := 0; i < 100; i++ {
		add(fmt.Sprintf("web1080-%d", i), fmt.Sprintf("The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-W%d", i), 8000)
	}
	for i := 0; i < 19; i++ {
		add(fmt.Sprintf("bluray1080-%d", i), fmt.Sprintf("The.Signal.1999.1080p.BluRay.x264-DTS-B%d", i), 12000)
	}
	add("huge1080", "The.Signal.1999.1080p.WEB-DL.H.264-HUGE", 40000)
	return out
}
