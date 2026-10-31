package openapi

import "testing"

func TestValidExtraRouteAndNestedValues(t *testing.T) {
	spec := `{"swagger":"2.0","paths":{"/Items":{"get":{"responses":{"200":{"schema":{"$ref":"#/definitions/Page"}}}}},"/Items/{id}":{"get":{"responses":{"200":{"schema":{"$ref":"#/definitions/Item"}}}}}},"definitions":{"Page":{"type":"object","properties":{"Items":{"type":"array","items":{"$ref":"#/definitions/Item"}}}},"Item":{"type":"object","properties":{"Overview":{"type":"string"},"ImageTags":{"type":"object","additionalProperties":{"type":"string"}},"Genres":{"type":"array","items":{"type":"string"}},"Nested":{"type":"object","properties":{"Good":{"type":"boolean"}}}}}}}`
	doc, err := Parse([]byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path, body, field string
		want              bool
	}{
		{"/Items", `{"Items":[{"Overview":"one"},{"Overview":"two"}]}`, "$/Items[]/Overview", true},
		{"/Items", `{"Items":[{},{"Overview":"one"}]}`, "$/Items[]/Overview", true},
		{"/Items", `{"Items":[{"Overview":"one"},{"Overview":2}]}`, "$/Items[]/Overview", false},
		{"/Items", `{"Items":[{"Nested":{}},{"Nested":{"Good":true}}]}`, "$/Items[]/Nested/Good", true},
		{"/Items", `{"Items":[{"Nested":{"Good":false}},{"Nested":{"Good":true}}]}`, "$/Items[]/Nested/Good", true},
		{"/Items", `{"Items":[{"Nested":{"Good":"no"}},{"Nested":{"Good":true}}]}`, "$/Items[]/Nested/Good", false},
		{"/Items", `{"Items":[{"ImageTags":{"Primary":"x"}}]}`, "$/Items[]/ImageTags", true},
		{"/Items", `{"Items":[{"ImageTags":{"Primary":2}}]}`, "$/Items[]/ImageTags", false},
		{"/Items", `{"Items":[{"Genres":["a","b"]}]}`, "$/Items[]/Genres[]", true},
		{"/Items", `{"Items":[{"Genres":["a",2]}]}`, "$/Items[]/Genres[]", false},
		{"/Items", `{"Items":[{"Nested":{"Good":true,"Unknown":1}}]}`, "$/Items[]/Nested", false},
		{"/Items", `{"Items":[{"Unknown":true}]}`, "$/Items[]/Unknown", false},
		{"/Items/42", `{"Overview":"x"}`, "$/Overview", true},
		{"/Items", `{"Overview":"x"}`, "$/Overview", false},
	}
	for _, tc := range cases {
		if got := doc.ValidExtra("GET", tc.path, 200, "application/json", []byte(tc.body), tc.field); got != tc.want {
			t.Errorf("%s %s: got %v want %v", tc.path, tc.field, got, tc.want)
		}
	}
}
