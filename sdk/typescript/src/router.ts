import type {
  ActionRequest,
  ActionResponse,
  CandidateFacts,
  CatalogRequest,
  CatalogResponse,
  ChannelsResponse,
  CondMatchFn,
  ConfigureResponse,
  EtherEvent,
  EventResponse,
  FieldOption,
  GuideRequest,
  GuideResponse,
  HealthResponse,
  JobRequest,
  JobResponse,
  Manifest,
  MarkersRequest,
  MarkersResponse,
  MaybePromise,
  MetaRequest,
  MetaResponse,
  NotifyRequest,
  NotifyResponse,
  OpenStreamRequest,
  OpenStreamResponse,
  RankKeyFn,
  RawRequest,
  RawResponse,
  StreamRequest,
  StreamResponse,
  SubtitlesRequest,
  SubtitlesResponse,
  TestResponse,
  ToolRequest,
  ToolResponse,
  ViewRequest,
  ViewResponse,
} from "./types.ts";
import { PluginHttpError } from "./types.ts";
import {
  createKindHandlers,
  dispatchKinds,
  type CatalogProvider,
  type FrontendAdapter,
  type KindMatcher,
  type KindResolver,
  type SetupTester,
} from "./kinds.ts";
import {
  createHostedClient,
  createRemoteHostClient,
  isHostedRuntime,
  type EtherHost,
  type RemoteHostOptions,
} from "./host.ts";
import { createI18n, type I18nHelper, type StringsCatalog } from "./i18n.ts";
import { collectSettingFields, decodeSettings } from "./settings.ts";

export { PluginHttpError as PluginRouteError };

export interface FileRequest {
  path: string;
  range?: string;
  headOnly?: boolean;
}

export interface FileRangeResponse {
  status?: number;
  contentType?: string;
  contentRange?: string;
  totalSize?: number;
  headers?: Record<string, string>;
  body: Uint8Array;
}

export interface PluginContext<S = Record<string, unknown>> {
  readonly manifest: Manifest;
  readonly settings: S;
  readonly locale: string;
  readonly pickToken?: string;
  readonly requestId?: string;
  readonly headers: Record<string, string | string[]>;
  readonly query: Record<string, string>;
  readonly ether: EtherHost;
  readonly i18n: I18nHelper;
  readonly t: (key: string, params?: Record<string, string | number>) => string;
}

export interface PluginRoutes<S = Record<string, unknown>> {
  catalog?: (req: CatalogRequest, ctx: PluginContext<S>) => MaybePromise<CatalogResponse>;
  meta?: (req: MetaRequest, ctx: PluginContext<S>) => MaybePromise<MetaResponse>;
  stream?: (req: StreamRequest, ctx: PluginContext<S>) => MaybePromise<StreamResponse>;
  openStream?: (req: OpenStreamRequest, ctx: PluginContext<S>) => MaybePromise<OpenStreamResponse>;
  subtitles?: (req: SubtitlesRequest, ctx: PluginContext<S>) => MaybePromise<SubtitlesResponse>;
  subtitlesDownload?: (req: { id: string; lang: string; format: string }, ctx: PluginContext<S>) => MaybePromise<{ format: string; encoding: "utf8" | "base64"; data: string }>;
  markers?: (req: MarkersRequest, ctx: PluginContext<S>) => MaybePromise<MarkersResponse>;
  channels?: (ctx: PluginContext<S>) => MaybePromise<ChannelsResponse>;
  guide?: (req: GuideRequest, ctx: PluginContext<S>) => MaybePromise<GuideResponse>;
  configure?: (settings: S, ctx: PluginContext<S>) => MaybePromise<ConfigureResponse | void>;
  test?: (ctx: PluginContext<S>) => MaybePromise<TestResponse>;
  options?: (field: string, ctx: PluginContext<S>) => MaybePromise<FieldOption[]>;
  health?: (ctx: PluginContext<S>) => MaybePromise<HealthResponse>;
  notify?: (req: NotifyRequest, ctx: PluginContext<S>) => MaybePromise<NotifyResponse>;
  event?: (events: EtherEvent[], ctx: PluginContext<S>) => MaybePromise<EventResponse | void>;
  view?: (req: ViewRequest, ctx: PluginContext<S>) => MaybePromise<ViewResponse>;
  action?: (req: ActionRequest, ctx: PluginContext<S>) => MaybePromise<ActionResponse>;
  tool?: (req: ToolRequest, ctx: PluginContext<S>) => MaybePromise<ToolResponse>;
  job?: (req: JobRequest, ctx: PluginContext<S>) => MaybePromise<JobResponse>;
  file?: (req: FileRequest, ctx: PluginContext<S>) => MaybePromise<FileRangeResponse>;
  rankKey?: RankKeyFn<S>;
  condMatch?: CondMatchFn;
}

