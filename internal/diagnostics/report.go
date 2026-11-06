package diagnostics

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type Finding struct {
	Code      string `json:"code"`
	Text      string `json:"text"`
	Lines     []int  `json:"journalLines"`
	Truncated bool   `json:"truncated,omitempty"`
}
type reportRow struct {
	line  int
	event exportEvent
}

// analyzeJournal consumes the same already-redacted JSONL that is written to
// the ZIP. References therefore point to actual lines, even when a record was
// dropped by the redaction check.
func analyzeJournal(body string) ([]reportRow, []Finding) {
	var rows []reportRow
	for i, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		if line == "" {
			continue
		}
		var e exportEvent
		if json.Unmarshal([]byte(line), &e) == nil {
			rows = append(rows, reportRow{i + 1, e})
		}
	}
	var findings []Finding
	type occurrence struct {
		directory string
		line      int
	}
	seen := map[string][]occurrence{}
	var errors, slow, empty, hero, playbackFailures, genericAdmissions, unattributedPlayback []int
	failureLines := map[int]bool{}
	addFailure := func(line int) {
		if !failureLines[line] {
			failureLines[line] = true
			playbackFailures = append(playbackFailures, line)
		}
	}
	for _, row := range rows {
		e := row.event
		if e.Kind == KindRequestStarted && e.Uncorrelated {
			unattributedPlayback = append(unattributedPlayback, row.line)
		}
		if e.Kind == KindClientCapabilities && e.Client != nil && e.Client.Registry == "generic" && !e.Client.ExplicitMaxHeight && e.Client.MaxHeight == e.Client.RegistryMaxHeight {
			genericAdmissions = append(genericAdmissions, row.line)
		}
		if e.Kind == KindRequest && e.Status >= 400 && (strings.Contains(strings.ToLower(e.Route), "/decision") || strings.Contains(strings.ToLower(e.Route), "playback")) {
			addFailure(row.line)
		}
		if e.Kind != KindRequest {
			continue
		}
		if e.Status >= 400 {
			errors = append(errors, row.line)
		}
		if e.DurationMS > 2000 || e.DBTimeMS > 500 || e.DBQueries > 300 {
			slow = append(slow, row.line)
		}
		if e.Response == nil {
			continue
		}
		if e.Response.ErrorCode != "" {
			addFailure(row.line)
		}
		q, _ := url.ParseQuery(e.Query)
		for _, h := range e.Response.Hubs {
			id := strings.ToLower(h.Identifier)
			if strings.Contains(id, "continue") && h.ItemCount == 0 && !e.Response.Truncated {
				empty = append(empty, row.line)
			}
			if strings.EqualFold(h.Style, "hero") && !strings.Contains(id, "continue") {
				hero = append(hero, row.line)
			}
			directory := q.Get("contentDirectoryID")
			if e.Route == "/hubs/promoted" && directory != "" && directory != "[redacted]" && !strings.Contains(directory, ",") && h.Identifier != "" {
				seen[h.Identifier] = append(seen[h.Identifier], occurrence{directory, row.line})
			}
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		occ := seen[id]
		dirs := map[string]bool{}
		lines := map[int]bool{}
		for _, o := range occ {
			dirs[o.directory] = true
			lines[o.line] = true
		}
		if len(dirs) < 2 {
			continue
		}
		nums := make([]int, 0, len(lines))
		for n := range lines {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		findings = append(findings, Finding{Code: "duplicate_hub_across_directories", Text: fmt.Sprintf("Hub %q appeared in promoted responses for multiple content directories.", id), Lines: nums})
	}
	add := func(code, text string, lines []int) {
		if len(lines) > 0 {
			findings = append(findings, Finding{Code: code, Text: text, Lines: lines})
		}
	}
	add("empty_continue_watching", "A Continue Watching hub contained no items.", empty)
	add("other_hero", "A hero row other than Continue Watching was returned.", hero)
	add("slow_request", "A request exceeded a wall-time or database budget.", slow)
	add("http_error", "A request returned an HTTP error.", errors)
	add("unattributed_playback", "Authenticated playback requests omitted a device identity. They belong to the selected viewer, but cannot be attributed to a specific device.", unattributedPlayback)
	add("generic_client_admission", "Playback admission used the generic client profile resolution ceiling. See the recorded identity and capability stages; this does not prove the device cannot play higher resolutions.", genericAdmissions)
	if len(playbackFailures) >= 2 {
		add("repeated_playback_failures", "Multiple playback decision requests failed.", playbackFailures)
	}
	if len(findings) > 128 {
		findings = append(findings[:128], Finding{Code: "findings_truncated", Text: "Additional findings were omitted to keep the report bounded.", Truncated: true})
	}
	for i := range findings {
		if len(findings[i].Lines) > 64 {
			findings[i].Lines = findings[i].Lines[:64]
			findings[i].Truncated = true
		}
	}
	return rows, findings
}

func reportMarkdown(rows []reportRow, findings []Finding, extra []archived) []byte {
	var b bytes.Buffer
	b.WriteString("# Recording report\n\n## What happened\n\n")
	b.WriteString(strings.Join(reportOverview(rows, findings), " "))
	b.WriteString("\n\nRequest numbers refer to lines in events.jsonl. Times are UTC. The server cannot see what the client displayed.\n\n## Findings\n\n")
	if len(findings) == 0 {
		b.WriteString("No automated rule matched the retained requests.\n")
	}
	byLine := make(map[int]exportEvent, len(rows))
	for _, row := range rows {
		byLine[row.line] = row.event
	}
	for _, f := range findings {
		if f.Code == "slow_request" {
			for _, line := range f.Lines {
				if e, ok := byLine[line]; ok {
					fmt.Fprintf(&b, "- Request #%d exceeded a budget: %s.\n", line, reportBudget(e))
				}
			}
			if f.Truncated {
				b.WriteString("- Additional budget references were omitted.\n")
			}
			continue
		}
		fmt.Fprintf(&b, "- %s (%s)", f.Text, reportRefs(f.Lines))
		if f.Truncated {
			b.WriteString(" (additional detail omitted)")
		}
		b.WriteByte('\n')
	}
	b.WriteString("\n## Request timeline\n\n| Time (UTC) | Client | Request | Status | Duration | Response |\n| --- | --- | --- | --- | --- | --- |\n")
	count := 0
	for _, row := range rows {
		e := row.event
		if e.Kind != KindRequest {
			continue
		}
		if count >= 200 || b.Len() > 200<<10 {
			b.WriteString("\nTimeline shortened; see events.jsonl for the remaining requests.\n")
			break
		}
		request := e.Method + " " + e.Route
		if q := reportKeyParams(e.Query); q != "" {
			request += "?" + q
		}
		fmt.Fprintf(&b, "| #%d %s | %s | %s | %d | %s | %s |\n", row.line, e.At.UTC().Format("15:04:05.000"), reportCell(reportClient(e)), reportCell(request), e.Status, reportDuration(e.DurationMS), reportCell(reportResponse(e)))
		count++
	}
	b.WriteString("\n## Playback request receipts and capabilities\n\nStarts are recorded before streaming. A start does not prove response completion. Admission rows show the capabilities actually passed to playback; request rows show declarations only.\n\n| Event | Stage | Attribution | Registry | Registry / owner override / declaration / effective max resolution | Video codecs |\n| --- | --- | --- | --- | --- | --- |\n")
	capCount := 0
	for _, row := range rows {
		e := row.event
		if e.Client == nil || (e.Kind != KindRequestStarted && e.Kind != KindClientCapabilities) {
			continue
		}
		if capCount >= 64 || b.Len() > 240<<10 {
			b.WriteString("\nAdditional capability observations are in events.jsonl.\n")
			break
		}
		c := e.Client
		attribution := e.Device
		if e.Uncorrelated {
			attribution = "selected viewer; device unknown"
		}
		fmt.Fprintf(&b, "| #%d %s | %s | %s | %s | %d / %d / %d / %d | %s |\n", row.line, e.Kind, reportCell(c.Stage), reportCell(attribution), reportCell(c.Registry), c.RegistryMaxHeight, c.OwnerMaxHeight, c.DeclaredMaxHeight, c.MaxHeight, reportCell(strings.Join(c.VideoCodecs, ", ")))
		capCount++
	}
	var viewer *ViewerFacts
	for _, file := range extra {
		if file.name == "viewer.json" {
			var v ViewerFacts
			if json.Unmarshal(file.body, &v) == nil {
				viewer = &v
			}
			break
		}
	}
	b.WriteString("\n## Viewer snapshot\n\n")
	if viewer == nil {
		b.WriteString("No viewer snapshot was available.\n")
	} else {
		v := viewer
		selected := 0
		for _, candidate := range v.Candidates {
			if candidate.Included {
				selected++
			}
		}
		b.WriteString("This snapshot was taken when the archive was built; counts may overlap and may have changed since the requests.\n\n| Measure | Result |\n| --- | --- |\n")
		fmt.Fprintf(&b, "| Watch states sampled | %d%s |\n", v.WatchSample, reportTruncated(v.WatchSampleTruncated))
		fmt.Fprintf(&b, "| In progress / watched in sample | %d / %d |\n", v.InProgress, v.Watched)
		fmt.Fprintf(&b, "| Candidates / selected | %d / %d%s |\n", len(v.Candidates), selected, reportTruncated(v.CandidatesTruncated))
		fmt.Fprintf(&b, "| Visible sections | %d%s |\n", len(v.Sections), reportTruncated(v.SectionsTruncated))
		fmt.Fprintf(&b, "| Home layout policies | %d%s |\n", len(v.Layout), reportTruncated(v.LayoutTruncated))
		for _, note := range v.Notes {
			fmt.Fprintf(&b, "\n%s\n", reportCell(note))
		}
		b.WriteString("\n### Viewer candidates\n\n")
		if len(v.Candidates) == 0 {
			b.WriteString("No candidates were included in this snapshot.\n")
		}
		for _, c := range v.Candidates {
			if b.Len() > 300<<10 {
				b.WriteString("Additional candidates are in viewer.json.\n")
				break
			}
			fmt.Fprintf(&b, "- %s (key %d, offset %s, included: %t)\n", reportCell(c.Reason), c.Key, reportDuration(c.OffsetMS), c.Included)
			if c.Title != "" {
				fmt.Fprintf(&b, "  - item: %s\n", strconv.Quote(c.Title))
			}
		}
	}
	b.WriteString("\n## Response details\n\n")
	for _, row := range rows {
		if b.Len() > 400<<10 {
			b.WriteString("\nReport shortened; see events.jsonl for remaining details.\n")
			break
		}
		e := row.event
		if e.Kind != KindRequest || e.Response == nil {
			continue
		}
		fmt.Fprintf(&b, "- Request #%d: %s %s, HTTP %d, %s, %s; %d database statements in %s.\n", row.line, reportCell(e.Method), reportCell(e.Route), e.Status, reportDuration(e.DurationMS), reportBytes(e.Bytes), e.DBQueries, reportDuration(e.DBTimeMS))
		if e.Response.Decision != "" {
			fmt.Fprintf(&b, "  - Playback decision: %s.\n", strconv.Quote(e.Response.Decision))
		}
		if e.Response.ErrorCode != "" {
			fmt.Fprintf(&b, "  - Error code: %s.\n", strconv.Quote(e.Response.ErrorCode))
		}
		if e.Response.Error != "" {
			fmt.Fprintf(&b, "  - Error: %s.\n", strconv.Quote(e.Response.Error))
		}
		fmt.Fprintf(&b, "  - Container size %d", e.Response.Size)
		if e.Response.ItemCount > 0 || len(e.Response.Hubs) == 0 {
			fmt.Fprintf(&b, ", %d direct items", e.Response.ItemCount)
		}
		if e.Response.TotalSize != nil {
			fmt.Fprintf(&b, ", total %d", *e.Response.TotalSize)
		}
		if e.Response.More != nil {
			fmt.Fprintf(&b, ", more %t", *e.Response.More)
		}
		if e.Response.Truncated {
			b.WriteString(" (summary truncated)")
		}
		b.WriteString(".\n")
		for _, h := range e.Response.Hubs {
			fmt.Fprintf(&b, "  - Hub %s: style %s, type %s, size %d, %d items, more %t.\n", strconv.Quote(h.Identifier), strconv.Quote(h.Style), strconv.Quote(h.Type), h.Size, h.ItemCount, h.More)
			for _, title := range h.Items {
				fmt.Fprintf(&b, "  - item: %s\n", strconv.Quote(title))
			}
		}
		for _, title := range e.Response.Items {
			fmt.Fprintf(&b, "  - item: %s\n", strconv.Quote(title))
		}
	}
	for _, file := range extra {
		switch file.name {
		case "server.json":
			var server struct {
				Version     string `json:"version"`
				Commit      string `json:"commit"`
				BuildDate   string `json:"buildDate"`
				Platform    string `json:"platform"`
				Container   string `json:"container"`
				InstallType string `json:"installType"`
				Uptime      int64  `json:"uptimeSeconds"`
			}
			if json.Unmarshal(file.body, &server) == nil {
				fmt.Fprintf(&b, "\n## Server\n\nVersion %s, commit %s, build date %s, platform %s, install type %s, container %s, uptime %s. Empty values were not available.\n", strconv.Quote(server.Version), strconv.Quote(server.Commit), strconv.Quote(server.BuildDate), strconv.Quote(server.Platform), strconv.Quote(server.InstallType), strconv.Quote(server.Container), reportUptime(server.Uptime))
			}
		case "storage.json":
			var storage StorageFact
			if json.Unmarshal(file.body, &storage) == nil {
				fmt.Fprintf(&b, "\n## Storage\n\nDatabase %s; WAL %s; filesystem %s; free space %s.\nCache filesystem %s; free space %s; configured budget %s.\n", reportOptionalBytes(storage.DatabaseBytes, storage.DatabaseKnown), reportOptionalBytes(storage.WALBytes, storage.WALKnown), strconv.Quote(storage.Filesystem), reportOptionalBytes(storage.DiskFreeBytes, storage.DiskFreeKnown), strconv.Quote(storage.CacheFilesystem), reportOptionalBytes(storage.CacheFreeBytes, storage.CacheFreeKnown), reportBytes(storage.CacheBudgetBytes))
				if storage.DatabaseAllocationStatus == "complete" {
					fmt.Fprintf(&b, "SQLite allocated pages: %s; free pages retained for reuse: %s (%d pages). Free pages exclude unused space inside occupied pages. These figures are a snapshot, not a growth history.\n", reportBytes(storage.DatabasePageCount*storage.DatabasePageSize), reportBytes(storage.DatabaseFreePages*storage.DatabasePageSize), storage.DatabaseFreePages)
				}
				if storage.DatabaseObjectsStatus == "complete" {
					b.WriteString("\nLargest database tables/indexes (up to 20):\n")
					for _, object := range storage.DatabaseObjects {
						fmt.Fprintf(&b, "- %s: %s\n", strconv.Quote(object.Name), reportBytes(object.Bytes))
					}
				} else if storage.DatabaseEngine == "sqlite" {
					b.WriteString("Database object allocation was unavailable within the bounded collection window.\n")
				}

			}
		case "logs/capture.log":
			printed := 0
			for i, line := range bytes.Split(file.body, []byte("\n")) {
				var entry exportLog
				if json.Unmarshal(line, &entry) != nil || (!strings.EqualFold(entry.Level, "WARN") && !strings.EqualFold(entry.Level, "ERROR")) {
					continue
				}
				if printed == 0 {
					b.WriteString("\n## Server warnings and errors\n\n")
				}
				if printed == 64 {
					b.WriteString("Additional log records are in logs/capture.log.\n")
					break
				}
				fmt.Fprintf(&b, "- %s: %s", entry.At.UTC().Format("15:04:05.000"), strconv.Quote(entry.Msg))
				if entry.Route != "" {
					fmt.Fprintf(&b, " on %s", strconv.Quote(entry.Route))
				}
				if entry.Dur != "" {
					fmt.Fprintf(&b, " (%s)", strconv.Quote(entry.Dur))
				}
				fmt.Fprintf(&b, " (logs/capture.log line %d).\n", i+1)
				printed++
			}
		}
	}
	return b.Bytes()
}
