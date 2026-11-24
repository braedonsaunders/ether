package frontend

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"

	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/datafs"
	"github.com/braedonsaunders/ether/internal/store"
)

// Host configuration snapshots and quality rules exchanged across the
// frontend boundary.
type HostConfig = config.Config
type HostQualityConfig = config.Quality

// ServerIdentity describes how this server presents its machine and build
// identity. Empty fields fall back to HostConfig or runtime defaults.
type ServerIdentity struct {
	MachineID       string
	FriendlyName    string
	Version         string
	Platform        string
	PlatformVersion string
	// Account describes the plex.tv account of an extra Plex server when
	// one request is for it (ServerIdentityFor); nil for the main server.
	Account *ServerAccount
}

// ServerAccount is the owning plex.tv account of one Plex server identity.
type ServerAccount struct {
	OwnerUsername string
	Claimed       bool
	PlexPass      bool
	OwnerFeatures []string
}

// Cluster coordination primitives used by protocol adapters when clustered.
type ClusterBus = clusterbus.Bus
type ClusterNode = clusterbus.Node

const (
	ClusterTopicResync    = clusterbus.TopicResync
	ClusterMaxPayload     = clusterbus.MaxPayload
	ClusterPublishTimeout = clusterbus.PublishTimeout
)

func NewClusterOrigin() string                         { return clusterbus.NewOrigin() }
func TagClusterMessage(origin string, b []byte) []byte { return clusterbus.Tag(origin, b) }
func UntagClusterMessage(msg []byte) (string, []byte)  { return clusterbus.Untag(msg) }
func StandaloneClusterNode(id string) ClusterNode      { return clusterbus.NewHub().Node(id) }

// Store helpers exposed across the frontend boundary.
func RegisterMigration(component string, version int, sql string) {
	store.Register(component, version, sql)
}

func DetachQueryStats(ctx context.Context) context.Context {
	return store.DetachQueryStats(ctx)
}

func SQLPlaceholders(n int) string {
	return store.Placeholders(n)
}

// DiskUsage reports the total and free bytes on the volume holding path. If a
// subdirectory has not been created yet, it inspects the nearest existing
// ancestor on the same path.
func DiskUsage(path string) (total, free int64, err error) {
	if path == "" {
		return 0, 0, fs.ErrNotExist
	}
	cur := filepath.Clean(path)
	for {
		total, free, err = datafs.Usage(cur)
		if !errors.Is(err, fs.ErrNotExist) {
			return total, free, err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return 0, 0, err
		}
		cur = parent
	}
}
