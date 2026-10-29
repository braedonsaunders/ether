package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/braedonsaunders/ether/internal/conform/specs"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontends/mediabrowser"
)

type inventoryRow struct {
	Method     string `json:"method"`
	Path       string `json:"path"`
	Registered bool   `json:"registered"`
}
type inventoryReport struct {
	Protocol   string         `json:"protocol"`
	SpecSHA256 string         `json:"spec_sha256"`
	Caveat     string         `json:"caveat"`
	Routes     []inventoryRow `json:"routes"`
}

func normalizeRoute(path string) string {
	return strings.ToLower(regexp.MustCompile(`\{[^{}]+\}`).ReplaceAllString(strings.TrimSuffix(path, "/"), "{}"))
}
func inventory(args []string) error {
	fs := flag.NewFlagSet("inventory", flag.ContinueOnError)
	protocol := fs.String("protocol", "", "jellyfin or emby")
	spec := fs.String("spec", "", "pinned OpenAPI JSON")
	if e := fs.Parse(args); e != nil {
		return e
	}
	if *spec == "" {
		return fmt.Errorf("spec is required")
	}
	data, e := os.ReadFile(*spec)
	if e != nil {
		return e
	}
	if e = validatePinnedSpec(*protocol, data); e != nil {
		return e
	}
	report, e := buildInventory(*protocol, data)
	if e != nil {
		return e
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}
func buildInventory(protocol string, data []byte) (inventoryReport, error) {
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if e := json.Unmarshal(data, &doc); e != nil || len(doc.Paths) == 0 {
		return inventoryReport{}, fmt.Errorf("invalid OpenAPI paths")
	}
	var instance frontend.Instance
	var err error
	host := frontend.Host{Identity: frontendtest.Identity{}}
	switch protocol {
	case "jellyfin":
		instance, err = (mediabrowser.Jellyfin{MachineID: "scorecard"}).Mount(context.Background(), host)
	case "emby":
		instance, err = (mediabrowser.Emby{MachineID: "scorecard"}).Mount(context.Background(), host)
	default:
		return inventoryReport{}, fmt.Errorf("protocol must be jellyfin or emby")
	}
	if err != nil {
		return inventoryReport{}, err
	}
	defer instance.Close(context.Background())
	source, ok := instance.(frontend.RouteSource)
	if !ok {
		return inventoryReport{}, fmt.Errorf("route inventory unavailable")
	}
	registered := map[string]bool{}
	for _, r := range source.Routes() {
		registered[strings.ToUpper(r.Method)+" "+normalizeRoute(r.Pattern)] = true
	}
	digest := sha256.Sum256(data)
	out := inventoryReport{Protocol: protocol, SpecSHA256: hex.EncodeToString(digest[:]), Caveat: "Route registration is not proof of feature completeness; unregistered routes require review for aliases, licensing, and applicability.", Routes: []inventoryRow{}}
	for p, operations := range doc.Paths {
		for method := range operations {
			switch strings.ToUpper(method) {
			case "GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS":
				m := strings.ToUpper(method)
				out.Routes = append(out.Routes, inventoryRow{m, p, registered[m+" "+normalizeRoute(p)]})
			}
		}
	}
	sort.Slice(out.Routes, func(i, j int) bool {
		if out.Routes[i].Path == out.Routes[j].Path {
			return out.Routes[i].Method < out.Routes[j].Method
		}
		return out.Routes[i].Path < out.Routes[j].Path
	})
	return out, nil
}

func validatePinnedSpec(protocol string, data []byte) error {
	var manifest struct {
		Specs []struct {
			Protocol string `json:"protocol"`
			SHA256   string `json:"sha256"`
		} `json:"specs"`
	}
	if e := json.Unmarshal(specs.Manifest, &manifest); e != nil {
		return e
	}
	sum := sha256.Sum256(data)
	actual := hex.EncodeToString(sum[:])
	for _, item := range manifest.Specs {
		if item.Protocol == protocol {
			if actual != item.SHA256 {
				return fmt.Errorf("spec digest does not match pinned %s specification", protocol)
			}
			return nil
		}
	}
	return fmt.Errorf("protocol is not pinned")
}
