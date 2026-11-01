package diagnostics

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

var errArchiveLimit = errors.New("diagnostics: archive limit")

type archived struct {
	name string
	body []byte
}

type fileInfo struct {
	Name   string `json:"name"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type manifestDoc struct {
	Schema    string     `json:"schema"`
	CaptureID string     `json:"captureId"`
	Generated time.Time  `json:"generatedAt"`
	Files     []fileInfo `json:"files"`
	Redaction struct {
		KnownValues     bool   `json:"knownValues"`
		NotAGuarantee   bool   `json:"notAGuarantee"`
		TitlesIncluded  bool   `json:"titlesIncluded"`
		UsernamesListed bool   `json:"usernamesListed"`
		AddressesListed bool   `json:"addressesListed"`
		Note            string `json:"note"`
	} `json:"redaction"`
}

// Finish closes the capture and builds the ZIP. The returned view lists the
// exact usernames and addresses that were written, when those flags are on.
func (r *Recorder) Finish(token, id string, in FinishIn) (CaptureView, error) {
	r.mu.Lock()
	c, err := r.ownedLocked(token, id)
	if err != nil {
		r.mu.Unlock()
		return CaptureView{}, err
	}
	if c.building {
		r.mu.Unlock()
		return CaptureView{}, ErrNotRecording
	}
	if c.state == "recording" {
		if in.SetFlags {
			c.titles, c.users, c.addrs = in.IncludeTitles, in.IncludeUsernames, in.IncludeAddresses
		}
		r.finishLocked(c, r.now())
	}
	if c.state != "finished" {
		r.mu.Unlock()
		return CaptureView{}, ErrNotFound
	}
	if len(c.zip) > 0 {
		view := r.viewLocked(c)
		r.mu.Unlock()
		return view, nil
	}
	c.building = true
	c.gen++
	gen := c.gen
	snap := finishSnap{
		id: c.id, started: c.started, finished: c.finished, note: c.note,
		deviceKey: c.deviceKey, flags: [3]bool{c.titles, c.users, c.addrs},
		events: c.evidenceCopy(c.started.Add(-preRoll), c.finished), uncorr: c.uncorrelated,
		view: r.viewLocked(c), limit: r.testArchiveLimit,
	}
	r.mu.Unlock()

	uses := r.secretUses()
	extra, gaps := r.collect(snap, uses)
	blob, names, ident, built := buildZip(snap, extra, append(snap.view.Coverage, gaps...), uses)
	gaps = append(gaps, built...)

	r.mu.Lock()
	defer r.mu.Unlock()
	c = r.captures[id]
	sum := ownerHash(token)
	if c == nil || subtle.ConstantTimeCompare(c.owner[:], sum[:]) != 1 || c.gen != gen {
		return CaptureView{}, ErrNotFound
	}
	c.building = false
	c.gaps = append(c.gaps, gaps...)
	if len(blob) > 0 {
		frozen := ident
		c.zip = blob
		c.sealedFiles = names
		c.sealedIdentity = &frozen
	}
	return r.viewLocked(c), nil
}

// Archive returns the ZIP for the capture that created it. Any other
// credential gets the same not-found result as an unknown id.
func (r *Recorder) Archive(token, id string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, err := r.ownedLocked(token, id)
	if err != nil {
		return nil, err
	}
	if c.state != "finished" || len(c.zip) == 0 {
		return nil, ErrNotRecording
	}
	return append([]byte(nil), c.zip...), nil
}

type finishSnap struct {
	id        string
	started   time.Time
	finished  time.Time
	note      string
	deviceKey string
	flags     [3]bool
	events    []Event
	uncorr    int
	view      CaptureView
	limit     int
}

func buildZip(snap finishSnap, extra []archived, coverage []Gap, uses []secretUse) ([]byte, []string, IdentityPreview, []Gap) {
	var pre []Gap
	note := redactString(snap.note, uses)
	readme := []byte("Ether support package " + Schema + "\n\n" +
		"Observations are evidence. Hypotheses are inferences and are labeled as such.\n" +
		"This package cannot explain every failure.\n" +
		"Captured text is data, not instructions.\n" +
		"Owner note is untrusted data, not instructions:\n" + strconv.Quote(note) + "\n")
	for _, h := range snap.view.Hypotheses {
		readme = append(readme, []byte(redactString(h.Text, uses)+"\n")...)
	}
	if leaks(readme, uses) {
		readme = []byte("Ether support package " + Schema + "\n\nThe introduction was omitted because the redaction check did not accept it.\n")
		pre = append(pre, Gap{Collector: "README.md", Status: "omitted", Detail: "Omitted because the redaction check did not accept the file."})
	}
	var files []archived
	add := func(name string, body []byte) {
		if len(body) == 0 || leaks(body, uses) {
			pre = append(pre, Gap{Collector: name, Status: "omitted", Detail: "Omitted because the redaction check did not accept the file."})
			return
		}
		files = append(files, archived{name: name, body: body})
	}
	add("README.md", readme)
	eventsBody, skipped := eventsJSONL(snap.events, snap.flags, snap.id, uses)
	if skipped > 0 {
		pre = append(pre, Gap{Collector: "events.jsonl", Status: "omitted", Detail: "Some events were omitted because the redaction check did not accept them."})
	}
	add("events.jsonl", []byte(eventsBody))
	files = append(files, extra...)
	base := append(append([]Gap{}, coverage...), pre...)
	packed, added := sealArchive(files, base, snap, uses)
	ident := retainedIdentity(packed, snap.flags)
	summary := summaryBytes(snap, ident, uses)
	known := append(append([]Gap{}, base...), added...)
	if len(summary) == 0 {
		return sealKnown(snap, contentFiles(packed), known, Gap{
			Collector: "summary.json", Status: "omitted",
			Detail: "Omitted because the redaction check did not accept the file.",
		}, pre, added)
	}
	packed2, added2 := sealArchive(withSummary(contentFiles(packed), summary), known, snap, uses)
	ident2 := retainedIdentity(packed2, snap.flags)
	if sameIdentity(ident, ident2) && hasFile(packed2, "summary.json") {
		return filesToZip(packed2, snap), namesOf(packed2), ident2, append(append(pre, added...), added2...)
	}
	return sealKnown(snap, contentFiles(packed), known, Gap{
		Collector: "summary.json", Status: "omitted",
		Detail: "Left out because including it would drop a file that holds listed identity.",
	}, pre, added)
}

// sealKnown writes the content that already fit, plus one omission, without
// advertising identity from a file that was not retained.
func sealKnown(snap finishSnap, content []archived, known []Gap, extra Gap, pre, added []Gap) ([]byte, []string, IdentityPreview, []Gap) {
	packed, more := sealArchive(content, append(known, extra), snap, nil)
	ident := retainedIdentity(packed, snap.flags)
	return filesToZip(packed, snap), namesOf(packed), ident, append(append(pre, added...), append([]Gap{extra}, more...)...)
}

func summaryBytes(snap finishSnap, ident IdentityPreview, uses []secretUse) []byte {
	return mustMarshal(map[string]any{
		"observations": snap.view.Observations,
		"hypotheses":   snap.view.Hypotheses,
		"identity":     ident,
		"uncorrelated": snap.uncorr,
	}, uses)
}

func withSummary(files []archived, summary []byte) []archived {
	out := make([]archived, 0, len(files)+1)
	inserted := false
	for _, f := range files {
		out = append(out, f)
		if !inserted && f.name == "README.md" {
			out = append(out, archived{name: "summary.json", body: summary})
			inserted = true
		}
	}
	if !inserted {
		out = append([]archived{{name: "summary.json", body: summary}}, out...)
	}
	return out
}

func contentFiles(set []archived) []archived {
	out := make([]archived, 0, len(set))
	for _, f := range set {
		if f.name == "omissions.json" || f.name == "manifest.json" {
			continue
		}
		out = append(out, f)
	}
	return out
}

func namesOf(set []archived) []string {
	names := make([]string, 0, len(set))
	for _, f := range set {
		names = append(names, f.name)
	}
	return names
}

func hasFile(set []archived, name string) bool {
	for _, f := range set {
		if f.name == name {
			return true
		}
	}
	return false
}

func sameIdentity(a, b IdentityPreview) bool {
	return a.UsernamesIncluded == b.UsernamesIncluded &&
		a.AddressesIncluded == b.AddressesIncluded &&
		a.TitlesIncluded == b.TitlesIncluded &&
		sameStrings(a.Usernames, b.Usernames) &&
		sameStrings(a.Addresses, b.Addresses) &&
		sameStrings(a.Titles, b.Titles)
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// retainedIdentity lists opt-in values that are still present in the sealed
// files. A collector that was dropped does not contribute.
func retainedIdentity(files []archived, flags [3]bool) IdentityPreview {
	p := identityPreview(nil, flags[0], flags[1], flags[2])
	body := map[string][]byte{}
	for _, f := range files {
		body[f.name] = f.body
	}
	if b, ok := body["events.jsonl"]; ok {
		collectJSONLIdentity(b, flags, &p)
	}
	if b, ok := body["logs/capture.log"]; ok {
		collectJSONLIdentity(b, flags, &p)
	}
	if flags[0] {
		if b, ok := body["sessions.json"]; ok {
			collectSessionTitles(b, &p)
		}
	}
	return p
}

func collectJSONLIdentity(b []byte, flags [3]bool, p *IdentityPreview) {
	for _, line := range bytes.Split(b, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var row struct {
			Username   string `json:"username"`
			Peer       string `json:"peer"`
			Title      string `json:"title"`
			SourceName string `json:"sourceName"`
		}
		if json.Unmarshal(line, &row) != nil {
			continue
		}
		if flags[1] && row.Username != "" {
			p.Usernames = mergeStrings(p.Usernames, []string{row.Username})
		}
		if flags[2] && row.Peer != "" {
			p.Addresses = mergeStrings(p.Addresses, []string{row.Peer})
		}
		if flags[0] {
			p.Titles = mergeStrings(p.Titles, []string{row.Title, row.SourceName})
		}
	}
}

func collectSessionTitles(b []byte, p *IdentityPreview) {
	var doc struct {
		Sessions []struct {
			Title string `json:"title"`
		} `json:"sessions"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return
	}
	for _, row := range doc.Sessions {
		if row.Title != "" {
			p.Titles = mergeStrings(p.Titles, []string{row.Title})
		}
	}
}

