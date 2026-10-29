package openapi

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// ValidExtra reports whether every actual occurrence of a collapsed JSON path
// is explicitly described by the matched response schema and validates there.
// Unknown schema constructs, absent paths and invalid JSON fail closed.
func (d *Document) ValidExtra(method, path string, status int, contentType string, body []byte, field string) bool {
	schema, err := d.responseSchema(method, path, status, contentType, body)
	if err != nil || schema == nil || !strings.HasPrefix(field, "$") {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var value any
	if dec.Decode(&value) != nil || dec.Decode(new(any)) != io.EOF {
		return false
	}
	rest := strings.TrimPrefix(field, "$")
	var parts []string
	if rest != "" {
		parts = strings.Split(strings.TrimPrefix(rest, "/"), "/")
	}
	found, valid := d.extraPath(schema, value, parts, 0)
	return found && valid
}

// property collects definitions from composed schemas; a typed dictionary is
// explicit too, but an unspecified additionalProperties is never sufficient.
func (d *Document) property(schema any, name string, depth int) []any {
	if depth > 128 {
		return nil
	}
	s, err := d.resolve(schema)
	if err != nil {
		return nil
	}
	var out []any
	if p, ok := obj(s["properties"])[name]; ok {
		out = append(out, p)
	} else if a, ok := s["additionalProperties"].(map[string]any); ok {
		out = append(out, a)
	}
	for _, k := range []string{"allOf", "oneOf", "anyOf"} {
		for _, sub := range array(s[k]) {
			out = append(out, d.property(sub, name, depth+1)...)
		}
	}
	return out
}

func (d *Document) extraPath(schema, value any, parts []string, depth int) (bool, bool) {
	if depth > 128 {
		return false, false
	}
	if len(parts) == 0 {
		return true, d.strictValue(schema, value, depth+1)
	}
	part := parts[0]
	if part == "" {
		return false, false
	}
	// Comparator represents each array dimension with a suffix on its path.
	name := strings.TrimSuffix(part, "[]")
	arrayPart := name != part
	var candidates []any
	v := value
	if name != "" {
		m, ok := value.(map[string]any)
		if !ok {
			return false, false
		}
		var exists bool
		v, exists = m[name]
		if !exists {
			return false, true
		}
		candidates = d.property(schema, name, depth+1)
	} else {
		candidates = []any{schema}
	}
	if len(candidates) == 0 {
		return true, false
	}
	for _, candidate := range candidates {
		if arrayPart {
			s, err := d.resolve(candidate)
			if err != nil || s["type"] != "array" || s["items"] == nil {
				continue
			}
			a, ok := v.([]any)
			if !ok {
				continue
			}
			if len(a) == 0 {
				continue
			}
			all, any := true, false
			for _, item := range a {
				f, valid := d.extraPath(s["items"], item, parts[1:], depth+1)
				any = any || f
				if f && !valid {
					all = false
				}
			}
			if any && all {
				return true, true
			}
		} else {
			f, valid := d.extraPath(candidate, v, parts[1:], depth+1)
			if f && valid {
				return true, true
			}
		}
	}
	return true, false
}

func (d *Document) strictValue(schema, value any, depth int) bool {
	if depth > 128 || d.validate(schema, value, "$", depth) != nil {
		return false
	}
	s, err := d.resolve(schema)
	if err != nil {
		return false
	}
	switch v := value.(type) {
	case map[string]any:
		for k, child := range v {
			options := d.property(schema, k, depth+1)
			ok := false
			for _, option := range options {
				if d.strictValue(option, child, depth+1) {
					ok = true
					break
				}
			}
			if !ok {
				return false
			}
		}
	case []any:
		item := s["items"]
		if item == nil {
			return false
		}
		for _, child := range v {
			if !d.strictValue(item, child, depth+1) {
				return false
			}
		}
	}
	return true
}
