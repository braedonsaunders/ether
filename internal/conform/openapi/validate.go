// Package openapi validates JSON responses against OpenAPI 3 and Swagger 2 documents.
package openapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

type Document struct{ root map[string]any }

func Parse(data []byte) (*Document, error) {
	var root map[string]any
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(&root); err != nil {
		return nil, err
	}
	version, _ := root["openapi"].(string)
	swagger, _ := root["swagger"].(string)
	if !strings.HasPrefix(version, "3.") && swagger != "2.0" {
		return nil, fmt.Errorf("unsupported OpenAPI version")
	}
	return &Document{root}, nil
}
func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }
func (d *Document) resolve(v any) (map[string]any, error) {
	m := obj(v)
	if m == nil {
		return nil, fmt.Errorf("expected schema object")
	}
	if r, ok := m["$ref"].(string); ok {
		if !strings.HasPrefix(r, "#/") {
			return nil, fmt.Errorf("external reference not supported: %s", r)
		}
		var v any = d.root
		for _, part := range strings.Split(r[2:], "/") {
			part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
			v = obj(v)[part]
			if v == nil {
				return nil, fmt.Errorf("missing reference %s", r)
			}
		}
		return obj(v), nil
	}
	return m, nil
}
func pathMatches(pattern, path string) bool {
	a, b := strings.Split(strings.Trim(pattern, "/"), "/"), strings.Split(strings.Trim(path, "/"), "/")
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if strings.HasPrefix(a[i], "{") && strings.HasSuffix(a[i], "}") {
			if b[i] == "" {
				return false
			}
		} else if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Validate checks status and JSON schema. HLS routes are deliberately excluded.
func (d *Document) Validate(method, requestPath string, status int, contentType string, body []byte) error {
	u, err := url.Parse(requestPath)
	if err != nil {
		return err
	}
	path := u.Path
	if strings.HasSuffix(path, ".m3u8") || strings.HasSuffix(path, ".ts") || strings.HasSuffix(path, ".m4s") {
		return nil
	}
	paths := obj(d.root["paths"])
	var operation map[string]any
	for route, p := range paths {
		if pathMatches(route, path) {
			op := obj(obj(p)[strings.ToLower(method)])
			if op != nil {
				if operation != nil {
					return fmt.Errorf("ambiguous path %s", path)
				}
				operation = op
			}
		}
	}
	if operation == nil {
		return fmt.Errorf("no specification for %s %s", method, path)
	}
	responses := obj(operation["responses"])
	v := responses[strconv.Itoa(status)]
	if v == nil {
		v = responses[fmt.Sprintf("%dXX", status/100)]
	}
	if v == nil {
		v = responses["default"]
	}
	if v == nil {
		return fmt.Errorf("status %d not documented", status)
	}
	response, err := d.resolve(v)
	if err != nil {
		return err
	}
	schema := response["schema"]
	if content := obj(response["content"]); content != nil {
		media := strings.TrimSpace(strings.Split(contentType, ";")[0])
		entry := obj(content[media])
		if entry == nil {
			entry = obj(content["application/json"])
		}
		if entry == nil {
			if len(body) == 0 {
				return nil
			}
			return fmt.Errorf("content type %q not documented", media)
		}
		schema = entry["schema"]
	}
	if schema == nil {
		if len(body) != 0 {
			return fmt.Errorf("response has body but no schema")
		}
		return nil
	}
	var value any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err = dec.Decode(&value); err != nil {
		return fmt.Errorf("response JSON: %w", err)
	}
	if dec.Decode(new(any)) == nil {
		return fmt.Errorf("multiple JSON values")
	}
	return d.validate(schema, value, "$", 0)
}
func (d *Document) validate(schema any, v any, path string, depth int) error {
	if depth > 128 {
		return fmt.Errorf("%s: schema nesting exceeds limit", path)
	}
	s, err := d.resolve(schema)
	if err != nil {
		return err
	}
	if v == nil {
		if s["nullable"] == true || s["type"] == "null" {
			return nil
		}
		return fmt.Errorf("%s: null not allowed", path)
	}
	for _, kind := range []string{"allOf", "anyOf", "oneOf"} {
		if variants, ok := s[kind].([]any); ok {
			matches := 0
			for _, a := range variants {
				if d.validate(a, v, path, depth+1) == nil {
					matches++
				}
			}
			if (kind == "allOf" && matches != len(variants)) || (kind == "anyOf" && matches == 0) || (kind == "oneOf" && matches != 1) {
				return fmt.Errorf("%s: %s mismatch", path, kind)
			}
		}
	}
	if enum, ok := s["enum"].([]any); ok {
		value, _ := json.Marshal(v)
		found := false
		for _, e := range enum {
			b, _ := json.Marshal(e)
			if bytes.Equal(value, b) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%s: value outside enum", path)
		}
	}
	typ, _ := s["type"].(string)
	switch typ {
	case "", "object":
		m, ok := v.(map[string]any)
		if !ok {
			if typ == "object" {
				return fmt.Errorf("%s: expected object", path)
			}
			break
		}
		for _, r := range array(s["required"]) {
			name, _ := r.(string)
			if _, ok = m[name]; !ok {
				return fmt.Errorf("%s: missing %s", path, name)
			}
		}
		props := obj(s["properties"])
		for k, val := range m {
			if spec, ok := props[k]; ok {
				if err := d.validate(spec, val, path+"/"+k, depth+1); err != nil {
					return err
				}
			} else if s["additionalProperties"] == false {
				return fmt.Errorf("%s: unexpected %s", path, k)
			} else if add, ok := s["additionalProperties"].(map[string]any); ok {
				if err := d.validate(add, val, path+"/"+k, depth+1); err != nil {
					return err
				}
			}
		}
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return fmt.Errorf("%s: expected array", path)
		}
		if item := s["items"]; item != nil {
			for i, val := range arr {
				if err := d.validate(item, val, fmt.Sprintf("%s/%d", path, i), depth+1); err != nil {
					return err
				}
			}
		}
	case "string":
		if _, ok := v.(string); !ok {
			return fmt.Errorf("%s: expected string", path)
		}
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return fmt.Errorf("%s: expected integer", path)
		}
		if _, err := strconv.ParseInt(string(n), 10, 64); err != nil {
			return fmt.Errorf("%s: expected integer", path)
		}
	case "number":
		if _, ok := v.(json.Number); !ok {
			return fmt.Errorf("%s: expected number", path)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%s: expected boolean", path)
		}
	default:
		return fmt.Errorf("%s: unsupported schema type %q", path, typ)
	}
	return nil
}
func array(v any) []any { a, _ := v.([]any); return a }
