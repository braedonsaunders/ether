package admin

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/config"
)

// sameProwlarrURL reports whether two Prowlarr base URLs name the same
// saved connection. A trailing slash does not make a new destination, so
// the stored API key can be restored. Any other change requires the key again.
func sameProwlarrURL(a, b string) bool {
	return strings.TrimRight(strings.TrimSpace(a), "/") == strings.TrimRight(strings.TrimSpace(b), "/")
}

func redact(s string) string {
	if s == "" {
		return ""
	}
	return RedactedSecret
}

func isRedacted(s string) bool { return s == RedactedSecret }

// A Stremio add-on install URL is a secret (its path carries the owner's
// add-on settings, often a debrid key), so the browser only ever sees the
// scheme and host plus a saved marker. The full URL round-trips through
// UnredactConfig when the marker comes back for the same entry and host.
const redactedSavedSuffix = " (saved)"

func redactStremioURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	trimmed := raw
	if rest, ok := strings.CutPrefix(trimmed, "stremio://"); ok {
		trimmed = "https://" + rest
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Host == "" {
		return RedactedSecret
	}
	return u.Scheme + "://" + u.Host + redactedSavedSuffix
}

func isRedactedStremioURL(s string) bool {
	return isRedacted(s) || strings.HasSuffix(strings.TrimSpace(s), redactedSavedSuffix)
}

// stremioURLHost is the host of a full or redacted add-on URL.
func stremioURLHost(raw string) string {
	raw = strings.TrimSuffix(strings.TrimSpace(raw), redactedSavedSuffix)
	if rest, ok := strings.CutPrefix(raw, "stremio://"); ok {
		raw = "https://" + rest
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

// RedactConfig returns a copy of cfg with every secret replaced by
// RedactedSecret ("••••"). Empty secrets stay empty so the UI can tell
// "unset" from "set". The input is not modified.
func RedactConfig(cfg config.Config) config.Config {
	out := cfg
	for _, a := range out.Posters.Accounts.Entries() {
		a.APIKey = redact(a.APIKey)
		a.URLTemplate = redact(a.URLTemplate)
	}
	out.Posters.APIKey = redact(out.Posters.APIKey)
	out.Posters.URLTemplate = redact(out.Posters.URLTemplate)
	out.Markers.APIKey = redact(out.Markers.APIKey)
	out.Plex.Token = redact(out.Plex.Token)
	if len(cfg.PlexExtraServers) > 0 {
		out.PlexExtraServers = make([]config.PlexExtraServer, len(cfg.PlexExtraServers))
		for i, ps := range cfg.PlexExtraServers {
			ps.Token = redact(ps.Token)
			ps.Account.Token = redact(ps.Account.Token)
			out.PlexExtraServers[i] = ps
		}
	}
	out.Catalog.TMDBKey = redact(out.Catalog.TMDBKey)
	out.Trakt.ClientSecret = redact(out.Trakt.ClientSecret)
	out.Simkl.ClientSecret = redact(out.Simkl.ClientSecret)
	out.OpenSubtitles = redactOpenSubtitles(out.OpenSubtitles)
	out.SubDL.APIKey = redact(out.SubDL.APIKey)
	out.SubSource.APIKey = redact(out.SubSource.APIKey)
	if cfg.Subtitles.Providers != nil {
		out.Subtitles.Providers = make([]config.SubtitleProvider, len(cfg.Subtitles.Providers))
		for i, p := range cfg.Subtitles.Providers {
			if p.Kind == config.SubtitleProviderStremio {
				p.URL = redactStremioURL(p.URL)
			}
			out.Subtitles.Providers[i] = p
		}
	}
	out.LLM = redactLLM(out.LLM)
	out.Outbound.Password = redact(out.Outbound.Password)
	out.Prowlarr.APIKey = redact(out.Prowlarr.APIKey)
	out.ClientIdentity.Rules = redactIdentityRules(cfg.ClientIdentity.Rules)
	out.Database = out.Database.Redacted()
	if cfg.Usenet.Providers != nil {
		out.Usenet.Providers = make([]config.UsenetProvider, len(cfg.Usenet.Providers))
		for i, p := range cfg.Usenet.Providers {
			p.Password = redact(p.Password)
			out.Usenet.Providers[i] = p
		}
	}
	if cfg.Indexers != nil {
		out.Indexers = make([]config.Indexer, len(cfg.Indexers))
		for i, ix := range cfg.Indexers {
			ix.APIKey = redact(ix.APIKey)
			ix.SecretSettings = redactMap(ix.SecretSettings)
			if ix.IsStremio() {
				ix.URL = redactStremioURL(ix.URL)
			}
			out.Indexers[i] = ix
		}
	}
	if cfg.Debrid.Providers != nil {
		out.Debrid.Providers = make([]config.DebridProvider, len(cfg.Debrid.Providers))
		for i, p := range cfg.Debrid.Providers {
			p.APIKey = redact(p.APIKey)
			out.Debrid.Providers[i] = p
		}
	}
	return out
}

func redactLLM(l config.LLM) config.LLM {
	l.APIKey = redact(l.APIKey)
	l.SpeechAPIKey = redact(l.SpeechAPIKey)
	return l
}

func redactOpenSubtitles(o config.OpenSubtitles) config.OpenSubtitles {
	o.APIKey = redact(o.APIKey)
	o.Password = redact(o.Password)
	return o
}

func redactTrakt(t config.Trakt) config.Trakt {
	t.ClientSecret = redact(t.ClientSecret)
	return t
}

// redactMap redacts every value of a secrets map (a copy).
func redactMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = redact(v)
	}
	return out
}

