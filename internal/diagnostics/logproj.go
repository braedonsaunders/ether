package diagnostics

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// LogQuery selects log records for one confirmed capture.
type LogQuery struct {
	From        time.Time
	Until       time.Time
	DeviceKey   string
	SessionKeys []string
	AttemptKeys []string
	MaxLines    int
}

// LogRecord is one structured log row. Message is free text and is not exported.
type LogRecord struct {
	Time    time.Time
	Level   string
	Message string
	Attrs   map[string]string
}

// Alias is a stable pseudonym. The same raw id always maps to the same
// alias, and different ids do not share one.
func Alias(scope, raw string) string {
	if raw == "" {
		return ""
	}
	if scope == "device" {
		return devicePublicID(raw)
	}
	sum := sha256.Sum256([]byte("ether-support-alias:" + scope + ":" + raw))
	return scope + ":" + hex.EncodeToString(sum[:8])
}

// LogCorrelated reports whether rec belongs to this capture by an exact
// correlation field inside the capture window. A shorter id is not a match
// for a longer one, and free text is not searched.
func LogCorrelated(rec LogRecord, q LogQuery) bool {
	if rec.Time.IsZero() || rec.Time.Before(q.From) || rec.Time.After(q.Until) {
		return false
	}
	if exactAttr(rec.Attrs, deviceAttrNames, q.DeviceKey) {
		return true
	}
	for _, key := range q.SessionKeys {
		if exactAttr(rec.Attrs, sessionAttrNames, key) {
			return true
		}
	}
	for _, key := range q.AttemptKeys {
		if exactAttr(rec.Attrs, attemptAttrNames, key) {
			return true
		}
	}
	return false
}

var (
	deviceAttrNames  = []string{"client", "clientid", "client_id", "device", "devicekey", "device_key"}
	sessionAttrNames = []string{"session", "sessionid", "session_id", "sessionkey"}
	attemptAttrNames = []string{"attempt", "attemptkey", "attempt_id"}
)

func exactAttr(attrs map[string]string, names []string, want string) bool {
	if want == "" || len(attrs) == 0 {
		return false
	}
	for k, v := range attrs {
		if v != want {
			continue
		}
		lk := strings.ToLower(k)
		for _, name := range names {
			if lk == name {
				return true
			}
		}
	}
	return false
}

// ParseLogLine reads a slog-style line into structured fields.
// It returns false when the line has no key=value fields.
func ParseLogLine(line string) (LogRecord, bool) {
	attrs := parseKV(line)
	if len(attrs) == 0 {
		return LogRecord{}, false
	}
	rec := LogRecord{Attrs: attrs, Level: attrFold(attrs, "level"), Message: attrFold(attrs, "msg")}
	if raw := attrFold(attrs, "time"); raw != "" {
		if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			rec.Time = t
		} else if t, err := time.Parse(time.RFC3339, raw); err == nil {
			rec.Time = t
		}
	}
	return rec, true
}