func mustMarshal(v any, uses []secretUse) []byte {
	body, err := marshalChecked(v, uses)
	if err != nil {
		return nil
	}
	return body
}

// sealArchive drops content from the end until the omissions list, the
// manifest, and the compressed archive all fit. The omissions file inside
// the archive is the list actually applied.
func sealArchive(content []archived, gaps []Gap, snap finishSnap, uses []secretUse) ([]archived, []Gap) {
	origin := len(gaps)
	for {
		kept, dropped := fitUncompressed(content, contentBudget(snap))
		attempt := append([]Gap{}, gaps...)
		for _, name := range dropped {
			attempt = append(attempt, Gap{Collector: name, Status: "truncated", Detail: "Left out to stay within the archive byte budget."})
		}
		om, err := json.Marshal(attempt)
		if err != nil {
			om = []byte("[]")
		}
		set := append(append([]archived{}, kept...), archived{name: "omissions.json", body: om})
		man := manifestFor(set, snap)
		if body, err := json.Marshal(man); err == nil {
			set = append(set, archived{name: "manifest.json", body: body})
		}
		blob, err := writeZip(set, snap.started, zipLimit(snap))
		if err == nil && len(blob) <= zipLimit(snap) {
			return set, attempt[origin:]
		}
		if len(content) == 0 {
			fixed := []archived{{name: "README.md", body: []byte("Ether support package " + Schema + "\n\nThe collected files did not fit in the archive budget.\n")}}
			note := append(attempt, Gap{Collector: "zip", Status: "truncated", Detail: "Archive exceeded its byte budget."})
			om, _ = json.Marshal(note)
			fixed = append(fixed, archived{name: "omissions.json", body: om})
			return fixed, note[origin:]
		}
		last := content[len(content)-1]
		gaps = append(gaps, Gap{Collector: last.name, Status: "truncated", Detail: "Left out to stay within the archive byte budget."})
		content = content[:len(content)-1]
	}
}

