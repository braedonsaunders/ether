package devreference

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// schemaFields documents requiredness from JSON Schema, never from omitempty.
func schemaFields(raw []byte) (string, error) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return "", err
	}
	var out strings.Builder
	out.WriteString("| Field | Type | Required in parent | Constraints and defaults |\n|---|---|---|---|\n")
	var walk func(map[string]any, string)
	walk = func(node map[string]any, prefix string) {
		props, _ := node["properties"].(map[string]any)
		required := map[string]bool{}
		if values, ok := node["required"].([]any); ok {
			for _, v := range values {
				required[fmt.Sprint(v)] = true
			}
		}
		keys := make([]string, 0, len(props))
		for key := range props {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			field, ok := props[key].(map[string]any)
			if !ok {
				continue
			}
			name := prefix + key
			typ := field["type"]
			if typ == nil {
				typ = "any"
			}
			constraints := map[string]any{}
			for k, v := range field {
				if k != "properties" && k != "items" && k != "type" {
					constraints[k] = v
				}
			}
			b, _ := json.Marshal(constraints)
			fmt.Fprintf(&out, "| `%s` | `%v` | %t | %s |\n", name, typ, required[key], strings.ReplaceAll(string(b), "|", "\\|"))
			walk(field, name+".")
			if items, ok := field["items"].(map[string]any); ok {
				walk(items, name+"[].")
			}
		}
	}
	walk(root, "")
	return out.String(), nil
}
