import { t, tn } from "../i18n";
import {
  formatBytes,
  formatDate,
  formatNumber,
  isPlainObject,
  parseTime,
  relativeTime,
} from "./format";

/** "unknown" means unreported or unverified, not necessarily still loading. */
export type HealthLevel = "ok" | "warn" | "error" | "off" | "unknown";

export interface HealthItem {
  id: "usenet" | "indexers" | "cache" | "catalog" | "remote" | "plexdirect";
  label: string;
  level: HealthLevel;
  summary: string;
  details: string[];
  /** Explain a known limitation without presenting it as a running check. */
  statusLabel?: string;
  note?: string;
  /** Where the owner fixes this, for "Needs attention" and health links. */
  href: string;
  /** 0..1 gauge when the item has a natural fill level. */
  fill?: number;
  /**
   * Unverified diagnostics set this false. Failures still count, and an
   * unreported "unknown" still keeps the server Incomplete. This never means
   * the row was checked and found healthy.
   */
  countsOverall?: boolean;
}

const RANK: Record<HealthLevel, number> = { error: 4, warn: 3, unknown: 2, ok: 1, off: 0 };

export function worstLevel(levels: HealthLevel[]): HealthLevel {
  let worst: HealthLevel = "ok";
  for (const l of levels) if (RANK[l] > RANK[worst]) worst = l;
  return worst;
}

/** Warn and error always count. Only an explicit unverified unknown may opt out. */
export function countsOverall(item: HealthItem): boolean {
  if (item.level !== "unknown") return true;
  return item.countsOverall !== false;
}

/** Overall rank after unverified diagnostics have opted out. */
export function overallLevel(items: HealthItem[]): HealthLevel {
  return worstLevel(items.filter(countsOverall).map((item) => item.level));
}

/** An installed certificate opted out because trust was not checked. */
export function hasExemptDiagnostic(items: HealthItem[]): boolean {
  return items.some((item) => item.countsOverall === false);
}

const num = (v: unknown): number =>
  typeof v === "number" && Number.isFinite(v) ? v : Number(v) || 0;
const str = (v: unknown): string => (typeof v === "string" ? v : "");

function errorOf(v: unknown): string {
  return isPlainObject(v) && typeof v.error === "string" ? v.error : "";
}

export function usenetHealth(v: unknown, debrid?: unknown): HealthItem {
  const base = {
    id: "usenet" as const,
    label: "Usenet",
    href: "/sources/usenet",
  };
  const err = errorOf(v);
  if (err) return { ...base, level: "error", summary: err, details: [] };
  if (!Array.isArray(v))
    return { ...base, level: "unknown", summary: t("Not reported"), details: [] };
  const providers = v.filter(isPlainObject);
  if (providers.length === 0)
    return debridOn(debrid)
      ? { ...base, level: "off", summary: t("Not used · streaming through debrid"), details: [] }
      : { ...base, level: "error", summary: t("No providers configured"), details: [] };
  const primary = providers.filter((p) => !p.backfill);
  const bad = providers.filter((p) => str(p.state) && str(p.state) !== "ok");
  const primaryBad = primary.filter((p) => str(p.state) && str(p.state) !== "ok");
  const active = providers.reduce((n, p) => n + num(p.activeConnections), 0);
  const configured = providers.reduce(
    (n, p) => n + num(p.configuredConnections ?? p.maxConnections),
    0,
  );
  const details = providers.map((p) => {
    const values = {
      name: str(p.name) || str(p.host) || str(p.id),
      state: str(p.state) || t("unknown"),
      active: num(p.activeConnections),
      max: num(p.configuredConnections ?? p.maxConnections),
    };
    return p.backfill
      ? t("{name}: {state} (backfill) · {active}/{max} connections", values)
      : t("{name}: {state} · {active}/{max} connections", values);
  });
  let level: HealthLevel = "ok";
  if (primary.length > 0 && primaryBad.length === primary.length) level = "error";
  else if (bad.length > 0) level = "warn";
  const summary =
    level === "ok"
      ? tn(
          providers.length,
          "{count} provider healthy · {active}/{configured} connections busy",
          "{count} providers healthy · {active}/{configured} connections busy",
          { active, configured },
        )
      : tn(
          providers.length,
          "{bad} of {count} provider degraded",
          "{bad} of {count} providers degraded",
          { bad: bad.length },
        );
  return {
    ...base,
    level,
    summary,
    details,
    fill: configured > 0 ? active / configured : undefined,
  };
}

