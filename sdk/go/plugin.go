package etherplugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
)

// Context is passed to every plugin route handler.
type Context struct {
	context.Context
	Manifest  Manifest
	Settings  map[string]any
	Locale    string
	PickToken string
	RequestID string
	Headers   map[string]string
	Query     map[string]string
	Ether     Host
	I18n      *I18n
}

// T resolves a localized string key with optional parameter interpolation.
func (c *Context) T(key string, params map[string]any) string {
	if c.I18n == nil {
		return key
	}
	return c.I18n.T(key, params, c.Locale)
}

// Routes holds the capability handlers for a Plugin.
type Routes struct {
	Catalog          func(ctx *Context, req CatalogRequest) (CatalogResponse, error)
	Meta             func(ctx *Context, req MetaRequest) (MetaResponse, error)
	Stream           func(ctx *Context, req StreamRequest) (StreamResponse, error)
	OpenStream       func(ctx *Context, req OpenStreamRequest) (OpenStreamResponse, error)
	Subtitles        func(ctx *Context, req SubtitlesRequest) (SubtitlesResponse, error)
	SubtitleDownload func(ctx *Context, req SubtitleDownloadRequest) (SubtitleDownloadResponse, error)
	Markers          func(ctx *Context, req MarkersRequest) (MarkersResponse, error)
	Channels         func(ctx *Context) (ChannelsResponse, error)
	Guide            func(ctx *Context, req GuideRequest) (GuideResponse, error)
	Configure        func(ctx *Context, settings map[string]any) (ConfigureResponse, error)
	Test             func(ctx *Context) (TestResponse, error)
	Options          func(ctx *Context, field string) ([]FieldOption, error)
	Health           func(ctx *Context) (HealthResponse, error)
	Notify           func(ctx *Context, req NotifyRequest) (NotifyResponse, error)
	Event            func(ctx *Context, events []EtherEvent) (EventResponse, error)
	View             func(ctx *Context, req ViewRequest) (ViewResponse, error)
	Action           func(ctx *Context, req ActionRequest) (ActionResponse, error)
	Tool             func(ctx *Context, req ToolRequest) (ToolResponse, error)
	Job              func(ctx *Context, req JobRequest) (JobResponse, error)
	File             func(ctx *Context, req FileRequest) (FileRangeResponse, error)
	RankKey          func(candidate CandidateFacts, config map[string]any) int64
	CondMatch        func(candidate CandidateFacts, operator string, value any) bool
}

// Definition configures a new Plugin instance.
type Definition struct {
	Manifest          Manifest
	Routes            Routes
	DefaultSettings   map[string]any
	Strings           StringsCatalog
	DefaultLocale     string
	Host              Host
	RemoteHostOptions RemoteHostOptions
}

// Plugin is a runnable Ether plugin instance.
type Plugin struct {
	manifest        Manifest
	routes          Routes
	defaultSettings map[string]any
	defaultLocale   string
	i18n            *I18n
	host            Host
	remoteOpts      RemoteHostOptions
	pageRoutes      map[string]string
	kindHandlers    *kindHandlers

	mu              sync.RWMutex
	settings        map[string]any
	settingsFailure sync.Once
}

