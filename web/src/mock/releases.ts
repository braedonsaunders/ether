/**
 * In-memory fake of the release console API (/ether/api/releases): search,
 * pins, blocklist and history over the mock catalogue. Used by mock mode and
 * the tests; never shipped.
 */
import type {
  BlockEntry,
  ChildTitle,
  ConsoleRelease,
  HealthResult,
  HistoryRecord,
  Pin,
  Release,
  ReleaseInfo,
} from "../lib/releases";
import type { ResolverTrace } from "../lib/types";
import { hash, type FakeTitle } from "./data";

type Reply = { status: number; body: unknown };

const ok = (body: unknown, status = 200): Reply => ({ status, body });
const fail = (status: number, code: string, message: string): Reply => ({
  status,
  body: { error: { code, message } },
});

const PROFILES = ["4K", "1080p", "720p"];

interface Variant {
  suffix: string;
  info: ReleaseInfo;
  gb: number;
  indexer: string;
}

const VARIANTS: Variant[] = [
  {
    suffix: "2160p.UHD.BluRay.REMUX.DV.HDR.HEVC.TrueHD.7.1.Atmos-FRAMESTOR",
    info: {
      height: 2160,
      source: "remux",
      videoCodec: "hevc",
      dolbyVision: true,
      hdr10: true,
      audioCodec: "truehd",
      channels: "7.1",
      atmos: true,
      group: "FRAMESTOR",
    },
    gb: 58,
    indexer: "Indexer One",
  },
  {
    suffix: "1080p.WEB-DL.DDP5.1.H.264-NTb",
    info: {
      height: 1080,
      source: "webdl",
      videoCodec: "h264",
      audioCodec: "eac3",
      channels: "5.1",
      group: "NTb",
    },
    gb: 7.2,
    indexer: "Indexer One",
  },
  {
    suffix: "1080p.BluRay.x264.DTS-HD.MA.5.1-SPARKS",
    info: {
      height: 1080,
      source: "bluray",
      videoCodec: "h264",
      audioCodec: "dtshd",
      channels: "5.1",
      group: "SPARKS",
    },
    gb: 12.4,
    indexer: "Indexer Two",
  },
  {
    suffix: "720p.WEBRip.x264.AAC-YTS",
    info: {
      height: 720,
      source: "webrip",
      videoCodec: "h264",
      audioCodec: "aac",
      channels: "2.0",
      group: "YTS",
    },
    gb: 1.1,
    indexer: "Indexer Two",
  },
  {
    suffix: "HDCAM.x264-CAMGRP",
    info: { height: 720, source: "cam", videoCodec: "h264", group: "CAMGRP" },
    gb: 1.4,
    indexer: "Indexer One",
  },
];

interface Playable {
  key: string;
  name: string;
  base: string;
  year?: number;
}

interface RuleDraft {
  id: string;
  name: string;
  action: string;
  match?: string;
  disabled?: boolean;
  conditions?: { field: string; operator: string; value?: string }[];
  unless?: { field: string; operator: string; value?: string }[];
  fallback?: { field: string; operator: string; value?: string }[];
}

function draftFact(name: string, field: string): string[] {
  const group = name.split("-").at(-1) ?? "";
  switch (field) {
    case "name":
      return [name];
    case "group":
      return group ? [group] : [];
    default:
      return [];
  }
}

function draftMatch(
  name: string,
  c: { field: string; operator: string; value?: string },
): "match" | "miss" | "unknown" {
  const values = draftFact(name, c.field);
  if (!values.length) return "unknown";
  const want = (c.value ?? "").toLowerCase();
  const hit = values.some((value) => {
    const got = value.toLowerCase();
    if (c.operator === "equals") return got === want;
    if (c.operator === "regex") return new RegExp(c.value ?? "", "i").test(value);
    return got.includes(want);
  });
  return hit ? "match" : "miss";
}