/** A debrid account is enabled (from the "debrid" status section). */
function debridOn(v: unknown): boolean {
  return isPlainObject(v) && v.enabled === true;
}

/**
 * Indexer budgets from the "indexers" status section. With the "debrid"
 * section it also warns when debrid is on but no enabled indexer returns
 * torrents: every play would then fail with nothing to stream.
 */
export function indexersHealth(v: unknown, debrid?: unknown): HealthItem {
  const base = {
    id: "indexers" as const,
    label: t("Indexers"),
    href: "/sources/indexers",
  };
  const err = errorOf(v);
  if (err) return { ...base, level: "error", summary: err, details: [] };
  if (!Array.isArray(v))
    return { ...base, level: "unknown", summary: t("Not reported"), details: [] };
  const rows = v.filter(isPlainObject);
  if (rows.length === 0)
    return { ...base, level: "error", summary: t("No indexers configured"), details: [] };
  const enabled = rows.filter((r) => r.enabled !== false);
  if (enabled.length === 0)
    return {
      ...base,
      level: "warn",
      summary: t("No indexers enabled"),
      details: [t("Ether has nothing to search until an indexer is enabled.")],
    };
  const noTorrent = debridOn(debrid) && !enabled.some((r) => r.torrent === true);
  let exhausted = 0;
  let remaining = 0;
  let limited = 0;
  let used = 0;
  let limit = 0;
  const details = rows.map((r) => {
    const hl = num(r.hitsLimit);
    const hr = num(r.hitsRemaining);
    if (hl > 0) {
      limited++;
      remaining += hr;
      used += num(r.hitsUsed);
      limit += hl;
      if (hr <= 0) exhausted++;
    }
    const parts = [
      hl > 0 ? t("{left}/{limit} searches left", { left: hr, limit: hl }) : t("unlimited searches"),
    ];
    if (num(r.grabsLimit) > 0)
      parts.push(
        t("{left}/{limit} grabs left", { left: num(r.grabsRemaining), limit: num(r.grabsLimit) }),
      );
    return `${str(r.indexer) || str(r.name)}: ${parts.join(" · ")}`;
  });
  let level: HealthLevel = "ok";
  if (exhausted > 0 && exhausted === rows.length) level = "error";
  else if (exhausted > 0) level = "warn";
  let summary =
    limited === 0
      ? tn(rows.length, "{count} indexer · no daily limits", "{count} indexers · no daily limits")
      : exhausted > 0
        ? tn(
            rows.length,
            "{exhausted} of {count} indexer out of searches today",
            "{exhausted} of {count} indexers out of searches today",
            { exhausted },
          )
        : tn(
            rows.length,
            "{count} indexer · {remaining} searches left today",
            "{count} indexers · {remaining} searches left today",
            { remaining },
          );
  if (noTorrent) {
    details.unshift(
      t(
        "Debrid streams torrents, but no enabled indexer returns them. Add a Stremio addon or Torznab indexer.",
      ),
    );
    if (level !== "error") {
      level = "warn";
      summary = t("No torrent indexer for debrid");
    }
  }
  return { ...base, level, summary, details, fill: limit > 0 ? used / limit : undefined };
}

export function cacheHealth(v: unknown): HealthItem {
  const base = { id: "cache" as const, label: t("Cache"), href: "/cache" };
  if (!isPlainObject(v))
    return { ...base, level: "unknown", summary: t("Not reported"), details: [] };
  const used = num(v.used ?? v.usedBytes);
  const budget = num(v.budget ?? v.budgetBytes);
  if (v.mode === "off")
    return { ...base, level: "off", summary: t("Persistent media bypassed"), details: [] };
  if (budget <= 0)
    return {
      ...base,
      level: "ok",
      summary: t("{used} · unlimited budget", { used: formatBytes(used) }),
      details: [],
    };
  const fill = Math.min(1, used / budget);
  return {
    ...base,
    level: "ok",
    fill,
    summary: t("{used} of {budget}", { used: formatBytes(used), budget: formatBytes(budget) }),
    details: [t("Older segments are evicted automatically when the budget is reached.")],
  };
}

