// certification-gate checks the published client matrix before a release tag.
//
// Minor and patch tags at or after matrix.enforce_from fail unless that minor
// is listed in recertified_for and every client row has been reviewed.
// Earlier tags only check that the user-guide tables match the matrix.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

const pendingCell = "\u2014"

type gateFailure struct {
	msg  string
	code int
}

func (g gateFailure) Error() string { return g.msg }

type clientRow struct {
	Name    string            `json:"name"`
	Version string            `json:"version"`
	Support string            `json:"support,omitempty"`
	Waiver  string            `json:"waiver,omitempty"`
	Results map[string]string `json:"results"`
}

type protocolDoc struct {
	ID       string      `json:"id"`
	Features []string    `json:"features"`
	Clients  []clientRow `json:"clients"`
}

type matrixDoc struct {
	EnforceFrom    string        `json:"enforce_from"`
	RecertifiedFor []string      `json:"recertified_for"`
	Notes          string        `json:"notes,omitempty"`
	Protocols      []protocolDoc `json:"protocols"`
}

var featureLabel = map[string]string{
	"browse":         "Browse",
	"direct":         "Direct",
	"remux":          "Remux",
	"transcode":      "Transcode",
	"resume":         "Resume",
	"watched":        "Watched",
	"skip_intro":     "Skip intro",
	"subtitles":      "Subtitles",
	"live_tv":        "Live TV",
	"quick_connect":  "Quick Connect",
	"device_pairing": "Device pairing",
}

var requiredFeatures = map[string][]string{
	"jellyfin": {"browse", "direct", "remux", "transcode", "resume", "watched", "skip_intro", "subtitles", "live_tv", "quick_connect"},
	"emby":     {"browse", "direct", "remux", "transcode", "resume", "watched", "skip_intro", "subtitles", "live_tv", "quick_connect"},
	"silo":     {"browse", "direct", "remux", "transcode", "resume", "watched", "subtitles", "device_pairing"},
}

