package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

const brokerStopTopic = "sessions:broker-stop"

type brokerCluster struct {
	store       clusterStore
	node        clusterbus.Node
	broker      *Broker
	stop        chan struct{}
	done        chan struct{}
	unsubscribe func()
}

type remoteStop struct {
	Node, ID, DeviceID string
	SourceID           string
	ActorID            int64
	Protocol           frontend.Protocol
	Owner              bool
}

func startBrokerCluster(b *Broker, db *sql.DB, node clusterbus.Node) (*brokerCluster, error) {
	c := &brokerCluster{store: clusterStore{db: db, node: node.NodeID()}, node: node, broker: b, stop: make(chan struct{}), done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := c.store.cleanup(ctx)
	cancel()
	if err != nil {
		return nil, err
	}
	c.unsubscribe = node.Subscribe(brokerStopTopic, c.receiveStop)
	go c.heartbeat()
	return c, nil
}

func (c *brokerCluster) heartbeat() {
	defer close(c.done)
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			c.broker.mu.Lock()
			ids := make([]string, 0, len(c.broker.sessions))
			for id := range c.broker.sessions {
				ids = append(ids, id)
			}
			c.broker.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			for _, id := range ids {
				if err := c.store.touch(ctx, id, c.broker.deps.Now()); err != nil && c.broker.deps.OnEventError != nil {
					c.broker.deps.OnEventError(err)
				}
			}
			if err := c.store.expire(ctx, c.broker.deps.Now().Add(-10*time.Minute)); err != nil && c.broker.deps.OnEventError != nil {
				c.broker.deps.OnEventError(err)
			}
			cancel()
		}
	}
}

func (c *brokerCluster) close() {
	c.unsubscribe()
	close(c.stop)
	<-c.done
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.store.cleanup(ctx); err != nil && c.broker.deps.OnEventError != nil {
		c.broker.deps.OnEventError(err)
	}
}

func (c *brokerCluster) remote(ctx context.Context) ([]remoteSession, error) {
	live, err := c.node.LiveNodes(ctx)
	if err != nil {
		return nil, err
	}
	return c.store.list(ctx, c.broker.deps.Now().Add(-90*time.Second), live)
}

func (c *brokerCluster) receiveStop(payload []byte) {
	var msg remoteStop
	if json.Unmarshal(payload, &msg) != nil || msg.Node != c.node.NodeID() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// This internal bus command is sent only by the owner Activity callback.
	if msg.SourceID != "" {
		c.broker.StopPlayback(ctx, msg.SourceID)
		return
	}
	if msg.ActorID <= 0 {
		return
	}
	actor, err := c.broker.deps.ActiveViewer(ctx, msg.ActorID)
	if err != nil || actor == nil {
		return
	}
	if err := c.broker.Stop(ctx, frontend.SessionAccess{Viewer: actor, Protocol: msg.Protocol, Device: core.Device{ID: msg.DeviceID}, Owner: msg.Owner}, msg.ID); err != nil && c.broker.deps.OnEventError != nil {
		c.broker.deps.OnEventError(err)
	}
}
