package mediabrowser

import "fmt"

// PlaybackOffer contains only the fields used by audited client selection.
// It describes a requested delivery, not proof of the server's actual encode.
type PlaybackOffer struct {
	ID                   string `json:"Id"`
	Protocol             string `json:"Protocol"`
	Path                 string `json:"Path"`
	IsRemote             bool   `json:"IsRemote"`
	SupportsDirectPlay   bool   `json:"SupportsDirectPlay"`
	SupportsDirectStream bool   `json:"SupportsDirectStream"`
	SupportsTranscoding  bool   `json:"SupportsTranscoding"`
	TranscodingURL       string `json:"TranscodingUrl"`
}

// RequestedDelivery models the documented source-derived VOD behavior for
// the concrete configurations in deviceprofiles. Unknown clients and missing
// compatible paths fail closed; server decisions must be verified separately.
func RequestedDelivery(profile string, offer PlaybackOffer) (string, error) {
	switch profile {
	case "jellycon-default":
		if offer.ID == "" {
			return "", fmt.Errorf("media source ID unavailable")
		}
		if offer.SupportsDirectPlay || offer.SupportsDirectStream {
			return "direct", nil
		}
		if offer.SupportsTranscoding {
			return "transcode", nil
		}
	case "streamyfin-android-exoplayer":
		if offer.TranscodingURL != "" {
			return "transcode", nil
		}
		return "direct", nil
	default:
		return "", fmt.Errorf("unsupported client profile")
	}
	return "", fmt.Errorf("client has no playable delivery")
}
