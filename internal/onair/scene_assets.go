package onair

import (
	"github.com/braedonsaunders/ether/internal/onair/scene"
	"reflect"
)

// Only source and font properties reference assets. Feed versions and ordinary
// text can also be 64-character strings; their length never makes them assets.
func collectSceneAssets(layers []scene.Layer, bindings scene.Bindings, refs map[string]string) {
	var collect func(any, bool)
	collect = func(v any, font bool) {
		switch x := v.(type) {
		case string:
			d, _ := scene.Eval(x, bindings, 0).(string)
			if d != "" && (!font || len(d) == 64) {
				refs[d] = d
			}
		case map[string]any:
			if keys, ok := x["keys"].([]any); ok {
				for _, key := range keys {
					if entry, ok := key.([]any); ok && len(entry) >= 2 {
						collect(entry[1], font)
					}
				}
			}
		}
	}
	for _, layer := range layers {
		collect(layer.Src, false)
		collect(layer.Font, true)
		collectSceneAssets(layer.Layers, bindings, refs)
	}
}

// Be conservative: any keyframes, procedural layer, behaviour or post effect
// retains the normal per-frame renderer. This optimization is plugin-only.
func staticPluginScene(doc scene.Document) bool {
	if len(doc.Post) != 0 {
		return false
	}
	var check func([]scene.Layer) bool
	check = func(layers []scene.Layer) bool {
		for _, layer := range layers {
			switch layer.Type {
			case "rect", "ellipse", "path", "text", "image", "art", "logo", "gradient", "group":
			default:
				return false
			}
			if len(layer.Behave) > 0 || layer.Reveal != nil || layer.Start > 0 || layer.End > 0 {
				return false
			}
			value := reflect.ValueOf(layer)
			for i := 0; i < value.NumField(); i++ {
				field := value.Field(i)
				if field.Kind() == reflect.Interface && !field.IsNil() {
					kind := reflect.ValueOf(field.Interface()).Kind()
					if kind == reflect.Map || kind == reflect.Slice {
						return false
					}
				}
			}
			if !check(layer.Layers) {
				return false
			}
		}
		return true
	}
	return check(doc.Layers)
}
