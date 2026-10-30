package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/braedonsaunders/ether/internal/cluster"
	"github.com/braedonsaunders/ether/internal/config"
)

// configureClusterNode creates a node-local copy of one seeded world. The
// database service must be the harness-owned Docker-internal fixture; no
// production or external database is accepted.
func configureClusterNode(seedDir, dest, databaseURL, nodeID string, port int) error {
	u, err := url.Parse(databaseURL)
	if err != nil || u.Scheme != "postgres" || u.Hostname() != "parity-postgres" || u.Path == "" {
		return fmt.Errorf("cluster database must be the offline parity-postgres fixture")
	}
	if len(nodeID) != 32 || strings.Trim(nodeID, "0123456789abcdef") != "" || port < 1 || port > 65535 {
		return fmt.Errorf("invalid fixture node identity or port")
	}
	if err := copyFixture(seedDir, dest); err != nil {
		return err
	}
	identityPath := filepath.Join(dest, cluster.NodeFile)
	if _, err := os.Stat(identityPath); err == nil || !os.IsNotExist(err) {
		return fmt.Errorf("fixture node identity already exists or cannot be checked: %v", err)
	}
	identity, err := json.Marshal(cluster.Identity{NodeID: nodeID, Name: "Parity node " + nodeID[:2]})
	if err != nil {
		return err
	}
	if err := os.WriteFile(identityPath, identity, 0600); err != nil {
		return err
	}
	c, err := config.Open(dest)
	if err != nil {
		return err
	}
	return c.Update(func(c *config.Config) error {
		c.Database = config.Database{Driver: config.DatabasePostgres, URL: databaseURL}
		c.Server.BindAddress = "127.0.0.1"
		c.Server.Port = port
		c.Cache.Dir = filepath.Join(dest, "cache")
		c.Transcode.TempDir = filepath.Join(dest, "transcode")
		return nil
	})
}
