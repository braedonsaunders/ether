package mediabrowser

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"sort"
	"strconv"
	"sync"
)

// Item pages repeat the same catalogue fields for every viewer. Those bytes
// are cached per title until the catalogue generation changes. User data and
// image grants are appended per request, so a miss does not re-encode the
// static fragment through reflection.

type itemJSONCache struct {
	mu    sync.Mutex
	gen   uint64
	items map[string][]byte
}

func (c *itemJSONCache) get(gen uint64, key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != gen || c.items == nil {
		return nil, false
	}
	frag, ok := c.items[key]
	if !ok {
		return nil, false
	}
	return frag, true
}

func (c *itemJSONCache) put(gen uint64, key string, frag []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != gen || c.items == nil || len(c.items) >= 1024 {
		c.gen = gen
		c.items = map[string][]byte{}
	}
	c.items[key] = append([]byte(nil), frag...)
}

func dynamicItemKey(key string) bool {
	switch key {
	case "UserData", "ImageTags", "BackdropImageTags", "ImageBlurHashes", "PrimaryImageAspectRatio", "MediaSources", "Path":
		return true
	default:
		return false
	}
}

func (i *instance) replyItems(w http.ResponseWriter, r *http.Request, v any) {
	gen := uint64(0)
	if i.host.PageFreshness != nil {
		gen = i.host.PageFreshness().Catalog
	}
	lang := ""
	if r != nil {
		lang = device(r).Language
	}
	if body, ok := i.encodeItemJSON(gen, lang, v); ok {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(body)
		return
	}
	jsonReply(w, v)
}

func (i *instance) encodeItemJSON(gen uint64, lang string, v any) ([]byte, bool) {
	switch body := v.(type) {
	case []any:
		list, ok := i.encodeItemList(gen, lang, body)
		if !ok {
			return nil, false
		}
		return append(list, '\n'), true
	case map[string]any:
		if items, ok := body["Items"].([]any); ok {
			list, ok := i.encodeItemList(gen, lang, items)
			if !ok {
				return nil, false
			}
			shell := make(map[string]any, len(body))
			for key, value := range body {
				if key != "Items" {
					shell[key] = value
				}
			}
			prefix, err := json.Marshal(shell)
			if err != nil || len(prefix) == 0 || prefix[len(prefix)-1] != '}' {
				return nil, false
			}
			out := make([]byte, 0, len(prefix)+len(list)+10)
			out = append(out, prefix[:len(prefix)-1]...)
			if len(prefix) > 2 {
				out = append(out, ',')
			}
			out = append(out, `"Items":`...)
			out = append(out, list...)
			out = append(out, '}', '\n')
			return out, true
		}
		if _, ok := body["Id"].(string); ok {
			if _, ok := body["Type"].(string); ok {
				one, ok := i.encodeItemObject(gen, lang, body)
				if !ok {
					return nil, false
				}
				return append(one, '\n'), true
			}
		}
	}
	return nil, false
}

func (i *instance) encodeItemList(gen uint64, lang string, items []any) ([]byte, bool) {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for n, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, false
		}
		if _, ok := item["Id"].(string); !ok {
			return nil, false
		}
		if _, ok := item["Type"].(string); !ok {
			return nil, false
		}
		body, ok := i.encodeItemObject(gen, lang, item)
		if !ok {
			return nil, false
		}
		if n > 0 {
			buf.WriteByte(',')
		}
		buf.Write(body)
	}
	buf.WriteByte(']')
	return buf.Bytes(), true
}

func (i *instance) encodeItemObject(gen uint64, lang string, item map[string]any) ([]byte, bool) {
	id, _ := item["Id"].(string)
	static := make(map[string]any, len(item))
	dynamic := map[string]any{}
	for key, value := range item {
		if dynamicItemKey(key) {
			dynamic[key] = value
			continue
		}
		static[key] = value
	}
	var frag []byte
	cacheKey := fragmentKey(lang, static)
	if id != "" {
		if cached, ok := i.itemJSON.get(gen, cacheKey); ok {
			frag = cached
		}
	}
	if frag == nil {
		raw, err := json.Marshal(static)
		if err != nil || len(raw) < 2 || raw[0] != '{' || raw[len(raw)-1] != '}' {
			return nil, false
		}
		frag = raw[:len(raw)-1]
		if id != "" {
			i.itemJSON.put(gen, cacheKey, frag)
			if cached, ok := i.itemJSON.get(gen, cacheKey); ok {
				frag = cached
			}
		}
	}
	out := append([]byte(nil), frag...)
	if len(dynamic) > 0 {
		extra, err := json.Marshal(dynamic)
		if err != nil || len(extra) < 2 {
			return nil, false
		}
		if len(frag) > 1 {
			out = append(out, ',')
		}
		out = append(out, extra[1:len(extra)-1]...)
	}
	out = append(out, '}')
	return out, true
}

func fragmentKey(lang string, static map[string]any) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(lang))
	writeCanonical(h, static)
	return strconv.FormatUint(h.Sum64(), 16)
}

func writeCanonical(h interface{ Write([]byte) (int, error) }, value any) {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			_, _ = h.Write([]byte{0})
			_, _ = h.Write([]byte(key))
			writeCanonical(h, typed[key])
		}
	case map[string]string:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			_, _ = h.Write([]byte{0})
			_, _ = h.Write([]byte(key))
			_, _ = h.Write([]byte(typed[key]))
		}
	case []any:
		for _, item := range typed {
			_, _ = h.Write([]byte{1})
			writeCanonical(h, item)
		}
	case []string:
		for _, item := range typed {
			_, _ = h.Write([]byte{1})
			_, _ = h.Write([]byte(item))
		}
	case string:
		_, _ = h.Write([]byte(typed))
	case bool:
		if typed {
			_, _ = h.Write([]byte{2})
		} else {
			_, _ = h.Write([]byte{3})
		}
	case nil:
		_, _ = h.Write([]byte{4})
	default:
		_, _ = fmt.Fprintf(h, "%T:%v", typed, typed)
	}
}