export function catalogHealth(v: unknown, now = new Date()): HealthItem {
  const base = {
    id: "catalog" as const,
    label: t("Catalog"),
    href: "/catalog",
  };
  const err = errorOf(v);
  if (err) return { ...base, level: "error", summary: err, details: [] };
  if (!isPlainObject(v))
    return { ...base, level: "unknown", summary: t("Not reported"), details: [] };
  const movies = num(v.browsableMovies ?? v.movies);
  const shows = num(v.browsableShows ?? v.shows);
  const sync = isPlainObject(v.sync) ? v.sync : {};
  const last = parseTime(v.lastSync);
  const details = [
    t("{movies} movies · {shows} shows browsable", {
      movies: formatNumber(movies),
      shows: formatNumber(shows),
    }),
    t("Last sync {when}", { when: relativeTime(last, now) }),
  ];
  if (typeof sync.error === "string" && sync.error)
    details.push(t("Last sync error: {error}", { error: sync.error }));
  if (sync.running === true) {
    return {
      ...base,
      level: "ok",
      summary: t("Syncing… {count} titles seen", { count: formatNumber(num(sync.titlesSeen)) }),
      details,
    };
  }
  if (!last) return { ...base, level: "warn", summary: t("Not synced yet"), details };
  const ageDays = (now.getTime() - last.getTime()) / 86400000;
  const level: HealthLevel =
    typeof sync.error === "string" && sync.error ? "warn" : ageDays > 3 ? "warn" : "ok";
  return {
    ...base,
    level,
    summary: t("{count} titles · synced {when}", {
      count: formatNumber(movies + shows),
      when: relativeTime(last, now),
    }),
    details,
  };
}

export function remoteHealth(v: unknown): HealthItem {
  const base = {
    id: "remote" as const,
    label: t("Remote access"),
    href: "/apps/plex",
  };
  if (!isPlainObject(v))
    return { ...base, level: "unknown", summary: t("Not reported"), details: [] };
  if (v.claimed === false)
    return {
      ...base,
      level: "error",
      summary: t("Server is not claimed"),
      details: [t("Finish the setup wizard to link Ether to your Plex account.")],
    };
  const presence = isPlainObject(v.presence) ? v.presence : {};
  const details: string[] = [];
  if (str(v.ownerUsername)) details.push(t("Claimed by {name}", { name: str(v.ownerUsername) }));
  if (str(v.publicAddress))
    details.push(t("Public address {address}", { address: str(v.publicAddress) }));
  details.push(presence.connected ? t("Connected to plex.tv") : t("Not connected to plex.tv"));
  if (v.listed === false) details.push(t("Not yet published to plex.tv"));
  if (v.tokenRejected === true)
    return {
      ...base,
      level: "error",
      summary: t("plex.tv rejects this server's sign-in"),
      details: [
        ...details,
        t(
          "If this server was claimed by another Plex account before, remove it from that account under Authorized Devices on plex.tv, then sign out and claim it again here.",
        ),
      ],
    };
  if (str(v.lastPublishError))
    return {
      ...base,
      level: "warn",
      summary: t("Publishing to plex.tv is failing"),
      details: [...details, str(v.lastPublishError)],
    };
  if (str(v.friendSyncError))
    return {
      ...base,
      level: "warn",
      summary: t("Shared users are not syncing"),
      details: [...details, str(v.friendSyncError)],
    };
  if (presence.connected === false)
    return { ...base, level: "warn", summary: t("plex.tv connection lost"), details };
  // remoteAccess is the saved setting (registrar refreshConfigStatus), not a
  // reachability result. plexReachable is only plex.tv's last notifyConnectivity
  // verdict and is not tied to a particular published route.
  const customPublished = publishedCustomURIs(v);
  for (const address of customPublished) {
    details.push(t("Published custom connection {address}", { address }));
  }
  const publicationNote = t(
    "A published address is not proof that clients outside your network can connect.",
  );
  if (customPublished.length > 0) details.push(publicationNote);
  const lastCheck =
    v.plexReachable === true
      ? t("plex.tv's last connectivity check reported the server reachable.")
      : v.plexReachable === false
        ? t("plex.tv's last connectivity check reported the server unreachable.")
        : "";
  if (v.remoteAccess === true) {
    const upnp = isPlainObject(v.upnp) ? v.upnp : {};
    if (upnp.active === true) details.push(t("Port mapped with UPnP"));
    if (v.plexReachable === false) {
      return {
        ...base,
        level: "warn",
        summary: lastCheck,
        details: [t("Direct remote access enabled"), ...details],
        note: customPublished.length > 0 ? publicationNote : undefined,
      };
    }
    if (lastCheck) details.push(lastCheck);
    return {
      ...base,
      level: "ok",
      summary: t("Direct remote access enabled"),
      details,
      statusLabel: t("Configured"),
      note: customPublished.length > 0 ? publicationNote : undefined,
    };
  }
  if (lastCheck) details.push(lastCheck);
  if (customPublished.length > 0) {
    return {
      ...base,
      level: "off",
      summary: t("Custom connection published"),
      details,
      statusLabel: t("Published"),
      note: publicationNote,
    };
  }
  return { ...base, level: "off", summary: t("Local network only"), details };
}

