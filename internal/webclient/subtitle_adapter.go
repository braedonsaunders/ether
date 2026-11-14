package webclient

import (
	_ "embed"
	"fmt"
	"strings"
)

//go:embed subtitle_adapter.js
var subtitleAdapter string

// AdaptSubtitles adds an explicit progressive-text capability and a reader for
// it to the pinned client. Changes are made in memory, leaving the verified
// upstream install intact. Exact anchors fail closed on an upstream upgrade.
func AdaptSubtitles(name string, raw []byte) ([]byte, error) {
	var replacements [][2]string
	prefix := ""
	switch name {
	case "index.html":
		replacements = [][2]string{
			{`src="main.jellyfin.bundle.js?`, `src="main.jellyfin.bundle.js?ether_subs=1&`},
			{`src="runtime.bundle.js?`, `src="runtime.bundle.js?ether_subs=1&`},
		}
	case "runtime.bundle.js":
		replacements = [][2]string{{`s.miniCssF=`, `s.u=(function(original){return function(id){var name=original(id);return name.startsWith("htmlVideoPlayer-plugin.")?name+"?ether_subs=1":name}})(s.u),s.miniCssF=`}}
	case "main.jellyfin.bundle.js":
		replacements = [][2]string{{"E.SubtitleProfiles=[];", `E.SubtitleProfiles=[{Format:"ether-progressive-vtt",Method:"External"}];`}}
	case "htmlVideoPlayer-plugin.02b37b7cfd0e06696bd1.chunk.js":
		replacements = [][2]string{
			{`function(e,t,n){return F.apply(this,arguments)}`, `function(e,t,n){if(t.DeliveryUrl&&t.DeliveryUrl.includes("ether_stream=1")){window.__etherSubtitles.start(this,e,t,n,arguments[3]||0,ne(t,n,".js"));return Promise.resolve()}return F.apply(this,arguments)}`},
			{`key:"destroyCustomTrack",value:function(e,t){`, `key:"destroyCustomTrack",value:function(e,t){window.__etherSubtitles.stop(this,t);`},
		}
		prefix = subtitleAdapter + "\n"
	default:
		return raw, nil
	}
	text := string(raw)
	for _, pair := range replacements {
		if strings.Count(text, pair[0]) != 1 {
			return nil, fmt.Errorf("unsupported Jellyfin Web subtitle adapter anchor in %s", name)
		}
		text = strings.Replace(text, pair[0], pair[1], 1)
	}
	return []byte(prefix + text), nil
}

func SubtitleAdapterAsset(name string) bool {
	return name == "index.html" || name == "runtime.bundle.js" || name == "main.jellyfin.bundle.js" || name == "htmlVideoPlayer-plugin.02b37b7cfd0e06696bd1.chunk.js"
}
