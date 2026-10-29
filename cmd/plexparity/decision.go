package main

import (
	"encoding/xml"
	"fmt"
)

// requireDecision rejects matching error responses and verifies which video
// operation the real transcoder chose for a fixture playback request.
func requireDecision(body []byte, mode string) error {
	var v struct {
		Videos []struct {
			Media []struct {
				Parts []struct {
					Decision string `xml:"decision,attr"`
					Streams  []struct {
						Type     int    `xml:"streamType,attr"`
						Decision string `xml:"decision,attr"`
					} `xml:"Stream"`
				} `xml:"Part"`
			} `xml:"Media"`
		} `xml:"Video"`
	}
	if err := xml.Unmarshal(body, &v); err != nil {
		return err
	}
	for _, video := range v.Videos {
		for _, media := range video.Media {
			for _, part := range media.Parts {
				if mode == "direct" && part.Decision == "directplay" {
					return nil
				}
				if mode != "direct" && part.Decision == "transcode" {
					for _, stream := range part.Streams {
						if stream.Type == 1 && ((mode == "remux" && stream.Decision == "copy") || (mode == "transcode" && stream.Decision == "transcode")) {
							return nil
						}
					}
				}
			}
		}
	}
	return fmt.Errorf("fixture did not select %s", mode)
}
