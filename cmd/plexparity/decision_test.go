package main

import "testing"

func TestRequireDecisionRejectsWrongMode(t *testing.T) {
	body := []byte(`<MediaContainer><Video><Media><Part decision="transcode"><Stream streamType="1" decision="copy"/></Part></Media></Video></MediaContainer>`)
	if err := requireDecision(body, "remux"); err != nil {
		t.Fatal(err)
	}
	if err := requireDecision(body, "transcode"); err == nil {
		t.Fatal("accepted copy as transcode")
	}
	if err := requireDecision([]byte(`<MediaContainer generalDecisionCode="2000"/>`), "direct"); err == nil {
		t.Fatal("accepted error as direct playback")
	}
}
