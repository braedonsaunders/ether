package usenet

import (
	"go/build"
	"testing"
)

func TestRapidBackendBuildMatrix(t *testing.T) {
	for _, tc := range []struct {
		name, os, arch   string
		cgo, simd, rapid bool
	}{
		{"Go SIMD", "linux", "amd64", false, true, true},
		{"Go SIMD ARM64", "darwin", "arm64", false, true, true},
		{"Go SIMD other OS", "freebsd", "arm64", false, true, true},
		{"no SIMD experiment", "linux", "amd64", false, false, false},
		{"unsupported Go architecture", "linux", "arm", false, true, false},
		{"C macOS ARM64", "darwin", "arm64", true, false, true},
		{"C macOS AMD64", "darwin", "amd64", true, true, true},
		{"C Linux AMD64", "linux", "amd64", true, false, true},
		{"C Linux ARM64", "linux", "arm64", true, true, true},
		{"C Windows AMD64", "windows", "amd64", true, false, true},
		{"unsupported C Windows ARM64", "windows", "arm64", true, true, false},
		{"unsupported C architecture", "linux", "arm", true, true, false},
		{"unsupported C OS", "freebsd", "amd64", true, true, false},
		{"Android is not desktop Linux", "android", "arm64", true, true, false},
		{"iOS is not macOS", "ios", "arm64", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := build.Default
			ctx.GOOS, ctx.GOARCH, ctx.CgoEnabled = tc.os, tc.arch, tc.cgo
			// Control the simulated experiment regardless of the test binary's build.
			ctx.BuildTags = nil
			ctx.ToolTags = nil
			if tc.simd {
				ctx.ToolTags = []string{"goexperiment.simd"}
			}
			rapid, err := ctx.MatchFile(".", "rapid.go")
			if err != nil {
				t.Fatal(err)
			}
			fallback, err := ctx.MatchFile(".", "rapid_fallback.go")
			if err != nil {
				t.Fatal(err)
			}
			if rapid != tc.rapid || fallback == rapid {
				t.Fatalf("rapid=%v fallback=%v; want rapid=%v", rapid, fallback, tc.rapid)
			}
		})
	}
}
