// Package capture records sanitized HTTP traffic while proxying client requests.
package capture

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/braedonsaunders/ether/internal/conform"
	"github.com/braedonsaunders/ether/internal/conform/mediabrowser"
)

// Entry is a single ordered request and response. Bodies are always redacted;
// binary bodies are represented by bounded hashes, never bytes.
type Entry struct {
	Sequence          int              `json:"sequence"`
	Protocol          string           `json:"protocol"`
	Client            string           `json:"client"`
	Request           Request          `json:"request"`
	Response          conform.Response `json:"response"`
	ResponseOrigin    string           `json:"response_origin,omitempty"`
	RequestTruncated  bool             `json:"request_truncated,omitempty"`
	ResponseTruncated bool             `json:"response_truncated,omitempty"`
}
type Request struct {
	Method     string      `json:"method"`
	Path       string      `json:"path"`
	Headers    http.Header `json:"headers"`
	Body       []byte      `json:"body,omitempty"`
	BodyKind   string      `json:"body_kind,omitempty"`
	BodyLength int64       `json:"body_length,omitempty"`
	SHA256     string      `json:"sha256,omitempty"`
}
type Recorder struct {
	mu                    sync.Mutex
	protocol, client, dir string
	sequence              int
	ids                   map[string]string
	secrets               map[string]bool
	proxy                 *httputil.ReverseProxy
	errors                chan error
}

