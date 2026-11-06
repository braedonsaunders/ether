package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/braedonsaunders/ether/internal/config"
)

// configSections maps URL section names to JSON field names on Config.
var configSections = map[string]string{
	"posters":      "posters",
	"plexservers":  "plexServers",
	"availability": "availability",
	"markers":      "markers", "server": "server", "plex": "plex", "usenet": "usenet", "indexers": "indexers",
	"cache": "cache", "catalog": "catalog", "resolver": "resolver", "quality": "quality",
	"transcode": "transcode", "trakt": "trakt", "simkl": "simkl", "opensubtitles": "openSubtitles",
	"openSubtitles": "openSubtitles", "llm": "llm", "updates": "updates", "spike": "spike",
	"debrid": "debrid", "indexerSites": "indexerSites", "outbound": "outbound", "clients": "clients",
	"clientIdentity": "clientIdentity",
}

func (h *Handler) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"config": effectiveRedacted(h.deps.Config.Get()),
	})
}

// saveConfig validates and persists next (secrets already unredacted),
// preserving identity fields the UI must never rotate accidentally.
func (h *Handler) saveConfig(next, prev config.Config, plexFields map[string]json.RawMessage) ([]FieldError, error) {
	errs := ValidateConfig(next)
	if h.deps.ValidateExtra != nil {
		errs = append(errs, h.deps.ValidateExtra(next)...)
	}
	if len(errs) > 0 {
		return errs, nil
	}
	err := h.deps.Config.Update(func(c *config.Config) error {
		keepMachine, keepClient := c.Server.MachineIdentifier, c.Plex.ClientIdentifier
		livePlex := c.Plex
		// An omitted field or a redacted token represents no identity edit.
		// Unchanged snapshot values also must not undo a concurrent claim.
		_, claimProvided := plexFields["claimed"]
		if !claimProvided {
			next.Plex.Claimed = livePlex.Claimed
		}
		// Older unclaimed documents contain empty identity values. Treat those
		// as stale when they omit claimed and a claim now exists; explicit
		// unclaim or a new nonempty account value remains an owner edit.
		staleEmptyIdentity := !claimProvided && livePlex.Claimed && next.Plex.Token == ""
		if raw, ok := plexFields["token"]; !ok || isRedactedJSON(raw) || staleEmptyIdentity || next.Plex.Token == prev.Plex.Token {
			next.Plex.Token = livePlex.Token
		}
		if _, ok := plexFields["ownerplexid"]; !ok || (staleEmptyIdentity && next.Plex.OwnerPlexID == 0) || next.Plex.OwnerPlexID == prev.Plex.OwnerPlexID {
			next.Plex.OwnerPlexID = livePlex.OwnerPlexID
		}
		if _, ok := plexFields["ownerusername"]; !ok || (staleEmptyIdentity && next.Plex.OwnerUsername == "") || next.Plex.OwnerUsername == prev.Plex.OwnerUsername {
			next.Plex.OwnerUsername = livePlex.OwnerUsername
		}
		// The database is node-local and chosen at startup (config file or
		// ETHER_DATABASE_URL); the admin API shows it redacted, never edits it.
		keepDatabase := c.Database
		// Setup completion gates tokenless local-network access
		// (requireAuth) and changes only through /api/setup/complete: a
		// config document loaded before setup finished must not clear it.
		keepSetup := c.SetupComplete
		*c = next
		c.Database = keepDatabase
		c.SetupComplete = keepSetup
		if keepMachine != "" {
			c.Server.MachineIdentifier = keepMachine
		}
		if keepClient != "" {
			c.Plex.ClientIdentifier = keepClient
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	h.log.Info("admin: config updated")
	return nil, nil
}

func (h *Handler) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	prev := h.deps.Config.Get()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil || len(body) > maxBodyBytes || len(body) == 0 {
		writeError(w, http.StatusBadRequest, "bad_json", "could not read request body")
		return
	}
	var next config.Config
	if err := strictDecode(body, &next); err != nil {
		// Accept the {"config": {...}} envelope the GET returns.
		var env struct {
			Config config.Config `json:"config"`
		}
		if err2 := strictDecode(body, &env); err2 != nil {
			writeError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		next = env.Config
	}
	// A full document may come from an older client that does not know the
	// claim field. Only an explicit false is an instruction to unclaim.
	if err := rejectDuplicateKeys(body, "config", "plex"); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(body, &fields)
	for key, envelope := range fields {
		if strings.EqualFold(key, "config") {
			if err := rejectDuplicateKeys(envelope, "plex"); err != nil {
				writeError(w, http.StatusBadRequest, "bad_json", err.Error())
				return
			}
			fields = nil
			_ = json.Unmarshal(envelope, &fields)
			break
		}
	}
	if _, present := fields["plexServers"]; !present {
		next.PlexServers = prev.PlexServers
	}
	var plexRaw json.RawMessage
	hasPlex := false
	for key, value := range fields {
		if strings.EqualFold(key, "plex") {
			if hasPlex {
				writeError(w, http.StatusBadRequest, "bad_json", "duplicate plex field")
				return
			}
			plexRaw, hasPlex = value, true
		}
	}
	present := false
	if hasPlex {
		var err error
		present, err = validatePlexClaimInput(plexRaw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
	}
	if !present {
		next.Plex.Claimed = prev.Plex.Claimed
	}
	next, perrs := UnredactConfig(next, prev)
	if len(perrs) > 0 {
		writeValidationError(w, perrs)
		return
	}
	next.Availability.ApplyMode()
	next.SetCachePolicy(next.Cache)
	errs, err := h.saveConfig(next, prev, identityFields(plexRaw))
	switch {
	case err != nil:
		h.log.Error("admin: save config", "err", err)
		writeError(w, http.StatusInternalServerError, "save_failed", "could not save configuration")
	case len(errs) > 0:
		writeValidationError(w, errs)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"config": effectiveRedacted(h.deps.Config.Get())})
	}
}

// strictDecode decodes one JSON document rejecting unknown fields.
func strictDecode(body []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.More() {
		return errBadJSON
	}
	return nil
}

func (h *Handler) handleGetSection(w http.ResponseWriter, r *http.Request) {
	field, ok := configSections[r.PathValue("section")]
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_section", "no such config section")
		return
	}
	raw, err := json.Marshal(effectiveRedacted(h.deps.Config.Get()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not encode config")
		return
	}
	var all map[string]json.RawMessage
	_ = json.Unmarshal(raw, &all)
	out := map[string]any{"section": field, "value": json.RawMessage(all[field])}
	if field == "clientIdentity" {
		// The owner's selectors show each preset's literal user agent; serving
		// the table keeps that display identical to what is sent.
		out["presets"] = config.IdentityPresets()
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePutSection replaces (PUT) or patches (PATCH) one section. Both
// verbs decode the body onto the current section value, so omitted fields
// keep their stored values — a full replace is achieved by sending every
// field, a patch by sending only the changed ones.
func (h *Handler) handlePutSection(w http.ResponseWriter, r *http.Request) {
	field, ok := configSections[r.PathValue("section")]
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_section", "no such config section")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil || len(body) > maxBodyBytes {
		writeError(w, http.StatusBadRequest, "bad_json", "could not read request body")
		return
	}
	// Reject ambiguous envelope keys before decoding into a map.
	if err := rejectDuplicateKeys(body, "value"); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	// Accept {"value": {...}} envelope too.
	var probe map[string]json.RawMessage
	if json.Unmarshal(body, &probe) == nil {
		if v, has := probe["value"]; has && len(probe) == 1 {
			body = v
		}
	}
	if field == "plex" {
		if _, err := validatePlexClaimInput(body); err != nil {
			writeError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
	}
	prev := h.deps.Config.Get()
	// Merge onto the redacted config: a stored secret the patch leaves out
	// then comes back as a placeholder and is restored only if still bound
	// to its destination (UnredactConfig), so a patch that repoints an
	// endpoint cannot carry the old secret along by omitting it.
	prevRaw, _ := json.Marshal(effectiveRedacted(prev))
	var all map[string]json.RawMessage
	if err := json.Unmarshal(prevRaw, &all); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not encode config")
		return
	}
	merged, err := mergeSection(all[field], body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	all[field] = merged
	mergedAll, _ := json.Marshal(all)
	var next config.Config
	if err := json.Unmarshal(mergedAll, &next); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "section value does not match the config schema: "+err.Error())
		return
	}
	next, perrs := UnredactConfig(next, prev)
	if len(perrs) > 0 {
		writeValidationError(w, perrs)
		return
	}
	if field == "availability" {
		next.Availability.ApplyMode()
	}
	if field == "cache" {
		next.SetCachePolicy(next.Cache)
	} else {
		next.Cache = prev.Cache
	}
	var identity map[string]json.RawMessage
	if field == "plex" {
		identity = identityFields(body)
	}
	errs, err := h.saveConfig(next, prev, identity)
	switch {
	case err != nil:
		h.log.Error("admin: save config section", "section", field, "err", err)
		writeError(w, http.StatusInternalServerError, "save_failed", "could not save configuration")
	case len(errs) > 0:
		writeValidationError(w, errs)
	default:
		raw, _ := json.Marshal(effectiveRedacted(h.deps.Config.Get()))
		var out map[string]json.RawMessage
		_ = json.Unmarshal(raw, &out)
		writeJSON(w, http.StatusOK, map[string]any{"section": field, "value": json.RawMessage(out[field])})
	}
}

// Reject null and duplicate spellings: JSON null decoded into a bool would
// otherwise silently become false and release the device.
func validatePlexClaimInput(raw json.RawMessage) (bool, error) {
	if err := rejectDuplicateKeys(raw, "claimed"); err != nil {
		return false, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return false, errors.New("plex configuration must be an object")
	}
	seen := false
	for key, value := range fields {
		if !strings.EqualFold(key, "claimed") {
			continue
		}
		if key != "claimed" {
			return false, errors.New("plex claimed field must use canonical spelling")
		}
		if seen {
			return false, errors.New("duplicate plex claimed field")
		}
		seen = true
		if string(bytes.TrimSpace(value)) != "true" && string(bytes.TrimSpace(value)) != "false" {
			return false, errors.New("plex claimed must be a boolean")
		}
	}
	return seen, nil
}

// rejectDuplicateKeys inspects object members before map decoding loses duplicates.
func rejectDuplicateKeys(raw []byte, keys ...string) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil
	} // schema validation handles non-objects
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := t.(string)
		if !ok {
			return errBadJSON
		}
		for _, target := range keys {
			if strings.EqualFold(key, target) {
				if seen[target] {
					return errors.New("duplicate " + target + " field")
				}
				seen[target] = true
			}
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return err
		}
	}
	_, err = dec.Token()
	return err
}

func identityFields(raw []byte) map[string]json.RawMessage {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	out := make(map[string]json.RawMessage, len(fields))
	for k, v := range fields {
		out[strings.ToLower(k)] = v
	}
	return out
}

func isRedactedJSON(raw json.RawMessage) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil && isRedacted(value)
}

// mergeSection overlays patch onto current: for objects the patch keys win
// with unspecified keys preserved; arrays/scalars are replaced wholesale.
func mergeSection(current, patch json.RawMessage) (json.RawMessage, error) {
	var curObj, patchObj map[string]json.RawMessage
	if json.Unmarshal(patch, &patchObj) != nil || json.Unmarshal(current, &curObj) != nil {
		// Not both objects: replace (validates on decode into Config).
		var probe any
		if err := json.Unmarshal(patch, &probe); err != nil {
			return nil, err
		}
		return patch, nil
	}
	for k, v := range patchObj {
		curObj[k] = v
	}
	return json.Marshal(curObj)
}

func effectiveRedacted(c config.Config) config.Config {
	c.Cache = c.EffectiveCache()
	return RedactConfig(c)
}