var requiredClients = map[string][]string{
	"jellyfin": {
		"Jellyfin for Android TV",
		"Swiftfin",
		"Findroid",
		"Streamyfin",
		"Infuse",
		"JellyCon",
	},
	"emby": {
		"Emby Web (externally hosted)",
		"Emby for Android TV",
	},
	"silo": {
		"Silo for Apple TV, iOS and macOS",
		"Silo for Android and Android TV",
	},
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		if g, ok := err.(gateFailure); ok && g.code != 0 {
			os.Exit(g.code)
		}
		os.Exit(2)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("certification-gate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	tag := fs.String("tag", "", "release tag, such as v0.3.0")
	matrixPath := fs.String("matrix", "docs/certification/matrix.json", "certification matrix")
	guidePath := fs.String("guide", "docs/user-guide.md", "user guide containing the published tables")
	if err := fs.Parse(args); err != nil || *tag == "" || fs.NArg() != 0 {
		return gateFailure{msg: "usage: certification-gate -tag vX.Y.Z [-matrix docs/certification/matrix.json] [-guide docs/user-guide.md]", code: 2}
	}
	doc, err := loadMatrix(*matrixPath)
	if err != nil {
		return gateFailure{msg: err.Error(), code: 2}
	}
	guide, err := os.ReadFile(*guidePath)
	if err != nil {
		return gateFailure{msg: err.Error(), code: 2}
	}
	if err := evaluate(*tag, doc, string(guide)); err != nil {
		return gateFailure{msg: err.Error(), code: 1}
	}
	fmt.Printf("certification gate passed for %s\n", *tag)
	return nil
}

func loadMatrix(path string) (matrixDoc, error) {
	var doc matrixDoc
	b, err := os.ReadFile(path)
	if err != nil {
		return doc, err
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return doc, fmt.Errorf("%s: %w", path, err)
	}
	return doc, nil
}

func evaluate(tag string, doc matrixDoc, guide string) error {
	minor, err := parseTag(tag)
	if err != nil {
		return err
	}
	if err := validateMatrix(doc); err != nil {
		return err
	}
	published, err := parseGuide(guide)
	if err != nil {
		return err
	}
	if err := guideMatches(doc, published); err != nil {
		return err
	}
	if !atLeast(minor, doc.EnforceFrom) {
		return nil
	}
	if !contains(doc.RecertifiedFor, minor) {
		return fmt.Errorf("minor %s is not in recertified_for; recertify the client matrix before this release", minor)
	}
	for _, protocol := range doc.Protocols {
		for _, client := range protocol.Clients {
			if err := rowReviewed(protocol.ID, client, protocol.Features); err != nil {
				return err
			}
		}
	}
	return nil
}

func parseTag(tag string) (string, error) {
	if !strings.HasPrefix(tag, "v") {
		return "", fmt.Errorf("tag %q must look like vX.Y.Z", tag)
	}
	body := strings.SplitN(strings.TrimPrefix(tag, "v"), "-", 2)[0]
	parts := strings.Split(body, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("tag %q must be vMAJOR.MINOR.PATCH", tag)
	}
	for _, part := range parts {
		if part == "" || strings.TrimLeft(part, "0123456789") != "" {
			return "", fmt.Errorf("tag %q must be vMAJOR.MINOR.PATCH", tag)
		}
		if _, err := strconv.Atoi(part); err != nil {
			return "", fmt.Errorf("tag %q must be vMAJOR.MINOR.PATCH", tag)
		}
	}
	return parts[0] + "." + parts[1], nil
}

func atLeast(minor, floor string) bool {
	return !minorLess(minor, floor)
}

func minorLess(a, b string) bool {
	ap, bp := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 2; i++ {
		ai, aerr := strconv.Atoi(ap[i])
		bi, berr := strconv.Atoi(bp[i])
		if aerr != nil || berr != nil {
			return a < b
		}
		if ai != bi {
			return ai < bi
		}
	}
	return false
}

func validateMatrix(doc matrixDoc) error {
	if _, err := splitMinor(doc.EnforceFrom); err != nil {
		return fmt.Errorf("enforce_from: %w", err)
	}
	for _, minor := range doc.RecertifiedFor {
		if _, err := splitMinor(minor); err != nil {
			return fmt.Errorf("recertified_for %q: %w", minor, err)
		}
	}
	if len(doc.Protocols) != len(requiredFeatures) {
		return fmt.Errorf("matrix must list jellyfin, emby, and silo")
	}
	seen := map[string]bool{}
	for _, protocol := range doc.Protocols {
		if seen[protocol.ID] {
			return fmt.Errorf("duplicate protocol %s", protocol.ID)
		}
		seen[protocol.ID] = true
		want, ok := requiredFeatures[protocol.ID]
		if !ok {
			return fmt.Errorf("unknown protocol %s", protocol.ID)
		}
		if !sameStrings(protocol.Features, want) {
			return fmt.Errorf("%s features must be %s", protocol.ID, strings.Join(want, ", "))
		}
		names := map[string]bool{}
		for _, client := range protocol.Clients {
			if client.Name == "" || names[client.Name] {
				return fmt.Errorf("%s has a missing or duplicate client", protocol.ID)
			}
			names[client.Name] = true
			if err := rowShape(protocol.ID, client, protocol.Features); err != nil {
				return err
			}
		}
		for _, name := range requiredClients[protocol.ID] {
			if !names[name] {
				return fmt.Errorf("%s matrix is missing %s", protocol.ID, name)
			}
		}
	}
	for id := range requiredFeatures {
		if !seen[id] {
			return fmt.Errorf("matrix is missing %s", id)
		}
	}
	return nil
}

func splitMinor(minor string) ([2]int, error) {
	parts := strings.Split(minor, ".")
	var out [2]int
	if len(parts) != 2 {
		return out, fmt.Errorf("minor %q must be MAJOR.MINOR", minor)
	}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || part == "" || strings.TrimLeft(part, "0123456789") != "" {
			return out, fmt.Errorf("minor %q must be MAJOR.MINOR", minor)
		}
		out[i] = n
	}
	return out, nil
}

func rowShape(protocol string, client clientRow, features []string) error {
	if len(client.Results) != len(features) {
		return fmt.Errorf("%s %s result columns do not match the feature list", protocol, client.Name)
	}
	counts := map[string]int{}
	for _, feature := range features {
		value, ok := client.Results[feature]
		if !ok {
			return fmt.Errorf("%s %s is missing %s", protocol, client.Name, feature)
		}
		switch value {
		case pendingCell, "pass", "fail", "unsupported", "waived":
			counts[value]++
		default:
			return fmt.Errorf("%s %s %s has result %q", protocol, client.Name, feature, value)
		}
	}
	unsupported := counts["unsupported"] > 0
	waived := counts["waived"] > 0
	if unsupported && (counts["unsupported"] != len(features) || client.Support != "unsupported" || client.Version == "" || client.Version == "Pending") {
		return fmt.Errorf("%s %s must mark every cell unsupported, with a real version and support unsupported", protocol, client.Name)
	}
	if waived && (counts["waived"] != len(features) || client.Support != "waived" || strings.TrimSpace(client.Waiver) == "") {
		return fmt.Errorf("%s %s waiver requires support waived, a reason, and every cell waived", protocol, client.Name)
	}
	if client.Version == "Pending" && counts[pendingCell] != len(features) {
		return fmt.Errorf("%s %s is Pending but has reviewed cells", protocol, client.Name)
	}
	if client.Support == "unsupported" && counts["unsupported"] != len(features) {
		return fmt.Errorf("%s %s support is unsupported but a cell is not", protocol, client.Name)
	}
	return nil
}