function judgeDraft(name: string, index: number, rules: RuleDraft[]): ConsoleRelease {
  const explanations = [];
  let rejected = false;
  let unknown = false;
  let fallback = 0;
  for (const rule of rules) {
    if (rule.disabled) continue;
    const conds = rule.conditions ?? [];
    let state: "match" | "miss" | "unknown" = rule.match === "any" ? "miss" : "match";
    for (const c of conds) {
      const s = draftMatch(name, c);
      if (rule.match === "any") {
        if (s === "match") state = "match";
        else if (state !== "match" && s === "unknown") state = "unknown";
      } else if (s === "miss") state = "miss";
      else if (state !== "miss" && s === "unknown") state = "unknown";
    }
    let outcome = "pass";
    if ((rule.unless ?? []).some((c) => draftMatch(name, c) === "match")) outcome = "skip";
    else if (rule.action === "require" && state !== "match") {
      if ((rule.fallback ?? []).some((c) => draftMatch(name, c) === "match")) {
        outcome = "fallback";
        fallback++;
      } else {
        rejected = true;
        unknown = unknown || state === "unknown";
        outcome = state === "unknown" ? "unknown" : "exclude";
      }
    } else if (rule.action === "avoid" && state !== "miss") {
      rejected = true;
      unknown = unknown || state === "unknown";
      outcome = state === "unknown" ? "unknown" : "exclude";
    } else if (rule.action === "prefer" && state === "match") outcome = "prefer";
    explanations.push({ id: rule.id, name: rule.name, action: rule.action, state, outcome });
  }
  return {
    release: {
      id: String(index),
      indexer: "",
      name,
      size: 0,
      pubDate: "",
      info: {},
      score: 0,
    },
    score: 0,
    formatScore: 0,
    rejected,
    reason: rejected
      ? explanations.find((e) => e.outcome === "exclude" || e.outcome === "unknown")?.name
      : undefined,
    rank: rejected ? 0 : 1,
    preferences: { rejected, unknown, fallback, preferences: [], explanations },
  };
}

function draftConflict(rules: RuleDraft[]): string {
  const needs: { name: string; field: string; value: string }[] = [];
  const single = new Set(["group", "source", "video", "audio", "indexer", "protocol", "edition"]);
  for (const rule of rules) {
    if (
      rule.disabled ||
      rule.action !== "require" ||
      rule.match === "any" ||
      rule.unless?.length ||
      rule.fallback?.length
    )
      continue;
    const seen = new Map<string, string>();
    for (const c of rule.conditions ?? []) {
      if (!single.has(c.field) || c.operator !== "equals") continue;
      const prev = seen.get(c.field);
      if (prev != null && prev.toLowerCase() !== (c.value ?? "").toLowerCase()) {
        return `required preference ${JSON.stringify(rule.name)} cannot match: ${c.field} would have to equal both ${JSON.stringify(prev)} and ${JSON.stringify(c.value ?? "")}`;
      }
      seen.set(c.field, c.value ?? "");
    }
    for (const [field, value] of seen) needs.push({ name: rule.name, field, value });
  }
  for (let i = 0; i < needs.length; i++) {
    for (let j = i + 1; j < needs.length; j++) {
      const a = needs[i]!;
      const b = needs[j]!;
      if (a.field === b.field && a.value.toLowerCase() !== b.value.toLowerCase()) {
        return `required preferences ${JSON.stringify(a.name)} and ${JSON.stringify(b.name)} cannot match the same release: ${a.field} would have to equal both ${JSON.stringify(a.value)} and ${JSON.stringify(b.value)}`;
      }
    }
  }
  return "";
}