// Placeholder binding.
//
// The UI sends "••••" back for a secret it did not change. The stored
// secret is put back only for the same entry going to the same place: a
// list entry must carry the stored entry's stable ID, and its destination
// identity (host, port, URL, base URL, kind, provider and the account the
// secret authenticates) must be unchanged. Anything else needs the secret
// typed again, so a placeholder can never carry a stored secret to a new
// server, and a new or copied entry never inherits another entry's secret.

// secretRequired is the message for a placeholder that cannot be restored.
const secretRequired = "enter it again: the stored value is kept only while the entry and where it connects are unchanged"

// placeholderErrors collects one FieldError per placeholder that has no
// stored secret bound to it.
type placeholderErrors []FieldError

func (e *placeholderErrors) add(field string) {
	*e = append(*e, FieldError{Field: field, Message: secretRequired})
}

// restore returns the stored value for a placeholder when bound is set;
// otherwise it records field and returns "". A non-placeholder is kept.
func (e *placeholderErrors) restore(field, next, prev string, bound bool) string {
	if !isRedacted(next) {
		return next
	}
	if bound {
		return prev
	}
	e.add(field)
	return ""
}

func (e *placeholderErrors) restoreMap(field string, next, prev map[string]string, bound bool) map[string]string {
	if next == nil {
		return nil
	}
	out := make(map[string]string, len(next))
	for k, v := range next {
		old, ok := prev[k]
		out[k] = e.restore(field+"."+k, v, old, bound && ok)
	}
	return out
}

func normHost(h string) string { return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".") }

// normURL compares URLs by scheme, host and path; scheme and host are case
// insensitive and a trailing slash does not matter.
func normURL(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	return strings.TrimRight(u.String(), "/")
}

func usenetIdentity(p config.UsenetProvider) string {
	return strings.Join([]string{normHost(p.Host), strconv.Itoa(p.Port), strconv.FormatBool(p.TLS), strings.TrimSpace(p.Username)}, "\x00")
}

// indexerIdentity covers everything that decides where an indexer's
// secrets are sent: the URL, kind and protocol, and for a built-in site the
// definition, its public settings (which can pick another site address)
// and the FlareSolverr service challenged requests are relayed through.
func indexerIdentity(ix config.Indexer, flareSolverr string) string {
	kind := ix.Kind
	if kind == "" {
		kind = config.IndexerKindNewznab
	}
	parts := []string{normURL(ix.URL), kind, ix.Protocol}
	if ix.IsBuiltin() {
		parts = append(parts, strings.TrimSpace(ix.Definition), normURL(flareSolverr))
		keys := make([]string, 0, len(ix.Settings))
		for k := range ix.Settings {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			parts = append(parts, k+"="+ix.Settings[k])
		}
	}
	return strings.Join(parts, "\x00")
}

func debridIdentity(p config.DebridProvider) string {
	return strings.ToLower(strings.TrimSpace(p.Kind))
}

