package mediabrowser

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestItemJSONReusesTheStaticFragment(t *testing.T) {
	inst := &instance{}
	item := map[string]any{
		"Id": "1", "Type": "Movie", "Name": "Big Buck Bunny", "Overview": "A large rabbit.",
		"UserData":  map[string]any{"Played": false, "PlayCount": 0},
		"ImageTags": map[string]string{"Primary": "e1.one"},
	}
	r := httptest.NewRequest("GET", "/Items", nil)
	w := httptest.NewRecorder()
	inst.replyItems(w, r, []any{item})
	var first []map[string]any
	if json.Unmarshal(w.Body.Bytes(), &first) != nil || first[0]["Name"] != "Big Buck Bunny" || first[0]["UserData"].(map[string]any)["Played"] != false {
		t.Fatal(w.Body.String())
	}
	item["UserData"] = map[string]any{"Played": true, "PlayCount": 1}
	item["ImageTags"] = map[string]string{"Primary": "e1.two"}
	w = httptest.NewRecorder()
	inst.replyItems(w, r, map[string]any{"Items": []any{item}, "TotalRecordCount": 1})
	var page map[string]any
	if json.Unmarshal(w.Body.Bytes(), &page) != nil || page["TotalRecordCount"] != float64(1) {
		t.Fatal(w.Body.String())
	}
	items := page["Items"].([]any)
	got := items[0].(map[string]any)
	if got["Name"] != "Big Buck Bunny" || got["Overview"] != "A large rabbit." || got["UserData"].(map[string]any)["Played"] != true || got["ImageTags"].(map[string]any)["Primary"] != "e1.two" {
		t.Fatal(w.Body.String())
	}
	if len(inst.itemJSON.items) != 1 {
		t.Fatalf("fragments %d", len(inst.itemJSON.items))
	}
}
