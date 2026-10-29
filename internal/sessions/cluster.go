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
const brokerReportTopic = "sessions:broker-report"
const brokerRetireTopic = "sessions:broker-retire"

type brokerCluster struct {
	store             clusterStore
	node              clusterbus.Node
	broker            *Broker
	stop              chan struct{}
	done              chan struct{}
	unsubscribe       func()
	unsubscribeReport func()
	unsubscribeRetire func()
}

type remoteStop struct {
	Node, ID, DeviceID string
	SourceID           string
	ActorID            int64
	Protocol           frontend.Protocol
	Owner              bool
}

type remoteReport struct {
	Node, ID, DeviceID string
	ActorID            int64
	Protocol           frontend.Protocol
	Owner              bool
	Timeline           frontend.Timeline
}
type remoteRetired struct {
	ID       string
	ViewerID int64
	Protocol frontend.Protocol
	DeviceID string
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
	c.unsubscribeReport = node.Subscribe(brokerReportTopic, c.receiveReport)
	c.unsubscribeRetire = node.Subscribe(brokerRetireTopic, c.receiveRetired)
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
	c.unsubscribeReport()
	c.unsubscribeRetire()
	close(c.stop)
	<-c.done
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.store.cleanup(ctx); err != nil && c.broker.deps.OnEventError != nil {
		c.broker.deps.OnEventError(err)
	}
}

func (c *brokerCluster) announceRetired(ctx context.Context, s *brokerSession) {
	data, err := json.Marshal(remoteRetired{ID: s.public.ID, ViewerID: s.public.ViewerID, Protocol: s.public.Protocol, DeviceID: s.public.Device.ID})
	if err == nil {
		err = c.node.Publish(ctx, brokerRetireTopic, data)
	}
	if err != nil && c.broker.deps.OnEventError != nil {
		c.broker.deps.OnEventError(err)
	}
}
func (c *brokerCluster) receiveRetired(data []byte) {
	var r remoteRetired
	if json.Unmarshal(data, &r) != nil || r.ID == "" || r.ViewerID <= 0 || r.Protocol == "" || r.DeviceID == "" {
		return
	}
	c.broker.mu.Lock()
	defer c.broker.mu.Unlock()
	if len(c.broker.retired) >= 4096 {
		for key := range c.broker.retired {
			delete(c.broker.retired, key)
			break
		}
	}
	c.broker.retired[r.ID] = retiredSession{viewerID: r.ViewerID, protocol: r.Protocol, deviceID: r.DeviceID, until: c.broker.deps.Now().Add(2 * time.Hour)}
}

func (c *brokerCluster) forwardReport(ctx context.Context, a frontend.SessionAccess, id string, timeline frontend.Timeline) error {
	v, err := c.broker.viewer(ctx, a.Viewer)
	if err != nil {
		return err
	}
	owner := a.Owner && v.IsOwner && !v.Delegated
	if a.Owner && !owner {
		return core.ErrUnauthorized
	}
	remote, err := c.remote(ctx)
	if err != nil {
		return err
	}
	for _, r := range remote {
		if r.session.ID != id {
			continue
		}
		if !owner && (r.session.ViewerID != v.ID || r.session.Protocol != a.Protocol || a.Device.ID == "" || r.session.Device.ID != a.Device.ID) {
			return core.ErrUnauthorized
		}
		payload, err := json.Marshal(remoteReport{Node: r.node, ID: id, ActorID: v.ID, DeviceID: a.Device.ID, Protocol: a.Protocol, Owner: owner, Timeline: timeline})
		if err != nil {
			return err
		}
		return c.node.Publish(ctx, brokerReportTopic, payload)
	}
	// A just-stopped or superseded session may already be absent from the
	// shared table before its retirement event reaches this node. Treat late
	// reports as idempotent no-ops, as Stop already does.
	return nil
}

func (c *brokerCluster) receiveReport(payload []byte) {
	var msg remoteReport
	if json.Unmarshal(payload, &msg) != nil || msg.Node != c.node.NodeID() || msg.ActorID <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	actor, err := c.broker.deps.ActiveViewer(ctx, msg.ActorID)
	if err != nil || actor == nil {
		return
	}
	err = c.broker.Report(ctx, frontend.SessionAccess{Viewer: actor, Protocol: msg.Protocol, Device: core.Device{ID: msg.DeviceID}, Owner: msg.Owner}, msg.ID, msg.Timeline)
	if err != nil && c.broker.deps.OnEventError != nil {
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