/** Custom routes plex.tv was actually given: Connection.Custom whose URI was published. */
function publishedCustomURIs(v: Record<string, unknown>): string[] {
  if (!Array.isArray(v.publishedUris) || !Array.isArray(v.connections)) return [];
  const published = new Set(
    v.publishedUris.filter((uri): uri is string => typeof uri === "string" && uri !== ""),
  );
  const out: string[] = [];
  for (const connection of v.connections) {
    if (!isPlainObject(connection) || connection.custom !== true) continue;
    const uri = typeof connection.uri === "string" ? connection.uri : "";
    if (uri && published.has(uri) && !out.includes(uri)) out.push(uri);
  }
  return out;
}

export function plexDirectHealth(v: unknown, now = new Date()): HealthItem {
  const base = {
    id: "plexdirect" as const,
    label: "plex.direct",
    href: "/apps/plex",
  };
  if (!isPlainObject(v))
    return { ...base, level: "unknown", summary: t("Not reported"), details: [] };
  const cert = isPlainObject(v.cert) ? v.cert : null;
  if (!cert || cert.present !== true) {
    return {
      ...base,
      level: "warn",
      summary: t("No certificate yet"),
      details: [
        t("Secure connections fall back to plain HTTP until a plex.direct certificate is issued."),
      ],
    };
  }
  const starts = parseTime(cert.notBefore);
  const expires = parseTime(cert.notAfter);
  const details = [str(cert.subject)].filter(Boolean);
  if (cert.custom === true) details.push(t("Custom certificate"));
  if (str(v.certError)) {
    details.push(str(v.certError));
    return { ...base, level: "warn", summary: t("Certificate check failed"), details };
  }
  if (cert.reissueRequired === true) {
    return { ...base, level: "warn", summary: t("Certificate reissue pending"), details };
  }
  if (cert.selfSigned === true) {
    details.push(t("Plex apps do not normally trust a self-signed certificate."));
    return { ...base, level: "warn", summary: t("Self-signed certificate"), details };
  }
  if (expires) details.push(t("Expires {date}", { date: formatDate(expires) }));
  if (expires && (expires.getTime() - now.getTime()) / 86400000 < 0)
    return { ...base, level: "error", summary: t("Certificate expired"), details };
  if (starts && starts.getTime() > now.getTime())
    return { ...base, level: "warn", summary: t("Certificate is not valid yet"), details };
  if (expires && (expires.getTime() - now.getTime()) / 86400000 < 14)
    return {
      ...base,
      level: "warn",
      summary: t("Certificate renews {when}", { when: relativeTime(expires, now) }),
      details,
    };
  if (!expires || !starts) {
    details.push(t("Both a start and an expiry have to be reported."));
    return { ...base, level: "unknown", summary: t("Certificate dates are incomplete"), details };
  }
  const note = t("Expiry alone does not verify certificate trust or device association.");
  details.push(note);
  return unverifiedCertificate(
    base,
    t("Expires {date}", { date: formatDate(expires) }),
    details,
    note,
  );
}