export function createReleaseConsoleMock(catalog: FakeTitle[]) {
  const byKey = new Map(catalog.map((t) => [t.key, t]));
  const pins = new Map<string, Pin>();
  const blocks = new Map<string, BlockEntry>();
  const checks = new Map<string, HealthResult>();
  const releases = new Map<string, { rel: Release; key: string; title: string }>();
  const history: HistoryRecord[] = [];
  let historySeq = 0;

  // Episode keys are "<showKey>.s<N>.e<M>"; season keys "<showKey>.s<N>".
  function playable(key: string): Playable | null {
    const ep = /^(\d+)\.s(\d+)\.e(\d+)$/.exec(key);
    if (ep) {
      const show = byKey.get(ep[1]!);
      if (!show || show.type !== "show") return null;
      const s = Number(ep[2]);
      const e = Number(ep[3]);
      const code = `S${String(s).padStart(2, "0")}E${String(e).padStart(2, "0")}`;
      return {
        key,
        name: `${show.title} ${code}`,
        base: `${show.title.replace(/ /g, ".")}.${code}`,
      };
    }
    const t = byKey.get(key);
    if (!t || t.type !== "movie") return null;
    return {
      key,
      name: `${t.title} (${t.year})`,
      base: `${t.title.replace(/ /g, ".")}.${t.year}`,
      year: t.year,
    };
  }

  function releasesFor(p: Playable): Release[] {
    return VARIANTS.map((v, i) => {
      const id = `${p.key}-r${i}`;
      const rel: Release = {
        id,
        guid: id,
        indexer: v.indexer,
        name: `${p.base}.${v.suffix}`,
        size: Math.round(v.gb * 1024 ** 3),
        pubDate: new Date(Date.now() - (20 + (hash(id) % 900)) * 86400000).toISOString(),
        grabs: 5 + (hash(id) % 400),
        info: { title: p.name, year: p.year, ...v.info },
        score: 0,
      };
      releases.set(id, { rel, key: p.key, title: p.name });
      return rel;
    });
  }

  function judge(rel: Release, profile: string): { reason?: string; score: number } {
    const info = rel.info;
    const score =
      (info.height ?? 0) / 20 +
      (info.source === "remux"
        ? 30
        : info.source === "webdl"
          ? 25
          : info.source === "bluray"
            ? 20
            : 5) +
      (info.dolbyVision ? 10 : 0) +
      Math.min(9, Math.log10((rel.grabs ?? 0) + 1) * 3);
    if (blocks.has(rel.id)) return { reason: "marked bad", score };
    if (info.source === "cam") return { reason: "cam not allowed", score };
    const max = profile === "4K" || profile === "none" ? 2160 : profile === "720p" ? 720 : 1080;
    if ((info.height ?? 0) > max) return { reason: "above profile max height", score };
    if (profile === "download" && info.audioCodec === "dtshd")
      return { reason: "size too large for runtime", score };
    return { score };
  }

  function search(key: string, profile: string): Reply {
    const p = playable(key);
    if (!p) return fail(404, "not_found", "no such title");
    const name = profile || "1080p";
    if (![...PROFILES, "none", "download"].includes(name))
      return fail(400, "invalid", `no such quality profile: "${name}"`);
    const pin = pins.get(key) ?? null;
    const pinUsable = pin && !blocks.has(pin.release.id) ? pin.release.id : "";
    const rows: ConsoleRelease[] = releasesFor(p).map((rel) => {
      const j = judge(rel, name === "download" ? "1080p" : name);
      const pinned = rel.id === pin?.release.id;
      const rejected = pinned ? blocks.has(rel.id) : Boolean(j.reason);
      const check = checks.get(rel.id);
      return {
        release: { ...rel, score: j.score },
        score: j.score,
        formatScore: rel.info.atmos ? 25 : 0,
        customFormats: rel.info.atmos ? [{ id: "atmos", name: "Atmos", score: 25 }] : [],
        rejected,
        reason: rejected ? (pinned ? "marked bad" : j.reason) : undefined,
        pinned,
        blocked: blocks.has(rel.id),
        blockReason: blocks.get(rel.id)?.reason,
        health: blocks.has(rel.id)
          ? { releaseId: rel.id, checkedAt: "", healthy: false, source: "blocked" as const }
          : (check ?? null),
      };
    });
    const accepted = rows
      .filter((r) => !r.rejected)
      .sort(
        (a, b) =>
          Number(b.release.id === pinUsable) - Number(a.release.id === pinUsable) ||
          b.score - a.score,
      );
    accepted.forEach((r, i) => {
      r.rank = i + 1;
      r.winner = i === 0;
    });
    return ok({
      titleKey: key,
      titleName: p.name,
      profile: name === "download" ? "1080p+download" : name,
      cached: true,
      searchedAt: new Date(Date.now() - 4 * 60000).toISOString(),
      duration: 1.2e9,
      winner: accepted[0]?.release.id,
      pin,
      pinNote: pin && !pinUsable ? "skipped: pinned release is blocklisted" : undefined,
      releases: rows.sort((a, b) => (a.rank || 99) - (b.rank || 99) || b.score - a.score),
    });
  }

  function children(key: string): Reply {
    const season = /^(\d+)\.s(\d+)$/.exec(key);
    if (season) {
      const show = byKey.get(season[1]!);
      if (!show) return fail(404, "not_found", "no such title");
      const s = Number(season[2]);
      const items: ChildTitle[] = Array.from({ length: 6 + (hash(key) % 4) }, (_, i) => ({
        key: `${show.key}.s${s}.e${i + 1}`,
        type: "episode",
        title: `Chapter ${i + 1}`,
        season: s,
        episode: i + 1,
        pinned: pins.has(`${show.key}.s${s}.e${i + 1}`),
      }));
      return ok({ items });
    }
    const show = byKey.get(key);
    if (!show || show.type !== "show")
      return fail(400, "invalid", "key must be a show or season rating key");
    const items: ChildTitle[] = Array.from({ length: 1 + (hash(key) % 4) }, (_, i) => ({
      key: `${show.key}.s${i + 1}`,
      type: "season",
      title: `Season ${i + 1}`,
      season: i + 1,
    }));
    return ok({ items });
  }

  function block(releaseId: string, reason: string, manual: boolean, key = ""): BlockEntry {
    const known = releases.get(releaseId);
    const now = new Date().toISOString();
    const prev = blocks.get(releaseId);
    const e: BlockEntry = {
      releaseId,
      name: known?.rel.name ?? prev?.name,
      indexer: known?.rel.indexer ?? prev?.indexer,
      titleKey: known?.key ?? (key || prev?.titleKey || "0"),
      titleName: known?.title ?? prev?.titleName,
      reason: prev?.manual && !manual ? prev.reason : reason,
      source: manual || prev?.manual ? "owner" : "auto",
      manual: manual || Boolean(prev?.manual),
      createdAt: prev?.createdAt ?? now,
      updatedAt: now,
    };
    blocks.set(releaseId, e);
    return e;
  }

  function seed() {
    const movies = catalog.filter((t) => t.type === "movie").slice(0, 6);
    movies.forEach((m, i) => {
      const p = playable(m.key)!;
      const rels = releasesFor(p);
      if (i === 0) {
        pins.set(m.key, {
          titleKey: m.key,
          titleName: p.name,
          release: rels[2]!,
          note: "The only encode with the original mix",
          pinnedAt: new Date(Date.now() - 3 * 86400000).toISOString(),
          lastStatus: "healthy",
          lastCheckedAt: new Date(Date.now() - 2 * 3600000).toISOString(),
        });
      }
      if (i === 1) block(rels[1]!.id, "health: missing articles", false);
      if (i === 2) block(rels[2]!.id, "Wrong audio language", true);
      const winner = i === 0 ? rels[2]! : rels[1]!;
      const trace: ResolverTrace = {
        titleKey: m.key,
        titleName: p.name,
        profile: "1080p",
        at: new Date(Date.now() - (i * 47 + 11) * 60000).toISOString(),
        searchDuration: 1.8e9,
        winner: i === 3 ? undefined : winner.id,
        err: i === 3 ? "no healthy candidates" : undefined,
        failover: i === 4,
        pinned: i === 0 ? rels[2]!.id : undefined,
        pinStatus: i === 0 ? "used" : undefined,
        candidates: rels.map((r) => {
          const j = judge(r, "1080p");
          const won = i !== 3 && r.id === winner.id;
          return {
            releaseId: r.id,
            name: r.name,
            indexer: r.indexer,
            score: j.score,
            size: r.size,
            rejected: i === 3 ? true : !won && Boolean(j.reason),
            reason: i === 3 && !j.reason ? "health: unhealthy" : j.reason,
            healthChecked: won || (i === 3 && !j.reason),
            healthy: won,
            fetchDuration: won ? 3.1e8 : 0,
            healthDuration: won ? 6.4e8 : 0,
            pinned: i === 0 && r.id === rels[2]!.id,
          };
        }),
      };
      history.push(record(trace));
    });
  }

  function record(trace: ResolverTrace): HistoryRecord {
    const won = trace.candidates?.find((c) => c.releaseId === trace.winner);
    return {
      id: ++historySeq,
      at: trace.at,
      titleKey: trace.titleKey,
      titleName: trace.titleName,
      profile: trace.profile,
      releaseId: trace.winner,
      releaseName: won?.name,
      indexer: won?.indexer,
      size: won?.size,
      error: trace.err,
      failover: trace.failover,
      pinned: trace.pinStatus === "used",
      candidates: trace.candidates?.length ?? 0,
      trace,
    };
  }

  seed();

  function previewRules(body: unknown): Reply {
    const input = (body ?? {}) as {
      profile?: { releaseRules?: RuleDraft[] };
      key?: string;
      names?: string[];
    };
    const rules = input.profile?.releaseRules ?? [];
    if (rules.length > 32)
      return fail(400, "invalid", "at most 32 release preferences are allowed");
    if ((input.names?.length ?? 0) > 0 && input.key)
      return fail(400, "invalid", "choose a title or filenames, not both");
    if ((input.names?.length ?? 0) > 100) return fail(400, "invalid", "at most 100 filenames");
    for (const rule of rules) {
      for (const c of [
        ...(rule.conditions ?? []),
        ...(rule.unless ?? []),
        ...(rule.fallback ?? []),
      ]) {
        if (c.operator === "regex") {
          try {
            new RegExp(c.value ?? "");
          } catch {
            return fail(400, "invalid", "invalid expression");
          }
        }
      }
    }
    const names = (input.names ?? []).map((n) => n.trim()).filter(Boolean);
    const rows = names.map((name, i) => judgeDraft(name, i, rules));
    if (input.key && names.length === 0) {
      const p = playable(input.key);
      if (!p) return fail(404, "not_found", "no such title");
      for (const rel of releasesFor(p)) {
        const row = judgeDraft(rel.name, rows.length, rules);
        row.release = rel;
        rows.push(row);
      }
    }
    const warning = draftConflict(rules);
    return ok({ releases: rows, warning });
  }

  function handle(method: string, seg: string[], q: URLSearchParams, body: unknown): Reply | null {
    if (seg[0] !== "releases") return null;
    const route = `${method} /${seg.slice(1).join("/")}`;
    const b = (body ?? {}) as Record<string, string>;
    switch (route) {
      case "GET /profiles":
        return ok({ default: "1080p", profiles: PROFILES, special: ["none", "download"] });
      case "POST /rules/preview":
        return previewRules(body);
      case "GET /search":
        return search(q.get("key") ?? "", q.get("profile") ?? "");
      case "POST /forget":
        return playable(b.key ?? "")
          ? ok({ ok: true })
          : fail(400, "invalid", "key must be a movie or episode rating key");
      case "POST /health": {
        const known = releases.get(b.releaseId ?? "");
        if (!known)
          return fail(
            404,
            "unknown_release",
            "release is not among recent search results; search again",
          );
        const healthy = !/YTS/.test(known.rel.name);
        const res: HealthResult = {
          releaseId: known.rel.id,
          checkedAt: new Date().toISOString(),
          healthy,
          reason: healthy ? undefined : "health: unhealthy",
          duration: 9e8,
          source: "check",
          blocked: !healthy,
        };
        if (!healthy) block(known.rel.id, "owner health check: health: unhealthy", false);
        checks.set(known.rel.id, res);
        return ok(res);
      }
      case "GET /pins":
        return ok({
          pins: [...pins.values()].sort((a, b2) => b2.pinnedAt.localeCompare(a.pinnedAt)),
        });
      case "POST /pins": {
        const known = releases.get(b.releaseId ?? "");
        if (!known)
          return fail(
            404,
            "unknown_release",
            "release is not among recent search results; search again",
          );
        if (blocks.has(known.rel.id))
          return fail(409, "blocked", "release is blocklisted; unblock it before pinning");
        const pin: Pin = {
          titleKey: b.key ?? known.key,
          titleName: known.title,
          release: known.rel,
          note: b.note || undefined,
          pinnedAt: new Date().toISOString(),
          lastCheckedAt: "0001-01-01T00:00:00Z",
        };
        pins.set(pin.titleKey, pin);
        return ok(pin);
      }
      case "GET /blocklist": {
        const needle = (q.get("q") ?? "").toLowerCase();
        const items = [...blocks.values()]
          .filter(
            (e) =>
              (!q.get("source") || e.source === q.get("source")) &&
              (!q.get("key") || e.titleKey === q.get("key")) &&
              (!needle || `${e.name ?? ""} ${e.titleName ?? ""}`.toLowerCase().includes(needle)),
          )
          .sort((a, b2) => b2.updatedAt.localeCompare(a.updatedAt));
        return ok({ items, total: items.length });
      }
      case "POST /blocklist":
        if (!b.releaseId) return fail(400, "invalid", "releaseId is required");
        return ok(block(b.releaseId, b.reason || "blocked by owner", true, b.key));
      case "GET /history": {
        const key = q.get("key");
        const items = history
          .filter((h) => !key || h.titleKey === key)
          .sort((a, b2) => b2.at.localeCompare(a.at))
          .map((h) => ({ ...h, trace: undefined }));
        return ok({ items, total: items.length });
      }
    }
    if (method === "DELETE" && seg[1] === "pins" && seg[2]) {
      return pins.delete(seg[2]) ? ok(null, 204) : fail(404, "not_found", "the title has no pin");
    }
    if (method === "DELETE" && seg[1] === "blocklist" && seg[2]) {
      checks.delete(seg[2]);
      return blocks.delete(seg[2]) ? ok(null, 204) : fail(404, "not_found", "not found");
    }
    if (method === "GET" && seg[1] === "history" && seg[2]) {
      const h = history.find((x) => x.id === Number(seg[2]));
      return h ? ok(h) : fail(404, "not_found", "not found");
    }
    if (method === "GET" && seg[1] === "titles" && seg[2] && seg[3] === "children") {
      return children(seg[2]);
    }
    return fail(404, "not_found", "no such release console endpoint");
  }

  return { handle, pins, blocks, history };
}
