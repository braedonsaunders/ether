package livetv

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Add-on channel sources (Stremio `tv` add-ons, installed as plugins) own
// their channels' inputs, which carry AddonInputPrefix in their ids:
//
//   - a refresh replaces them, keeping the owner's enabled flags and any
//     input the owner added to the channel;
//   - channels new to the source are matched to guides after the import;
//   - their URLs and request headers can repeat the add-on's configuration
//     (often a debrid key), so the owner API never returns them.
const AddonInputPrefix = "stremio-"

// legacyStremioKind is the built-in Stremio source kind from before
// add-ons were plugins. Its rows are read, and kept redacted, until the
// start-up migration moves each one onto its add-on (AdoptAddonSource).
const legacyStremioKind = "stremio"

var errLegacyStremio = errors.New("this Stremio add-on source has not moved to Plugins yet; Ether moves it the next time it starts")

// AddonInputID is the id of a channel's n-th add-on input (from 0).
func AddonInputID(channelID string, n int) string {
	return AddonInputPrefix + stableID(channelID, strconv.Itoa(n))
}

func isAddonInput(id string) bool { return strings.HasPrefix(id, AddonInputPrefix) }

// addonSource reports a source whose inputs may be add-on inputs: a legacy
// Stremio source or a plugin channel source. Native plugin channel sources
// never mint AddonInputPrefix ids, so their inputs stay visible.
func addonSource(src Source) bool {
	return src.Kind == legacyStremioKind || strings.HasPrefix(src.Kind, "plugin:")
}

// mergeAddonStreams refreshes add-on inputs while preserving their owner
// enablement and separately added inputs.
func mergeAddonStreams(fresh, previous []StreamSource) []StreamSource {
	old := make(map[string]StreamSource, len(previous))
	for _, input := range previous {
		old[input.ID] = input
	}
	out := make([]StreamSource, 0, len(fresh)+len(previous))
	for _, input := range fresh {
		if prior, ok := old[input.ID]; ok {
			input.Enabled = prior.Enabled
		}
		out = append(out, input)
	}
	for _, input := range previous {
		if !isAddonInput(input.ID) {
			out = append(out, input)
		}
	}
	return out
}

// redactAddonChannels hides add-on input URLs and header values. The owner
// can still reorder and switch the inputs off.
func (s *Service) redactAddonChannels(_ context.Context, channels []Channel) {
	for i := range channels {
		c := &channels[i]
		// Channel snapshots share their nested inputs with request copies.
		// Redaction owns its slice so an owner read cannot overwrite playback
		// URLs/headers or turn a later health sweep into failed probes.
		c.StreamSources = slices.Clone(c.StreamSources)
		for j := range c.StreamSources {
			input := &c.StreamSources[j]
			if !isAddonInput(input.ID) {
				continue
			}
			if c.StreamURL == input.URL {
				c.StreamURL = redactedSecret
			}
			input.URL = redactedSecret
			if len(input.Headers) > 0 {
				h := make(map[string]string, len(input.Headers))
				for k := range input.Headers {
					h[k] = redactedSecret
				}
				input.Headers = h
			}
		}
	}
}

