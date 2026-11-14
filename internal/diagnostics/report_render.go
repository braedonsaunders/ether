package diagnostics

import (
	"fmt"
	"html"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

func reportBytes(n int64) string {
	if n < 0 {
		return "unknown"
	}
	const unit = int64(1024)
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	index := 0
	for value >= 1024 && index < len(units)-1 {
		value /= 1024
		index++
	}
	return fmt.Sprintf("%.1f %s", value, units[index])
}

func reportDuration(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%d ms", ms)
	}
	if ms < 60000 {
		return fmt.Sprintf("%.1f s", float64(ms)/1000)
	}
	seconds := ms / 1000
	if seconds < 3600 {
		return fmt.Sprintf("%d min %d s", seconds/60, seconds%60)
	}
	return fmt.Sprintf("%d h %d min", seconds/3600, (seconds%3600)/60)
}

// reportRefs uses actual JSONL line numbers; they remain stable when earlier
// records were dropped by the archive redaction or byte budget.
func reportRefs(lines []int) string {
	if len(lines) == 0 {
		return "no retained request references"
	}
	ids := append([]int(nil), lines...)
	sort.Ints(ids)
	unique := ids[:0]
	for _, id := range ids {
		if len(unique) == 0 || unique[len(unique)-1] != id {
			unique = append(unique, id)
		}
	}
	ids = unique
	var ranges []string
	for i := 0; i < len(ids); {
		start, end := ids[i], ids[i]
		i++
		for i < len(ids) && ids[i] <= end+1 {
			if ids[i] > end {
				end = ids[i]
			}
			i++
		}
		if end > start {
			ranges = append(ranges, fmt.Sprintf("#%d–%d", start, end))
		} else {
			ranges = append(ranges, fmt.Sprintf("#%d", start))
		}
	}
	label := "requests "
	if len(ids) == 1 {
		label = "request "
	}
	return label + strings.Join(ranges, ", ")
}

func reportCell(s string) string {
	s = html.EscapeString(s)
	return strings.NewReplacer("|", "&#124;", "\n", " ", "\r", " ", "`", "&#96;").Replace(s)
}

func reportKeyParams(raw string) string {
	q, err := url.ParseQuery(raw)
	if err != nil {
		return ""
	}
	keys := []string{"contentDirectoryID", "pinnedContentDirectoryID", "excludeContinueWatching", "count", "startIndex", "limit", "skip"}
	var fields []string
	for _, key := range keys {
		for actual, vals := range q {
			if strings.EqualFold(actual, key) {
				for _, v := range vals {
					fields = append(fields, key+"="+v)
				}
				break
			}
		}
	}
	return strings.Join(fields, "&")
}

func reportResponse(e exportEvent) string {
	if e.Response == nil {
		return "Response details not captured"
	}
	r := e.Response
	var parts []string
	if len(r.Manifest) > 0 {
		return "DASH: " + strings.Join(r.Manifest, "; ")
	}
	if len(r.Hubs) > 0 {
		heroes := 0
		for _, h := range r.Hubs {
			if strings.EqualFold(h.Style, "hero") {
				heroes++
			}
		}
		label := "rows"
		if len(r.Hubs) == 1 {
			label = "row"
		}
		if e.Route == "/hubs/promoted" {
			label = "promoted " + label
		}
		heroLabel := "heroes"
		if heroes == 1 {
			heroLabel = "hero"
		}
		parts = append(parts, fmt.Sprintf("%d %s, %d %s", len(r.Hubs), label, heroes, heroLabel))
	} else if r.Decision != "" {
		parts = append(parts, r.Decision)
	} else {
		itemLabel := "items"
		if r.ItemCount == 1 {
			itemLabel = "item"
		}
		parts = append(parts, fmt.Sprintf("container size %d, %d direct %s", r.Size, r.ItemCount, itemLabel))
	}
	if r.ErrorCode != "" {
		parts = append(parts, "error "+r.ErrorCode)
	}
	if r.Truncated {
		parts = append(parts, "summary truncated")
	}
	return strings.Join(parts, "; ")
}

func reportBudget(e exportEvent) string {
	var parts []string
	if e.DurationMS > 2000 {
		parts = append(parts, fmt.Sprintf("wall time %s (%s over %s)", reportDuration(e.DurationMS), reportDuration(e.DurationMS-2000), reportDuration(2000)))
	}
	if e.DBTimeMS > 500 {
		parts = append(parts, fmt.Sprintf("database time %s (%s over %s)", reportDuration(e.DBTimeMS), reportDuration(e.DBTimeMS-500), reportDuration(500)))
	}
	if e.DBQueries > 300 {
		parts = append(parts, fmt.Sprintf("%d database statements (%d over 300)", e.DBQueries, e.DBQueries-300))
	}
	return strings.Join(parts, "; ")
}

