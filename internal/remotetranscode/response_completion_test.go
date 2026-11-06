package remotetranscode

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

type completionTestBody struct {
	data string
	err  error
}

func (b *completionTestBody) Read(p []byte) (int, error) {
	if b.data == "" {
		return 0, io.EOF
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, b.err
}
func TestRepresentationCompletionReadErrors(t *testing.T) {
	for _, fail := range []bool{false, true} {
		facts := RepresentationCompletion{StatusCode: 200, ContentLength: 4, BodyPresent: true, Closed: true}
		body := &completionTestBody{data: "full"}
		if fail {
			body.err = io.ErrUnexpectedEOF
		}
		_, _ = io.Copy(completionWriter{target: io.Discard, facts: &facts}, completionReader{source: body, facts: &facts})
		if facts.Complete() == fail {
			t.Fatal("full bytecount hid read error", facts)
		}
		facts.CloseError = true
		if facts.Complete() {
			t.Fatal("close error accepted")
		}
	}
}
func TestRepresentationCompletionRejectsMissingEvidence(t *testing.T) {
	good := RepresentationCompletion{StatusCode: 200, ContentLength: 4, ReadBytes: 4, WrittenBytes: 4, BodyPresent: true, EOF: true, Closed: true}
	for _, change := range []func(*RepresentationCompletion){func(c *RepresentationCompletion) { c.EOF = false }, func(c *RepresentationCompletion) { c.BodyPresent = false }, func(c *RepresentationCompletion) { c.Closed = false }, func(c *RepresentationCompletion) { c.WriteError = true }, func(c *RepresentationCompletion) { c.ReadError = true }, func(c *RepresentationCompletion) { c.CloseError = true }, func(c *RepresentationCompletion) { c.Trailers = true }, func(c *RepresentationCompletion) { c.EncodedOrChunked = true }, func(c *RepresentationCompletion) { c.ContentLength = -1 }, func(c *RepresentationCompletion) { c.StatusCode = 206 }} {
		c := good
		change(&c)
		if c.Complete() {
			t.Fatal(c)
		}
	}
}

type completionRoundTrip func(*http.Request) (*http.Response, error)

func (f completionRoundTrip) RoundTrip(q *http.Request) (*http.Response, error) { return f(q) }

type completionCloseBody struct {
	*completionTestBody
	closeError bool
	closed     bool
}

func (b *completionCloseBody) Close() error {
	b.closed = true
	if b.closeError {
		return io.ErrClosedPipe
	}
	return nil
}
func TestRepresentationCompletionObservedServing(t *testing.T) {
	for _, name := range []string{"good", "read-error", "close-error", "unknown", "chunked", "trailer", "head", "206"} {
		t.Run(name, func(t *testing.T) {
			body := &completionCloseBody{completionTestBody: &completionTestBody{data: "full"}}
			response := &http.Response{StatusCode: 200, ContentLength: 4, Header: http.Header{"Content-Type": []string{"video/mp2t"}, "Content-Length": []string{"4"}}, Body: body}
			q := httptest.NewRequest("GET", "/segment", nil)
			switch name {
			case "read-error":
				body.err = io.ErrUnexpectedEOF
			case "close-error":
				body.closeError = true
			case "unknown":
				response.ContentLength = -1
				response.Header.Del("Content-Length")
			case "chunked":
				response.TransferEncoding = []string{"chunked"}
			case "trailer":
				response.Trailer = http.Header{"Digest": []string{"bad"}}
			case "head":
				q.Method = "HEAD"
			case "206":
				response.StatusCode = 206
			}
			calls := 0
			session := &Session{adapter: &Adapter{client: &http.Client{Transport: completionRoundTrip(func(*http.Request) (*http.Response, error) { calls++; return response, nil })}}}
			w := httptest.NewRecorder()
			facts := session.ServeSegmentObserved(w, q, 0)
			if calls != 1 || !body.closed || !facts.Closed || facts.Complete() != (name == "good") {
				t.Fatal(name, calls, facts)
			}
			if w.Code != response.StatusCode || w.Header().Get("Content-Type") != "video/mp2t" {
				t.Fatal("response forwarding changed")
			}
			if name != "head" && w.Body.String() != "full" {
				t.Fatal("bytes changed")
			}
		})
	}
}
