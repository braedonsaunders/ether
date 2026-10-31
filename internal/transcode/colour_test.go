package transcode

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func withoutFilter(f features, names ...string) features {
	f.filters = map[string]bool{}
	for name, ok := range testFeatures().filters {
		f.filters[name] = ok
	}
	for _, name := range names {
		delete(f.filters, name)
	}
	return f
}

func TestUnknownFiltersDoNotAssumeZscale(t *testing.T) {
	info := mustParse(t, "mkv_hevc_hdr10.json")
	d := Decide(info, plexWeb, allow(), Policy{})
	s := baseSpec(info, d)
	s.Features = features{}
	err := graphFailure(s)
	if err == nil || !strings.Contains(err.Error(), "tone-map") {
		t.Fatalf("graphFailure = %v", err)
	}
	if strings.Contains(strings.Join(buildArgs(s), " "), "zscale") {
		t.Fatal("unknown filters produced a zscale graph")
	}
	if _, ok := cpuToneMap(features{}); ok {
		t.Fatal("cpuToneMap treated an unknown filter list as zscale")
	}
}

func TestSDRColourGraphs(t *testing.T) {
	dvd := core.MediaInfo{Streams: []core.StreamInfo{
		{Index: 0, Type: "video", Codec: "h264", Width: 720, Height: 480, BitDepth: 8},
		{Index: 1, Type: "audio", Codec: "aac", Channels: 2},
	}}
	hd := mustParse(t, "mp4_h264_aac.json")
	c, ok := contractOf(dvd, copyMKV())
	if !ok {
		t.Fatal("no contract")
	}
	d := adaptToContract(copyMKV(), dvd, hd, ContainerMatroska, c)
	noZ := withoutFilter(testFeatures(), "zscale")
	noZ.filters["colorspace"] = true
	noZ.filters["scale"] = true
	if err := c.check(hd, noZ); err != nil {
		t.Fatal(err)
	}
	s := contractSpec(t, hd, d, c)
	s.Features = noZ
	args := strings.Join(buildArgs(s), " ")
	if strings.Contains(args, "zscale") || !strings.Contains(args, "colorspace=ispace=bt709:iprimaries=bt709:itrc=bt709:irange=tv:space=smpte170m:primaries=smpte170m:trc=bt709:range=tv") {
		t.Fatalf("BT.709 into BT.601 without zscale:\n%s", args)
	}

	// Same primaries, full range into limited: scale is the fast path.
	full := hd
	full.Streams = append([]core.StreamInfo(nil), hd.Streams...)
	full.Streams[0].ColorRange = "pc"
	full.Streams[0].ColorPrimaries, full.Streams[0].ColorSpace = "bt709", "bt709"
	yc, _ := contractOf(hd, copyMKV())
	fd := adaptToContract(copyMKV(), hd, full, ContainerMatroska, yc)
	s = contractSpec(t, full, fd, yc)
	s.Features = noZ
	args = strings.Join(buildArgs(s), " ")
	if strings.Contains(args, "zscale") || strings.Contains(args, "colorspace=") || !strings.Contains(args, "scale=in_color_matrix=bt709:out_color_matrix=bt709:in_range=pc:out_range=tv") {
		t.Fatalf("range conversion:\n%s", args)
	}

	// Nothing that can convert primaries.
	bare := withoutFilter(testFeatures(), "zscale", "colorspace")
	if err := c.check(hd, bare); err == nil || !errors.Is(err, ErrIncompatibleSwap) {
		t.Fatalf("check = %v", err)
	}
	s = contractSpec(t, hd, d, c)
	s.Features = bare
	if err := graphFailure(s); err == nil || strings.Contains(strings.Join(buildArgs(s), " "), "zscale") {
		t.Fatalf("graphFailure = %v", err)
	}
}

