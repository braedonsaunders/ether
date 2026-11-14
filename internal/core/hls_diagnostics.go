package core

import "time"

// HLSPreparationEvent records an observed server milestone, never client readiness.
type HLSPreparationEvent struct {
	Stage string    `json:"stage"`
	At    time.Time `json:"at"`
	Rung  string    `json:"rung,omitempty"`
}

// HLSDiagnosticRecord contains bounded local resource facts, never request URLs,
// source names, scoped credentials, or encoder command lines.
type HLSDiagnosticRecord struct {
	At       time.Time `json:"at"`
	Resource string    `json:"resource"`
	MIME     string    `json:"mimeType,omitempty"`
	Status   int       `json:"status,omitempty"`
	Bytes    int64     `json:"bytes,omitempty"`
	Codecs   string    `json:"codecs,omitempty"`
	Playlist string    `json:"playlist,omitempty"`
	Error    string    `json:"error,omitempty"`
}

type HLSPlaybackDiagnostics struct {
	Events  []HLSPreparationEvent `json:"events"`
	Records []HLSDiagnosticRecord `json:"records"`
}

type HLSDiagnosticSession interface {
	PlaybackDiagnostics() HLSPlaybackDiagnostics
}
