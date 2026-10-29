package openapi

import "testing"

func TestResponseValidation(t *testing.T) {
	spec := []byte(`{"openapi":"3.0.3","paths":{"/Items/{id}":{"get":{"responses":{"200":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Item"}}}}}}}},"components":{"schemas":{"Item":{"type":"object","required":["Id"],"properties":{"Id":{"type":"string"},"Size":{"type":"integer"}}}}}}`)
	d, err := Parse(spec)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		status int
		body   string
		bad    bool
	}{{200, `{"Id":"abc","Size":2}`, false}, {200, `{"Size":2}`, true}, {200, `{"Id":1}`, true}, {404, `{}`, true}}
	for _, c := range cases {
		err = d.Validate("GET", "/Items/123", c.status, "application/json", []byte(c.body))
		if (err != nil) != c.bad {
			t.Errorf("%+v: %v", c, err)
		}
	}
	if err = d.Validate("GET", "/Items/123", 200, "text/plain", []byte(`{"Id":"abc"}`)); err == nil {
		t.Fatal("undocumented media type accepted")
	}
	if err = d.Validate("GET", "/Videos/abc/master.m3u8", 200, "application/vnd.apple.mpegurl", []byte("#EXTM3U")); err != nil {
		t.Fatal(err)
	}
}
func TestSwagger2(t *testing.T) {
	d, err := Parse([]byte(`{"swagger":"2.0","paths":{"/health":{"get":{"responses":{"200":{"schema":{"type":"array","items":{"type":"string"}}}}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Validate("GET", "/health", 200, "application/json", []byte(`["ok"]`)); err != nil {
		t.Fatal(err)
	}
	if err = d.Validate("GET", "/health", 200, "application/json", []byte(`[1]`)); err == nil {
		t.Fatal("bad item accepted")
	}
}

func TestLiteralPathPrecedence(t *testing.T) {
	spec := []byte(`{"openapi":"3.0.3","paths":{"/Users/Public":{"get":{"responses":{"200":{"content":{"application/json":{"schema":{"type":"array"}}}}}}},"/Users/{userId}":{"get":{"responses":{"200":{"content":{"application/json":{"schema":{"type":"object"}}}}}}}}}`)
	d, err := Parse(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Validate("GET", "/Users/Public", 200, "application/json", []byte(`[]`)); err != nil {
		t.Fatal(err)
	}
	if err := d.Validate("GET", "/Users/123", 200, "application/json", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
}
