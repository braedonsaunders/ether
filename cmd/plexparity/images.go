package main

import (
	"encoding/base64"
	"fmt"
	"net/url"
)

func imagePaths() []string {
	var out []string
	for _, key := range []string{"movieKey", "showKey", "seasonKey", "episodeKey"} {
		for _, kind := range []string{"thumb", "art"} {
			out = append(out, fmt.Sprintf("/library/metadata/{{%s}}/%s", key, kind))
		}
	}
	out = append(out, "/library/metadata/{{movieKey}}/person/actor/"+base64.RawURLEncoding.EncodeToString([]byte("/fixture.jpg")), "/library/metadata/{{movieKey}}/discoveryPoster/1?kind=poster", "/library/metadata/{{movieKey}}/discoveryPoster/1?kind=backdrop")
	out = append(out, "/library/collections/{{collectionKey}}/thumb/1", "/library/collections/{{collectionKey}}/art/1", "/playlists/{{playlistKey}}/composite/1")
	for _, width := range []int{92, 342, 780} {
		out = append(out, fmt.Sprintf("/photo/:/transcode?url=%s&width=%d", url.QueryEscape("/library/metadata/")+"{{movieKey}}"+url.QueryEscape("/thumb"), width))
	}
	return out
}
