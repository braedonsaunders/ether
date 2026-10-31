package transcode

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

func validatedInputHeaders(headers map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(headers))
	for name, value := range headers {
		if name == "" || strings.ContainsAny(value, "\r\n\x00") {
			return nil, fmt.Errorf("invalid media input header")
		}
		for _, ch := range name {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", ch)) {
				return nil, fmt.Errorf("invalid media input header name")
			}
		}
		key := http.CanonicalHeaderKey(name)
		if key == core.EncoderInputScopeHeader {
			return nil, fmt.Errorf("reserved media input header")
		}
		if _, duplicate := out[key]; duplicate {
			return nil, fmt.Errorf("duplicate media input header")
		}
		out[key] = value
	}
	return out, nil
}

func inputHeaderArgs(headers map[string]string) []string {
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var args []string
	var block strings.Builder
	for _, key := range keys {
		switch strings.ToLower(key) {
		case "user-agent":
			args = append(args, "-user_agent", headers[key])
		case "referer":
			args = append(args, "-referer", headers[key])
		default:
			fmt.Fprintf(&block, "%s: %s\r\n", key, headers[key])
		}
	}
	if block.Len() != 0 {
		args = append(args, "-headers", block.String())
	}
	return args
}

func appendInputHeaders(args []string, headers map[string]string) []string {
	if headers["User-Agent"] != "" {
		filtered := make([]string, 0, len(args))
		for i := 0; i < len(args); i++ {
			if args[i] == "-user_agent" && i+1 < len(args) {
				i++
				continue
			}
			filtered = append(filtered, args[i])
		}
		args = filtered
	}
	return append(args, inputHeaderArgs(headers)...)
}

func redactedInputArgs(args []string) []string {
	out := append([]string(nil), args...)
	for i := 0; i+1 < len(out); i++ {
		switch out[i] {
		case "-headers", "-user_agent", "-referer", "-i":
			i++
			out[i] = "[redacted]"
		}
	}
	return out
}

type probeHeaderRunner struct {
	Runner
	headers map[string]string
}

func (r probeHeaderRunner) Run(ctx context.Context, c Command) error {
	// Probe commands end in -i URL. Insert protocol options before that input.
	args := append([]string(nil), c.Args[:len(c.Args)-2]...)
	args = appendInputHeaders(args, r.headers)
	c.Args = append(args, c.Args[len(c.Args)-2:]...)
	return r.Runner.Run(ctx, c)
}

// ProbeLiveWithHeaders is the authenticated variant of the optional live probe.
func (m *Manager) ProbeLiveWithHeaders(ctx context.Context, input string, headers map[string]string) (*core.MediaInfo, error) {
	normalized, err := validatedInputHeaders(headers)
	if err != nil {
		return nil, err
	}
	secrets := make([]string, 0, len(normalized))
	for _, v := range normalized {
		secrets = append(secrets, v)
	}
	r := redactingRunner{Runner: probeHeaderRunner{Runner: m.runner, headers: normalized}, input: input, secrets: secrets}
	info, err := probeMode(ctx, r, m.ffprobe, input, true)
	if err != nil && ctx.Err() == nil {
		message := strings.ReplaceAll(err.Error(), input, "[source]")
		for _, value := range normalized {
			if value != "" {
				message = strings.ReplaceAll(message, value, "[redacted]")
			}
		}
		err = fmt.Errorf("live probe: %s", message)
	}
	return info, err
}