func reportOverview(rows []reportRow, findings []Finding) []string {
	var sentences []string
	var duplicateCount int
	duplicateLines := map[int]bool{}
	has := map[string]bool{}
	for _, f := range findings {
		has[f.Code] = true
		if f.Code == "duplicate_hub_across_directories" {
			duplicateCount++
			for _, n := range f.Lines {
				duplicateLines[n] = true
			}
		}
	}
	if has["repeated_playback_failures"] {
		sentences = append(sentences, "Several playback decisions failed, so the affected playback requests may not have started.")
	}
	if has["http_error"] {
		sentences = append(sentences, "Some requests returned HTTP errors; the timeline shows which ones.")
	}
	if duplicateCount > 0 {
		lines := make([]int, 0, len(duplicateLines))
		for n := range duplicateLines {
			lines = append(lines, n)
		}
		qualifier := ""
		if has["findings_truncated"] {
			qualifier = "at least "
		}
		sentences = append(sentences, fmt.Sprintf("Plex requested Home separately for libraries or playlists, and Ether repeated %s%d row identifiers across those responses (%s); this could lead to duplicate or blank rows in the app.", qualifier, duplicateCount, reportRefs(lines)))
	}
	foundContinue := false
	sawHome := false
	for _, row := range rows {
		if row.event.Kind == KindRequest && strings.HasPrefix(strings.ToLower(row.event.Route), "/hubs") {
			sawHome = true
		}
		if row.event.Kind != KindRequest || row.event.Response == nil {
			continue
		}
		for _, hub := range row.event.Response.Hubs {
			if strings.Contains(strings.ToLower(hub.Identifier), "continue") {
				if row.event.Response.Truncated {
					sentences = append(sentences, fmt.Sprintf("Continue Watching appeared in request #%d, but its response summary was truncated.", row.line))
				} else {
					sentences = append(sentences, fmt.Sprintf("Continue Watching returned %d items in request #%d.", hub.ItemCount, row.line))
				}
				foundContinue = true
				break
			}
		}
		if foundContinue {
			break
		}
	}
	if !foundContinue && sawHome {
		sentences = append(sentences, "No Continue Watching hub was captured, so its contents are unknown.")
	}
	if has["slow_request"] {
		sentences = append(sentences, "Some requests exceeded the server's performance budgets; the amounts are listed below.")
	}
	if has["other_hero"] {
		sentences = append(sentences, "Ether also returned a prominent hero row that was not Continue Watching.")
	}
	if len(sentences) > 5 {
		sentences = sentences[:5]
	}
	count := 0
	for _, row := range rows {
		if row.event.Kind == KindRequest {
			count++
		}
	}
	if len(sentences) == 0 {
		sentences = append(sentences, fmt.Sprintf("The recording retained %d requests.", count))
	}
	if len(sentences) < 2 {
		if len(findings) == 0 {
			sentences = append(sentences, "No automated finding identified a problem.")
		} else {
			sentences = append(sentences, fmt.Sprintf("The recording retained %d requests.", count))
		}
	}
	if len(sentences) < 3 {
		sentences = append(sentences, "The timeline shows what the server returned for each retained request.")
	}
	return sentences
}

func reportClient(e exportEvent) string {
	name := e.Product
	if name == "" {
		name = e.Protocol
	}
	if name == "" {
		name = "Unknown"
	}
	if e.AppVersion != "" {
		name += " " + e.AppVersion
	}
	return name
}

func reportOptionalBytes(n int64, known bool) string {
	if !known {
		return "unknown"
	}
	return reportBytes(n)
}
func reportUptime(seconds int64) string {
	if seconds < 0 {
		return "unknown"
	}
	return reportDuration(seconds * 1000)
}
func reportTruncated(b bool) string {
	if b {
		return " (truncated)"
	}
	return ""
}

// writeStorageDetail renders what grew: tables with their index bytes,
// catalogue counts, the browsable bar, On Air assets and the files beside
// the database.
func writeStorageDetail(b io.Writer, s StorageFact) {
	if s.DatabaseAutoVacuum != "" || s.DatabaseJournalMode != "" {
		fmt.Fprintf(b, "Auto-vacuum %s; journal mode %s.\n", strconv.Quote(s.DatabaseAutoVacuum), strconv.Quote(s.DatabaseJournalMode))
	}
	if len(s.DatabaseTables) > 0 {
		fmt.Fprintf(b, "\nTables with their indexes (%s, %d ms; rows are planner estimates unless exact):\n", s.DatabaseTablesStatus, s.DatabaseTablesMillis)
		for _, t := range s.DatabaseTables {
			fmt.Fprintf(b, "- %s: %s (table %s, %d indexes %s), %d rows\n", strconv.Quote(t.Name), reportBytes(t.TableBytes+t.IndexBytes), reportBytes(t.TableBytes), t.Indexes, reportBytes(t.IndexBytes), t.Rows)
		}
	}
	if c := s.CatalogSettings; c != nil {
		fmt.Fprintf(b, "\nCatalogue browsable bar: minimum popularity %g, minimum votes %d, hide obscure titles %t, adult %t, upcoming movies %t.\n", c.MinPopularity, c.MinVotes, c.HideObscure, c.IncludeAdult, c.ShowUpcomingMovies)
	}
	if len(s.CatalogCounts) > 0 {
		io.WriteString(b, "Catalogue rows:\n")
		for _, c := range s.CatalogCounts {
			if c.Detailed == nil {
				fmt.Fprintf(b, "- type %s, browsable %t: %d\n", strconv.Quote(c.Type), c.Browsable, c.Rows)
			} else {
				fmt.Fprintf(b, "- browsable %t, detailed %t: %d\n", c.Browsable, *c.Detailed, c.Rows)
			}
		}
	}
	if len(s.OnAirAssets) > 0 {
		io.WriteString(b, "On Air assets by kind:\n")
		for _, a := range s.OnAirAssets {
			fmt.Fprintf(b, "- %s: %d, %s\n", strconv.Quote(a.Kind), a.Count, reportBytes(a.Bytes))
		}
	}
	fmt.Fprintf(b, "Pre-migrate snapshots: %d files, %s. Backups folder: %d files, %s.\n", s.PreMigrateSnapshots.Files, reportBytes(s.PreMigrateSnapshots.Bytes), s.Backups.Files, reportBytes(s.Backups.Bytes))
}
