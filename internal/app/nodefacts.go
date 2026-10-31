package app

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/braedonsaunders/ether/internal/config"
)

// nodeFacts are the few settings a start needs before (or without) opening
// them properly: where to listen, whether setup is finished, whether this
// node shares a PostgreSQL database, and who owns it. They are read from
// config.json as plain JSON, so sealed credentials (and a missing
// credential key) do not matter. A config.json that exists but cannot be
// read fails closed: setup counts as finished, the node counts as clustered
// (no restore of a shared database from here), and the port is bound to
// loopback only.
type nodeFacts struct {
	// Readable is false when config.json exists but is not valid JSON.
	Readable      bool
	SetupComplete bool
	Server        config.Server
	// Postgres is set for a node of a cluster (or when that is unknown).
	Postgres         bool
	OwnerPlexID      int64
	ClientIdentifier string
}

// Standalone reports whether this node's database is its own SQLite file,
// which a restore may replace.
func (f nodeFacts) Standalone() bool { return f.Readable && !f.Postgres }

func readNodeFacts(dataDir string, getenv func(string) string) nodeFacts {
	defaults := config.Defaults(dataDir)
	f := nodeFacts{Readable: true, Server: defaults.Server}
	raw, err := os.ReadFile(filepath.Join(dataDir, "config.json"))
	var doc struct {
		SetupComplete bool `json:"setupComplete"`
		Server        struct {
			Port              int      `json:"port"`
			BindAddress       *string  `json:"bindAddress"`
			FriendlyName      string   `json:"friendlyName"`
			TrustedProxyCIDRs []string `json:"trustedProxyCIDRs"`
		} `json:"server"`
		Plex struct {
			OwnerPlexID      int64  `json:"ownerPlexId"`
			ClientIdentifier string `json:"clientIdentifier"`
		} `json:"plex"`
		Database struct {
			Driver string `json:"driver"`
			URL    string `json:"url"`
		} `json:"database"`
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// A new data directory: nothing to protect yet.
	case err != nil || !settingsDocument(raw) || json.Unmarshal(raw, &doc) != nil:
		f.Readable, f.SetupComplete, f.Postgres = false, true, true
		f.Server.BindAddress = "127.0.0.1"
	default:
		f.SetupComplete = doc.SetupComplete
		if doc.Server.Port > 0 {
			f.Server.Port = doc.Server.Port
		}
		if doc.Server.BindAddress != nil {
			f.Server.BindAddress = *doc.Server.BindAddress
		}
		f.Server.FriendlyName = doc.Server.FriendlyName
		f.Server.TrustedProxyCIDRs = doc.Server.TrustedProxyCIDRs
		f.OwnerPlexID, f.ClientIdentifier = doc.Plex.OwnerPlexID, doc.Plex.ClientIdentifier
		// A sealed URL is still a URL: any database URL means PostgreSQL.
		f.Postgres = strings.TrimSpace(doc.Database.URL) != "" ||
			config.Database{Driver: doc.Database.Driver}.Effective(nil).IsPostgres()
	}
	if getenv != nil && (config.Database{}).Effective(getenv).IsPostgres() {
		f.Postgres = true
	}
	return f
}

// settingsDocument reports whether raw is a settings file at all: a JSON
// object with server settings. null, {} or an unrelated document fail
// closed like unparsable JSON.
func settingsDocument(raw []byte) bool {
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil || top == nil {
		return false
	}
	server, ok := top["server"]
	return ok && len(server) > 0 && server[0] == '{'
}