export interface PluginDefinition<S = Record<string, unknown>> {
  manifest: Manifest;
  routes: PluginRoutes<S>;
  defaultSettings?: Partial<S>;
  strings?: StringsCatalog;
  defaultLocale?: string;
  host?: EtherHost;
  remoteHostOptions?: RemoteHostOptions;
}

export interface PluginInstance<S = Record<string, unknown>> {
  readonly manifest: Manifest;
  readonly routes: PluginRoutes<S>;
  getSettings(): S;
  setSettings(incoming: Record<string, unknown>): { settings: S; errors: Record<string, string> };
  handle(req: RawRequest): MaybePromise<RawResponse>;
  dispatch(req: RawRequest): Promise<RawResponse>;
  rank_key(candidate: CandidateFacts, config?: S): number;
  cond_match(candidate: CandidateFacts, operator: string, value: unknown): boolean;
  onCatalog(kind: string, provider: CatalogProvider<S>): void;
  onResolve(kind: string, resolver: KindResolver<S>): void;
  onMatch(kind: string, matcher: KindMatcher<S>): void;
  onFrontend(key: string, adapter: FrontendAdapter): void;
  onSetupTest(tester: SetupTester<S>): void;
}

function getHeader(
  headers: Record<string, string | string[]> | undefined,
  name: string,
): string | undefined {
  if (!headers) return undefined;
  const target = name.toLowerCase();
  for (const [k, v] of Object.entries(headers)) {
    if (k.toLowerCase() === target) {
      return Array.isArray(v) ? v[0] : v;
    }
  }
  return undefined;
}

function parseUrlPathAndQuery(
  rawPath: string,
  rawQuery?: Record<string, string | string[]>,
): { path: string; query: Record<string, string> } {
  let pathOnly = rawPath || "/";
  const query: Record<string, string> = {};
  if (rawQuery) {
    for (const [k, v] of Object.entries(rawQuery)) {
      if (Array.isArray(v)) {
        if (v.length > 0) query[k] = String(v[0]);
      } else if (v !== undefined && v !== null) {
        query[k] = String(v);
      }
    }
  }
  const qIndex = pathOnly.indexOf("?");
  if (qIndex >= 0) {
    const qs = pathOnly.slice(qIndex + 1);
    pathOnly = pathOnly.slice(0, qIndex) || "/";
    const params = parseParams(qs);
    Object.entries(params).forEach(([k, v]) => {
      if (query[k] === undefined) {
        query[k] = v;
      }
    });
  }
  if (!pathOnly.startsWith("/")) {
    pathOnly = "/" + pathOnly;
  }
  return { path: pathOnly, query };
}

function parseJsonBody(body: unknown): Record<string, unknown> {
  if (body === undefined || body === null || body === "") {
    return {};
  }
  if (typeof body === "string") {
    const trimmed = body.trim();
    if (!trimmed) return {};
    const parsed = JSON.parse(trimmed);
    if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
      return parsed as Record<string, unknown>;
    }
    return {};
  }
  if (typeof body === "object" && !Array.isArray(body)) {
    return body as Record<string, unknown>;
  }
  return {};
}

function okResponse(body: unknown, headers?: Record<string, string>, status = 200): RawResponse {
  return {
    status,
    headers: {
      "Content-Type": "application/json",
      ...(headers ?? {}),
    },
    body,
  };
}

