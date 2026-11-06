package plex

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("plex_downloads", 1, `CREATE TABLE plex_downloads (server_id TEXT PRIMARY KEY, snapshot TEXT NOT NULL);`)
}

// The single writer coalesces queue mutations; GET handlers never perform SQL
// writes. Only queue state is visited, independently of the catalogue size.
type savedDownload struct {
	ID                                int64
	Key                               core.RatingKey
	Title                             string
	Caps                              core.ClientCaps
	Status, Error, Path, Video, Audio string
	Size                              int64
	Ready                             time.Time
	Duration                          time.Duration
	Bandwidth                         int
	Burn                              bool
}
type savedDownloadQueue struct {
	ID, UserID int64
	ClientID   string
	Items      []savedDownload
}
type savedDownloads struct {
	NextQueue, NextItem, NextRun int64
	Queues                       []savedDownloadQueue
}

type retainedDownloads interface{ RetainDownload(string, time.Time) error }

func (s *Server) retainDownload(path string, ready time.Time) {
	if s.d.DB == nil {
		return
	}
	if d, ok := s.d.Downloads.(retainedDownloads); ok {
		expiry := s.cfg().Downloads.Expiry.D()
		if expiry <= 0 {
			expiry = 24 * time.Hour
		}
		if err := d.RetainDownload(path, ready.Add(expiry)); err != nil {
			s.log.Warn("retain download", "err", err)
		}
	}
}
func (s *Server) stageDownloads() {
	if s.d.DB == nil {
		return
	}
	select {
	case s.downloads.wake <- struct{}{}:
	default:
	}
}
func (s *Server) downloadStorageID() string {
	id := s.cfg().Server.MachineIdentifier
	if s.d.Cluster != nil {
		id += "/" + s.d.Cluster.NodeID()
	}
	return id
}
func (s *Server) saveDownloads() error {
	if s.d.DB == nil {
		return nil
	}
	st := s.downloads
	st.mu.Lock()
	snap := savedDownloads{NextQueue: st.nextQueue, NextItem: st.nextItem, NextRun: st.nextRun}
	for _, q := range st.queues {
		sq := savedDownloadQueue{ID: q.id, UserID: q.userID, ClientID: q.clientID}
		for _, it := range q.items {
			sq.Items = append(sq.Items, savedDownload{ID: it.id, Key: it.key, Title: it.title, Caps: it.caps, Status: it.status, Error: it.errText, Path: it.path, Video: it.videoCodec, Audio: it.audioCodec, Size: it.size, Ready: it.readyAt, Duration: it.duration, Bandwidth: it.bandwidth, Burn: it.burn})
		}
		snap.Queues = append(snap.Queues, sq)
	}
	st.mu.Unlock()
	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = s.d.DB.ExecContext(ctx, `INSERT INTO plex_downloads(server_id,snapshot) VALUES(?,?) ON CONFLICT(server_id) DO UPDATE SET snapshot=excluded.snapshot`, s.downloadStorageID(), string(data))
	return err
}
func (s *Server) loadDownloads() {
	if s.d.DB == nil || s.d.Auth == nil {
		return
	}
	var data string
	if err := s.d.DB.QueryRowContext(s.ctx, `SELECT snapshot FROM plex_downloads WHERE server_id=?`, s.downloadStorageID()).Scan(&data); err != nil {
		return
	}
	var snap savedDownloads
	if err := json.Unmarshal([]byte(data), &snap); err != nil {
		s.log.Warn("load downloads", "err", err)
		return
	}
	users, err := s.d.Auth.Users(s.ctx)
	if err != nil {
		return
	}
	byID := map[int64]*core.User{}
	for i := range users {
		byID[users[i].ID] = &users[i]
	}
	st := s.downloads
	st.nextQueue, st.nextItem, st.nextRun = snap.NextQueue, snap.NextItem, snap.NextRun
	for _, sq := range snap.Queues {
		u := byID[sq.UserID]
		if u == nil {
			continue
		}
		q := &downloadQueue{id: sq.ID, userID: sq.UserID, clientID: sq.ClientID}
		for _, si := range sq.Items {
			it := &downloadItem{id: si.ID, queueID: q.id, key: si.Key, title: si.Title, caps: si.Caps, user: u, status: si.Status, errText: si.Error, path: si.Path, size: si.Size, readyAt: si.Ready, videoCodec: si.Video, audioCodec: si.Audio, duration: si.Duration, bandwidth: si.Bandwidth, burn: si.Burn}
			switch it.status {
			case downloadDeciding, downloadWaiting, downloadProcessing:
				it.status = downloadDeciding
			}
			if it.status == downloadAvailable {
				if _, err := os.Stat(it.path); err != nil {
					it.status = downloadExpired
					it.path = ""
					it.size = 0
				}
				if it.path != "" {
					s.retainDownload(it.path, it.readyAt)
					it.progress = 1
				}
			}
			q.items = append(q.items, it)
		}
		st.queues[q.id] = q
		st.byClient[strconv.FormatInt(q.userID, 10)+"\x00"+q.clientID] = q.id
	}
}
func (s *Server) writeDownloads() {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.downloads.wake:
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if err := s.saveDownloads(); err != nil {
			s.log.Warn("persist downloads", "err", err)
		}
	}
}

// A restart keeps completed files; pending jobs restart from their saved caps
// only after the current account is authorized again.
func (s *Server) stopDownloadsForRestart() {
	s.downloads.mu.Lock()
	for _, q := range s.downloads.queues {
		for _, it := range q.items {
			if it.cancel != nil {
				it.cancel()
			}
		}
	}
	s.downloads.mu.Unlock()
}
