package perfharness

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// budget is the latency a class must meet: p95 of the warm phase, the
// single cold request, and p95 under concurrent viewers or a busy writer.
type Budget struct {
	Warm, Cold, Loaded time.Duration
	Gated              bool
}

var Budgets = map[string]Budget{
	classStatic:   {Warm: 50 * time.Millisecond, Cold: 500 * time.Millisecond, Loaded: 150 * time.Millisecond, Gated: true},
	classSignIn:   {Warm: 100 * time.Millisecond, Cold: 500 * time.Millisecond, Loaded: 300 * time.Millisecond, Gated: true},
	classHome:     {Warm: 150 * time.Millisecond, Cold: time.Second, Loaded: 450 * time.Millisecond, Gated: true},
	classBrowse:   {Warm: 150 * time.Millisecond, Cold: time.Second, Loaded: 450 * time.Millisecond, Gated: true},
	classDetail:   {Warm: 150 * time.Millisecond, Cold: time.Second, Loaded: 450 * time.Millisecond, Gated: true},
	classSearch:   {Warm: 150 * time.Millisecond, Cold: time.Second, Loaded: 450 * time.Millisecond, Gated: true},
	classGuide:    {Warm: 150 * time.Millisecond, Cold: time.Second, Loaded: 450 * time.Millisecond, Gated: true},
	classConsole:  {Warm: 250 * time.Millisecond, Cold: 1500 * time.Millisecond, Loaded: 750 * time.Millisecond, Gated: true},
	classPlayback: {Warm: time.Second, Cold: 3 * time.Second, Loaded: 3 * time.Second},
}

// traceRecord mirrors app.PerfTraceRecord (one line of ETHER_PERF_TRACE).
type TraceRecord struct {
	Tag        string          `json:"tag"`
	Surface    string          `json:"surface"`
	Method     string          `json:"method"`
	Path       string          `json:"path"`
	Status     int             `json:"status"`
	Duration   time.Duration   `json:"duration"`
	Queries    int64           `json:"queries"`
	Rows       int64           `json:"rows"`
	DBTime     time.Duration   `json:"dbTime"`
	LockWait   time.Duration   `json:"lockWait"`
	Statements []statementStat `json:"statements"`
}

type statementStat struct {
	SQL   string        `json:"sql"`
	Site  string        `json:"site"`
	Count int64         `json:"count"`
	Rows  int64         `json:"rows"`
	Time  time.Duration `json:"time"`
}

func ReadTrace(path string) (map[string]TraceRecord, []TraceRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	byTag := map[string]TraceRecord{}
	var untagged []TraceRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var rec TraceRecord
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		if rec.Tag == "" {
			untagged = append(untagged, rec)
			continue
		}
		byTag[rec.Tag] = rec
	}
	return byTag, untagged, sc.Err()
}

// row is one endpoint's line in the report.
type Row struct {
	Client, Step, Class                    string
	Cold, WarmP50, WarmP95, ConcP95, BgP95 time.Duration
	Queries, Rows                          int64
	DBTime, LockWait                       time.Duration
	Status                                 string
	Fails                                  []string
	Skipped                                string
	Errors                                 []string
	samples                                map[string][]time.Duration
	queries, rowsRead                      []int64
	dbTimes, lockWaits                     []time.Duration
}

func percentile(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(float64(len(s)-1) * p)
	return s[i]
}

func median64(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]int64(nil), xs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