function errorResponse(status: number, code: string, message: string): RawResponse {
  return {
    status,
    headers: { "Content-Type": "application/json" },
    body: { error: { code, message } },
  };
}

function isEtherHostFault(err: unknown): boolean {
  if (!err) return false;
  const msg = err instanceof Error ? err.message : String(err);
  return msg.includes("__ETHER_FAULT__");
}

interface ParsedStremioRoute {
  resource: "catalog" | "meta" | "stream" | "subtitles";
  type: string;
  id: string;
  extra: Record<string, string>;
}

function parseParams(input: string): Record<string, string> {
  const result: Record<string, string> = {};
  for (const part of input.split("&")) {
    if (!part) continue;
    const index = part.indexOf("=");
    const decode = (value: string) => decodeURIComponent(value.replace(/\+/g, " "));
    result[decode(index < 0 ? part : part.slice(0, index))] = decode(index < 0 ? "" : part.slice(index + 1));
  }
  return result;
}

function parseExtraSegment(rawExtra: string | undefined): Record<string, string> {
  const out: Record<string, string> = {};
  if (!rawExtra) return out;
  const params = parseParams(rawExtra);
  Object.entries(params).forEach(([k, v]) => {
    out[k] = v;
  });
  return out;
}

function matchStremioRoute(path: string, query: Record<string, string>): ParsedStremioRoute | null {
  if (!path.endsWith(".json")) return null;
  const trimmed = path.slice(1, -5);
  const parts = trimmed.split("/");
  if (parts.length < 3 || parts.length > 4) return null;
  const resource = parts[0];
  if (
    resource !== "catalog" &&
    resource !== "meta" &&
    resource !== "stream" &&
    resource !== "subtitles"
  ) {
    return null;
  }
  const type = decodeURIComponent(parts[1] ?? "");
  const id = decodeURIComponent(parts[2] ?? "");
  const extra = parseExtraSegment(parts[3]);
  for (const [k, v] of Object.entries(query)) {
    if (extra[k] === undefined) {
      extra[k] = v;
    }
  }
  return { resource, type, id, extra };
}

function buildInlineStrings(manifest: Manifest, explicit?: StringsCatalog): StringsCatalog {
  const catalog: StringsCatalog = {};
  if (manifest.ether?.strings) {
    for (const [loc, val] of Object.entries(manifest.ether.strings)) {
      if (val && typeof val === "object") {
        catalog[loc] = { ...(val as Record<string, string>) };
      }
    }
  }
  if (explicit) {
    for (const [loc, table] of Object.entries(explicit)) {
      catalog[loc] = { ...(catalog[loc] ?? {}), ...table };
    }
  }
  return catalog;
}