var slug = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
var opaqueID = regexp.MustCompile(`(?i)^(?:[0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)
var opaqueTextID = regexp.MustCompile(`(?i)\b(?:[0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\b`)
var textDate = regexp.MustCompile(`\b[0-9]{4}-[0-9]{2}-[0-9]{2}(?:T[0-9:.+Z-]+)?\b`)
var hostPath = regexp.MustCompile(`(?i)/(?:users|home|mnt|media|srv|volumes)/[^\s"<>]+`)
var inlineSecret = regexp.MustCompile(`(?i)\b(st|api_key|access_token|token)=([^&\s"\x27]+)`)
var secretQuery = regexp.MustCompile(`(?i)([?&](?:api_key|access_token|token|authorization|st)=)[^&\s"']+`)

const textLimit = 16 << 20
const binaryLimit = 65536

// New constructs a proxy without opening a listener.
func New(protocol, client, dir string, upstream *url.URL) (*Recorder, error) {
	switch protocol {
	case "jellyfin", "emby", "silo":
	default:
		return nil, fmt.Errorf("unknown protocol %q", protocol)
	}
	if !slug.MatchString(client) {
		return nil, fmt.Errorf("invalid client name")
	}
	if upstream == nil || (upstream.Scheme != "http" && upstream.Scheme != "https") || upstream.Host == "" || upstream.User != nil {
		return nil, fmt.Errorf("invalid upstream URL")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	r := &Recorder{protocol: protocol, client: client, dir: dir, ids: map[string]string{}, secrets: map[string]bool{}, errors: make(chan error, 1)}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		var n int
		if _, e := fmt.Sscanf(filepath.Base(f), "%d.json", &n); e == nil && n > r.sequence {
			r.sequence = n
		}
	}
	r.proxy = httputil.NewSingleHostReverseProxy(upstream)
	r.proxy.ErrorHandler = func(w http.ResponseWriter, req *http.Request, e error) {
		if s, ok := req.Context().Value(stateKey{}).(*state); ok {
			s.entry.Response.Status = http.StatusBadGateway
			s.entry.ResponseOrigin = "proxy-error"
			s.finish()
		}
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}
	r.proxy.ModifyResponse = func(resp *http.Response) error {
		s, ok := resp.Request.Context().Value(stateKey{}).(*state)
		if !ok {
			return nil
		}
		s.entry.Response.Status = resp.StatusCode
		s.entry.ResponseOrigin = "reference"
		s.entry.Response.Headers = safeResponseHeaders(resp.Header)
		if location := resp.Header.Get("Location"); location != "" {
			if u, e := url.Parse(location); e == nil {
				r.mu.Lock()
				safe := r.safePath(u)
				r.mu.Unlock()
				s.entry.Response.Headers.Set("Location", safe)
			}
		}
		if resp.StatusCode == http.StatusSwitchingProtocols {
			s.entry.Response.Kind = "websocket"
			s.finish()
			return nil
		}
		isText := textContent(resp.Header.Get("Content-Type")) && !binaryRequest(s.entry.Request)
		s.response = &collector{limit: binaryLimit}
		if isText {
			s.response.limit = textLimit
		}
		s.textResponse = isText
		s.responseEncoding = resp.Header.Get("Content-Encoding")
		if isText && s.responseEncoding != "" && !strings.EqualFold(s.responseEncoding, "identity") {
			s.entry.Response.Headers.Del("Content-Length")
		}
		resp.Body = &readCapture{ReadCloser: resp.Body, c: s.response, onClose: s.finish}
		return nil
	}
	return r, nil
}

// Errors reports a persistent capture failure so the listener can stop.
func (r *Recorder) Errors() <-chan error { return r.errors }

type stateKey struct{}
type state struct {
	rec               *Recorder
	entry             Entry
	request, response *collector
	textResponse      bool
	responseEncoding  string
	once              sync.Once
}

func (r *Recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.sequence++
	seq := r.sequence
	r.addSecrets(req)
	path := r.safePath(req.URL)
	secrets := r.secretList()
	r.mu.Unlock()
	state := &state{rec: r, entry: Entry{Sequence: seq, Protocol: r.protocol, Client: r.client, Request: Request{Method: req.Method, Path: path, Headers: safeRequestHeaders(req.Header, secrets)}}}
	if req.Body != nil && (req.ContentLength != 0 || len(req.TransferEncoding) > 0) {
		limit := textLimit
		if !textContent(req.Header.Get("Content-Type")) {
			limit = binaryLimit
		}
		state.request = &collector{limit: limit}
		req.Body = &readCapture{ReadCloser: req.Body, c: state.request}
	}
	req = req.WithContext(context.WithValue(req.Context(), stateKey{}, state))
	r.proxy.ServeHTTP(w, req)
}

type collector struct {
	mu       sync.Mutex
	limit    int
	total    int64
	buf      []byte
	complete bool
}

func (c *collector) add(p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total += int64(len(p))
	n := c.limit - len(c.buf)
	if n > len(p) {
		n = len(p)
	}
	if n > 0 {
		c.buf = append(c.buf, p[:n]...)
	}
}
func (c *collector) snapshot() ([]byte, int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf...), c.total, c.complete
}

type readCapture struct {
	io.ReadCloser
	c       *collector
	onClose func()
	once    sync.Once
}

func (r *readCapture) Read(p []byte) (int, error) {
	n, e := r.ReadCloser.Read(p)
	if n > 0 {
		r.c.add(p[:n])
	}
	if e == io.EOF {
		r.c.mu.Lock()
		r.c.complete = true
		r.c.mu.Unlock()
		if r.onClose != nil {
			r.once.Do(r.onClose)
		}
	}
	return n, e
}
func (r *readCapture) Close() error {
	err := r.ReadCloser.Close()
	if r.onClose != nil {
		r.once.Do(r.onClose)
	}
	return err
}
func textContent(contentType string) bool {
	m, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		m = strings.TrimSpace(strings.Split(contentType, ";")[0])
	}
	return m == "application/x-www-form-urlencoded" || m == "application/json" || strings.HasSuffix(m, "+json") || m == "application/xml" || m == "text/xml" || strings.HasPrefix(m, "text/") || m == "application/vnd.apple.mpegurl" || m == "application/x-mpegurl"
}
func (r *Recorder) addSecrets(req *http.Request) {
	for _, name := range []string{"Authorization", "X-Emby-Token", "X-Plex-Token", "Cookie"} {
		value := req.Header.Get(name)
		if value != "" {
			r.secrets[value] = true
		}
		if strings.HasPrefix(strings.ToLower(value), "bearer ") {
			r.secrets[strings.TrimSpace(value[7:])] = true
		}
		for _, part := range strings.Split(value, ",") {
			kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
			if len(kv) == 2 && strings.Contains(strings.ToLower(kv[0]), "token") {
				r.secrets[strings.Trim(kv[1], ` "`)] = true
			}
		}
	}
	for k, values := range req.URL.Query() {
		if sensitiveKey(k) {
			for _, v := range values {
				if v != "" {
					r.secrets[v] = true
				}
			}
		}
	}
}
func (r *Recorder) secretList() []string {
	var out []string
	for s := range r.secrets {
		if s != "" {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}
func sensitiveKey(k string) bool {
	n := strings.ToLower(k)
	return strings.Contains(n, "token") || strings.Contains(n, "password") || n == "pw" || n == "api_key" || n == "apikey" || n == "st" || n == "authorization" || n == "username" || n == "cookie"
}
func (r *Recorder) slot(value string) string {
	if v, ok := r.ids[value]; ok {
		return v
	}
	v := fmt.Sprintf("id_%d", len(r.ids)+1)
	r.ids[value] = v
	return v
}
func (r *Recorder) safePath(u *url.URL) string {
	parts := strings.Split(u.EscapedPath(), "/")
	for i, part := range parts {
		decoded, err := url.PathUnescape(part)
		if err == nil && (opaqueID.MatchString(decoded) || (i > 0 && userPathID(parts[i-1], decoded))) {
			parts[i] = "${" + r.slot(decoded) + "}"
		} else if err == nil {
			safe := decoded
			for secret := range r.secrets {
				if secret != "" && strings.Contains(safe, secret) {
					safe = strings.ReplaceAll(safe, secret, "[REDACTED]")
				}
			}
			safe = string(conform.Redact([]byte(safe)))
			if safe != decoded {
				parts[i] = url.PathEscape(safe)
			}
		}
	}
	path := strings.Join(parts, "/")
	query := u.Query()
	for k, vs := range query {
		for i, v := range vs {
			switch {
			case sensitiveKey(k):
				switch strings.ToLower(k) {
				case "st":
					vs[i] = "${stream_token}"
				case "username":
					vs[i] = "${username}"
				case "password", "pw":
					vs[i] = "${password}"
				case "cookie":
					vs[i] = "${cookie}"
				default:
					vs[i] = "${access_token}"
				}
			case strings.Contains(strings.ToLower(k), "path") || strings.Contains(strings.ToLower(k), "url") || strings.HasSuffix(strings.ToLower(k), "uri"):
				vs[i] = "${path}"
			case (strings.Contains(strings.ToLower(k), "date") || strings.Contains(strings.ToLower(k), "timestamp") || textDate.MatchString(v)) && v != "":
				vs[i] = "${date}"
			case (strings.HasSuffix(strings.ToLower(k), "id") || strings.HasSuffix(strings.ToLower(k), "_id")) && v != "":
				vs[i] = "${" + r.slot(v) + "}"
			case opaqueID.MatchString(v):
				vs[i] = "${" + r.slot(v) + "}"
			default:
				vs[i] = string(conform.Redact([]byte(v)))
			}
		}
		query[k] = vs
	}
	if len(query) > 0 {
		q := query.Encode()
		q = strings.ReplaceAll(q, "%24%7B", "${")
		q = strings.ReplaceAll(q, "%7D", "}")
		path += "?" + q
	}
	return path
}
func safeRequestHeaders(h http.Header, secrets []string) http.Header {
	out := http.Header{}
	for _, k := range []string{"Accept", "Content-Type", "Range", "If-Range", "User-Agent", "X-Silo-Client", "X-Silo-Client-Version", "X-Emby-Client", "X-Emby-Client-Version", "Connection", "Upgrade", "Sec-Websocket-Version", "Sec-Websocket-Protocol", "Sec-Websocket-Extensions"} {
		if v := h.Values(k); len(v) > 0 {
			out[http.CanonicalHeaderKey(k)] = append([]string(nil), v...)
		}
	}
	if h.Get("Sec-Websocket-Key") != "" {
		out.Set("Sec-Websocket-Key", "${websocket_key}")
	}
	if out.Get("If-Range") != "" {
		out.Set("If-Range", "${etag}")
	}
	if h.Get("Authorization") != "" {
		if strings.HasPrefix(strings.ToLower(h.Get("Authorization")), "bearer ") {
			out.Set("Authorization", "Bearer ${access_token}")
		} else {
			out.Set("Authorization", mediaBrowserHeader(h.Get("Authorization"), true))
		}
	}
	if value := h.Get("X-Emby-Authorization"); value != "" {
		out.Set("X-Emby-Authorization", mediaBrowserHeader(value, false))
	}
	if h.Get("Cookie") != "" {
		out.Set("Cookie", "${cookie}")
	}
	if h.Get("X-Emby-Token") != "" {
		out.Set("X-Emby-Token", "${access_token}")
	}
	for k, values := range out {
		if strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "X-Emby-Authorization") || strings.EqualFold(k, "X-Emby-Token") || strings.EqualFold(k, "Cookie") {
			continue
		}
		for i, v := range values {
			values[i] = string(scrubText(conform.Redact([]byte(v))))
		}
		out[k] = values
	}
	return redactRequestHeaders(out, secrets)
}
func safeResponseHeaders(h http.Header) http.Header {
	out := http.Header{}
	for _, k := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Cache-Control", "Connection", "Upgrade", "Sec-Websocket-Protocol"} {
		if v := h.Values(k); len(v) > 0 {
			if k == "ETag" {
				out.Set(k, "[PRESENT]")
			} else {
				out[http.CanonicalHeaderKey(k)] = append([]string(nil), v...)
			}
		}
	}
	return out
}
func learnBodySecrets(v any, dst map[string]bool) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if sensitiveKey(k) {
				if s, ok := val.(string); ok && s != "" {
					dst[s] = true
				}
			}
			learnBodySecrets(val, dst)
		}
	case []any:
		for _, val := range x {
			learnBodySecrets(val, dst)
		}
	}
}
func (r *Recorder) sanitize(body []byte, secrets []string, path string) []byte {
	p := &mediabrowser.Protocol{}
	res := p.Redact(conform.Response{Body: body})
	res = conform.RedactSecrets(res, secrets...)
	// Preserve JSON shape while removing opaque server/item identifiers. Tokens
	// in URLs and headers are handled before values are transformed.
	var v any
	dec := json.NewDecoder(bytes.NewReader(res.Body))
	dec.UseNumber()
	if dec.Decode(&v) == nil {
		r.mu.Lock()
		var walk func(any) any
		walk = func(v any) any {
			switch x := v.(type) {
			case map[string]any:
				for k, val := range x {
					n := strings.ToLower(k)
					if _, ok := val.(string); ok && n == "id" && (strings.Contains(strings.ToLower(path), "system/info")) {
						x[k] = "[REDACTED]"
					} else if str, ok := val.(string); ok && strings.HasSuffix(n, "id") && opaqueID.MatchString(str) {
						x[k] = "${" + r.slot(str) + "}"
					} else {
						x[k] = walk(val)
					}
				}
			case []any:
				for i, val := range x {
					x[i] = walk(val)
				}
			case string:
				return opaqueTextID.ReplaceAllStringFunc(x, func(id string) string { return "${" + r.slot(id) + "}" })
			}
			return v
		}
		v = walk(v)
		r.mu.Unlock()
		if b, err := json.Marshal(v); err == nil {
			return b
		}
	}
	// XML is parsed and re-encoded so sensitive attributes and element text are
	// removed even when field values contain spaces or escaped characters.
	if len(bytes.TrimSpace(res.Body)) > 0 && bytes.TrimSpace(res.Body)[0] == '<' {
		dec := xml.NewDecoder(bytes.NewReader(res.Body))
		var buf bytes.Buffer
		enc := xml.NewEncoder(&buf)
		var private []bool
		for {
			tok, err := dec.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil
			}
			switch x := tok.(type) {
			case xml.StartElement:
				hide := privateField(x.Name.Local)
				if len(private) > 0 && private[len(private)-1] {
					hide = true
				}
				private = append(private, hide)
				for i := range x.Attr {
					if privateField(x.Attr[i].Name.Local) || (strings.EqualFold(x.Attr[i].Name.Local, "id") && strings.Contains(strings.ToLower(path), "system/info")) {
						x.Attr[i].Value = "[REDACTED]"
					} else {
						x.Attr[i].Value = string(conform.Redact([]byte(x.Attr[i].Value)))
					}
				}
				tok = x
			case xml.CharData:
				if len(private) > 0 && private[len(private)-1] {
					tok = xml.CharData([]byte("[REDACTED]"))
				} else {
					tok = xml.CharData(conform.Redact([]byte(x)))
				}
			case xml.EndElement:
				if len(private) > 0 {
					private = private[:len(private)-1]
				}
			}
			if enc.EncodeToken(tok) != nil {
				return nil
			}
		}
		if enc.Flush() != nil {
			return nil
		}
		r.mu.Lock()
		masked := opaqueTextID.ReplaceAllStringFunc(buf.String(), func(id string) string { return "${" + r.slot(id) + "}" })
		r.mu.Unlock()
		return scrubText([]byte(masked))
	}
	r.mu.Lock()
	masked := opaqueTextID.ReplaceAllStringFunc(string(res.Body), func(id string) string { return "${" + r.slot(id) + "}" })
	r.mu.Unlock()
	return scrubText([]byte(masked))
}
func privateField(k string) bool {
	n := strings.ToLower(k)
	return sensitiveKey(k) || n == "serverid" || n == "server_id" || n == "deviceid" || n == "device_id" || n == "sessionid" || n == "session_id" || strings.HasSuffix(n, "path") || strings.HasPrefix(n, "date") || strings.HasSuffix(n, "date") || n == "machineidentifier"
}

