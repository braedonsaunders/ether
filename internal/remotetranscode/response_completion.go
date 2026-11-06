package remotetranscode

import (
	"io"
	"net/http"
)

// RepresentationCompletion records one synchronous response read and body close.
// It contains no transport errors, URLs, credentials or playback-continuity proof.
// Byte counts alone cannot certify completion. Only an actual EOF without a
// read, downstream write, or close error can complete a known full representation.
type RepresentationCompletion struct {
	StatusCode                             int
	ContentLength, ReadBytes, WrittenBytes int64
	BodyPresent, EOF, Closed               bool
	ReadError, WriteError, CloseError      bool
	EncodedOrChunked, Trailers             bool
}

func (c RepresentationCompletion) Complete() bool {
	return c.StatusCode == http.StatusOK && c.ContentLength > 0 && c.BodyPresent && c.EOF && c.Closed && !c.ReadError && !c.WriteError && !c.CloseError && !c.EncodedOrChunked && !c.Trailers && c.ReadBytes == c.ContentLength && c.WrittenBytes == c.ContentLength
}

// ServeSegmentObserved has exactly the ordinary segment path and returns only
// after its response body is closed. It performs no retry or additional read.
func (s *Session) ServeSegmentObserved(w http.ResponseWriter, q *http.Request, n int) RepresentationCompletion {
	return s.serveRepresentation(w, q, message{Op: "segment", Segment: n})
}
func (s *Session) ServeRungSegmentObserved(w http.ResponseWriter, q *http.Request, rung string, n int) RepresentationCompletion {
	return s.serveRepresentation(w, q, message{Op: "rung-segment", Rung: rung, Segment: n})
}

type completionReader struct {
	source io.Reader
	facts  *RepresentationCompletion
}

func (r completionReader) Read(p []byte) (int, error) {
	n, err := r.source.Read(p)
	if n < 0 || n > len(p) {
		r.facts.ReadError = true
	} else {
		r.facts.ReadBytes += int64(n)
	}
	if err == io.EOF {
		r.facts.EOF = true
	} else if err != nil {
		r.facts.ReadError = true
	}
	return n, err
}

type completionWriter struct {
	target io.Writer
	facts  *RepresentationCompletion
}

func (w completionWriter) Write(p []byte) (int, error) {
	n, err := w.target.Write(p)
	if n < 0 || n > len(p) {
		w.facts.WriteError = true
	} else {
		w.facts.WrittenBytes += int64(n)
	}
	if err != nil || n != len(p) {
		w.facts.WriteError = true
	}
	return n, err
}