// New constructs a Plugin from a Definition.
func New(def Definition) *Plugin {
	defaultLocale := def.DefaultLocale
	if defaultLocale == "" {
		defaultLocale = "en"
	}
	catalog := StringsCatalog{}
	if def.Manifest.Ether != nil && def.Manifest.Ether.Strings != nil {
		for loc, raw := range def.Manifest.Ether.Strings {
			if m, ok := raw.(map[string]string); ok {
				tbl := map[string]string{}
				for k, v := range m {
					tbl[k] = v
				}
				catalog[loc] = tbl
			} else if m, ok := raw.(map[string]any); ok {
				tbl := map[string]string{}
				for k, v := range m {
					if s, ok := v.(string); ok {
						tbl[k] = s
					}
				}
				catalog[loc] = tbl
			}
		}
	}
	for loc, tbl := range def.Strings {
		if catalog[loc] == nil {
			catalog[loc] = map[string]string{}
		}
		for k, v := range tbl {
			catalog[loc][k] = v
		}
	}

	var schema *SettingsSchema
	pageRoutes := map[string]string{}
	if def.Manifest.Ether != nil {
		schema = def.Manifest.Ether.Settings
		if def.Manifest.Ether.Contributes != nil {
			for _, p := range def.Manifest.Ether.Contributes.Pages {
				if p.Route != "" && p.Key != "" {
					pageRoutes[p.Route] = p.Key
				}
			}
			for _, s := range def.Manifest.Ether.Contributes.Slots {
				if s.Route != "" && s.Key != "" {
					pageRoutes[s.Route] = s.Key
				}
			}
		}
	}
	initialSettings, _ := DecodeSettings(schema, nil, def.DefaultSettings)

	return &Plugin{
		manifest:        def.Manifest,
		routes:          def.Routes,
		defaultSettings: def.DefaultSettings,
		defaultLocale:   defaultLocale,
		i18n:            NewI18n(catalog, defaultLocale),
		host:            def.Host,
		remoteOpts:      def.RemoteHostOptions,
		pageRoutes:      pageRoutes,
		settings:        initialSettings,
	}
}

// Manifest returns the plugin's Manifest.
func (p *Plugin) Manifest() Manifest {
	return p.manifest
}

// GetSettings returns a copy of the plugin's active settings.
func (p *Plugin) GetSettings() map[string]any {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]any, len(p.settings))
	for k, v := range p.settings {
		out[k] = v
	}
	return out
}

// SetSettings validates and updates the plugin's settings.
func (p *Plugin) SetSettings(incoming map[string]any) (map[string]any, map[string]string) {
	var schema *SettingsSchema
	if p.manifest.Ether != nil {
		schema = p.manifest.Ether.Settings
	}
	decoded, errs := DecodeSettings(schema, incoming, p.defaultSettings)
	if len(errs) == 0 {
		p.mu.Lock()
		p.settings = decoded
		p.mu.Unlock()
	}
	return decoded, errs
}

// RankKey invokes the plugin's RankKey hot-path function.
func (p *Plugin) RankKey(candidate CandidateFacts, config map[string]any) int64 {
	if p.routes.RankKey == nil {
		return 0
	}
	if config == nil {
		config = p.GetSettings()
	}
	return p.routes.RankKey(candidate, config)
}

// CondMatch invokes the plugin's CondMatch hot-path function.
func (p *Plugin) CondMatch(candidate CandidateFacts, operator string, value any) bool {
	if p.routes.CondMatch == nil {
		return false
	}
	return p.routes.CondMatch(candidate, operator, value)
}