/**
 * Installed, currently inside its dates, and not failing. Trust was not
 * checked. Shown as Not verified and kept out of the overall rank. Missing
 * dates do not use this path. Not a healthy or trusted result.
 */
function unverifiedCertificate(
  base: { id: "plexdirect"; label: string; href: string },
  summary: string,
  details: string[],
  note: string,
): HealthItem {
  return {
    ...base,
    level: "unknown",
    summary,
    details,
    statusLabel: t("Not verified"),
    note,
    countsOverall: false,
  };
}

export function computeHealth(
  sections: Record<string, unknown> | undefined,
  now = new Date(),
): HealthItem[] {
  const s = sections ?? {};
  return [
    usenetHealth(s.providers, s.debrid),
    indexersHealth(s.indexers, s.debrid),
    cacheHealth(s.cache),
    catalogHealth(s.catalog, now),
    remoteHealth(s.remote),
    plexDirectHealth(s.remote, now),
  ];
}

// ---- Now playing --------------------------------------------------------------

export interface NowPlaying {
  protocol: string;
  id: string;
  key: string;
  title: string;
  user: string;
  device: string;
  product: string;
  decision: string;
  decisionReason: string;
  remote: boolean;
  bytesServed: number;
  size: number;
  speedBps: number;
  startedAt: Date | null;
  durationMs: number;
  /** 0..1 when the server reports a playback position, else null. */
  progress: number | null;
  release: string;
  /** Where the release streams from: "Usenet" or a debrid service such as "Real-Debrid". */
  origin: string;
  failovers: number;
}

function pick(o: Record<string, unknown>, ...keys: string[]): unknown {
  for (const k of keys) if (o[k] !== undefined && o[k] !== null) return o[k];
  return undefined;
}

/** Sessions come from playback.Sessions() (Go field names, no JSON tags); accept camelCase too. */
export function normalizeSessions(v: unknown): NowPlaying[] {
  if (!Array.isArray(v)) return [];
  return v.filter(isPlainObject).map((s) => {
    const info = (pick(s, "Info", "info") ?? {}) as Record<string, unknown>;
    const client = (pick(s, "Client", "client") ?? {}) as Record<string, unknown>;
    const resolution = (pick(s, "Resolution", "resolution") ?? {}) as Record<string, unknown>;
    const release = isPlainObject(resolution.release) ? resolution.release : {};
    const durationNs = num(pick(info, "duration", "Duration"));
    const offsetMs = pick(s, "viewOffset", "ViewOffset", "positionMs", "PositionMs");
    const durationMs = durationNs / 1e6;
    const explicit = pick(s, "progress", "Progress");
    let progress: number | null = null;
    if (typeof explicit === "number" && Number.isFinite(explicit))
      progress = Math.max(0, Math.min(1, explicit));
    else if (typeof offsetMs === "number" && Number.isFinite(offsetMs) && durationMs > 0)
      progress = Math.max(0, Math.min(1, offsetMs / durationMs));
    return {
      protocol: str(pick(client, "protocol", "Protocol")) || "plex",
      id: str(pick(s, "ID", "id")),
      key: String(pick(s, "Key", "key") ?? ""),
      title: str(pick(s, "TitleName", "titleName", "title")) || t("Untitled"),
      user: str(pick(s, "Username", "username")) || t("Someone"),
      device:
        str(pick(client, "deviceName", "device")) ||
        str(pick(client, "product")) ||
        t("Unknown device"),
      product: str(pick(client, "product")),
      decision: str(pick(s, "Decision", "decision")),
      decisionReason: str(pick(s, "decisionReason", "DecisionReason")),
      remote: pick(client, "remote") === true,
      bytesServed: num(pick(s, "BytesServed", "bytesServed")),
      size: num(pick(info, "size", "Size")),
      speedBps: num(pick(s, "SpeedBps", "speedBps")),
      startedAt: parseTime(pick(s, "StartedAt", "startedAt")),
      durationMs,
      progress,
      release: str(release.name),
      origin: str(pick(s, "origin", "Origin")),
      failovers: num(pick(s, "Failovers", "failovers")),
    };
  });
}