export function plugin<S = Record<string, unknown>>(
  def: PluginDefinition<S>,
): PluginInstance<S> {
  if (!def || !def.manifest) {
    throw new Error("Plugin manifest is required.");
  }
  const manifest = def.manifest;
  if (!manifest.id || !manifest.name || !manifest.version) {
    throw new Error("Plugin manifest requires id, name, and version.");
  }

  const routes = def.routes ?? {};
  const kindHandlers = createKindHandlers<S>();
  const i18n = createI18n(
    buildInlineStrings(manifest, def.strings),
    def.defaultLocale ?? "en",
  );
  const settingsSchema = manifest.ether?.settings;
  let currentSettings: S = decodeSettings<S>(
    settingsSchema,
    undefined,
    def.defaultSettings,
  ).settings;

  const pageRoutes = new Map<string, string>();
  if (manifest.ether?.contributes?.pages) {
    for (const p of manifest.ether.contributes.pages) {
      if (p.route) {
        pageRoutes.set(p.route, p.key);
      }
    }
  }
  if (manifest.ether?.contributes?.slots) {
    for (const s of manifest.ether.contributes.slots) {
      if (s.route) {
        pageRoutes.set(s.route, s.key);
      }
    }
  }

  let settingsFailureLogged = false;
  function buildContext(req: RawRequest): MaybePromise<PluginContext<S>> {
    const { query } = parseUrlPathAndQuery(req.path, req.query);
    const localeHeader =
      req.locale ??
      query.locale ??
      getHeader(req.headers, "X-Ether-Locale") ??
      getHeader(req.headers, "Accept-Language") ??
      def.defaultLocale ??
      "en";
    const locale = localeHeader.split(",")[0]?.trim() || "en";
    const pickToken = getHeader(req.headers, "X-Ether-Pick");
    const requestId = getHeader(req.headers, "X-Ether-Request-ID");
    const hostClient =
      def.host ??
      (isHostedRuntime()
        ? createHostedClient()
        : createRemoteHostClient(def.remoteHostOptions));
    const makeContext = (settings: S): PluginContext<S> => ({
      manifest,
      settings,
      locale,
      pickToken,
      requestId,
      headers: req.headers ?? {},
      query,
      ether: hostClient,
      i18n,
      t: (key, params) => i18n.t(key, params, locale),
    });
    if (!hostClient.settings || req.path === "/ether/configure") return makeContext(currentSettings);
    const fallback = () => {
      if (!settingsFailureLogged) { console.warn("plugin settings lookup failed"); settingsFailureLogged = true; }
      return makeContext({} as S);
    };
    let fresh: MaybePromise<Record<string, unknown>>;
    try { fresh = hostClient.settings.get(); } catch { return fallback(); }
    const resolve = (v: Record<string, unknown>) => {
      const merged = { ...v };
      for (const field of collectSettingFields(settingsSchema)) {
        if (field.kind === "secret" && (currentSettings as Record<string, unknown>)[field.key] !== undefined) merged[field.key] = (currentSettings as Record<string, unknown>)[field.key];
      }
      return makeContext(decodeSettings<S>(settingsSchema, merged, def.defaultSettings).settings);
    };
    return fresh instanceof Promise ? fresh.then(resolve, fallback) : resolve(fresh);
  }

  function mapResult<T>(
    value: MaybePromise<T>,
    transform: (resolved: T) => RawResponse = (r) => okResponse(r),
  ): MaybePromise<RawResponse> {
    if (value instanceof Promise) {
      return value.then(transform);
    }
    return transform(value);
  }

  function dispatchCore(req: RawRequest, ctx: PluginContext<S>): MaybePromise<RawResponse> {
    const method = (req.method || "GET").toUpperCase();
    const { path, query } = parseUrlPathAndQuery(req.path, req.query);

    if (method === "GET" && path === "/manifest.json") {
      return okResponse(manifest);
    }

    if (method === "GET" && path === "/ether/health") {
      if (routes.health) {
        return mapResult(routes.health(ctx));
      }
      const fallback: HealthResponse = { status: "ok" };
      return okResponse(fallback);
    }

    if (method === "POST" && path === "/ether/configure") {
      let bodyObj: Record<string, unknown>;
      try {
        bodyObj = parseJsonBody(req.body);
      } catch {
        return errorResponse(400, "invalid_json", "Request body must be valid JSON.");
      }
      const incoming =
        bodyObj.settings && typeof bodyObj.settings === "object" && !Array.isArray(bodyObj.settings)
          ? (bodyObj.settings as Record<string, unknown>)
          : bodyObj;
      const decoded = decodeSettings<S>(settingsSchema, incoming, def.defaultSettings);
      if (Object.keys(decoded.errors).length > 0) {
        return okResponse({ errors: decoded.errors });
      }
      if (routes.configure) {
        const nextCtx: PluginContext<S> = { ...ctx, settings: decoded.settings };
        return mapResult(routes.configure(decoded.settings, nextCtx), (res) => {
          const errors = res?.errors ?? {};
          if (Object.keys(errors).length === 0) {
            currentSettings = decoded.settings;
          }
          return okResponse({ errors });
        });
      }
      currentSettings = decoded.settings;
      return okResponse({ errors: {} });
    }

    if (method === "POST" && path === "/ether/subtitles/download") {
      if (!ctx.pickToken?.trim()) return errorResponse(403, "pick_required", "A subtitle pick is required.");
      if (!routes.subtitlesDownload) return errorResponse(404, "not_found", "Subtitle download handler is not registered.");
      const body = parseJsonBody(req.body);
      return mapResult(routes.subtitlesDownload({ id: String(body.id || ""), lang: String(body.lang || ""), format: String(body.format || "") }, ctx));
    }

    if (method === "POST" && path === "/ether/test") {
      if (routes.test) {
        return mapResult(routes.test(ctx));
      }
      const fallback: TestResponse = { ok: true };
      return okResponse(fallback);
    }

    if (method === "GET" && path.startsWith("/ether/options/")) {
      const field = decodeURIComponent(path.slice("/ether/options/".length));
      if (!routes.options) {
        return okResponse([]);
      }
      return mapResult(routes.options(field, ctx));
    }

    if (method === "POST" && path === "/ether/stream/open") {
      if (!routes.openStream) {
        return errorResponse(404, "not_found", "Stream open handler is not registered.");
      }
      const bodyObj = parseJsonBody(req.body);
      return mapResult(
        routes.openStream({ streamId: String(bodyObj.streamId ?? "") }, ctx),
      );
    }

    if (method === "GET" && path.startsWith("/ether/markers/") && path.endsWith(".json")) {
      if (!routes.markers) {
        return errorResponse(404, "not_found", "Markers handler is not registered.");
      }
      const trimmed = path.slice("/ether/markers/".length, -5);
      const parts = trimmed.split("/");
      if (parts.length < 2) {
        return errorResponse(400, "invalid_path", "Markers route requires type and id.");
      }
      return mapResult(
        routes.markers(
          {
            type: decodeURIComponent(parts[0] ?? ""),
            id: decodeURIComponent(parts.slice(1).join("/")),
          },
          ctx,
        ),
      );
    }

    if (method === "GET" && path === "/ether/channels.json") {
      if (!routes.channels) {
        return errorResponse(404, "not_found", "Channels handler is not registered.");
      }
      return mapResult(routes.channels(ctx));
    }

    if (method === "GET" && path.startsWith("/ether/guide/") && path.endsWith(".json")) {
      if (!routes.guide) {
        return errorResponse(404, "not_found", "Guide handler is not registered.");
      }
      const channelId = decodeURIComponent(path.slice("/ether/guide/".length, -5));
      return mapResult(
        routes.guide({ channelId, from: query.from, to: query.to }, ctx),
      );
    }

    if (method === "POST" && path === "/ether/notify") {
      if (!routes.notify) {
        return errorResponse(404, "not_found", "Notify handler is not registered.");
      }
      const bodyObj = parseJsonBody(req.body);
      return mapResult(
        routes.notify(
          {
            title: typeof bodyObj.title === "string" ? bodyObj.title : undefined,
            message: String(bodyObj.message ?? bodyObj.text ?? ""),
            event: typeof bodyObj.event === "string" ? bodyObj.event : undefined,
            settings:
              bodyObj.settings && typeof bodyObj.settings === "object"
                ? (bodyObj.settings as Record<string, unknown>)
                : undefined,
          },
          ctx,
        ),
      );
    }

    if (method === "POST" && path === "/ether/event") {
      if (!routes.event) {
        return errorResponse(404, "not_found", "Event handler is not registered.");
      }
      const bodyObj = parseJsonBody(req.body);
      const events = Array.isArray(bodyObj.events) ? (bodyObj.events as EtherEvent[]) : [];
      return mapResult(routes.event(events, ctx), (res) => {
        if (res && Array.isArray(res.ack)) {
          return okResponse({ ack: res.ack });
        }
        return okResponse({ ack: events.map((e) => e.id) });
      });
    }

    if (
      method === "GET" &&
      (path === "/ether/view" || path.startsWith("/ether/view/") || pageRoutes.has(path))
    ) {
      if (!routes.view) {
        return errorResponse(404, "not_found", "View handler is not registered.");
      }
      let page = pageRoutes.get(path);
      if (!page) {
        page =
          path === "/ether/view" ? "home" : decodeURIComponent(path.slice("/ether/view/".length));
      }
      return mapResult(routes.view({ page, query }, ctx));
    }

    if (method === "POST" && path.startsWith("/ether/action/")) {
      if (!routes.action) {
        return errorResponse(404, "not_found", "Action handler is not registered.");
      }
      const action = decodeURIComponent(path.slice("/ether/action/".length));
      const input = parseJsonBody(req.body);
      return mapResult(routes.action({ action, input }, ctx));
    }

    if (method === "POST" && path.startsWith("/ether/tool/")) {
      if (!routes.tool) {
        return errorResponse(404, "not_found", "Tool handler is not registered.");
      }
      const name = decodeURIComponent(path.slice("/ether/tool/".length));
      const input = parseJsonBody(req.body);
      return mapResult(routes.tool({ name, input }, ctx));
    }

    if (method === "POST" && path.startsWith("/ether/job/")) {
      if (!routes.job) {
        return errorResponse(404, "not_found", "Job handler is not registered.");
      }
      const name = decodeURIComponent(path.slice("/ether/job/".length));
      return mapResult(routes.job({ name, continuation: parseJsonBody(req.body).continuation }, ctx));
    }

    if (method === "POST" && path === "/ether/rank-key") {
      if (!routes.rankKey) {
        return errorResponse(404, "not_found", "Rank key handler is not registered.");
      }
      const bodyObj = parseJsonBody(req.body);
      const candidate = (bodyObj.candidate ?? bodyObj) as unknown as CandidateFacts;
      const cfg = (bodyObj.config as S | undefined) ?? currentSettings;
      const score = Number(routes.rankKey(candidate, cfg)) || 0;
      return okResponse({ score });
    }

    if (method === "POST" && path === "/ether/cond-match") {
      if (!routes.condMatch) {
        return errorResponse(404, "not_found", "Condition match handler is not registered.");
      }
      const bodyObj = parseJsonBody(req.body);
      const candidate = (bodyObj.candidate ?? bodyObj) as unknown as CandidateFacts;
      const operator = String(bodyObj.operator ?? "eq");
      const value = bodyObj.value;
      const matched = Boolean(routes.condMatch(candidate, operator, value));
      return okResponse({ match: matched });
    }

    if ((method === "GET" || method === "HEAD") && path.startsWith("/ether/file/")) {
      if (!routes.file) {
        return errorResponse(404, "not_found", "File handler is not registered.");
      }
      const relPath = decodeURIComponent(path.slice("/ether/file/".length));
      if (!relPath || relPath.startsWith("/") || relPath.split("/").includes("..")) {
        return errorResponse(400, "invalid_path", "File path must be a clean relative path.");
      }
      const rangeHeader = getHeader(req.headers, "Range");
      return mapResult(
        routes.file({ path: relPath, range: rangeHeader, headOnly: method === "HEAD" }, ctx),
        (fileRes) => {
          const headers: Record<string, string> = {
            "Content-Type": fileRes.contentType || "application/octet-stream",
            "Accept-Ranges": "bytes",
            ...(fileRes.headers ?? {}),
          };
          if (fileRes.contentRange) {
            headers["Content-Range"] = fileRes.contentRange;
          }
          if (fileRes.totalSize !== undefined && !fileRes.contentRange) {
            headers["Content-Length"] = String(fileRes.totalSize);
          }
          return {
            status: fileRes.status ?? (fileRes.contentRange ? 206 : 200),
            headers,
            body: method === "HEAD" ? new Uint8Array(0) : fileRes.body,
          };
        },
      );
    }

    if (method === "GET") {
      const stremio = matchStremioRoute(path, query);
      if (stremio) {
        if (stremio.resource === "subtitles") {
          if (!routes.subtitles) {
            return errorResponse(404, "not_found", "Subtitles handler is not registered.");
          }
          const videoSize = stremio.extra.videoSize ? Number(stremio.extra.videoSize) : undefined;
          return mapResult(
            routes.subtitles(
              {
                type: stremio.type,
                id: stremio.id,
                extra: stremio.extra,
                pickToken: ctx.pickToken,
                videoHash: stremio.extra.videoHash,
                videoSize: Number.isFinite(videoSize) ? videoSize : undefined,
                filename: stremio.extra.filename,
              },
              ctx,
            ),
          );
        }

        if (stremio.resource === "catalog") {
          if (!routes.catalog) {
            return errorResponse(404, "not_found", "Catalog handler is not registered.");
          }
          const skip = stremio.extra.skip ? Number(stremio.extra.skip) || 0 : 0;
          return mapResult(
            routes.catalog(
              {
                type: stremio.type,
                id: stremio.id,
                extra: stremio.extra,
                skip,
                search: stremio.extra.search,
                genre: stremio.extra.genre,
              },
              ctx,
            ),
          );
        }

        if (stremio.resource === "meta") {
          if (!routes.meta) {
            return errorResponse(404, "not_found", "Meta handler is not registered.");
          }
          return mapResult(routes.meta({ type: stremio.type, id: stremio.id }, ctx));
        }

        if (stremio.resource === "stream") {
          if (!routes.stream) {
            return errorResponse(404, "not_found", "Stream handler is not registered.");
          }
          return mapResult(
            routes.stream({ type: stremio.type, id: stremio.id, extra: stremio.extra }, ctx),
          );
        }
      }
    }

    if (path.startsWith("/ether/v1/")) {
      return dispatchKinds(kindHandlers, req, ctx).then(
        (r) => r ?? errorResponse(404, "not_found", `No route matched ${method} ${path}.`),
      );
    }

    return errorResponse(404, "not_found", `No route matched ${method} ${path}.`);
  }

  function handleRouteError(err: unknown): RawResponse {
    if (isEtherHostFault(err)) {
      throw err;
    }
    if (err instanceof PluginHttpError) {
      return errorResponse(err.status, err.code, err.message);
    }
    if (
      err &&
      typeof err === "object" &&
      "status" in err &&
      typeof (err as { status: unknown }).status === "number"
    ) {
      const e = err as { status: number; code?: string; message?: string };
      return errorResponse(e.status, e.code || "plugin_error", e.message || "Request failed.");
    }
    const message = err instanceof Error ? err.message : String(err);
    return errorResponse(500, "internal_error", message || "Internal plugin error.");
  }

  function handleRequest(req: RawRequest): MaybePromise<RawResponse> {
    try {
      const ctx = buildContext(req);
      const res = ctx instanceof Promise ? ctx.then(c => dispatchCore(req, c)) : dispatchCore(req, ctx);
      return res instanceof Promise ? res.catch(handleRouteError) : res;
    } catch (err) {
      return handleRouteError(err);
    }
  }

  async function dispatch(req: RawRequest): Promise<RawResponse> {
    try {
      const ctx = await buildContext(req);
      return await dispatchCore(req, ctx);
    } catch (err) {
      return handleRouteError(err);
    }
  }

  function rankKey(candidate: CandidateFacts, config?: S): number {
    if (!routes.rankKey) return 0;
    return Number(routes.rankKey(candidate, config ?? currentSettings)) || 0;
  }

  function condMatch(candidate: CandidateFacts, operator: string, value: unknown): boolean {
    if (!routes.condMatch) return false;
    return Boolean(routes.condMatch(candidate, operator, value));
  }

  const instance: PluginInstance<S> = {
    manifest,
    routes,
    getSettings: () => currentSettings,
    setSettings: (incoming) => {
      const decoded = decodeSettings<S>(settingsSchema, incoming, def.defaultSettings);
      if (Object.keys(decoded.errors).length === 0) {
        currentSettings = decoded.settings;
      }
      return decoded;
    },
    handle: handleRequest,
    dispatch,
    rank_key: rankKey,
    cond_match: condMatch,
    onCatalog: (kind, provider) => { kindHandlers.catalog.set(kind, provider); },
    onResolve: (kind, resolver) => { kindHandlers.resolve.set(kind, resolver); },
    onMatch: (kind, matcher) => { kindHandlers.match.set(kind, matcher); },
    onFrontend: (key, adapter) => { kindHandlers.frontend.set(key, adapter); },
    onSetupTest: (tester) => { kindHandlers.setup = tester; },
  };

  if (isHostedRuntime()) {
    const g = globalThis as Record<string, unknown>;
    g.handle = handleRequest;
    g.rank_key = rankKey;
    g.cond_match = condMatch;
  }

  return instance;
}
