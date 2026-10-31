package diagnostics

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

func TestPercentEncodingRedactsMixedFullAndDouble(t *testing.T) {
	const secret = "jazz"
	uses, unsafe := prepareSecrets([]string{secret})
	if unsafe {
		t.Fatal("ordinary secret must not suppress free text")
	}
	cases := []string{
		`{"token":"%6a%61%7A%7a"}`,
		`{"token":"%6A%61%7a%7A"}`,
		`{"token":"%256a%2561%257A%257a"}`,
		`{"token":"%256A%2561%257a%257A"}`,
		`prefix jazz suffix`,
	}
	for _, in := range cases {
		out := redactString(in, uses)
		if strings.Contains(out, secret) || strings.Contains(strings.ToLower(out), "%6a") || strings.Contains(strings.ToLower(out), "%256a") {
			t.Fatalf("secret remains in %q -> %q", in, out)
		}
		if strings.HasPrefix(in, "{") {
			var doc map[string]string
			if err := json.Unmarshal([]byte(out), &doc); err != nil {
				t.Fatalf("json broken: %q: %v", out, err)
			}
		}
		if leaks([]byte(out), uses) {
			t.Fatalf("leak check: %q", out)
		}
	}
}

func TestLongPercentURLIsKeptWhenItHasNoSecret(t *testing.T) {
	uses, _ := prepareSecrets([]string{"jazz"})
	in := strings.Repeat("%41", 1500) + "/keep-me"
	out := redactString(in, uses)
	if !strings.Contains(out, "/keep-me") || strings.Contains(out, redactMask) {
		t.Fatalf("url damaged: %s", out[len(out)-40:])
	}
}

func TestClassifyDoesNotTreatErrorBodyAsMedia(t *testing.T) {
	got := ClassifyRequest("/library/parts/9/file.mkv", 404, "application/json", 100)
	if got != KindMediaRequest {
		t.Fatalf("404 body kind %s", got)
	}
	if got := ClassifyRequest("/library/parts/9/file.mkv", 500, "text/plain", 100); got != KindMediaRequest {
		t.Fatalf("500 body kind %s", got)
	}
	if got := ClassifyRequest("/library/parts/9/file.mkv", 200, "application/json", 100); got != KindMediaRequest {
		t.Fatalf("json 200 kind %s", got)
	}
	if got := ClassifyRequest("/video/:/transcode/universal/session/abc/seg/1.ts", 206, "video/mp2t", 100); got != KindMediaBytes {
		t.Fatalf("media kind %s", got)
	}
	if got := ClassifyRequest("/player/proxy/poll", 404, "text/plain", 80); got != KindPoll {
		t.Fatalf("poll kind %s", got)
	}
}

func TestClassifyTimelineIsNotPlaybackUntilAccepted(t *testing.T) {
	for _, status := range []int{401, 400, 500, 200} {
		got := ClassifyRequest("/:/timeline", status, "application/json", 40)
		if got != KindTimelineReceived {
			t.Fatalf("status %d kind %s", status, got)
		}
	}
	if got := ClassifyRequest("/:/progress", 200, "", 10); got != KindTimelineReceived {
		t.Fatalf("progress kind %s", got)
	}
}

func TestSharedPrefixKeysDoNotMerge(t *testing.T) {
	a := strings.Repeat("a", 128) + "A"
	b := strings.Repeat("a", 128) + "B"
	ka, dropA := boundKey(a)
	kb, dropB := boundKey(b)
	if dropA || dropB || ka != a || kb != b || ka == kb {
		t.Fatalf("short keys %q %q drops %v %v", ka, kb, dropA, dropB)
	}
	longA := strings.Repeat("b", maxAcceptKey) + "A"
	longB := strings.Repeat("b", maxAcceptKey) + "B"
	ha, dA := boundKey(longA)
	hb, dB := boundKey(longB)
	if dA || dB || ha == hb || !strings.HasPrefix(ha, "h:") || strings.HasPrefix(ha, "b") {
		t.Fatalf("hashed %q %q", ha, hb)
	}
	huge := strings.Repeat("c", maxHashKey+1)
	k, drop := boundKey(huge)
	if k != "" || !drop {
		t.Fatalf("overlong %q %v", k, drop)
	}
	evA := boundEvent(Event{DeviceKey: a, Kind: KindPoll}, nil)
	evB := boundEvent(Event{DeviceKey: b, Kind: KindPoll}, nil)
	if evA.DeviceKey == evB.DeviceKey {
		t.Fatal("events merged")
	}
}

func TestLongCredentialCrossingCutoffIsNotPrefixed(t *testing.T) {
	secret := strings.Repeat("S3cret!", 80)
	if len(secret) <= maxExcerpt {
		t.Fatal(len(secret))
	}
	uses, _ := prepareSecrets([]string{secret})
	for _, max := range []int{maxExcerpt, maxString} {
		got := clipFree("err "+secret+" tail", max, uses)
		if strings.Contains(got, secret[:24]) {
			t.Fatalf("max %d leaked prefix %q", max, got)
		}
	}
	longer := strings.Repeat("S3cret!", 4000)
	if len(longer) <= maxFieldScan {
		t.Fatal(len(longer))
	}
	uses, _ = prepareSecrets([]string{longer})
	got := clipFree(longer+"TAIL", maxExcerpt, uses)
	if strings.Contains(got, longer[:24]) || strings.Contains(got, "TAIL") {
		t.Fatalf("hard window leaked %q", got)
	}
}

func TestClipDoesNotCopyWholeInput(t *testing.T) {
	in := strings.Repeat("y", 1<<20)
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < 20; i++ {
		if n := len(clipFree(in, maxString, nil)); n > maxString {
			t.Fatal(n)
		}
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 2<<20 {
		t.Fatalf("allocated %d", grew)
	}
}
