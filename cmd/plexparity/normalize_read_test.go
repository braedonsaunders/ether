package main

import "testing"

func TestNormalizeMasksReadCapability(t *testing.T) {
	a := normalizeResponse([]byte("00000.ts?X-Ether-Stream-Token=AAAA&X-Ether-Read-Token=BBBB\n"))
	b := normalizeResponse([]byte("00000.ts?X-Ether-Stream-Token=CCCC&X-Ether-Read-Token=DDDD\n"))
	if string(a) != string(b) || string(a) != "00000.ts?X-Ether-Stream-Token=<STREAM-TOKEN>&X-Ether-Read-Token=<READ-TOKEN>\n" {
		t.Fatalf("%q vs %q", a, b)
	}
	if c := normalizeResponse([]byte("00000.ts?X-Ether-Stream-Token=CCCC\n")); string(c) == string(a) {
		t.Fatal("a missing read token must still differ")
	}
}
