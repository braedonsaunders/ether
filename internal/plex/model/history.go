package model

// HistoryMetadata is a viewing event projection, with its own field order.
// The list and single-entry endpoints share the display fields; only lists
// supply navigation and artwork.
type HistoryMetadata struct {
	HistoryKey            string `plex:"historyKey"`
	Key                   string `plex:"key,omitempty"`
	RatingKey             string `plex:"ratingKey,omitempty"`
	LibrarySectionID      string `plex:"librarySectionID"`
	ParentKey             string `plex:"parentKey,omitempty"`
	GrandparentKey        string `plex:"grandparentKey,omitempty"`
	Title                 string `plex:"title"`
	GrandparentTitle      string `plex:"grandparentTitle,omitempty"`
	Type                  string `plex:"type"`
	Thumb                 string `plex:"thumb,omitempty"`
	ParentThumb           string `plex:"parentThumb,omitempty"`
	GrandparentThumb      string `plex:"grandparentThumb,omitempty"`
	Index                 *int   `plex:"index"`
	ParentIndex           *int   `plex:"parentIndex"`
	OriginallyAvailableAt string `plex:"originallyAvailableAt,omitempty"`
	ViewedAt              int64  `plex:"viewedAt"`
	AccountID             int64  `plex:"accountID"`
	DeviceID              int64  `plex:"deviceID"`
	ViewType              *int   `plex:"viewType"`
}

func (h HistoryMetadata) XMLElementName() string { return Metadata{Type: h.Type}.XMLElementName() }