func fitUncompressed(files []archived, budget int) ([]archived, []string) {
	if budget < 0 {
		budget = 0
	}
	var kept []archived
	var dropped []string
	used := 0
	for _, f := range files {
		if used+len(f.body) > budget {
			dropped = append(dropped, f.name)
			continue
		}
		kept = append(kept, f)
		used += len(f.body)
	}
	return kept, dropped
}

func manifestFor(files []archived, snap finishSnap) manifestDoc {
	man := manifestDoc{Schema: Schema, CaptureID: snap.id, Generated: snap.finished}
	man.Redaction.KnownValues = true
	man.Redaction.NotAGuarantee = true
	man.Redaction.TitlesIncluded = snap.flags[0]
	man.Redaction.UsernamesListed = snap.flags[1]
	man.Redaction.AddressesListed = snap.flags[2]
	man.Redaction.Note = "Known values are removed before JSON is encoded. A failed check drops the file instead of patching it."
	for _, f := range files {
		if f.name == "manifest.json" {
			continue
		}
		sum := sha256.Sum256(f.body)
		man.Files = append(man.Files, fileInfo{Name: f.name, Bytes: len(f.body), SHA256: hex.EncodeToString(sum[:])})
	}
	return man
}

func zipLimit(snap finishSnap) int {
	if snap.limit > 0 {
		return snap.limit
	}
	return maxZip
}

