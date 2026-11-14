import { RuntimeLimitsEditor } from "../RuntimeLimitsEditor";
import { RuntimeLimitsCard } from "../PermissionSheet";
import { ActionJobs } from "./ActionJobs";
import { pluginCollectionHref } from "../../collections/links";
import { DashboardPanel } from "@braedonsaunders/appkit-dashboard/primitives";
import { Badge, Button, Skeleton, cn } from "@braedonsaunders/appkit-ui";
import {
  AlertTriangle,
  ArrowUpRight,
  Film,
  Library,
  Plus,
  RefreshCw,
  Subtitles,
  Tv,
} from "lucide-react";
import { useEffect, useMemo, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { Artwork } from "../../../components/Artwork";
import { t, tn } from "../../../i18n";
import { useLocale } from "../../../i18n/react";
import { apiFetch, errorMessage } from "../../../lib/api";
import { invalidateRegistry, textOf, useRegistry } from "../../../lib/extend";
import { relativeTime } from "../../../lib/format";
import { GuideStrip, type GuideChannel } from "../Stage";
import { SidecarPairing } from "../SidecarPairing";
import { buildPlacements, type PluginDetail } from "../types";
import { callImdb, resolveTitles, resultCount, type PluginCall, type TitleCardLite } from "./calls";
import { healthSummary, type HealthSummary } from "./health";
import { liveTVKind, primaryContribution } from "./kinds";

/**
 * The plugin at a glance: what it is doing right now in the surface it
 * feeds and where it shows up. Problems get a card of
 * their own only when there is one.
 */
export function PluginOverview({
  detail,
  calls,
  onClose,
  onRetry,
  onFresh,
  onActivity,
}: {
  detail: PluginDetail;
  calls: PluginCall[] | null;
  onClose: () => void;
  onRetry: () => Promise<void>;
  /** Reload the plugin quietly after a live call, so its health is current. */
  onFresh?: () => void;
  /** Open the Activity tab, where the technical detail lives. */
  onActivity?: () => void;
}) {
  const health = healthSummary(detail);
  return (
    <div className="space-y-5">
      {detail.manifest.ether?.runtime?.kind === "remote" ? (
        <SidecarPairing key={detail.id} id={detail.id} sidecar={detail.manifest.ether?.sidecar} />
      ) : null}
      {health.card ? (
        <ProblemCard health={health} onRetry={onRetry} onActivity={onActivity} />
      ) : null}
      <LiveNow detail={detail} calls={calls} onClose={onClose} onFresh={onFresh} />
      {detail.manifest.ether?.runtime?.kind && detail.manifest.ether.runtime.kind !== "data" ? (
        <ActionJobs id={detail.id} />
      ) : null}
      <RuntimeLimitsCard
        limits={detail.runtimeLimits}
        requested={detail.manifest.ether?.runtime?.limits}
      />
      {detail.manifest.ether?.runtime?.kind && detail.manifest.ether.runtime.kind !== "data" ? (
        <RuntimeLimitsEditor plugin={detail.id} onSaved={onFresh} />
      ) : null}
      <Placements detail={detail} onClose={onClose} />
    </div>
  );
}

function ProblemCard({
  health,
  onRetry,
  onActivity,
}: {
  health: HealthSummary;
  onRetry: () => Promise<void>;
  onActivity?: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const card = health.card!;
  const severe = health.level === "error";
  return (
    <div
      role="status"
      className={cn(
        "flex flex-wrap items-center gap-3 rounded-xl border p-4",
        severe ? "border-danger/40 bg-danger/5" : "border-warning/40 bg-warning/5",
      )}
    >
      <AlertTriangle
        size={18}
        className={cn("shrink-0", severe ? "text-danger" : "text-warning")}
        aria-hidden
      />
      <div className="min-w-0 flex-1">
        <p className="text-sm font-medium text-fg">{card.title}</p>
        <p className="mt-0.5 text-xs text-fg-muted">{card.line}</p>
      </div>
      <div className="flex shrink-0 items-center gap-2">
        {onActivity ? (
          <Button size="sm" variant="ghost" onClick={onActivity}>
            {t("See activity")}
          </Button>
        ) : null}
        {card.retry ? (
          <Button
            size="sm"
            variant="outline"
            disabled={busy}
            onClick={() => {
              setBusy(true);
              void onRetry().finally(() => setBusy(false));
            }}
          >
            <RefreshCw size={13} className={cn(busy && "animate-spin")} /> {t("Try again")}
          </Button>
        ) : null}
      </div>
    </div>
  );
}

/* ---------------------------------------------------------------- live card */

function LiveNow({
  detail,
  calls,
  onClose,
  onFresh,
}: {
  detail: PluginDetail;
  calls: PluginCall[] | null;
  onClose: () => void;
  onFresh?: () => void;
}) {
  switch (primaryContribution(detail)) {
    case "livetv_channels":
      return <LiveTVNow detail={detail} onClose={onClose} />;
    case "list_source":
      return <CatalogueNow detail={detail} onClose={onClose} onFresh={onFresh} />;
    case "stream_source":
      return (
        <LookupsNow
          detail={detail}
          calls={calls}
          resource="stream"
          icon={<Film size={16} />}
          title={t("Recent source lookups")}
          empty={t("Titles show here after someone presses play and Ether asks {name}.", {
            name: detail.name,
          })}
          count={(n) => tn(n, "{count} source", "{count} sources")}
          none={t("No sources")}
        />
      );
    case "subtitles":
      return (
        <LookupsNow
          detail={detail}
          calls={calls}
          resource="subtitles"
          icon={<Subtitles size={16} />}
          title={t("Recent subtitle lookups")}
          empty={t("Titles show here after a viewer looks for subtitles.")}
          count={(n) => tn(n, "{count} subtitle", "{count} subtitles")}
          none={t("No subtitles")}
        />
      );
    case "metadata":
      return (
        <LookupsNow
          detail={detail}
          calls={calls}
          resource="meta"
          icon={<Library size={16} />}
          title={t("Details it filled in")}
          empty={t("Titles show here after Ether asks {name} for details.", { name: detail.name })}
        />
      );
    default:
      return null;
  }
}

interface LiveSource {
  id: string;
  name: string;
  kind: string;
  channelCount?: number;
}

interface LiveChannel {
  id: string;
  number: string;
  name: string;
  enabled: boolean;
  sourceId?: string;
}

interface Programme {
  channelId: string;
  start: string;
  end: string;
  item?: { title?: string };
}

function LiveTVNow({ detail, onClose }: { detail: PluginDetail; onClose: () => void }) {
  const kind = liveTVKind(detail.id);
  const [source, setSource] = useState<LiveSource | null | undefined>(undefined);
  const [rows, setRows] = useState<GuideChannel[] | null>(null);
  const [adding, setAdding] = useState(false);
  const [error, setError] = useState("");
  const [revision, setRevision] = useState(0);

  useEffect(() => {
    let live = true;
    setRows(null);
    void (async () => {
      try {
        const sources = await apiFetch<LiveSource[]>("/livetv/sources");
        const src = sources.find((s) => s.kind === kind) ?? null;
        if (!live) return;
        setSource(src);
        if (!src) return;
        const now = new Date();
        const to = new Date(now.getTime() + 3 * 3600_000);
        const [channels, guide] = await Promise.all([
          apiFetch<LiveChannel[]>("/livetv/channels"),
          apiFetch<Programme[]>(
            `/livetv/guide?from=${encodeURIComponent(now.toISOString())}&to=${encodeURIComponent(to.toISOString())}`,
          ).catch(() => [] as Programme[]),
        ]);
        const byChannel = new Map<string, Programme[]>();
        for (const p of guide) {
          const list = byChannel.get(p.channelId) ?? [];
          list.push(p);
          byChannel.set(p.channelId, list);
        }
        const mine = channels
          .filter((c) => c.sourceId === src.id && c.enabled)
          .sort((a, b) => Number(a.number) - Number(b.number));
        const out: GuideChannel[] = [];
        for (const c of mine) {
          const progs = (byChannel.get(c.id) ?? []).sort((a, b) => a.start.localeCompare(b.start));
          const idx = progs.findIndex(
            (p) => Date.parse(p.start) <= now.getTime() && now.getTime() < Date.parse(p.end),
          );
          if (idx < 0) continue;
          const cur = progs[idx]!;
          const start = Date.parse(cur.start);
          const end = Date.parse(cur.end);
          out.push({
            number: Number(c.number) || out.length + 1,
            name: c.name,
            now: cur.item?.title ?? "",
            next: progs[idx + 1]?.item?.title ?? "",
            progress: Math.min(1, Math.max(0, (now.getTime() - start) / Math.max(1, end - start))),
          });
          if (out.length >= 6) break;
        }
        if (live) setRows(out);
      } catch (e) {
        if (live) setError(errorMessage(e));
      }
    })();
    return () => {
      live = false;
    };
  }, [kind, revision]);

  const add = async () => {
    setAdding(true);
    setError("");
    try {
      await apiFetch("/livetv/sources", {
        method: "POST",
        body: JSON.stringify({ name: detail.name, kind, playlistUrl: kind }),
      });
      invalidateRegistry();
      setRevision((r) => r + 1);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setAdding(false);
    }
  };

  if (source === null) {
    return (
      <DashboardPanel title={t("Not in Live TV yet")} icon={<Tv size={16} />}>
        <div className="flex flex-wrap items-center justify-between gap-3">
          <p className="text-sm text-fg-muted">{t("Add its channels and guide to Live TV.")}</p>
          <Button size="sm" disabled={adding || !detail.enabled} onClick={() => void add()}>
            <Plus size={14} /> {adding ? t("Adding…") : t("Add to Live TV")}
          </Button>
        </div>
        {error ? <p className="mt-2 text-sm text-danger">{error}</p> : null}
      </DashboardPanel>
    );
  }
  return (
    <DashboardPanel
      title={
        source?.channelCount
          ? tn(source.channelCount, "{count} channel in Live TV", "{count} channels in Live TV")
          : t("Live TV")
      }
      icon={<Tv size={16} />}
      actions={<OpenLink to="/livetv" label={t("Open Live TV")} onClose={onClose} />}
    >
      {rows === null && !error ? (
        <div className="space-y-2">
          {Array.from({ length: 4 }, (_, i) => (
            <Skeleton key={i} className="h-10 rounded-md" />
          ))}
        </div>
      ) : rows?.length ? (
        <GuideStrip channels={rows} />
      ) : (
        <p className="text-sm text-fg-muted">
          {error || t("No programme guide for the next few hours.")}
        </p>
      )}
    </DashboardPanel>
  );
}

interface PreviewItem {
  key: string;
  title: string;
  year?: number;
  poster?: string;
}

/** How long the drawer waits for a slow add-on's catalogue. */
const PREVIEW_WAIT_S = 30;

function CatalogueNow({
  detail,
  onClose,
  onFresh,
}: {
  detail: PluginDetail;
  onClose: () => void;
  onFresh?: () => void;
}) {
  const { descriptors } = useRegistry<{ params?: { required?: boolean }[]; catalog?: string }>(
    "collectionSource",
  );
  const { locale } = useLocale();
  // Lists that show something without a value to fill in, the add-on's
  // headline lists (Popular, Top, Trending) first.
  const lists = useMemo(() => {
    const headline = /popular|top|trending|featured/i;
    return descriptors
      .filter((d) => d.owner === detail.id && !(d.meta?.params ?? []).some((p) => p.required))
      .sort(
        (a, b) => Number(headline.test(textOf(b.label))) - Number(headline.test(textOf(a.label))),
      );
  }, [descriptors, detail.id, locale]);
  const [pick, setPick] = useState<string>("");
  const current = lists.find((d) => d.id === pick) ?? lists[0];
  const [items, setItems] = useState<PreviewItem[] | null>(null);
  const [total, setTotal] = useState(0);
  const [error, setError] = useState("");
  const [waited, setWaited] = useState(0);
  const [attempt, setAttempt] = useState(0);
  const source = detail.host || detail.name;
  // Stable across quiet reloads of the plugin, so a refresh never refetches.
  const ownCatalogs = Boolean(detail.manifest.catalogs?.length);

  // The drawer opens at once; the catalogue arrives when the add-on answers.
  // Public add-on servers can take many seconds, so the panel says it is
  // still loading and gives up, in plain words, after PREVIEW_WAIT_S.
  useEffect(() => {
    if (!current) return;
    const ctl = new AbortController();
    const started = Date.now();
    let live = true;
    setItems(null);
    setError("");
    setWaited(0);
    const tick = window.setInterval(
      () => setWaited(Math.floor((Date.now() - started) / 1000)),
      1000,
    );
    const giveUp = window.setTimeout(() => ctl.abort(), PREVIEW_WAIT_S * 1000);
    const finish = () => {
      window.clearInterval(tick);
      window.clearTimeout(giveUp);
    };
    const label = textOf(current.label);
    const ref = typeof current.meta?.catalog === "string" ? current.meta.catalog : "";
    const slash = ref.indexOf("/");
    const load: Promise<{ items: PreviewItem[]; total: number }> =
      slash > 0 && ownCatalogs
        ? // The add-on's own catalogue page, exactly as it answers it.
          apiFetch<{
            metas?: {
              id: string;
              name?: string;
              poster?: string;
              releaseInfo?: string;
              year?: number;
            }[];
          }>(
            `/plugins/${encodeURIComponent(detail.id)}/resources/catalog?type=${encodeURIComponent(ref.slice(0, slash))}&contentId=${encodeURIComponent(ref.slice(slash + 1))}`,
            { signal: ctl.signal },
          ).then((r) => {
            const metas = r.metas ?? [];
            return {
              total: metas.length,
              items: metas.slice(0, 12).map((m) => ({
                key: m.id,
                title: m.name ?? m.id,
                poster: m.poster,
                year: m.year ?? (Number.parseInt(String(m.releaseInfo ?? ""), 10) || undefined),
              })),
            };
          })
        : apiFetch<{ items: PreviewItem[]; total: number }>("/collections/preview?n=12", {
            method: "POST",
            signal: ctl.signal,
            body: JSON.stringify({
              name: label,
              mediaType: /series|show|tv/i.test(label) ? "show" : "movie",
              sources: [{ kind: current.id }],
            }),
          });
    load
      .then((r) => {
        if (!live) return;
        setItems(r.items ?? []);
        setTotal(r.total ?? 0);
        // The add-on just answered: re-read its health so the header,
        // tiles and card describe this answer.
        onFresh?.();
      })
      .catch((e) => {
        if (!live) return;
        setError(
          ctl.signal.aborted
            ? t(
                "{source} didn't send this catalogue within {seconds} seconds. Its server may be busy; try again in a minute.",
                {
                  source,
                  seconds: PREVIEW_WAIT_S,
                },
              )
            : errorMessage(e),
        );
      })
      .finally(() => {
        finish();
        // A failure or give-up is news for the health readout too.
        if (live && ctl.signal.aborted) onFresh?.();
      });
    return () => {
      live = false;
      finish();
      ctl.abort();
    };
  }, [current, attempt, detail.id, ownCatalogs, source]);

  if (!lists.length) {
    return (
      <DashboardPanel title={t("Catalogues")} icon={<Library size={16} />}>
        <p className="text-sm text-fg-muted">
          {detail.enabled
            ? t("It offers no catalogues Ether can use right now.")
            : t("Turn it on to use its catalogues.")}
        </p>
      </DashboardPanel>
    );
  }
  return (
    <DashboardPanel
      title={t("{plugin} · {catalog}", {
        plugin: detail.name,
        catalog: textOf(current!.label).split("·")[0]!.trim(),
      })}
      icon={<Library size={16} />}
      actions={
        <OpenLink
          to={pluginCollectionHref(current!.id, textOf(current!.label))}
          label={t("Make a collection")}
          onClose={onClose}
        />
      }
    >
      {lists.length > 1 ? (
        <div className="mb-3 flex flex-wrap gap-1.5" role="group" aria-label={t("Catalogues")}>
          {lists.slice(0, 8).map((d) => (
            <button
              key={d.id}
              type="button"
              aria-pressed={d.id === current?.id}
              onClick={() => setPick(d.id)}
              className={cn(
                "rounded-full border px-2.5 py-0.5 text-xs font-medium transition-colors",
                d.id === current?.id
                  ? "border-primary/40 bg-primary-subtle text-primary"
                  : "border-border text-fg-muted hover:text-fg",
              )}
            >
              {textOf(d.label)}
            </button>
          ))}
        </div>
      ) : null}
      {items === null && !error && waited >= 3 ? (
        <p className="mb-3 flex items-center gap-2 text-xs text-fg-muted" role="status">
          <RefreshCw size={12} className="animate-spin" aria-hidden />
          {t("Still loading from {source}… {seconds}s", { source, seconds: waited })}
        </p>
      ) : null}
      {error ? (
        <div className="flex flex-wrap items-center justify-between gap-3">
          <p className="text-sm text-fg-muted">{error}</p>
          <Button size="sm" variant="outline" onClick={() => setAttempt((a) => a + 1)}>
            <RefreshCw size={13} /> {t("Try again")}
          </Button>
        </div>
      ) : (
        <PosterRow
          items={items?.map((i) => ({
            key: i.key,
            title: i.title,
            poster: i.poster,
            caption: i.year ? String(i.year) : "",
          }))}
          empty={t("This catalogue is empty right now.")}
        />
      )}
      {items?.length ? (
        <div className="mt-3 flex justify-end">
          <Badge variant="secondary">{tn(total, "{count} title", "{count} titles")}</Badge>
        </div>
      ) : null}
    </DashboardPanel>
  );
}

function LookupsNow({
  detail,
  calls,
  resource,
  icon,
  title,
  empty,
  count,
  none,
}: {
  detail: PluginDetail;
  calls: PluginCall[] | null;
  resource: "stream" | "subtitles" | "meta";
  icon: ReactNode;
  title: string;
  empty: string;
  count?: (n: number) => string;
  none?: string;
}) {
  const recent = useMemo(() => {
    const seen = new Set<string>();
    const out: { imdb: string; call: PluginCall }[] = [];
    for (const c of calls ?? []) {
      if (!c.route.startsWith(`/${resource}/`) || c.route.startsWith("/stream/tv/")) continue;
      const imdb = callImdb(c.route);
      if (!imdb || seen.has(imdb)) continue;
      seen.add(imdb);
      out.push({ imdb, call: c });
      if (out.length >= 12) break;
    }
    return out;
  }, [calls, resource]);
  const [titles, setTitles] = useState<Record<string, TitleCardLite> | null>(null);
  useEffect(() => {
    let live = true;
    setTitles(null);
    void resolveTitles(recent.map((r) => r.imdb)).then((m) => live && setTitles(m));
    return () => {
      live = false;
    };
  }, [recent]);

  const items =
    calls === null || titles === null
      ? undefined
      : recent
          .filter((r) => titles[r.imdb])
          .map((r) => {
            const n = resultCount(r.call);
            const caption =
              r.call.status !== 200
                ? t("Failed")
                : count && n !== undefined
                  ? n > 0
                    ? count(n)
                    : (none ?? "")
                  : relativeTime(r.call.created);
            return {
              key: r.imdb,
              title: titles[r.imdb]!.title,
              poster: titles[r.imdb]!.poster,
              caption,
              muted: r.call.status !== 200 || !n,
            };
          });
  return (
    <DashboardPanel title={title} icon={icon}>
      <PosterRow items={items} empty={detail.enabled ? empty : t("Turn it on to use it.")} />
    </DashboardPanel>
  );
}

function PosterRow({
  items,
  error,
  empty,
}: {
  items?: { key: string; title: string; poster?: string; caption?: string; muted?: boolean }[];
  error?: string;
  empty: string;
}) {
  if (error) return <p className="text-sm text-danger">{error}</p>;
  if (!items) {
    return (
      <div className="grid grid-cols-4 gap-3 sm:grid-cols-6">
        {Array.from({ length: 6 }, (_, i) => (
          <Skeleton key={i} className="aspect-[2/3] rounded-md" />
        ))}
      </div>
    );
  }
  if (!items.length) return <p className="text-sm text-fg-muted">{empty}</p>;
  return (
    <ul className="grid grid-cols-4 gap-3 sm:grid-cols-6">
      {items.slice(0, 12).map((it) => (
        <li key={it.key} className="min-w-0">
          <Artwork
            src={it.poster}
            alt={it.title}
            title={it.title}
            className="aspect-[2/3] w-full overflow-hidden rounded-md shadow-sm ring-1 ring-border-subtle"
          />
          <p className="mt-1.5 truncate text-xs font-medium text-fg" title={it.title}>
            {it.title}
          </p>
          {it.caption ? (
            <p className={cn("truncate text-[11px]", it.muted ? "text-fg-subtle" : "text-success")}>
              {it.caption}
            </p>
          ) : null}
        </li>
      ))}
    </ul>
  );
}

function OpenLink({ to, label, onClose }: { to: string; label: string; onClose: () => void }) {
  return (
    <Link
      to={to}
      onClick={onClose}
      className="inline-flex items-center gap-1 text-xs font-medium text-primary hover:underline"
    >
      {label} <ArrowUpRight size={13} aria-hidden />
    </Link>
  );
}

/* ---------------------------------------------------------- where it appears */

function Placements({ detail, onClose }: { detail: PluginDetail; onClose: () => void }) {
  const { descriptors } = useRegistry();
  const placements = buildPlacements(descriptors, detail.id);
  const [collections, setCollections] = useState(0);
  useEffect(() => {
    let live = true;
    apiFetch<{ collections: { sources?: { kind?: string }[] }[] }>("/collections")
      .then((r) => {
        const prefix = `plugin:${detail.id}/`;
        const n = (r.collections ?? []).filter((c) =>
          (c.sources ?? []).some((s) => s.kind?.startsWith(prefix)),
        ).length;
        if (live) setCollections(n);
      })
      .catch(() => undefined);
    return () => {
      live = false;
    };
  }, [detail.id]);

  // One chip per surface; a plugin with many catalogues still reads as one.
  const chips = new Map<string, { label: string; href: string }>();
  for (const p of placements) {
    if (p.kind === "console") continue;
    const label = p.kind === "page" ? p.label : p.area;
    if (!chips.has(label)) chips.set(label, { label, href: p.href });
  }
  if (collections > 0) {
    chips.set("collections-used", {
      label: tn(collections, "Used by {count} collection", "Used by {count} collections"),
      href: "/collections",
    });
  }
  if (!chips.size) return null;
  return (
    <div className="flex flex-wrap items-center gap-2" aria-label={t("Where it appears")}>
      <span className="text-xs font-medium text-fg-subtle">{t("Appears in")}</span>
      {[...chips.values()].map((c) => (
        <Link
          key={c.label}
          to={c.href}
          onClick={onClose}
          className="inline-flex items-center gap-1 rounded-full border border-border bg-surface px-3 py-1 text-xs font-medium text-fg transition-colors hover:border-primary/40 hover:text-primary"
        >
          {c.label} <ArrowUpRight size={12} aria-hidden />
        </Link>
      ))}
    </div>
  );
}