func (s *state) finish() {
	s.once.Do(func() {
		var requestBody []byte
		var requestTotal int64
		requestComplete := true
		if s.request != nil {
			requestBody, requestTotal, requestComplete = s.request.snapshot()
		}
		r := s.rec
		r.mu.Lock()
		var v any
		if json.Unmarshal(requestBody, &v) == nil {
			learnBodySecrets(v, r.secrets)
		} else if isForm(s.entry.Request.Headers.Get("Content-Type")) {
			if form, e := url.ParseQuery(string(requestBody)); e == nil {
				for key, values := range form {
					if sensitiveKey(key) {
						for _, value := range values {
							if value != "" {
								r.secrets[value] = true
							}
						}
					}
				}
			}
		}
		secrets := r.secretList()
		r.mu.Unlock()
		s.entry.Request.Headers = redactRequestHeaders(s.entry.Request.Headers, secrets)
		s.entry.RequestTruncated = !requestComplete || requestTotal > int64(len(requestBody))
		if len(requestBody) > 0 {
			if isJSON(s.entry.Request.Headers.Get("Content-Type")) && !json.Valid(requestBody) {
				s.entry.RequestTruncated = true
			}
			if !s.entry.RequestTruncated && textContent(s.entry.Request.Headers.Get("Content-Type")) {
				if isForm(s.entry.Request.Headers.Get("Content-Type")) {
					s.entry.Request.Body = sanitizeForm(requestBody)
				} else {
					s.entry.Request.Body = r.sanitize(requestBody, secrets, s.entry.Request.Path)
				}
			} else {
				if !textContent(s.entry.Request.Headers.Get("Content-Type")) {
					sum := sha256.Sum256(requestBody)
					s.entry.Request.BodyKind = "binary"
					s.entry.Request.BodyLength = requestTotal
					s.entry.Request.SHA256 = hex.EncodeToString(sum[:])
				}
				s.entry.RequestTruncated = true
			}
		}
		if s.response != nil {
			body, total, complete := s.response.snapshot()
			if s.textResponse {
				s.entry.ResponseTruncated = !complete || total > int64(len(body))
				if !s.entry.ResponseTruncated && s.responseEncoding != "" && !strings.EqualFold(s.responseEncoding, "identity") {
					if strings.EqualFold(s.responseEncoding, "gzip") {
						zr, err := gzip.NewReader(bytes.NewReader(body))
						if err == nil {
							body, err = io.ReadAll(io.LimitReader(zr, textLimit+1))
							if closeErr := zr.Close(); err == nil {
								err = closeErr
							}
						}
						if err != nil || len(body) > textLimit {
							s.entry.ResponseTruncated = true
						}
					} else {
						s.entry.ResponseTruncated = true
					}
				}
				if isJSON(s.entry.Response.Headers.Get("Content-Type")) && len(body) > 0 && !json.Valid(body) {
					s.entry.ResponseTruncated = true
				}
				if !s.entry.ResponseTruncated {
					s.entry.Response.Body = r.sanitize(body, secrets, s.entry.Request.Path)
					if len(body) > 0 && len(s.entry.Response.Body) == 0 {
						s.entry.ResponseTruncated = true
					}
				}
			} else {
				sum := sha256.Sum256(body)
				s.entry.Response.Kind = "binary-range"
				s.entry.Response.BodyLength = total
				s.entry.Response.SHA256 = hex.EncodeToString(sum[:])
				s.entry.Response.Truncated = !complete || total > int64(len(body))
			}
		}
		b, err := json.MarshalIndent(s.entry, "", "  ")
		if err == nil {
			var f *os.File
			f, err = os.CreateTemp(r.dir, ".capture-*")
			if err == nil {
				err = f.Chmod(0600)
				if err == nil {
					_, err = f.Write(append(b, '\n'))
				}
				if e := f.Close(); err == nil {
					err = e
				}
				if err == nil {
					err = os.Rename(f.Name(), filepath.Join(r.dir, fmt.Sprintf("%08d.json", s.entry.Sequence)))
				}
				if err != nil {
					os.Remove(f.Name())
				}
			}
		}
		if err != nil {
			select {
			case r.errors <- fmt.Errorf("capture write failed: %w", err):
			default:
			}
		}
	})
}