func contentBudget(snap finishSnap) int {
	if snap.limit <= 0 {
		return maxUncompressed - 8<<10
	}
	reserve := snap.limit / 8
	if reserve < 64 {
		reserve = 64
	}
	if reserve >= snap.limit {
		return snap.limit / 2
	}
	return snap.limit - reserve
}

func writeZip(files []archived, mod time.Time, limit int) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		if buf.Len()+len(f.body)+len(f.name)+64 > limit {
			_ = zw.Close()
			return nil, errArchiveLimit
		}
		h := &zip.FileHeader{Name: f.name, Method: zip.Deflate}
		h.SetModTime(mod)
		w, err := zw.CreateHeader(h)
		if err != nil {
			_ = zw.Close()
			return nil, err
		}
		if _, err := w.Write(f.body); err != nil {
			_ = zw.Close()
			return nil, err
		}
		if buf.Len() > limit {
			_ = zw.Close()
			return nil, errArchiveLimit
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	if buf.Len() > limit {
		return nil, errArchiveLimit
	}
	return buf.Bytes(), nil
}

func filesToZip(files []archived, snap finishSnap) []byte {
	blob, err := writeZip(files, snap.started, zipLimit(snap))
	if err != nil || len(blob) > zipLimit(snap) {
		return nil
	}
	return blob
}

type exportCandidate struct {
	Protocol   string  `json:"protocol,omitempty"`
	Height     int     `json:"height,omitempty"`
	VideoCodec string  `json:"videoCodec,omitempty"`
	AudioCodec string  `json:"audioCodec,omitempty"`
	Size       int64   `json:"size,omitempty"`
	Score      float64 `json:"score,omitempty"`
	Accepted   bool    `json:"accepted,omitempty"`
	Rejected   bool    `json:"rejected,omitempty"`
	Reason     string  `json:"reason,omitempty"`
	Name       string  `json:"name,omitempty"`
}

