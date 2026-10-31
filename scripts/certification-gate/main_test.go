package main

import (
	"os"
	"strings"
	"testing"
)

func TestPublishedMatrixMatchesTheUserGuide(t *testing.T) {
	doc, err := loadMatrix("../../docs/certification/matrix.json")
	if err != nil {
		t.Fatal(err)
	}
	guide, err := readGuide()
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"v0.2.48", "v0.2.0", "v0.2.0-beta.1"} {
		if err := evaluate(tag, doc, guide); err != nil {
			t.Fatalf("%s: %v", tag, err)
		}
	}
	for _, tag := range []string{"v0.3.0", "v0.3.1", "v0.3.0-beta.1", "v0.10.0", "v1.0.0"} {
		err := evaluate(tag, doc, guide)
		if err == nil || !strings.Contains(err.Error(), "recertified_for") {
			t.Fatalf("%s: %v", tag, err)
		}
	}
}

func TestReviewedMinorPasses(t *testing.T) {
	doc := reviewedDoc()
	guide := guideFor(doc)
	if err := evaluate("v0.3.0", doc, guide); err != nil {
		t.Fatal(err)
	}
	if err := evaluate("v0.3.4", doc, guide); err != nil {
		t.Fatal(err)
	}
	doc.RecertifiedFor = []string{"0.2"}
	if err := evaluate("v0.3.0", doc, guide); err == nil {
		t.Fatal("older minor must not satisfy 0.3")
	}
}

func TestShapeRejectsPartialPendingAndBareWaiver(t *testing.T) {
	doc := reviewedDoc()
	doc.Protocols[0].Clients[0].Version = "Pending"
	doc.Protocols[0].Clients[0].Results["browse"] = "pass"
	if err := validateMatrix(doc); err == nil {
		t.Fatal("pending version with a pass cell accepted")
	}
	doc = reviewedDoc()
	for feature := range doc.Protocols[0].Clients[0].Results {
		doc.Protocols[0].Clients[0].Results[feature] = "waived"
	}
	if err := validateMatrix(doc); err == nil {
		t.Fatal("waiver without a reason accepted")
	}
	doc.Protocols[0].Clients[0].Support = "waived"
	doc.Protocols[0].Clients[0].Waiver = "no device in the lab"
	doc.Protocols[0].Clients[0].Version = "1.2.3"
	if err := validateMatrix(doc); err != nil {
		t.Fatal(err)
	}
	if err := evaluate("v0.3.0", doc, guideFor(doc)); err != nil {
		t.Fatal(err)
	}
}

func TestGuideDriftFails(t *testing.T) {
	doc := reviewedDoc()
	guide := strings.Replace(guideFor(doc), "Swiftfin", "Swiftfin Beta", 1)
	if err := evaluate("v0.2.1", doc, guide); err == nil {
		t.Fatal("renamed guide row accepted")
	}
}

func TestBadTag(t *testing.T) {
	doc := reviewedDoc()
	if err := evaluate("0.3.0", doc, guideFor(doc)); err == nil {
		t.Fatal("accepted a tag without v")
	}
	if err := evaluate("v0.3", doc, guideFor(doc)); err == nil {
		t.Fatal("accepted a tag without a patch")
	}
}

func readGuide() (string, error) {
	data, err := os.ReadFile("../../docs/user-guide.md")
	return string(data), err
}

func reviewedDoc() matrixDoc {
	doc, err := loadMatrix("../../docs/certification/matrix.json")
	if err != nil {
		panic(err)
	}
	doc.RecertifiedFor = []string{"0.3"}
	for pi := range doc.Protocols {
		for ci := range doc.Protocols[pi].Clients {
			client := &doc.Protocols[pi].Clients[ci]
			if client.Support == "unsupported" {
				continue
			}
			client.Version = "1.0.0"
			for feature := range client.Results {
				client.Results[feature] = "pass"
			}
		}
	}
	return doc
}

func guideFor(doc matrixDoc) string {
	var b strings.Builder
	for _, protocol := range doc.Protocols {
		b.WriteString("<!-- certification-matrix:" + protocol.ID + " -->\n")
		header := append([]string{"Client", "Version"}, labels(protocol.Features)...)
		b.WriteString("| " + strings.Join(header, " | ") + " |\n")
		b.WriteString("|" + strings.Repeat(" --- |", len(header)) + "\n")
		for _, client := range protocol.Clients {
			cells := []string{client.Name, client.Version}
			for _, feature := range protocol.Features {
				cells = append(cells, client.Results[feature])
			}
			b.WriteString("| " + strings.Join(cells, " | ") + " |\n")
		}
		b.WriteString("<!-- /certification-matrix:" + protocol.ID + " -->\n")
	}
	return b.String()
}