func headerValue(headers map[string]string, name string) string {
	for k, v := range headers {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

func parsePathAndQuery(rawPath string, rawQuery map[string]string) (string, map[string]string) {
	pathOnly := rawPath
	if pathOnly == "" {
		pathOnly = "/"
	}
	query := map[string]string{}
	for k, v := range rawQuery {
		query[k] = v
	}
	if idx := strings.IndexByte(pathOnly, '?'); idx >= 0 {
		qs := pathOnly[idx+1:]
		pathOnly = pathOnly[:idx]
		if pathOnly == "" {
			pathOnly = "/"
		}
		if vals, err := url.ParseQuery(qs); err == nil {
			for k, list := range vals {
				if _, exists := query[k]; !exists && len(list) > 0 {
					query[k] = list[0]
				}
			}
		}
	}
	if !strings.HasPrefix(pathOnly, "/") {
		pathOnly = "/" + pathOnly
	}
	return pathOnly, query
}

func (p *Plugin) buildContext(ctx context.Context, req RawRequest) *Context {
	_, query := parsePathAndQuery(req.Path, req.Query)
	locale := req.Locale
	if locale == "" {
		locale = query["locale"]
	}
	if locale == "" {
		locale = headerValue(req.Headers, "X-Ether-Locale")
	}
	if locale == "" {
		locale = headerValue(req.Headers, "Accept-Language")
	}
	if locale == "" {
		locale = p.defaultLocale
	}
	if idx := strings.IndexByte(locale, ','); idx >= 0 {
		locale = locale[:idx]
	}
	locale = strings.TrimSpace(locale)
	if locale == "" {
		locale = "en"
	}

	hostClient := p.host
	if hostClient == nil {
		hostClient = defaultHostForRuntime(p.remoteOpts)
	}

	hdrs := map[string]string{}
	for k, v := range req.Headers {
		hdrs[k] = v
	}

	return &Context{
		Context:   ctx,
		Manifest:  p.manifest,
		Settings:  p.GetSettings(),
		Locale:    locale,
		PickToken: headerValue(req.Headers, "X-Ether-Pick"),
		RequestID: headerValue(req.Headers, "X-Ether-Request-ID"),
		Headers:   hdrs,
		Query:     query,
		Ether:     hostClient,
		I18n:      p.i18n,
	}
}

func okJSON(v any, status int) RawResponse {
	if status == 0 {
		status = http.StatusOK
	}
	b, err := json.Marshal(v)
	if err != nil {
		return errorJSON(http.StatusInternalServerError, "marshal_error", err.Error())
	}
	return RawResponse{
		Status:  status,
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    b,
	}
}

func errorJSON(status int, code, message string) RawResponse {
	b, _ := json.Marshal(map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
	return RawResponse{
		Status:  status,
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    b,
	}
}

func toRouteErrorResponse(err error) RawResponse {
	var re *RouteError
	if errors.As(err, &re) {
		return errorJSON(re.Status, re.Code, re.Message)
	}
	return errorJSON(http.StatusInternalServerError, "internal_error", err.Error())
}

type parsedStremioRoute struct {
	resource string
	typ      string
	id       string
	extra    map[string]string
}

func matchStremioRoute(path string, query map[string]string) *parsedStremioRoute {
	if !strings.HasSuffix(path, ".json") {
		return nil
	}
	trimmed := strings.TrimPrefix(strings.TrimSuffix(path, ".json"), "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 3 || len(parts) > 4 {
		return nil
	}
	res := parts[0]
	switch res {
	case "catalog", "meta", "stream", "subtitles":
	default:
		return nil
	}
	typ, _ := url.PathUnescape(parts[1])
	id, _ := url.PathUnescape(parts[2])
	extra := map[string]string{}
	if len(parts) == 4 && parts[3] != "" {
		if vals, err := url.ParseQuery(parts[3]); err == nil {
			for k, list := range vals {
				if len(list) > 0 {
					extra[k] = list[0]
				}
			}
		}
	}
	for k, v := range query {
		if _, exists := extra[k]; !exists {
			extra[k] = v
		}
	}
	return &parsedStremioRoute{
		resource: res,
		typ:      typ,
		id:       id,
		extra:    extra,
	}
}

func decodeJSONMap(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var asStr string
	if json.Unmarshal(raw, &asStr) == nil {
		if strings.TrimSpace(asStr) == "" {
			return map[string]any{}, nil
		}
		raw = json.RawMessage(asStr)
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Dispatch routes a RawRequest through the plugin and returns a RawResponse.
func (p *Plugin) Dispatch(ctx context.Context, req RawRequest) RawResponse {
	pctx := p.buildContext(ctx, req)
	if getter, ok := pctx.Ether.(interface {
		SettingsGet(context.Context) (map[string]any, error)
	}); ok && req.Path != "/ether/configure" {
		remote, isRemote := pctx.Ether.(*RemoteHostClient)
		if !isRemote || remote.credential != "" {
			settings, err := getter.SettingsGet(ctx)
			if err != nil {
				p.settingsFailure.Do(func() { log.Print("plugin settings lookup failed") })
				pctx.Settings = map[string]any{}
			} else {
				var schema *SettingsSchema
				if p.manifest.Ether != nil {
					schema = p.manifest.Ether.Settings
				}
				for _, field := range CollectSettingFields(schema) {
					if field.Kind == "secret" {
						if v, ok := pctx.Settings[field.Key]; ok {
							settings[field.Key] = v
						}
					}
				}
				pctx.Settings, _ = DecodeSettings(schema, settings, p.defaultSettings)
			}
		}
	}
	method := strings.ToUpper(req.Method)
	if method == "" {
		method = http.MethodGet
	}
	path, query := parsePathAndQuery(req.Path, req.Query)

	if method == http.MethodGet && path == "/manifest.json" {
		return okJSON(p.manifest, http.StatusOK)
	}

	if method == http.MethodGet && path == "/ether/health" {
		if p.routes.Health != nil {
			res, err := p.routes.Health(pctx)
			if err != nil {
				return toRouteErrorResponse(err)
			}
			return okJSON(res, http.StatusOK)
		}
		return okJSON(HealthResponse{Status: "ok"}, http.StatusOK)
	}

	if method == http.MethodPost && path == "/ether/subtitles/download" {
		if strings.TrimSpace(pctx.PickToken) == "" {
			return errorJSON(http.StatusForbidden, "pick_required", "Subtitle downloads require an X-Ether-Pick token.")
		}
		if p.routes.SubtitleDownload == nil {
			return errorJSON(http.StatusNotFound, "not_found", "Subtitle download handler is not registered.")
		}
		var input SubtitleDownloadRequest
		if err := json.Unmarshal(req.Body, &input); err != nil {
			return errorJSON(http.StatusBadRequest, "invalid_json", "Request body must be valid JSON.")
		}
		res, err := p.routes.SubtitleDownload(pctx, input)
		if err != nil {
			return toRouteErrorResponse(err)
		}
		return okJSON(res, http.StatusOK)
	}
	if method == http.MethodPost && path == "/ether/configure" {
		bodyMap, err := decodeJSONMap(req.Body)
		if err != nil {
			return errorJSON(http.StatusBadRequest, "invalid_json", "Request body must be valid JSON.")
		}
		incoming := bodyMap
		if nested, ok := bodyMap["settings"].(map[string]any); ok {
			incoming = nested
		}
		var schema *SettingsSchema
		if p.manifest.Ether != nil {
			schema = p.manifest.Ether.Settings
		}
		decoded, errs := DecodeSettings(schema, incoming, p.defaultSettings)
		if len(errs) > 0 {
			return okJSON(ConfigureResponse{Errors: errs}, http.StatusOK)
		}
		if p.routes.Configure != nil {
			pctx.Settings = decoded
			res, err := p.routes.Configure(pctx, decoded)
			if err != nil {
				return toRouteErrorResponse(err)
			}
			if len(res.Errors) == 0 {
				p.mu.Lock()
				p.settings = decoded
				p.mu.Unlock()
			}
			if res.Errors == nil {
				res.Errors = map[string]string{}
			}
			return okJSON(res, http.StatusOK)
		}
		p.mu.Lock()
		p.settings = decoded
		p.mu.Unlock()
		return okJSON(ConfigureResponse{Errors: map[string]string{}}, http.StatusOK)
	}

	if method == http.MethodPost && path == "/ether/test" {
		if p.routes.Test != nil {
			res, err := p.routes.Test(pctx)
			if err != nil {
				return toRouteErrorResponse(err)
			}
			return okJSON(res, http.StatusOK)
		}
		return okJSON(TestResponse{OK: true}, http.StatusOK)
	}

	if method == http.MethodGet && strings.HasPrefix(path, "/ether/options/") {
		field, _ := url.PathUnescape(strings.TrimPrefix(path, "/ether/options/"))
		if p.routes.Options == nil {
			return okJSON([]FieldOption{}, http.StatusOK)
		}
		res, err := p.routes.Options(pctx, field)
		if err != nil {
			return toRouteErrorResponse(err)
		}
		if res == nil {
			res = []FieldOption{}
		}
		return okJSON(res, http.StatusOK)
	}

	if method == http.MethodPost && path == "/ether/stream/open" {
		if p.routes.OpenStream == nil {
			return errorJSON(http.StatusNotFound, "not_found", "Stream open handler is not registered.")
		}
		bodyMap, err := decodeJSONMap(req.Body)
		if err != nil {
			return errorJSON(http.StatusBadRequest, "invalid_json", "Request body must be valid JSON.")
		}
		streamID, _ := bodyMap["streamId"].(string)
		res, err := p.routes.OpenStream(pctx, OpenStreamRequest{StreamID: streamID})
		if err != nil {
			return toRouteErrorResponse(err)
		}
		return okJSON(res, http.StatusOK)
	}

	if method == http.MethodGet && strings.HasPrefix(path, "/ether/markers/") && strings.HasSuffix(path, ".json") {
		if p.routes.Markers == nil {
			return errorJSON(http.StatusNotFound, "not_found", "Markers handler is not registered.")
		}
		trimmed := strings.TrimSuffix(strings.TrimPrefix(path, "/ether/markers/"), ".json")
		parts := strings.SplitN(trimmed, "/", 2)
		if len(parts) < 2 {
			return errorJSON(http.StatusBadRequest, "invalid_path", "Markers route requires type and id.")
		}
		typ, _ := url.PathUnescape(parts[0])
		id, _ := url.PathUnescape(parts[1])
		res, err := p.routes.Markers(pctx, MarkersRequest{Type: typ, ID: id})
		if err != nil {
			return toRouteErrorResponse(err)
		}
		return okJSON(res, http.StatusOK)
	}

	if method == http.MethodGet && path == "/ether/channels.json" {
		if p.routes.Channels == nil {
			return errorJSON(http.StatusNotFound, "not_found", "Channels handler is not registered.")
		}
		res, err := p.routes.Channels(pctx)
		if err != nil {
			return toRouteErrorResponse(err)
		}
		return okJSON(res, http.StatusOK)
	}

	if method == http.MethodGet && strings.HasPrefix(path, "/ether/guide/") && strings.HasSuffix(path, ".json") {
		if p.routes.Guide == nil {
			return errorJSON(http.StatusNotFound, "not_found", "Guide handler is not registered.")
		}
		channelID, _ := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(path, "/ether/guide/"), ".json"))
		res, err := p.routes.Guide(pctx, GuideRequest{
			ChannelID: channelID,
			From:      query["from"],
			To:        query["to"],
		})
		if err != nil {
			return toRouteErrorResponse(err)
		}
		return okJSON(res, http.StatusOK)
	}

	if method == http.MethodPost && path == "/ether/notify" {
		if p.routes.Notify == nil {
			return errorJSON(http.StatusNotFound, "not_found", "Notify handler is not registered.")
		}
		bodyMap, err := decodeJSONMap(req.Body)
		if err != nil {
			return errorJSON(http.StatusBadRequest, "invalid_json", "Request body must be valid JSON.")
		}
		title, _ := bodyMap["title"].(string)
		msg, _ := bodyMap["message"].(string)
		if msg == "" {
			msg, _ = bodyMap["text"].(string)
		}
		eventName, _ := bodyMap["event"].(string)
		settingsMap, _ := bodyMap["settings"].(map[string]any)
		res, err := p.routes.Notify(pctx, NotifyRequest{
			Title:    title,
			Message:  msg,
			Event:    eventName,
			Settings: settingsMap,
		})
		if err != nil {
			return toRouteErrorResponse(err)
		}
		return okJSON(res, http.StatusOK)
	}

	if method == http.MethodPost && path == "/ether/event" {
		if p.routes.Event == nil {
			return errorJSON(http.StatusNotFound, "not_found", "Event handler is not registered.")
		}
		var batch struct {
			Events []EtherEvent `json:"events"`
		}
		if len(req.Body) > 0 {
			if err := json.Unmarshal(req.Body, &batch); err != nil {
				return errorJSON(http.StatusBadRequest, "invalid_json", "Event batch body must be valid JSON.")
			}
		}
		res, err := p.routes.Event(pctx, batch.Events)
		if err != nil {
			return toRouteErrorResponse(err)
		}
		if res.Ack == nil {
			res.Ack = make([]string, 0, len(batch.Events))
			for _, ev := range batch.Events {
				res.Ack = append(res.Ack, ev.ID)
			}
		}
		return okJSON(res, http.StatusOK)
	}

	if method == http.MethodGet {
		pageKey, matchedPage := p.pageRoutes[path]
		if matchedPage || path == "/ether/view" || strings.HasPrefix(path, "/ether/view/") {
			if p.routes.View == nil {
				return errorJSON(http.StatusNotFound, "not_found", "View handler is not registered.")
			}
			if !matchedPage {
				if path == "/ether/view" {
					pageKey = "home"
				} else {
					pageKey, _ = url.PathUnescape(strings.TrimPrefix(path, "/ether/view/"))
				}
			}
			res, err := p.routes.View(pctx, ViewRequest{Page: pageKey, Query: query})
			if err != nil {
				return toRouteErrorResponse(err)
			}
			return okJSON(res, http.StatusOK)
		}
	}

	if method == http.MethodPost && strings.HasPrefix(path, "/ether/action/") {
		if p.routes.Action == nil {
			return errorJSON(http.StatusNotFound, "not_found", "Action handler is not registered.")
		}
		actionName, _ := url.PathUnescape(strings.TrimPrefix(path, "/ether/action/"))
		inputMap, err := decodeJSONMap(req.Body)
		if err != nil {
			return errorJSON(http.StatusBadRequest, "invalid_json", "Action input must be valid JSON.")
		}
		res, err := p.routes.Action(pctx, ActionRequest{Action: actionName, Input: inputMap})
		if err != nil {
			return toRouteErrorResponse(err)
		}
		return okJSON(res, http.StatusOK)
	}

	if method == http.MethodPost && strings.HasPrefix(path, "/ether/tool/") {
		if p.routes.Tool == nil {
			return errorJSON(http.StatusNotFound, "not_found", "Tool handler is not registered.")
		}
		toolName, _ := url.PathUnescape(strings.TrimPrefix(path, "/ether/tool/"))
		inputMap, err := decodeJSONMap(req.Body)
		if err != nil {
			return errorJSON(http.StatusBadRequest, "invalid_json", "Tool input must be valid JSON.")
		}
		res, err := p.routes.Tool(pctx, ToolRequest{Name: toolName, Input: inputMap})
		if err != nil {
			return toRouteErrorResponse(err)
		}
		return okJSON(res, http.StatusOK)
	}

	if method == http.MethodPost && strings.HasPrefix(path, "/ether/job/") {
		if p.routes.Job == nil {
			return errorJSON(http.StatusNotFound, "not_found", "Job handler is not registered.")
		}
		jobName, _ := url.PathUnescape(strings.TrimPrefix(path, "/ether/job/"))
		jobReq := JobRequest{Name: jobName}
		if len(req.Body) > 0 {
			if err := json.Unmarshal(req.Body, &jobReq); err != nil {
				return errorJSON(http.StatusBadRequest, "invalid_json", "Job body must be valid JSON.")
			}
		}
		jobReq.Name = jobName
		res, err := p.routes.Job(pctx, jobReq)
		if err != nil {
			return toRouteErrorResponse(err)
		}
		return okJSON(res, http.StatusOK)
	}

	if method == http.MethodPost && path == "/ether/rank-key" {
		if p.routes.RankKey == nil {
			return errorJSON(http.StatusNotFound, "not_found", "Rank key handler is not registered.")
		}
		var payload struct {
			Candidate *CandidateFacts `json:"candidate"`
			Config    map[string]any  `json:"config"`
		}
		_ = json.Unmarshal(req.Body, &payload)
		var cand CandidateFacts
		if payload.Candidate != nil {
			cand = *payload.Candidate
		} else {
			_ = json.Unmarshal(req.Body, &cand)
		}
		score := p.RankKey(cand, payload.Config)
		return okJSON(map[string]int64{"score": score}, http.StatusOK)
	}

	if method == http.MethodPost && path == "/ether/cond-match" {
		if p.routes.CondMatch == nil {
			return errorJSON(http.StatusNotFound, "not_found", "Condition match handler is not registered.")
		}
		var payload struct {
			Candidate CandidateFacts `json:"candidate"`
			Operator  string         `json:"operator"`
			Value     any            `json:"value"`
		}
		_ = json.Unmarshal(req.Body, &payload)
		if payload.Operator == "" {
			payload.Operator = "eq"
		}
		matched := p.CondMatch(payload.Candidate, payload.Operator, payload.Value)
		return okJSON(map[string]bool{"match": matched}, http.StatusOK)
	}

	if (method == http.MethodGet || method == http.MethodHead) && strings.HasPrefix(path, "/ether/file/") {
		if p.routes.File == nil {
			return errorJSON(http.StatusNotFound, "not_found", "File handler is not registered.")
		}
		relPath, _ := url.PathUnescape(strings.TrimPrefix(path, "/ether/file/"))
		if relPath == "" || strings.HasPrefix(relPath, "/") || containsDotDot(relPath) {
			return errorJSON(http.StatusBadRequest, "invalid_path", "File path must be a clean relative path.")
		}
		rangeHeader := headerValue(req.Headers, "Range")
		fileRes, err := p.routes.File(pctx, FileRequest{
			Path:     relPath,
			Range:    rangeHeader,
			HeadOnly: method == http.MethodHead,
		})
		if err != nil {
			return toRouteErrorResponse(err)
		}
		contentType := fileRes.ContentType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		hdrs := map[string]string{
			"Content-Type":  contentType,
			"Accept-Ranges": "bytes",
		}
		for k, v := range fileRes.Headers {
			hdrs[k] = v
		}
		if fileRes.ContentRange != "" {
			hdrs["Content-Range"] = fileRes.ContentRange
		} else if fileRes.TotalSize != nil {
			hdrs["Content-Length"] = strconv.FormatInt(*fileRes.TotalSize, 10)
		}
		status := fileRes.Status
		if status == 0 {
			if fileRes.ContentRange != "" {
				status = http.StatusPartialContent
			} else {
				status = http.StatusOK
			}
		}
		bodyBytes := fileRes.Body
		if method == http.MethodHead {
			bodyBytes = []byte{}
		}
		return RawResponse{
			Status:     status,
			Headers:    hdrs,
			BinaryBody: bodyBytes,
		}
	}

	if method == http.MethodGet {
		if stremio := matchStremioRoute(path, query); stremio != nil {
			switch stremio.resource {
			case "subtitles":
				if p.routes.Subtitles == nil {
					return errorJSON(http.StatusNotFound, "not_found", "Subtitles handler is not registered.")
				}
				var videoSize int64
				if rawSize, ok := stremio.extra["videoSize"]; ok {
					videoSize, _ = strconv.ParseInt(rawSize, 10, 64)
				}
				res, err := p.routes.Subtitles(pctx, SubtitlesRequest{
					Type:      stremio.typ,
					ID:        stremio.id,
					Extra:     stremio.extra,
					PickToken: pctx.PickToken,
					VideoHash: stremio.extra["videoHash"],
					VideoSize: videoSize,
					Filename:  stremio.extra["filename"],
				})
				if err != nil {
					return toRouteErrorResponse(err)
				}
				return okJSON(res, http.StatusOK)

			case "catalog":
				if p.routes.Catalog == nil {
					return errorJSON(http.StatusNotFound, "not_found", "Catalog handler is not registered.")
				}
				skip, _ := strconv.Atoi(stremio.extra["skip"])
				res, err := p.routes.Catalog(pctx, CatalogRequest{
					Type:   stremio.typ,
					ID:     stremio.id,
					Extra:  stremio.extra,
					Skip:   skip,
					Search: stremio.extra["search"],
					Genre:  stremio.extra["genre"],
				})
				if err != nil {
					return toRouteErrorResponse(err)
				}
				return okJSON(res, http.StatusOK)

			case "meta":
				if p.routes.Meta == nil {
					return errorJSON(http.StatusNotFound, "not_found", "Meta handler is not registered.")
				}
				res, err := p.routes.Meta(pctx, MetaRequest{Type: stremio.typ, ID: stremio.id})
				if err != nil {
					return toRouteErrorResponse(err)
				}
				return okJSON(res, http.StatusOK)

			case "stream":
				if p.routes.Stream == nil {
					return errorJSON(http.StatusNotFound, "not_found", "Stream handler is not registered.")
				}
				res, err := p.routes.Stream(pctx, StreamRequest{
					Type:  stremio.typ,
					ID:    stremio.id,
					Extra: stremio.extra,
				})
				if err != nil {
					return toRouteErrorResponse(err)
				}
				return okJSON(res, http.StatusOK)
			}
		}
	}

	if resp, handled := p.dispatchKinds(pctx, method, path, req); handled {
		return resp
	}

	return errorJSON(http.StatusNotFound, "not_found", "No route matched "+method+" "+path+".")
}

func containsDotDot(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// ServeHTTP implements net/http.Handler for running a Plugin as an HTTP sidecar.
func (p *Plugin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"read_error","message":"Failed to read request body."}}`))
		return
	}
	hdrs := map[string]string{}
	for k, vals := range r.Header {
		if len(vals) > 0 {
			hdrs[k] = vals[0]
		}
	}
	query := map[string]string{}
	for k, vals := range r.URL.Query() {
		if len(vals) > 0 {
			query[k] = vals[0]
		}
	}
	res := p.Dispatch(r.Context(), RawRequest{
		Method:  r.Method,
		Path:    r.URL.EscapedPath(),
		Query:   query,
		Headers: hdrs,
		Body:    bodyBytes,
	})
	for k, v := range res.Headers {
		w.Header().Set(k, v)
	}
	payload := res.Body
	if res.BinaryBody != nil {
		payload = res.BinaryBody
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	w.WriteHeader(res.Status)
	if r.Method != http.MethodHead && len(payload) > 0 {
		_, _ = w.Write(payload)
	}
}

// ServeOptions configures how Plugin.Serve runs in sidecar mode.
type ServeOptions struct {
	Mode       string
	Host       string
	Port       int
	TunnelURL  string
	Credential string
	// URL is the server's http(s) base (ETHER_URL). When set, the tunnel URL
	// and pair URL are derived from it and the SDK pairs with a code.
	URL string
	// StateDir overrides where the pairing credential is saved
	// (ETHER_PLUGIN_STATE_DIR).
	StateDir string
}

// ServeHandle represents a running HTTP or tunnel sidecar session.
type ServeHandle struct {
	// Done closes when the tunnel supervisor stops (ctx cancellation or Close).
	// Transient disconnects reconnect automatically. It is nil for HTTP servers.
	Done  <-chan struct{}
	Mode  string
	URL   string
	Port  int
	Close func() error
}

// Start launches the plugin sidecar in the background (either HTTP or WebSocket tunnel)
// and returns a ServeHandle that stops it when Close is called or ctx is cancelled.
func (p *Plugin) Start(ctx context.Context, opts ServeOptions) (*ServeHandle, error) {
	tunnelURL := opts.TunnelURL
	if tunnelURL == "" {
		tunnelURL = os.Getenv("ETHER_TUNNEL_URL")
	}
	inputs := pairingInputsFromEnv(opts)
	if tunnelURL == "" && inputs.URL != "" {
		derived, err := TunnelURL(inputs.URL)
		if err != nil {
			return nil, err
		}
		tunnelURL = derived
	}
	mode := opts.Mode
	if mode == "" {
		if tunnelURL != "" {
			mode = "tunnel"
		} else {
			mode = "http"
		}
	}
	if mode == "tunnel" {
		if tunnelURL == "" {
			return nil, fmt.Errorf("tunnel mode requires TunnelURL, ETHER_TUNNEL_URL or ETHER_URL")
		}
		credential, fromFile, err := resolveCredential(ctx, inputs, p.manifest.ID)
		if err != nil {
			return nil, err
		}
		return p.startTunnel(ctx, tunnelURL, credential, fromFile, inputs.StateDir)
	}

	host := opts.Host
	if host == "" {
		host = os.Getenv("ETHER_PLUGIN_HOST")
	}
	if host == "" {
		host = "127.0.0.1"
	}
	port := opts.Port
	if port == 0 && os.Getenv("ETHER_PLUGIN_PORT") != "" {
		port, _ = strconv.Atoi(os.Getenv("ETHER_PLUGIN_PORT"))
	}

	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port
	srv := &http.Server{Handler: p}

	go func() {
		_ = srv.Serve(ln)
	}()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	return &ServeHandle{
		Mode: "http",
		URL:  "http://" + net.JoinHostPort(host, strconv.Itoa(actualPort)),
		Port: actualPort,
		Close: func() error {
			return srv.Close()
		},
	}, nil
}

// Serve runs the plugin sidecar until ctx is cancelled.
func (p *Plugin) Serve(ctx context.Context, opts ServeOptions) error {
	h, err := p.Start(ctx, opts)
	if err != nil {
		return err
	}
	if h.Done != nil {
		select {
		case <-ctx.Done():
			return h.Close()
		case <-h.Done:
			_ = h.Close()
			if ctx.Err() != nil {
				return nil
			}
			return errors.New("tunnel disconnected")
		}
	}
	<-ctx.Done()
	return h.Close()
}