// summarize groups results by client and step and checks each against its
// class budget. A step with no samples in a phase is not judged for it.
func Summarize(results []Result, trace map[string]TraceRecord, order []Client) []*Row {
	rows := map[string]*Row{}
	var keys []string
	for _, c := range order {
		steps := c.Steps
		if c.SignIn != nil {
			steps = append([]Step{*c.SignIn}, steps...)
		}
		for _, st := range steps {
			k := c.Name + "\x00" + st.Name
			if rows[k] == nil {
				rows[k] = &Row{Client: c.Name, Step: st.Name, Class: st.Class, samples: map[string][]time.Duration{}}
				keys = append(keys, k)
			}
		}
	}
	for _, res := range results {
		r := rows[res.Client+"\x00"+res.Step]
		if r == nil {
			continue
		}
		if res.Skipped != "" {
			r.Skipped = res.Skipped
			continue
		}
		if res.Err != "" {
			if len(r.Errors) < 3 {
				r.Errors = append(r.Errors, fmt.Sprintf("%s %d %s", res.Phase, res.Status, res.Err))
			}
			continue
		}
		r.samples[res.Phase] = append(r.samples[res.Phase], res.Dur)
		if tr, ok := trace[res.Tag]; ok && (res.Phase == "warm" || res.Phase == "cold") {
			r.queries = append(r.queries, tr.Queries)
			r.rowsRead = append(r.rowsRead, tr.Rows)
			r.dbTimes = append(r.dbTimes, tr.DBTime)
			r.lockWaits = append(r.lockWaits, tr.LockWait)
		}
	}
	out := make([]*Row, 0, len(keys))
	for _, k := range keys {
		r := rows[k]
		b := Budgets[r.Class]
		if s := r.samples["cold"]; len(s) > 0 {
			r.Cold = s[0]
		}
		r.WarmP50 = percentile(r.samples["warm"], 0.5)
		r.WarmP95 = percentile(r.samples["warm"], 0.95)
		r.ConcP95 = percentile(r.samples["concurrent"], 0.95)
		r.BgP95 = percentile(r.samples["background"], 0.95)
		r.Queries, r.Rows = median64(r.queries), median64(r.rowsRead)
		r.DBTime = percentile(r.dbTimes, 0.5)
		r.LockWait = percentile(r.lockWaits, 1)
		check := func(name string, got, limit time.Duration, n int) {
			if n > 0 && got > limit {
				r.Fails = append(r.Fails, fmt.Sprintf("%s %s>%s", name, ms(got), ms(limit)))
			}
		}
		check("cold", r.Cold, b.Cold, len(r.samples["cold"]))
		check("warm", r.WarmP95, b.Warm, len(r.samples["warm"]))
		check("conc", r.ConcP95, b.Loaded, len(r.samples["concurrent"]))
		check("bg", r.BgP95, b.Loaded, len(r.samples["background"]))
		switch {
		case r.Skipped != "" && len(r.samples) == 0:
			r.Status = "SKIP"
		case len(r.Errors) > 0 && len(r.samples) == 0:
			r.Status = "ERROR"
		case !b.Gated:
			r.Status = "info"
		case len(r.Fails) > 0:
			r.Status = "FAIL"
		case len(r.Errors) > 0:
			r.Status = "ERROR"
		default:
			r.Status = "ok"
		}
		out = append(out, r)
	}
	return out
}

func ms(d time.Duration) string {
	if d == 0 {
		return "–"
	}
	if d < 10*time.Millisecond {
		return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
	}
	return fmt.Sprintf("%dms", d.Milliseconds())
}

func WriteReport(w io.Writer, title string, meta []string, rows []*Row) (failed int) {
	fmt.Fprintf(w, "# %s\n\n", title)
	for _, m := range meta {
		fmt.Fprintf(w, "- %s\n", m)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| client | step | class | cold | warm p50 | warm p95 | concurrent p95 | busy-writer p95 | queries | rows read | db time | lock wait | result |")
	fmt.Fprintln(w, "|---|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|")
	for _, r := range rows {
		status := r.Status
		if len(r.Fails) > 0 {
			status += " " + strings.Join(r.Fails, ", ")
		}
		if r.Status == "SKIP" {
			status += " " + r.Skipped
		}
		if len(r.Errors) > 0 {
			status += " " + strings.ReplaceAll(strings.Join(r.Errors, "; "), "|", "/")
		}
		if r.Status == "FAIL" || r.Status == "ERROR" {
			failed++
		}
		fmt.Fprintf(w, "| %s | %s | %s | %s | %s | %s | %s | %s | %d | %d | %s | %s | %s |\n",
			r.Client, strings.ReplaceAll(r.Step, "|", "/"), r.Class, ms(r.Cold), ms(r.WarmP50), ms(r.WarmP95), ms(r.ConcP95), ms(r.BgP95),
			r.Queries, r.Rows, ms(r.DBTime), ms(r.LockWait), status)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Budgets (p95): ")
	for _, c := range []string{classStatic, classSignIn, classHome, classBrowse, classDetail, classSearch, classGuide, classConsole, classPlayback} {
		b := Budgets[c]
		gate := "gated"
		if !b.Gated {
			gate = "reported only"
		}
		fmt.Fprintf(w, "- %s: warm %s, cold %s, loaded %s (%s)\n", c, ms(b.Warm), ms(b.Cold), ms(b.Loaded), gate)
	}
	return failed
}