// llmIdentity is the provider and the endpoint requests actually go to
// (hosted providers ignore BaseURL).
func llmIdentity(l config.LLM) string {
	id := strings.TrimSpace(l.Provider)
	if id == "" {
		id = config.LLMProviderCustom
	}
	_, base := l.Endpoint()
	return id + "\x00" + normURL(base)
}

// UnredactConfig restores secrets in next that the client sent back as the
// redaction placeholder, using prev (the currently stored config). List
// entries are matched by stable ID only, so reordering a list does not
// swap secrets. A placeholder whose entry is new, has no ID, or now points
// somewhere else is not restored: it is reported as a FieldError and left
// empty.
func UnredactConfig(next, prev config.Config) (config.Config, []FieldError) {
	var errs placeholderErrors
	out := next
	for id, a := range out.Posters.Accounts.Entries() {
		old := prev.Posters.Normalize().Accounts
		prior := old.Entries()[id]
		same := a.BaseURL == prior.BaseURL
		a.URLTemplate = errs.restore("posters.accounts."+id+".urlTemplate", a.URLTemplate, prior.URLTemplate, same)
		a.APIKey = errs.restore("posters.accounts."+id+".apiKey", a.APIKey, prior.APIKey, same && a.URLTemplate == prior.URLTemplate)
	}
	samePosterDestination := next.Posters.Source == prev.Posters.Source && next.Posters.BaseURL == prev.Posters.BaseURL
	out.Posters.URLTemplate = errs.restore("posters.urlTemplate", next.Posters.URLTemplate, prev.Posters.URLTemplate, samePosterDestination)
	out.Posters.APIKey = errs.restore("posters.apiKey", next.Posters.APIKey, prev.Posters.APIKey, samePosterDestination && out.Posters.URLTemplate == prev.Posters.URLTemplate)
	out.Markers.APIKey = errs.restore("markers.apiKey", next.Markers.APIKey, prev.Markers.APIKey, true)
	out.Plex.Token = errs.restore("plex.token", next.Plex.Token, prev.Plex.Token, true)
	out.Catalog.TMDBKey = errs.restore("catalog.tmdbKey", next.Catalog.TMDBKey, prev.Catalog.TMDBKey, true)
	out.Trakt = errs.unredactTrakt(next.Trakt, prev.Trakt)
	out.Simkl.ClientSecret = errs.restore("simkl.clientSecret", next.Simkl.ClientSecret, prev.Simkl.ClientSecret, strings.TrimSpace(next.Simkl.ClientID) == strings.TrimSpace(prev.Simkl.ClientID))
	out.OpenSubtitles = errs.unredactOpenSubtitles(next.OpenSubtitles, prev.OpenSubtitles)
	out.SubDL.APIKey = errs.restore("subdl.apiKey", next.SubDL.APIKey, prev.SubDL.APIKey, true)
	out.SubSource.APIKey = errs.restore("subsource.apiKey", next.SubSource.APIKey, prev.SubSource.APIKey, true)
	if next.Subtitles.Providers != nil {
		out.Subtitles.Providers = make([]config.SubtitleProvider, len(next.Subtitles.Providers))
		for i, p := range next.Subtitles.Providers {
			if p.Kind == config.SubtitleProviderStremio && isRedactedStremioURL(p.URL) {
				restored := ""
				for _, o := range prev.Subtitles.Providers {
					if p.ID != "" && o.ID == p.ID && o.Kind == config.SubtitleProviderStremio &&
						(isRedacted(p.URL) || stremioURLHost(p.URL) == stremioURLHost(o.URL)) {
						restored = o.URL
						break
					}
				}
				if restored == "" {
					errs.add(fmt.Sprintf("subtitles.providers[%d].url", i))
				}
				p.URL = restored
			}
			out.Subtitles.Providers[i] = p
		}
	}
	out.LLM = errs.unredactLLM(next.LLM, prev.LLM)
	out.Outbound.Password = errs.restore("outbound.password", next.Outbound.Password, prev.Outbound.Password, strings.TrimSpace(next.Outbound.URL) == strings.TrimSpace(prev.Outbound.URL))
	out.Prowlarr.APIKey = errs.restore("prowlarr.apiKey", next.Prowlarr.APIKey, prev.Prowlarr.APIKey, sameProwlarrURL(next.Prowlarr.URL, prev.Prowlarr.URL))
	out.ClientIdentity.Rules = errs.unredactIdentityRules(next.ClientIdentity.Rules, prev.ClientIdentity.Rules)
	if next.Usenet.Providers != nil {
		out.Usenet.Providers = make([]config.UsenetProvider, len(next.Usenet.Providers))
		for i, p := range next.Usenet.Providers {
			out.Usenet.Providers[i] = errs.unredactProvider(fmt.Sprintf("usenet.providers[%d]", i), p, prev.Usenet.Providers)
		}
	}
	if next.Indexers != nil {
		out.Indexers = make([]config.Indexer, len(next.Indexers))
		for i, ix := range next.Indexers {
			out.Indexers[i] = errs.unredactIndexer(fmt.Sprintf("indexers[%d]", i), ix, next.IndexerSites.FlareSolverrURL, prev)
		}
	}
	if next.Debrid.Providers != nil {
		out.Debrid.Providers = make([]config.DebridProvider, len(next.Debrid.Providers))
		for i, p := range next.Debrid.Providers {
			out.Debrid.Providers[i] = errs.unredactDebrid(fmt.Sprintf("debrid.providers[%d]", i), p, prev.Debrid.Providers)
		}
	}
	return out, errs
}

