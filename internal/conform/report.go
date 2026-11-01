package conform

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// PathSummary is one schema difference aggregated across the clients that
// showed it.
type PathSummary struct {
	Format    string   `json:"format"`
	Path      string   `json:"path"`
	Kind      string   `json:"kind"`
	Reference string   `json:"reference,omitempty"`
	Target    string   `json:"target,omitempty"`
	Clients   []string `json:"clients"`
	Count     int      `json:"count,omitempty"`
	Nested    int      `json:"nested,omitempty"`
	Blocking  bool     `json:"blocking,omitempty"`
}

// EndpointSummary groups the differences of one endpoint (all clients and
// formats).
type EndpointSummary struct {
	Endpoint string        `json:"endpoint"`
	Missing  []PathSummary `json:"missing,omitempty"`
	Extra    []PathSummary `json:"extra,omitempty"`
	Type     []PathSummary `json:"type,omitempty"`
	Value    []PathSummary `json:"value,omitempty"`
	// Other holds status, header, parse and request errors.
	Other []PathSummary `json:"other,omitempty"`
}

// Blocking counts the endpoint's blocking path differences.
func (e EndpointSummary) Blocking() int {
	n := len(e.Missing) + len(e.Extra) + len(e.Type) + len(e.Other)
	for _, p := range e.Value {
		if p.Blocking {
			n++
		}
	}
	return n
}

// Summarize groups a report by endpoint, then by (format, kind, path), so a
// gap that every client shows is listed once.
func Summarize(r Report) []EndpointSummary {
	type key struct{ endpoint, format, kind, path string }
	agg := map[key]*PathSummary{}
	var order []key
	endpoints := map[string]bool{}
	for _, e := range r.Endpoints {
		endpoints[e] = true
	}
	for _, d := range r.Differences {
		ep := d.Endpoint
		if ep == "" {
			ep = "(unknown)"
		}
		endpoints[ep] = true
		k := key{ep, d.Format, d.Kind, d.Path}
		ps := agg[k]
		if ps == nil {
			ps = &PathSummary{Format: d.Format, Path: d.Path, Kind: d.Kind, Reference: str(d.Reference), Target: str(d.Target)}
			agg[k] = ps
			order = append(order, k)
		}
		if d.Client != "" && !containsString(ps.Clients, d.Client) {
			ps.Clients = append(ps.Clients, d.Client)
		}
		if d.Count > ps.Count {
			ps.Count = d.Count
		}
		if d.Nested > ps.Nested {
			ps.Nested = d.Nested
		}
		if d.Blocking {
			ps.Blocking = true
		}
	}
	byEP := map[string]*EndpointSummary{}
	for ep := range endpoints {
		byEP[ep] = &EndpointSummary{Endpoint: ep}
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if a.format != b.format {
			return a.format > b.format // xml first
		}
		return a.path < b.path
	})
	for _, k := range order {
		ps := *agg[k]
		sort.Strings(ps.Clients)
		e := byEP[k.endpoint]
		switch k.kind {
		case KindMissing:
			e.Missing = append(e.Missing, ps)
		case KindExtra:
			e.Extra = append(e.Extra, ps)
		case KindType:
			e.Type = append(e.Type, ps)
		case KindValue:
			e.Value = append(e.Value, ps)
		default:
			e.Other = append(e.Other, ps)
		}
	}
	out := make([]EndpointSummary, 0, len(byEP))
	for _, e := range byEP {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Endpoint < out[j].Endpoint })
	return out
}

