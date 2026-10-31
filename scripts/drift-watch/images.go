package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	dockerRegistryHost = "registry-1.docker.io"
	dockerAuthService  = "registry.docker.io"
	ghcrHost           = "ghcr.io"
)

var (
	semverTag = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)+$`)
	digestRE  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type imageChange struct {
	Service string `json:"service"`
	From    string `json:"from"`
	To      string `json:"to"`
	Changed bool   `json:"changed"`
}

type imageSpec struct {
	service    string
	repository string
	release    string
	tag        string
	ghcr       bool
}

var catalog = []imageSpec{
	{service: "jellyfin", repository: "jellyfin/jellyfin", release: "jellyfin/jellyfin"},
	{service: "emby", repository: "emby/embyserver", release: "MediaBrowser/Emby.Releases"},
	{service: "silo", repository: "silo-server/silo-server", tag: "aio", ghcr: true},
}

type resolver struct {
	client         *http.Client
	githubAPI      string
	dockerAuth     string
	dockerRegistry string
	ghcr           string
}

func defaultResolver() resolver {
	return resolver{
		client:         &http.Client{Timeout: 45 * time.Second},
		githubAPI:      "https://api.github.com",
		dockerAuth:     "https://auth.docker.io",
		dockerRegistry: "https://" + dockerRegistryHost,
		ghcr:           "https://" + ghcrHost,
	}
}

func (r resolver) plan(ctx context.Context, compose string) (string, []imageChange, error) {
	current, err := serviceImages(compose)
	if err != nil {
		return "", nil, err
	}
	updated := compose
	changes := make([]imageChange, 0, len(catalog))
	for _, spec := range catalog {
		tag := spec.tag
		if spec.release != "" {
			tag, err = r.latestReleaseTag(ctx, spec.release)
			if err != nil {
				return "", nil, fmt.Errorf("%s: %w", spec.service, err)
			}
		}
		digest, err := r.manifestDigest(ctx, spec, tag)
		if err != nil {
			return "", nil, fmt.Errorf("%s: %w", spec.service, err)
		}
		ref := imageRef(spec, tag, digest)
		from := current[spec.service]
		if from == "" {
			return "", nil, fmt.Errorf("service %s has no image", spec.service)
		}
		updated, err = replaceServiceImage(updated, spec.service, ref)
		if err != nil {
			return "", nil, err
		}
		changes = append(changes, imageChange{Service: spec.service, From: from, To: ref, Changed: from != ref})
	}
	return updated, changes, nil
}

func (r resolver) latestReleaseTag(ctx context.Context, repo string) (string, error) {
	endpoint := strings.TrimRight(r.githubAPI, "/") + "/repos/" + repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ether-drift-watch")
	if req.URL.Host == "api.github.com" {
		if tok := githubToken(); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	body, _, err := r.do(req)
	if err != nil {
		return "", err
	}
	var rel struct {
		Tag string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		return "", err
	}
	return stableTag(rel.Tag)
}

func stableTag(name string) (string, error) {
	name = strings.TrimPrefix(name, "v")
	lower := strings.ToLower(name)
	if strings.ContainsAny(name, "-+") || strings.Contains(lower, "beta") || strings.Contains(lower, "rc") {
		return "", fmt.Errorf("refusing pre-release tag %q", name)
	}
	if !semverTag.MatchString(name) {
		return "", fmt.Errorf("release tag %q is not a numeric version", name)
	}
	return name, nil
}

func (r resolver) manifestDigest(ctx context.Context, spec imageSpec, tag string) (string, error) {
	origin, authOrigin, service := r.dockerRegistry, r.dockerAuth, dockerAuthService
	if spec.ghcr {
		origin, authOrigin, service = r.ghcr, r.ghcr, ghcrHost
	}
	token, err := r.registryToken(ctx, authOrigin, service, spec.repository)
	if err != nil {
		return "", err
	}
	manifest := strings.TrimRight(origin, "/") + "/v2/" + spec.repository + "/manifests/" + url.PathEscape(tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, manifest, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
	}, ", "))
	req.Header.Set("User-Agent", "ether-drift-watch")
	_, headers, err := r.do(req)
	if err != nil {
		return "", err
	}
	digest := headers.Get("Docker-Content-Digest")
	if !digestRE.MatchString(digest) {
		return "", fmt.Errorf("registry returned digest %q", digest)
	}
	return digest, nil
}

func (r resolver) registryToken(ctx context.Context, authOrigin, service, repository string) (string, error) {
	q := url.Values{}
	q.Set("service", service)
	q.Set("scope", "repository:"+repository+":pull")
	endpoint := strings.TrimRight(authOrigin, "/") + "/token?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "ether-drift-watch")
	body, _, err := r.do(req)
	if err != nil {
		return "", err
	}
	var auth struct {
		Token  string `json:"token"`
		Access string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &auth); err != nil {
		return "", err
	}
	if auth.Token == "" {
		auth.Token = auth.Access
	}
	if auth.Token == "" {
		return "", fmt.Errorf("registry token response was empty")
	}
	return auth.Token, nil
}

func (r resolver) do(req *http.Request) ([]byte, http.Header, error) {
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("HTTP %d for %s", resp.StatusCode, req.URL.Path)
	}
	return body, resp.Header, nil
}

func imageRef(spec imageSpec, tag, digest string) string {
	ref := spec.repository + ":" + tag + "@" + digest
	if spec.ghcr {
		return ghcrHost + "/" + ref
	}
	return ref
}

func githubToken() string {
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		return tok
	}
	return os.Getenv("GH_TOKEN")
}

func serviceImages(compose string) (map[string]string, error) {
	found := map[string]string{}
	err := walkServices(compose, func(service, line string) error {
		if service != "jellyfin" && service != "emby" && service != "silo" {
			return nil
		}
		ref, ok := imageValue(line)
		if !ok || found[service] != "" {
			return nil
		}
		found[service] = ref
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, spec := range catalog {
		if found[spec.service] == "" {
			return nil, fmt.Errorf("compose is missing an image for %s", spec.service)
		}
	}
	return found, nil
}

func replaceServiceImage(compose, service, ref string) (string, error) {
	replaced := false
	var lines []string
	err := walkServices(compose, func(current, line string) error {
		if !replaced && current == service {
			if _, ok := imageValue(line); ok {
				indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
				line = indent + "image: " + ref
				replaced = true
			}
		}
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		return "", err
	}
	if !replaced {
		return "", fmt.Errorf("service %s has no image", service)
	}
	return strings.Join(lines, "\n"), nil
}

// walkServices calls fn for every line. Service image lines see their service
// name; other lines see an empty service. The compose text is rebuilt only by
// replaceServiceImage, which records fn's possibly edited line.
func walkServices(compose string, fn func(service, line string) error) error {
	inServices := false
	current := ""
	for _, line := range strings.Split(compose, "\n") {
		trim := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if trim != "" && !strings.HasPrefix(trim, "#") {
			name, key := yamlKey(trim)
			switch {
			case indent == 0 && key && name == "services":
				inServices = true
				current = ""
			case indent == 0 && key:
				inServices = false
				current = ""
			case inServices && indent == 2 && key:
				current = name
			}
		}
		service := ""
		if inServices && current != "" && strings.HasPrefix(trim, "image:") {
			service = current
		}
		if err := fn(service, line); err != nil {
			return err
		}
	}
	return nil
}

func yamlKey(trim string) (string, bool) {
	field := strings.Fields(trim)
	if len(field) == 0 || !strings.HasSuffix(field[0], ":") {
		return "", false
	}
	name := strings.TrimSuffix(field[0], ":")
	if name == "" || strings.Contains(name, ":") {
		return "", false
	}
	return name, true
}

func imageValue(line string) (string, bool) {
	trim := strings.TrimSpace(line)
	if !strings.HasPrefix(trim, "image:") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(trim, "image:")), true
}