var mediaAuthField = regexp.MustCompile(`(?i)(client|version)="([^"\r\n]*)"`)

func mediaBrowserHeader(value string, withToken bool) string {
	client, version := "${client}", "${version}"
	for _, m := range mediaAuthField.FindAllStringSubmatch(value, -1) {
		clean := string(conform.Redact([]byte(m[2])))
		if strings.EqualFold(m[1], "client") {
			client = clean
		} else {
			version = clean
		}
	}
	out := fmt.Sprintf(`MediaBrowser Client="%s", Device="${device}", DeviceId="${device_id}", Version="%s"`, client, version)
	if withToken {
		out += `, Token="${access_token}"`
	}
	return out
}

var captureTemplate = regexp.MustCompile(`\$\{[A-Za-z0-9_-]+\}`)

func redactRequestHeaders(h http.Header, secrets []string) http.Header {
	out := h.Clone()
	for k, values := range out {
		for i, v := range values {
			var result strings.Builder
			last := 0
			for _, bounds := range captureTemplate.FindAllStringIndex(v, -1) {
				piece := v[last:bounds[0]]
				result.Write(conform.RedactSecrets(conform.Response{Body: []byte(piece)}, secrets...).Body)
				result.WriteString(v[bounds[0]:bounds[1]])
				last = bounds[1]
			}
			result.Write(conform.RedactSecrets(conform.Response{Body: []byte(v[last:])}, secrets...).Body)
			values[i] = result.String()
		}
		out[k] = values
	}
	return out
}