func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func containsString(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// WriteSummary prints, per endpoint, the missing and extra attributes or
// elements and the type mismatches (and, with values, the informational
// value differences), followed by a count table.
func WriteSummary(w io.Writer, r Report, values bool) {
	sums := Summarize(r)
	for _, d := range r.ReferenceDefects {
		fmt.Fprintf(w, "REFERENCE DEFECT: %s (%s): %s\n", d.ID, d.Observed, d.Detail)
	}
	for _, line := range r.Discovery {
		fmt.Fprintf(w, "INFO: %s\n", line)
	}
	for _, e := range sums {
		if e.Blocking() == 0 && (!values || len(e.Value) == 0) {
			continue
		}
		fmt.Fprintf(w, "\n== %s\n", e.Endpoint)
		section := func(title string, xs []PathSummary, detail func(PathSummary) string) {
			if len(xs) == 0 {
				return
			}
			fmt.Fprintf(w, "  %s (%d):\n", title, len(xs))
			for _, p := range xs {
				fmt.Fprintf(w, "    %-4s %s%s [%s]\n", p.Format, p.Path, detail(p), clientList(p.Clients))
			}
		}
		section("errors", e.Other, func(p PathSummary) string {
			return fmt.Sprintf(" %s ref=%q target=%q", p.Kind, p.Reference, p.Target)
		})
		section("missing", e.Missing, func(p PathSummary) string {
			s := fmt.Sprintf(" x%d", p.Count)
			if p.Nested > 0 {
				s += fmt.Sprintf(" (+%d nested)", p.Nested)
			}
			if p.Reference != "" {
				s += fmt.Sprintf(" e.g. %q", p.Reference)
			}
			return s
		})
		section("extra", e.Extra, func(p PathSummary) string {
			s := fmt.Sprintf(" x%d", p.Count)
			if p.Nested > 0 {
				s += fmt.Sprintf(" (+%d nested)", p.Nested)
			}
			if p.Target != "" {
				s += fmt.Sprintf(" e.g. %q", p.Target)
			}
			return s
		})
		section("type", e.Type, func(p PathSummary) string {
			return fmt.Sprintf(" ref=%s target=%s", p.Reference, p.Target)
		})
		shown := e.Value
		if !values {
			shown = nil
			for _, p := range e.Value {
				if p.Blocking {
					shown = append(shown, p)
				}
			}
		}
		if len(shown) > 0 {
			section("value", shown, func(p PathSummary) string {
				return fmt.Sprintf(" ref=%q target=%q", p.Reference, p.Target)
			})
		}
	}
	fmt.Fprintln(w)
	WriteTable(w, sums)
}

// WriteTable prints a Markdown table of per-endpoint difference counts
// (distinct schema paths across clients).
func WriteTable(w io.Writer, sums []EndpointSummary) {
	fmt.Fprintln(w, "| Endpoint | Errors | Missing | Extra | Type | Value |")
	fmt.Fprintln(w, "| --- | ---: | ---: | ---: | ---: | ---: |")
	var t [5]int
	for _, e := range sums {
		row := [5]int{len(e.Other), len(e.Missing), len(e.Extra), len(e.Type), len(e.Value)}
		for i := range t {
			t[i] += row[i]
		}
		fmt.Fprintf(w, "| `%s` | %d | %d | %d | %d | %d |\n", e.Endpoint, row[0], row[1], row[2], row[3], row[4])
	}
	fmt.Fprintf(w, "| **total** | %d | %d | %d | %d | %d |\n", t[0], t[1], t[2], t[3], t[4])
}

func clientList(cs []string) string {
	if len(cs) == 0 {
		return "-"
	}
	return strings.Join(cs, ",")
}

// WriteText prints every difference grouped by request (client, format,
// endpoint).
func WriteText(w io.Writer, r Report, values bool) {
	for _, d := range r.ReferenceDefects {
		fmt.Fprintf(w, "REFERENCE DEFECT: %s (%s): %s\n", d.ID, d.Observed, d.Detail)
	}
	for _, line := range r.Discovery {
		fmt.Fprintf(w, "INFO: %s\n", line)
	}
	type req struct{ client, format, endpoint string }
	groups := map[req][]Difference{}
	var keys []req
	for _, d := range r.Differences {
		if !values && !d.Blocking {
			continue
		}
		k := req{d.Client, d.Format, d.Endpoint}
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], d)
	}
	if len(keys) == 0 {
		fmt.Fprintln(w, "No errors or structural differences.")
		return
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.endpoint != b.endpoint {
			return a.endpoint < b.endpoint
		}
		if a.client != b.client {
			return a.client < b.client
		}
		return a.format > b.format
	})
	for _, k := range keys {
		fmt.Fprintf(w, "\n== %s %s %s\n", k.client, k.format, k.endpoint)
		for _, d := range groups[k] {
			tag := "BLOCK"
			if !d.Blocking {
				tag = "info "
			}
			fmt.Fprintf(w, "  %s %-7s %s", tag, d.Kind, d.Path)
			switch d.Kind {
			case KindMissing:
				fmt.Fprintf(w, " x%d", d.Count)
			case KindExtra:
				fmt.Fprintf(w, " x%d", d.Count)
			}
			if d.Nested > 0 {
				fmt.Fprintf(w, " (+%d nested)", d.Nested)
			}
			if d.Reference != nil || d.Target != nil {
				fmt.Fprintf(w, " ref=%q target=%q", str(d.Reference), str(d.Target))
			}
			fmt.Fprintln(w)
		}
	}
}