func (e *placeholderErrors) unredactTrakt(next, prev config.Trakt) config.Trakt {
	bound := strings.TrimSpace(next.ClientID) == strings.TrimSpace(prev.ClientID)
	next.ClientSecret = e.restore("trakt.clientSecret", next.ClientSecret, prev.ClientSecret, bound)
	return next
}

func (e *placeholderErrors) unredactOpenSubtitles(next, prev config.OpenSubtitles) config.OpenSubtitles {
	next.APIKey = e.restore("openSubtitles.apiKey", next.APIKey, prev.APIKey, true)
	bound := strings.TrimSpace(next.Username) == strings.TrimSpace(prev.Username)
	next.Password = e.restore("openSubtitles.password", next.Password, prev.Password, bound)
	return next
}

func (e *placeholderErrors) unredactLLM(next, prev config.LLM) config.LLM {
	next.APIKey = e.restore("llm.apiKey", next.APIKey, prev.APIKey, llmIdentity(next) == llmIdentity(prev))
	next.SpeechAPIKey = e.restore("llm.speechApiKey", next.SpeechAPIKey, prev.SpeechAPIKey, speechIdentity(next) == speechIdentity(prev))
	return next
}

func speechIdentity(l config.LLM) string {
	return normURL(l.SpeechBaseURL) + "\x00" + strings.TrimSpace(l.SpeechModel)
}

func (e *placeholderErrors) unredactProvider(field string, p config.UsenetProvider, prev []config.UsenetProvider) config.UsenetProvider {
	var old config.UsenetProvider
	bound := false
	for _, o := range prev {
		if p.ID != "" && o.ID == p.ID {
			old, bound = o, usenetIdentity(o) == usenetIdentity(p)
			break
		}
	}
	p.Password = e.restore(field+".password", p.Password, old.Password, bound)
	return p
}

// unredactIndexer restores ix's placeholders from prev; flareSolverr is the
// FlareSolverr URL ix will be used with.
func (e *placeholderErrors) unredactIndexer(field string, ix config.Indexer, flareSolverr string, prev config.Config) config.Indexer {
	var old config.Indexer
	found := false
	for _, o := range prev.Indexers {
		if ix.ID != "" && o.ID == ix.ID {
			old, found = o, true
			break
		}
	}
	// A legacy Stremio indexer row (migrated at start into an installed
	// add-on) keeps its link redacted. A redacted install URL restores only for the same entry
	// still pointing at the same host; anything else must be pasted again.
	if ix.IsStremio() && isRedactedStremioURL(ix.URL) {
		if found && old.IsStremio() && stremioURLHost(ix.URL) != "" && stremioURLHost(ix.URL) == stremioURLHost(old.URL) {
			ix.URL = old.URL
		} else if found && isRedacted(ix.URL) && old.IsStremio() {
			ix.URL = old.URL
		} else {
			e.add(field + ".url")
			ix.URL = ""
		}
	}
	bound := found && indexerIdentity(old, prev.IndexerSites.FlareSolverrURL) == indexerIdentity(ix, flareSolverr)
	ix.APIKey = e.restore(field+".apiKey", ix.APIKey, old.APIKey, bound)
	ix.SecretSettings = e.restoreMap(field+".secretSettings", ix.SecretSettings, old.SecretSettings, bound)
	return ix
}