func attrFold(attrs map[string]string, name string) string {
	for k, v := range attrs {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

func parseKV(line string) map[string]string {
	out := map[string]string{}
	i := 0
	for i < len(line) {
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		if i >= len(line) {
			break
		}
		eq := strings.IndexByte(line[i:], '=')
		if eq <= 0 {
			break
		}
		key := line[i : i+eq]
		if strings.ContainsAny(key, " \t") {
			break
		}
		i += eq + 1
		if i >= len(line) {
			out[key] = ""
			break
		}
		if line[i] == '"' {
			var b strings.Builder
			i++
			for i < len(line) {
				if line[i] == '\\' && i+1 < len(line) {
					b.WriteByte(line[i+1])
					i += 2
					continue
				}
				if line[i] == '"' {
					i++
					break
				}
				b.WriteByte(line[i])
				i++
			}
			out[key] = b.String()
			continue
		}
		j := i
		for j < len(line) && line[j] != ' ' && line[j] != '\t' {
			j++
		}
		out[key] = line[i:j]
		i = j
	}
	return out
}

type exportLog struct {
	At       time.Time `json:"at"`
	Level    string    `json:"level,omitempty"`
	Device   string    `json:"device,omitempty"`
	Session  string    `json:"session,omitempty"`
	Attempt  string    `json:"attempt,omitempty"`
	Status   string    `json:"status,omitempty"`
	Kind     string    `json:"kind,omitempty"`
	Username string    `json:"username,omitempty"`
	Peer     string    `json:"peer,omitempty"`
	Title    string    `json:"title,omitempty"`
}

type logIdentity struct {
	users  []string
	peers  []string
	titles []string
}

// projectLogs writes one JSON object per correlated record. Free text,
// paths, and fields the owner did not opt into are left out.
func projectLogs(records []LogRecord, q LogQuery, flags [3]bool, uses []secretUse) (body []byte, ident logIdentity, truncated bool, gaps []Gap) {
	gaps = append(gaps, Gap{
		Collector: "logs",
		Status:    "omitted",
		Detail:    "Free-text log messages, paths, and unrecognized fields were omitted. Usernames, addresses, and titles are included only when the summary lists them.",
	})
	limit := q.MaxLines
	if limit <= 0 || limit > maxLogLines {
		limit = maxLogLines
	}
	var b strings.Builder
	n := 0
	for _, rec := range records {
		if !LogCorrelated(rec, q) {
			continue
		}
		if n >= limit || b.Len() >= maxLogBytes {
			truncated = true
			break
		}
		row := exportLog{At: rec.Time.UTC(), Level: boundToken(rec.Level)}
		if key := exactValue(rec.Attrs, deviceAttrNames, q.DeviceKey); key != "" {
			row.Device = Alias("device", key)
		}
		if key := firstExact(rec.Attrs, sessionAttrNames, q.SessionKeys); key != "" {
			row.Session = Alias("session", key)
		}
		if key := firstExact(rec.Attrs, attemptAttrNames, q.AttemptKeys); key != "" {
			row.Attempt = Alias("attempt", key)
		}
		row.Status = boundToken(attrFold(rec.Attrs, "status"))
		row.Kind = boundToken(attrFold(rec.Attrs, "kind"))
		if flags[1] {
			if u := firstPresent(rec.Attrs, "user", "username"); u != "" {
				u = redactString(u, uses)
				row.Username = u
				ident.users = append(ident.users, u)
			}
		}
		if flags[2] {
			if p := firstPresent(rec.Attrs, "peer", "ip", "remote", "addr"); p != "" {
				p = redactString(p, uses)
				row.Peer = p
				ident.peers = append(ident.peers, p)
			}
		}
		if flags[0] {
			if title := firstPresent(rec.Attrs, "title", "release", "source"); title != "" {
				title = redactString(title, uses)
				row.Title = title
				ident.titles = append(ident.titles, title)
			}
		}
		line, err := marshalChecked(row, uses)
		if err != nil {
			gaps = append(gaps, Gap{Collector: "logs/capture.log", Status: "omitted", Detail: "A log record was omitted because the redaction check did not accept it."})
			continue
		}
		if b.Len()+len(line)+1 > maxLogBytes {
			truncated = true
			break
		}
		b.Write(line)
		b.WriteByte('\n')
		n++
	}
	if truncated {
		gaps = append(gaps, Gap{Collector: "logs", Status: "truncated", Detail: "The log extract was shortened to the line limit. No line was cut in the middle."})
	}
	return []byte(b.String()), ident, truncated, gaps
}

func boundToken(s string) string {
	if s == "" || len(s) > 40 || strings.ContainsAny(s, " /\\\t\"'") {
		return ""
	}
	return s
}

func exactValue(attrs map[string]string, names []string, want string) string {
	if exactAttr(attrs, names, want) {
		return want
	}
	return ""
}

func firstExact(attrs map[string]string, names []string, wants []string) string {
	for _, want := range wants {
		if exactAttr(attrs, names, want) {
			return want
		}
	}
	return ""
}

func firstPresent(attrs map[string]string, names ...string) string {
	for _, name := range names {
		if v := attrFold(attrs, name); v != "" {
			return v
		}
	}
	return ""
}

func logQuery(snap finishSnap) LogQuery {
	q := LogQuery{
		From:      snap.started.Add(-preRoll),
		Until:     snap.finished,
		DeviceKey: snap.deviceKey,
		MaxLines:  maxLogLines,
	}
	seenS := map[string]struct{}{}
	seenA := map[string]struct{}{}
	for _, e := range snap.events {
		if e.SessionKey != "" {
			if _, ok := seenS[e.SessionKey]; !ok {
				seenS[e.SessionKey] = struct{}{}
				q.SessionKeys = append(q.SessionKeys, e.SessionKey)
			}
		}
		if e.AttemptKey != "" {
			if _, ok := seenA[e.AttemptKey]; !ok {
				seenA[e.AttemptKey] = struct{}{}
				q.AttemptKeys = append(q.AttemptKeys, e.AttemptKey)
			}
		}
	}
	return q
}
