package config

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestSpeechAndSkipDBKeysProtected(t *testing.T) {
	var c Config
	c.LLM.SpeechAPIKey = "sk-speech-SECRET-123"
	c.Markers.SkipDBAPIKey = "skipdb-SECRET-456"
	vals := ProtectedValues(c)
	for _, s := range []string{c.LLM.SpeechAPIKey, c.Markers.SkipDBAPIKey} {
		if !slices.Contains(vals, s) {
			t.Errorf("%q not in ProtectedValues", s)
		}
	}
}

// Walk Config json tags for string fields whose name suggests a credential and
// verify secretField covers them (excluding explicit non-secret identifier tags).
func TestConfigSecretJSONTagsCovered(t *testing.T) {
	nonSecret := map[string]bool{
		"key":       true,
		"keyframe":  true,
		"publickey": true,
	}
	seen := map[reflect.Type]bool{}
	var walk func(typ reflect.Type, path string)
	walk = func(typ reflect.Type, path string) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || seen[typ] {
			return
		}
		seen[typ] = true
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if !f.IsExported() {
				continue
			}
			tag := strings.Split(f.Tag.Get("json"), ",")[0]
			if tag == "-" {
				continue
			}
			name := tag
			if name == "" {
				name = f.Name
			}
			full := name
			if path != "" {
				full = path + "." + name
			}
			if f.Type.Kind() == reflect.String {
				lower := strings.ToLower(name)
				if nonSecret[lower] {
					continue
				}
				if strings.Contains(lower, "apikey") || strings.Contains(lower, "secret") ||
					strings.Contains(lower, "token") || strings.Contains(lower, "password") ||
					strings.HasSuffix(lower, "key") {
					if !secretField(name) {
						t.Errorf("Config field %s (json %q) looks like a secret but is not matched by secretField", full, name)
					}
				}
				continue
			}
			walk(f.Type, full)
		}
	}
	walk(reflect.TypeOf(Config{}), "")
}