func ruleDestination(r config.IdentityRule) string {
	return strings.Join([]string{
		string(r.Target),
		strings.TrimSpace(r.IndexerID),
		normHost(r.Host),
		strings.ToLower(strings.TrimSpace(r.Scope)),
	}, "\x00")
}

// redactIdentityRules hides sensitive header and parameter override values in
// API responses; non-secret edits remain visible so the owner can edit them.
func redactIdentityRules(rules []config.IdentityRule) []config.IdentityRule {
	if rules == nil {
		return nil
	}
	out := make([]config.IdentityRule, len(rules))
	for i, r := range rules {
		if len(r.Headers) > 0 {
			hs := make([]config.KVOverride, len(r.Headers))
			copy(hs, r.Headers)
			for j := range hs {
				if !hs[j].Remove && config.SensitiveHeader(hs[j].Name) {
					hs[j].Value = redact(hs[j].Value)
				}
			}
			r.Headers = hs
		}
		if len(r.Params) > 0 {
			ps := make([]config.KVOverride, len(r.Params))
			copy(ps, r.Params)
			for j := range ps {
				if !ps[j].Remove && config.SensitiveParam(ps[j].Name) {
					ps[j].Value = redact(ps[j].Value)
				}
			}
			r.Params = ps
		}
		out[i] = r
	}
	return out
}

func (e *placeholderErrors) unredactIdentityRules(next, prev []config.IdentityRule) []config.IdentityRule {
	if next == nil {
		return nil
	}
	out := make([]config.IdentityRule, len(next))
	for i, r := range next {
		var old config.IdentityRule
		bound := false
		for _, o := range prev {
			if r.ID != "" && o.ID == r.ID {
				old, bound = o, ruleDestination(o) == ruleDestination(r)
				break
			}
		}
		prefix := fmt.Sprintf("clientIdentity.rules[%d]", i)
		if len(r.Headers) > 0 {
			hs := make([]config.KVOverride, len(r.Headers))
			copy(hs, r.Headers)
			for j := range hs {
				if hs[j].Remove || !config.SensitiveHeader(hs[j].Name) || !isRedacted(hs[j].Value) {
					continue
				}
				oldVal, has := findOverrideValue(old.Headers, hs[j].Name)
				hs[j].Value = e.restore(prefix+".headers", hs[j].Value, oldVal, bound && has)
			}
			r.Headers = hs
		}
		if len(r.Params) > 0 {
			ps := make([]config.KVOverride, len(r.Params))
			copy(ps, r.Params)
			for j := range ps {
				if ps[j].Remove || !config.SensitiveParam(ps[j].Name) || !isRedacted(ps[j].Value) {
					continue
				}
				oldVal, has := findOverrideValue(old.Params, ps[j].Name)
				ps[j].Value = e.restore(prefix+".params", ps[j].Value, oldVal, bound && has)
			}
			r.Params = ps
		}
		out[i] = r
	}
	return out
}

func findOverrideValue(ovs []config.KVOverride, name string) (string, bool) {
	for _, o := range ovs {
		if !o.Remove && strings.EqualFold(strings.TrimSpace(o.Name), strings.TrimSpace(name)) {
			return o.Value, true
		}
	}
	return "", false
}

func (e *placeholderErrors) unredactDebrid(field string, p config.DebridProvider, prev []config.DebridProvider) config.DebridProvider {
	var old config.DebridProvider
	bound := false
	for _, o := range prev {
		if p.ID != "" && o.ID == p.ID {
			old, bound = o, debridIdentity(o) == debridIdentity(p)
			break
		}
	}
	p.APIKey = e.restore(field+".apiKey", p.APIKey, old.APIKey, bound)
	return p
}

// UnredactIndexer restores an indexer's API key and secret settings that
// the UI sent back as placeholders, from the stored config (for tests of
// unsaved edits run outside this package). A placeholder that is not bound
// to the stored entry (see UnredactConfig) comes back empty.
func UnredactIndexer(ix config.Indexer, prev config.Config) config.Indexer {
	var errs placeholderErrors
	return errs.unredactIndexer("indexer", ix, prev.IndexerSites.FlareSolverrURL, prev)
}