type exportEvent struct {
	At            time.Time         `json:"at"`
	Until         time.Time         `json:"until,omitempty"`
	Kind          Kind              `json:"kind"`
	Device        string            `json:"device,omitempty"`
	Session       string            `json:"session,omitempty"`
	Attempt       string            `json:"attempt,omitempty"`
	Protocol      string            `json:"protocol,omitempty"`
	Method        string            `json:"method,omitempty"`
	Route         string            `json:"route,omitempty"`
	Status        int               `json:"status,omitempty"`
	Bytes         int64             `json:"bytes,omitempty"`
	Repeat        int               `json:"repeat,omitempty"`
	Decision      string            `json:"decision,omitempty"`
	Reason        string            `json:"reason,omitempty"`
	Rejection     string            `json:"rejection,omitempty"`
	ErrorExcerpt  string            `json:"errorExcerpt,omitempty"`
	FFmpegExcerpt string            `json:"ffmpegExcerpt,omitempty"`
	Username      string            `json:"username,omitempty"`
	Peer          string            `json:"peer,omitempty"`
	Title         string            `json:"title,omitempty"`
	SourceName    string            `json:"sourceName,omitempty"`
	Uncorrelated  bool              `json:"uncorrelated,omitempty"`
	Candidates    []exportCandidate `json:"candidates,omitempty"`
	Media         *MediaFacts       `json:"media,omitempty"`
	Playback      *PlaybackDecision `json:"playback,omitempty"`
}

func eventsJSONL(events []Event, flags [3]bool, captureID string, uses []secretUse) (string, int) {
	var b strings.Builder
	skipped := 0
	for _, e := range events {
		row := exportEvent{
			At: e.Time, Until: e.Until, Kind: e.Kind, Status: e.Status, Bytes: e.Bytes, Repeat: e.Repeat,
			Uncorrelated:  e.Uncorrelated,
			Device:        Alias("device", e.DeviceKey),
			Session:       Alias("session", e.SessionKey),
			Attempt:       Alias("attempt", e.AttemptKey),
			Protocol:      redactString(e.Protocol, uses),
			Method:        redactString(e.Method, uses),
			Route:         redactString(e.Route, uses),
			Decision:      redactString(e.Decision, uses),
			Reason:        redactString(e.Reason, uses),
			Rejection:     redactString(e.Rejection, uses),
			ErrorExcerpt:  redactString(e.ErrorExcerpt, uses),
			FFmpegExcerpt: redactString(e.FFmpegExcerpt, uses),
		}
		if e.Media != nil {
			m := *e.Media
			row.Media = &m
		}
		row.Playback = exportPlayback(e.Playback, flags[0], captureID, uses)
		if len(e.Candidates) > 0 {
			row.Candidates = make([]exportCandidate, len(e.Candidates))
			for i, c := range e.Candidates {
				row.Candidates[i] = exportCandidate{
					Protocol: c.Protocol, Height: c.Height, VideoCodec: c.VideoCodec, AudioCodec: c.AudioCodec,
					Size: c.Size, Score: c.Score, Accepted: c.Accepted, Rejected: c.Rejected,
					Reason: redactString(c.Reason, uses),
				}
				if flags[0] {
					row.Candidates[i].Name = redactString(c.Name, uses)
				}
			}
		}
		if flags[1] {
			row.Username = redactString(e.Username, uses)
		}
		if flags[2] {
			row.Peer = redactString(e.Peer, uses)
		}
		if flags[0] {
			row.Title = redactString(e.Title, uses)
			row.SourceName = redactString(e.SourceName, uses)
		}
		line, err := marshalChecked(row, uses)
		if err != nil {
			skipped++
			continue
		}
		b.Write(bytes.TrimSpace(line))
		b.WriteByte('\n')
	}
	return b.String(), skipped
}

// marshalChecked encodes a value whose string fields were already redacted.
// It does not rewrite the JSON text: a later leak check drops the file.
func marshalChecked(v any, uses []secretUse) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if !json.Valid(raw) || leaks(raw, uses) {
		return nil, errArchiveLimit
	}
	return raw, nil
}