// restoreAddonChannel puts the stored URLs and headers back into add-on
// inputs the owner API sent back redacted. Only the same channel's stored
// input with the same id is used.
func (s *Service) restoreAddonChannel(ctx context.Context, c *Channel) error {
	needs := c.StreamURL == redactedSecret
	for _, v := range c.StreamSources {
		needs = needs || v.URL == redactedSecret || hasRedactedHeader(v.Headers)
	}
	if !needs {
		return nil
	}
	old, err := s.Channel(ctx, c.ID)
	if err != nil || old.SourceID != c.SourceID {
		return fmt.Errorf("re-enter the stream URL")
	}
	src, err := s.storedSourceByID(ctx, old.SourceID)
	if err != nil || !addonSource(src) {
		return fmt.Errorf("re-enter the stream URL")
	}
	for i := range c.StreamSources {
		input := &c.StreamSources[i]
		if input.URL != redactedSecret && !hasRedactedHeader(input.Headers) {
			continue
		}
		found := false
		for _, v := range old.StreamSources {
			if v.ID == input.ID && isAddonInput(v.ID) {
				if input.URL == redactedSecret {
					input.URL = v.URL
				}
				input.Headers = v.Headers
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("re-enter the stream URL")
		}
	}
	if c.StreamURL == redactedSecret {
		c.StreamURL = old.StreamURL
	}
	return nil
}

func hasRedactedHeader(h map[string]string) bool {
	for _, v := range h {
		if v == redactedSecret {
			return true
		}
	}
	return false
}

// GuideWindow is the span of listings an import keeps: a day back and two
// weeks ahead of now.
func (s *Service) GuideWindow() (from, until time.Time) {
	now := s.o.Now().UTC()
	return now.Add(-guideLookback), now.Add(guideLookahead)
}

// ParseSourceGuide reads an XMLTV guide (plain or gzip) for a source kind's
// Fetch: listings outside the guide window are dropped, as are channels
// that are neither among channels nor mapped to src by the owner.
func (s *Service) ParseSourceGuide(ctx context.Context, src Source, channels []Channel, r io.Reader) (XMLGuide, error) {
	allow, err := s.guideAllow(ctx, src, channels, strings.TrimSpace(src.Playlist) != "" || src.PlaylistURL != "")
	if err != nil {
		return XMLGuide{}, err
	}
	rd, err := maybeGunzip(r, maxGuideBytes, "guide")
	if err != nil {
		return XMLGuide{}, err
	}
	defer closeReader(rd)
	from, until := s.GuideWindow()
	g, _, err := parseXMLTV(rd, guideFilter{From: from, Until: until, IDs: allow})
	return g, err
}

// SourceChannels returns the stored channels of one source, inputs
// unredacted (for a source kind's Fetch).
func (s *Service) SourceChannels(ctx context.Context, sourceID string) ([]Channel, error) {
	all, err := s.allChannels(ctx)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, c := range all {
		if c.SourceID == sourceID {
			out = append(out, c)
		}
	}
	return out, nil
}

// LegacyStremioSources returns the sources still on the built-in Stremio
// kind, with their add-on links opened, for the start-up migration.
func (s *Service) LegacyStremioSources(ctx context.Context) ([]Source, error) {
	rows, err := s.o.DB.QueryContext(ctx, `SELECT data FROM livetv_sources ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		src, _, err := s.decodeSource(b)
		if err != nil {
			return nil, err
		}
		if src.Kind == legacyStremioKind {
			out = append(out, src)
		}
	}
	return out, rows.Err()
}

// AdoptAddonSource moves a legacy Stremio source onto an add-on's channel
// source kind, in place. The source keeps its id, name and refresh setting;
// the add-on link is dropped (the plugin record holds it now). Channels
// are untouched: the add-on channel source mints the same channel and input
// ids, so numbers, enabled flags, favourites, ratings, guide matches and
// transcode settings carry over. A source that already moved is left alone.
func (s *Service) AdoptAddonSource(ctx context.Context, id, kind string) error {
	if !strings.HasPrefix(kind, "plugin:") {
		return fmt.Errorf("livetv: %q is not a plugin channel source kind", kind)
	}
	tx, err := s.o.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var b []byte
	err = tx.QueryRowContext(ctx, `SELECT data FROM livetv_sources WHERE id=?`, id).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return core.ErrNotFound
	}
	if err != nil {
		return err
	}
	src, _, err := s.decodeSource(b)
	if err != nil {
		return err
	}
	if src.Kind != legacyStremioKind {
		return nil
	}
	src.Kind, src.PlaylistURL, src.Playlist, src.Auth = kind, kind, "", nil
	sb, err := s.encodeSource(src)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE livetv_sources SET data=? WHERE id=?`, sb, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.forgetSourceAuth(id)
	return nil
}