func rowReviewed(protocol string, client clientRow, features []string) error {
	if client.Version == "" || client.Version == "Pending" {
		return fmt.Errorf("%s %s has no reviewed version", protocol, client.Name)
	}
	kinds := map[string]bool{}
	for _, feature := range features {
		value := client.Results[feature]
		switch value {
		case "pass", "unsupported", "waived":
			kinds[value] = true
		default:
			return fmt.Errorf("%s %s %s is %q; recertify it before this release", protocol, client.Name, feature, value)
		}
	}
	if len(kinds) != 1 {
		return fmt.Errorf("%s %s mixes pass, unsupported, and waived results", protocol, client.Name)
	}
	return nil
}

func parseGuide(guide string) (map[string][]clientRow, error) {
	out := map[string][]clientRow{}
	for id, features := range requiredFeatures {
		start := "<!-- certification-matrix:" + id + " -->"
		end := "<!-- /certification-matrix:" + id + " -->"
		i := strings.Index(guide, start)
		j := strings.Index(guide, end)
		if i < 0 || j < i {
			return nil, fmt.Errorf("user guide is missing %s", start)
		}
		rows, err := parseTable(id, features, guide[i+len(start):j])
		if err != nil {
			return nil, err
		}
		out[id] = rows
	}
	return out, nil
}

func parseTable(protocol string, features []string, body string) ([]clientRow, error) {
	var header []string
	var rows []clientRow
	for _, line := range strings.Split(body, "\n") {
		trim := strings.TrimSpace(line)
		if !strings.HasPrefix(trim, "|") {
			continue
		}
		cells := splitRow(trim)
		if len(cells) == 0 {
			continue
		}
		if isSeparator(cells) {
			continue
		}
		if header == nil {
			header = cells
			want := append([]string{"Client", "Version"}, labels(features)...)
			if !sameStrings(header, want) {
				return nil, fmt.Errorf("%s table header is %s", protocol, strings.Join(header, " | "))
			}
			continue
		}
		if len(cells) != len(header) {
			return nil, fmt.Errorf("%s row %q has %d columns", protocol, cells[0], len(cells))
		}
		row := clientRow{Name: cells[0], Version: cells[1], Results: map[string]string{}}
		for i, feature := range features {
			row.Results[feature] = cells[i+2]
		}
		rows = append(rows, row)
	}
	if header == nil || len(rows) == 0 {
		return nil, fmt.Errorf("%s table is empty", protocol)
	}
	return rows, nil
}

func labels(features []string) []string {
	out := make([]string, len(features))
	for i, feature := range features {
		label, ok := featureLabel[feature]
		if !ok {
			label = feature
		}
		out[i] = label
	}
	return out
}

func splitRow(line string) []string {
	parts := strings.Split(line, "|")
	if len(parts) < 2 {
		return nil
	}
	parts = parts[1 : len(parts)-1]
	out := make([]string, len(parts))
	for i, part := range parts {
		out[i] = strings.TrimSpace(part)
	}
	return out
}

func isSeparator(cells []string) bool {
	for _, cell := range cells {
		trim := strings.Trim(cell, " :-")
		if trim != "" {
			return false
		}
	}
	return true
}

func guideMatches(doc matrixDoc, published map[string][]clientRow) error {
	for _, protocol := range doc.Protocols {
		rows := published[protocol.ID]
		if len(rows) != len(protocol.Clients) {
			return fmt.Errorf("%s user-guide table has %d clients, matrix has %d", protocol.ID, len(rows), len(protocol.Clients))
		}
		for i, want := range protocol.Clients {
			got := rows[i]
			if got.Name != want.Name || got.Version != want.Version {
				return fmt.Errorf("%s row %d is %s %s, matrix has %s %s", protocol.ID, i+1, got.Name, got.Version, want.Name, want.Version)
			}
			for _, feature := range protocol.Features {
				if got.Results[feature] != want.Results[feature] {
					return fmt.Errorf("%s %s %s is %q in the user guide and %q in the matrix", protocol.ID, want.Name, feature, got.Results[feature], want.Results[feature])
				}
			}
		}
	}
	return nil
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

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
