package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type blockingRow struct {
	Protocol string `json:"protocol"`
	Source   string `json:"source"`
	Endpoint string `json:"endpoint"`
	Kind     string `json:"kind"`
	Path     string `json:"path"`
	Client   string `json:"client,omitempty"`
	Format   string `json:"format,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

type reportDoc struct {
	APIDriftMarkdown string        `json:"api_drift_markdown,omitempty"`
	Images           []imageChange `json:"images"`
	Recorded         bool          `json:"recorded"`
	RecordError      string        `json:"record_error,omitempty"`
	RecordNote       string        `json:"record_note"`
	Blocking         []blockingRow `json:"blocking,omitempty"`
}

func loadBlockingDir(dir string) ([]blockingRow, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var rows []blockingRow
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		protocol, source, ok := strings.Cut(strings.TrimSuffix(name, ".json"), "-")
		if !ok || (protocol != "jellyfin" && protocol != "emby" && protocol != "silo") || source == "" {
			return nil, fmt.Errorf("blocking report %s must be named <protocol>-<source>.json", name)
		}
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		parsed, err := blockingFrom(protocol, source, body)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		rows = append(rows, parsed...)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Protocol != rows[j].Protocol {
			return rows[i].Protocol < rows[j].Protocol
		}
		if rows[i].Endpoint != rows[j].Endpoint {
			return rows[i].Endpoint < rows[j].Endpoint
		}
		if rows[i].Path != rows[j].Path {
			return rows[i].Path < rows[j].Path
		}
		return rows[i].Client < rows[j].Client
	})
	return rows, nil
}

func blockingFrom(protocol, source string, body []byte) ([]blockingRow, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, err
	}
	if raw, ok := probe["differences"]; ok {
		var diffs []struct {
			Client   string `json:"client"`
			Format   string `json:"format"`
			Endpoint string `json:"endpoint"`
			Path     string `json:"path"`
			Kind     string `json:"kind"`
			Blocking bool   `json:"blocking"`
			Reason   string `json:"reason"`
		}
		if err := json.Unmarshal(raw, &diffs); err != nil {
			return nil, err
		}
		var rows []blockingRow
		for _, diff := range diffs {
			if !diff.Blocking {
				continue
			}
			endpoint := diff.Endpoint
			if endpoint == "" {
				endpoint = diff.Path
			}
			rows = append(rows, blockingRow{
				Protocol: protocol, Source: source, Endpoint: endpoint, Kind: diff.Kind,
				Path: diff.Path, Client: diff.Client, Format: diff.Format, Reason: diff.Reason,
			})
		}
		return rows, nil
	}
	if raw, ok := probe["endpoints"]; ok {
		var endpoints []struct {
			Endpoint string        `json:"endpoint"`
			Missing  []pathSummary `json:"missing"`
			Extra    []pathSummary `json:"extra"`
			Type     []pathSummary `json:"type"`
			Other    []pathSummary `json:"other"`
		}
		if err := json.Unmarshal(raw, &endpoints); err != nil {
			return nil, err
		}
		var rows []blockingRow
		add := func(endpoint, kind string, items []pathSummary) {
			for _, item := range items {
				itemKind := item.Kind
				if itemKind == "" {
					itemKind = kind
				}
				rows = append(rows, blockingRow{
					Protocol: protocol, Source: source, Endpoint: endpoint, Kind: itemKind,
					Path: item.Path, Client: strings.Join(item.Clients, ","), Format: item.Format,
				})
			}
		}
		for _, endpoint := range endpoints {
			add(endpoint.Endpoint, "missing", endpoint.Missing)
			add(endpoint.Endpoint, "extra", endpoint.Extra)
			add(endpoint.Endpoint, "type", endpoint.Type)
			add(endpoint.Endpoint, "other", endpoint.Other)
		}
		return rows, nil
	}
	return nil, fmt.Errorf("report has neither differences nor endpoints")
}

type pathSummary struct {
	Format  string   `json:"format"`
	Path    string   `json:"path"`
	Kind    string   `json:"kind"`
	Clients []string `json:"clients"`
}

func recordNote(recorded bool) string {
	if recorded {
		return "Corpus re-recorded under drift-watch-out/, which is gitignored. Blocking rows are the endpoint diffs. Fixtures stay out of the pull request unless DRIFT_WATCH_COMMIT_FIXTURES=1."
	}
	return "This run did not re-record the corpus. On bb-worker-server or gpu-workstation, during a Lab Keeper window, run DRIFT_WATCH_APPLY=1 DRIFT_WATCH_OPEN_PR=1 scripts/drift-watch.sh run. Keep scripts/lab/lab.env mode 600 and do not commit it."
}

func shouldOpen(doc reportDoc) bool {
	if strings.TrimSpace(doc.APIDriftMarkdown) != "" || strings.TrimSpace(doc.RecordError) != "" || len(doc.Blocking) > 0 {
		return true
	}
	for _, image := range doc.Images {
		if image.Changed {
			return true
		}
	}
	return false
}

func renderPRBody(doc reportDoc, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Drift watch %s. This pull request lists OpenAPI changes for endpoints Ether implements, reference image pin bumps, and blocking conformance diffs. It does not merge itself.\n\n", now.UTC().Format("2006-01-02"))
	b.WriteString("## Reference containers\n\n")
	b.WriteString("| Service | Action | Image |\n| --- | --- | --- |\n")
	for _, image := range doc.Images {
		action := "unchanged"
		if image.Changed {
			action = "bump from `" + image.From + "`"
		}
		fmt.Fprintf(&b, "| %s | %s | `%s` |\n", cell(image.Service), cell(action), cell(image.To))
	}
	b.WriteString("\nSilo's ffmpeg seed container is left on its current pin.\n\n")
	writeDriftSection(&b, doc.APIDriftMarkdown)
	b.WriteString("\n## Blocking conformance diffs\n\n")
	switch {
	case doc.RecordError != "":
		fmt.Fprintf(&b, "Recording failed: %s\n\n", cell(doc.RecordError))
	case len(doc.Blocking) == 0 && doc.Recorded:
		b.WriteString("The re-recorded corpus has no blocking conformance diffs.\n\n")
	case len(doc.Blocking) == 0:
		b.WriteString("No blocking conformance diffs were collected on this run.\n\n")
	default:
		limit := len(doc.Blocking)
		if limit > 80 {
			limit = 80
		}
		b.WriteString("| Protocol | Source | Endpoint | Kind | Path | Client |\n| --- | --- | --- | --- | --- | --- |\n")
		for _, row := range doc.Blocking[:limit] {
			fmt.Fprintf(&b, "| %s | %s | `%s` | %s | `%s` | %s |\n", cell(row.Protocol), cell(row.Source), cell(row.Endpoint), cell(row.Kind), cell(row.Path), cell(row.Client))
		}
		if limit < len(doc.Blocking) {
			fmt.Fprintf(&b, "\nShowing %d of %d blocking rows. The rest are in `docs/acceptance/drift-watch-report.json`.\n", limit, len(doc.Blocking))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "%s\n\n", doc.RecordNote)
	b.WriteString("Re-record from the lab checkout on bb-worker-server or gpu-workstation. Do not run the reference stack on the Mac or on 10.0.0.43.\n\n```sh\nDRIFT_WATCH_APPLY=1 DRIFT_WATCH_OPEN_PR=1 scripts/drift-watch.sh run\n```\n")
	return b.String()
}

func writeDriftSection(b *strings.Builder, markdown string) {
	b.WriteString("## Endpoints Ether implements\n\n")
	implemented, other, ok := splitDriftTable(markdown)
	if strings.TrimSpace(markdown) == "" {
		b.WriteString("No OpenAPI drift was detected.\n")
		return
	}
	if !ok {
		b.WriteString(strings.TrimSpace(markdown))
		b.WriteString("\n")
		return
	}
	if len(implemented) == 0 {
		b.WriteString("No added, removed, or changed endpoints that Ether implements.\n\n")
	} else {
		writeChangeTable(b, implemented)
	}
	if len(other) == 0 {
		return
	}
	fmt.Fprintf(b, "\n## Other upstream endpoints\n\n%d upstream endpoints changed outside the routes Ether implements or the recorded clients call.\n\n", len(other))
	shown := other
	if len(shown) > 40 {
		shown = shown[:40]
		fmt.Fprintf(b, "Showing the first 40 of %d. The full table is in `docs/acceptance/drift-watch-report.json`.\n\n", len(other))
	}
	writeChangeTable(b, shown)
}

func writeChangeTable(b *strings.Builder, rows [][]string) {
	b.WriteString("| Priority | Protocol | Change | Method | Endpoint |\n| --- | --- | --- | --- | --- |\n")
	for _, row := range rows {
		for len(row) < 5 {
			row = append(row, "")
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s |\n", cell(row[0]), cell(row[1]), cell(row[2]), cell(row[3]), cell(row[4]))
	}
}

func splitDriftTable(markdown string) (implemented, other [][]string, ok bool) {
	var header []string
	for _, line := range strings.Split(markdown, "\n") {
		trim := strings.TrimSpace(line)
		if !strings.HasPrefix(trim, "|") {
			continue
		}
		cells := splitCells(trim)
		if len(cells) == 0 || isRule(cells) {
			continue
		}
		if header == nil {
			header = cells
			if len(header) < 5 || header[0] != "Priority" {
				return nil, nil, false
			}
			continue
		}
		if len(cells) < 5 {
			continue
		}
		switch cells[0] {
		case "used-by-clients", "implemented":
			implemented = append(implemented, cells)
		default:
			other = append(other, cells)
		}
	}
	if header == nil {
		return nil, nil, false
	}
	return implemented, other, true
}

func splitCells(line string) []string {
	parts := strings.Split(line, "|")
	if len(parts) < 2 {
		return nil
	}
	parts = parts[1 : len(parts)-1]
	out := make([]string, len(parts))
	for i, part := range parts {
		out[i] = strings.TrimSpace(strings.ReplaceAll(part, "\\|", "|"))
	}
	return out
}

func isRule(cells []string) bool {
	for _, cell := range cells {
		if strings.Trim(cell, " :-") != "" {
			return false
		}
	}
	return true
}

func cell(value string) string {
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "|", "\\|")
	if len(value) > 180 {
		value = value[:177] + "..."
	}
	return value
}
