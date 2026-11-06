// Package hostfenceprobe measures prospective Linux observer access. It does not
// provide retirement evidence, admission authority, or a signing API.
package hostfenceprobe

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path"
	"strings"
)

const MaxBinding = 4096

// Binding is supplied by the trusted lab launcher from its filtered runtime
// inspection. This reader validates shape, not the authenticity of that input.
// PID is the exact outer namespace init, not a PID from inside the kind node.
type Binding struct {
	Version        int    `json:"version"`
	HostBoot       string `json:"hostBoot"`
	DaemonID       string `json:"daemonID"`
	ContainerID    string `json:"containerID"`
	ContainerStart string `json:"containerStart"`
	PID            int    `json:"pid"`
	Cgroup         string `json:"cgroup"`
}

func ReadBinding(r io.Reader) (Binding, error) {
	var b Binding
	raw, err := io.ReadAll(io.LimitReader(r, MaxBinding+1))
	if err != nil || len(raw) > MaxBinding {
		return b, errors.New("binding: read or size")
	}
	// Reject duplicate keys as well as unknown fields: a decoder's last-key-wins
	// behavior must not let launcher and observer interpret different bindings.
	d := json.NewDecoder(strings.NewReader(string(raw)))
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return b, errors.New("binding: object required")
	}
	seen := map[string]bool{}
	for d.More() {
		t, err = d.Token()
		key, ok := t.(string)
		if err != nil || !ok || seen[key] {
			return b, errors.New("binding: duplicate or invalid key")
		}
		seen[key] = true
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return b, errors.New("binding: value")
		}
	}
	d = json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if d.Decode(&b) != nil || d.Decode(new(any)) != io.EOF || b.Validate() != nil {
		return Binding{}, errors.New("binding: invalid")
	}
	return b, nil
}

func (b Binding) Validate() error {
	id, err := hex.DecodeString(b.ContainerID)
	if b.Version != 1 || b.PID <= 1 || err != nil || len(id) != 32 || strings.ToLower(b.ContainerID) != b.ContainerID || !bounded(b.HostBoot) || !bounded(b.DaemonID) || !bounded(b.ContainerStart) || !strings.HasPrefix(b.Cgroup, "/") || b.Cgroup == "/" || path.Clean(b.Cgroup) != b.Cgroup || !bounded(b.Cgroup) {
		return errors.New("binding: invalid")
	}
	return nil
}

func bounded(s string) bool {
	return len(s) > 0 && len(s) <= 512 && !strings.ContainsAny(s, "\x00\r\n")
}