func TestToneMapGraphs(t *testing.T) {
	hdr := mustParse(t, "mkv_hevc_hdr10.json")
	hlg := hdr
	hlg.Streams = append([]core.StreamInfo(nil), hdr.Streams...)
	hlg.Streams[0].ColorTransfer = "arib-std-b67"
	dv := mustParse(t, "mkv_dv_p8.json")
	none := withoutFilter(testFeatures(), "zscale", "tonemap", "scale_vt", "tonemap_vaapi", "tonemap_videotoolbox", "tonemap_cuda", "tonemap_opencl", "libplacebo")
	for _, tc := range []struct {
		name string
		info core.MediaInfo
	}{
		{"hdr10", hdr},
		{"hlg", hlg},
		{"dv", dv},
	} {
		d := Decide(tc.info, plexWeb, allow(), Policy{})
		if !d.ToneMap {
			t.Fatalf("%s: decision %+v", tc.name, d)
		}
		s := baseSpec(tc.info, d)
		if !strings.Contains(argValue(buildArgs(s), "-filter_complex"), "zscale=t=linear:npl=100") {
			t.Fatalf("%s: zscale path missing", tc.name)
		}
		s.Features = none
		err := graphFailure(s)
		if err == nil || !strings.Contains(err.Error(), "tone-map") || strings.Contains(strings.Join(buildArgs(s), " "), "zscale") {
			t.Fatalf("%s: graphFailure = %v", tc.name, err)
		}
		c, _ := contractOf(tc.info, core.Decision{Kind: core.DecisionTranscode, VideoCodec: "h264", MaxHeight: 1080, Container: ContainerMatroska})
		if err := c.check(tc.info, none); err == nil || !errors.Is(err, ErrIncompatibleSwap) {
			t.Fatalf("%s check = %v", tc.name, err)
		}
	}
}

func TestMissingFilterName(t *testing.T) {
	for _, stderr := range []string{
		"No such filter: 'zscale'",
		`[fc] No such filter: "libplacebo"`,
		"No such filter: tonemap_opencl",
		"Filter 'colorspace' not found",
	} {
		if missingFilter(stderr) == "" {
			t.Errorf("missingFilter(%q) empty", stderr)
		}
	}
	if missingFilter("Error while filtering") != "" || missingFilter("[h264_videotoolbox @ 0x1] Error: cannot create compression session: -12902") != "" {
		t.Fatal("unrelated stderr looked like a missing filter")
	}
}

func TestProbeFeaturesDegradesWhenFiltersUnreadable(t *testing.T) {
	fr := newFakeRunner()
	fr.filters = "Filters:\n"
	f, err := probeFeatures(context.Background(), fr, "ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	if f.filters == nil || f.filtersProbed || f.hasFilter("zscale") || f.hasFilter("colorspace") || f.probeDetail == "" {
		t.Fatalf("filters %#v probed %v detail %q", f.filters, f.filtersProbed, f.probeDetail)
	}
	n := 0
	for _, args := range fr.runCalls {
		if hasArg(args, "-filters") {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("filter probes = %d, want 3", n)
	}
}

func TestProbeFeaturesRetriesFilterRead(t *testing.T) {
	fr := newFakeRunner()
	attempts := 0
	fr.runFunc = func(_ context.Context, cmd Command) error {
		if hasArg(cmd.Args, "-filters") {
			attempts++
			if attempts == 1 {
				return errors.New("exit status 1")
			}
		}
		return errUnhandled
	}
	f, err := probeFeatures(context.Background(), fr, "ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	if !f.filtersProbed || !f.hasFilter("zscale") || !f.hasFilter("colorspace") || !f.hasEncoder("libx264") {
		t.Fatalf("features filters=%v encoders=%v", f.filters, f.encoders)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d", attempts)
	}
}

func TestParseRealFFmpeg8FilterLines(t *testing.T) {
	out := []byte(`Filters:
  T.. = Timeline support
  .S. = Slice threading
 TS colorspace        V->V       Convert between colorspaces.
 .. scale             V->V       Scale the input video size and/or convert the image format.
 .. scale_vt          V->V       Scale Videotoolbox frames
 .S tonemap           V->V       Conversion to/from different dynamic ranges.
`)
	f := parseFilters(out)
	for _, name := range []string{"colorspace", "scale", "scale_vt", "tonemap"} {
		if !f[name] {
			t.Errorf("missing %s in %v", name, f)
		}
	}
	if f["zscale"] {
		t.Fatal("zscale parsed from a build that does not have it")
	}
}