func userPathID(parent, value string) bool {
	switch strings.ToLower(parent) {
	case "users", "sessions", "devices":
		switch strings.ToLower(value) {
		case "public", "me", "authenticatebyname", "playing", "resume", "views":
			return false
		}
		return value != ""
	}
	return false
}

func binaryRequest(request Request) bool {
	path := strings.ToLower(strings.SplitN(request.Path, "?", 2)[0])
	if strings.HasSuffix(path, ".m3u8") {
		return false
	}
	if request.Headers.Get("Range") != "" || strings.HasSuffix(path, "/stream") {
		return true
	}
	for _, ext := range []string{".ts", ".m4s", ".mp4", ".mkv", ".avi", ".webm", ".mp3", ".aac", ".flac", ".m4a"} {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}

func isJSON(contentType string) bool {
	m, _, _ := mime.ParseMediaType(contentType)
	return m == "application/json" || strings.HasSuffix(m, "+json")
}

func scrubText(body []byte) []byte {
	body = secretQuery.ReplaceAll(body, []byte("${1}[REDACTED]"))
	body = inlineSecret.ReplaceAll(body, []byte("${1}=[REDACTED]"))
	body = textDate.ReplaceAll(body, []byte("[REDACTED_DATE]"))
	return hostPath.ReplaceAll(body, []byte("[REDACTED_PATH]"))
}

func isForm(contentType string) bool {
	m, _, _ := mime.ParseMediaType(contentType)
	return m == "application/x-www-form-urlencoded"
}
func sanitizeForm(body []byte) []byte {
	v, err := url.ParseQuery(string(body))
	if err != nil {
		return nil
	}
	for k := range v {
		if privateField(k) || strings.Contains(strings.ToLower(k), "path") {
			v[k] = []string{"[REDACTED]"}
		}
	}
	return []byte(v.Encode())
}
