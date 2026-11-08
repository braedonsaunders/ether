import { GuideAIAction } from "./GuideAIReview";
import { sourceImportError } from "./sourceErrors";
import { Badge, Button, Input, Popover, cn, toast } from "@braedonsaunders/appkit-ui";
import {
  AlertTriangle,
  ArrowRight,
  CalendarRange,
  CheckCircle2,
  Link2,
  Link2Off,
  Pencil,
  RefreshCw,
  Search,
  Wand2,
} from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { Segmented } from "../../components/fields";
import { errorMessage, isAbort } from "../../lib/api";
import { t, tn, tx } from "../../i18n";
import { formatNumber, relativeTime } from "../../lib/format";
import { useDebouncedValue } from "../../lib/hooks";
import { ChannelLogo } from "./ChannelLogo";
import {
  clock,
  dayClock,
  progress,
  isGuideSource,
  plexLineupName,
  tv,
  type Channel,
  type ChannelSummary,
  type Coverage,
  type GuideChannel,
  type MatchChange,
  type Programme,
  type Source,
} from "./model";

/** Search every imported XMLTV guide for a channel to take listings from. */
export function GuideChannelSearch({
  initial,
  onPick,
  autoFocus,
}: {
  initial?: string;
  onPick: (g: GuideChannel) => void;
  autoFocus?: boolean;
}) {
  const [q, setQ] = useState(initial ?? "");
  const debounced = useDebouncedValue(q, 200);
  const [items, setItems] = useState<GuideChannel[] | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    const ctl = new AbortController();
    tv.guideChannels(debounced.trim(), ctl.signal, 40)
      .then((r) => {
        setItems(r ?? []);
        setError("");
      })
      .catch((e) => {
        if (!isAbort(e)) setError(errorMessage(e));
      });
    return () => ctl.abort();
  }, [debounced]);
  return (
    <div className="space-y-2">
      <div className="relative">
        <Search
          size={14}
          className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-fg-subtle"
          aria-hidden
        />
        <Input
          autoFocus={autoFocus}
          className="pl-8"
          value={q}
          onChange={(e) => setQ(e.currentTarget.value)}
          placeholder={t("Search guide channels by name or id…")}
          aria-label={t("Search guide channels")}
        />
      </div>
      {error ? <p className="text-xs text-danger">{error}</p> : null}
      {items && items.length === 0 ? (
        <p className="px-1 py-3 text-center text-xs text-fg-muted">
          {t("No guide channels match. Add an XMLTV guide source to bring in more listings.")}
        </p>
      ) : null}
      <ul className="max-h-72 space-y-0.5 overflow-y-auto" aria-label={t("Guide channels")}>
        {(items ?? []).map((g) => (
          <li key={`${g.sourceId}/${g.guideId}`}>
            <button
              type="button"
              onClick={() => onPick(g)}
              className="flex w-full items-center gap-2.5 rounded-md p-1.5 text-left hover:bg-surface-hover"
            >
              <ChannelLogo name={g.name} logo={g.logo} seed={g.guideId} size={28} />
              <span className="min-w-0 flex-1">
                <span className="block truncate text-sm text-fg">{g.name}</span>
                <span className="block truncate text-[11px] text-fg-muted">
                  {g.guideId} · {g.sourceName}
                </span>
              </span>
              <span className="shrink-0 text-right text-[11px] text-fg-subtle">
                {g.programmes
                  ? tn(g.programmes, "{count} listing", "{count} listings")
                  : g.sourceKind === "plex"
                    ? t("Plex guide · listings load when used")
                    : t("No listings")}
                {g.until ? (
                  <span className="block">{t("to {time}", { time: dayClock(g.until) })}</span>
                ) : null}
              </span>
            </button>
          </li>
        ))}
      </ul>
    </div>
  );
}

/** The mapping popover used on each coverage row and the channel's Guide tab. */
export function MapGuideButton({
  channel,
  onMapped,
  label = t("Map guide"),
  variant = "outline",
}: {
  channel: ChannelSummary;
  onMapped: (c: ChannelSummary) => void | Promise<void>;
  label?: string;
  variant?: "outline" | "ghost" | "default";
}) {
  const [open, setOpen] = useState(false);
  return (
    <Popover
      open={open}
      onOpenChange={setOpen}
      align="end"
      className="w-[22rem] p-3"
      trigger={
        <Button
          size="sm"
          variant={variant}
          aria-label={t("Map guide for {channel}", { channel: channel.name })}
          onClick={() => setOpen((o) => !o)}
        >
          <Link2 size={13} /> {label}
        </Button>
      }
    >
      <p className="mb-2 text-xs text-fg-muted">
        {tx("Listings for {channel} come from the guide channel you pick.", {
          channel: <strong className="text-fg">{channel.name}</strong>,
        })}
      </p>
      <GuideChannelSearch
        autoFocus
        initial={channel.name}
        onPick={(g) => {
          setOpen(false);
          void onMapped({ ...channel, guideSourceId: g.sourceId, guideId: g.guideId });
        }}
      />
    </Popover>
  );
}

const ROW_PAGE = 80;

const sourceTitle = (s: Source) => (s.kind === "plex" ? plexLineupName(s.name) : s.name);

/** A guide source is stale when it has failed, or its listings end within six hours. */
const STALE_MS = 6 * 60 * 60 * 1000;

const usesGuide = (c: ChannelSummary, s: Source) =>
  c.kind === "iptv" && Boolean(c.guideId) && (c.guideSourceId || c.sourceId) === s.id;

/** One line on how a guide source is doing: size, use, horizon, or its error. */
function guideSourceStatus(s: Source, used: number, now: number): { text: string; warn: boolean } {
  const until = s.guideUntil ? Date.parse(s.guideUntil) : 0;
  if (s.lastError) return { text: t("Last refresh failed"), warn: true };
  const total = s.guideChannelCount ?? 0;
  if (s.kind === "plex" && !used)
    return {
      text: tn(total, "{count} channel · none in use yet", "{count} channels · none in use yet"),
      warn: false,
    };
  if (until && until <= now) return { text: t("Listings have run out"), warn: true };
  const parts = [tn(total, "{count} channel", "{count} channels")];
  if (used) parts.push(tn(used, "{count} in use", "{count} in use"));
  if (until) parts.push(t("to {time}", { time: dayClock(new Date(until)) }));
  return { text: parts.join(" · "), warn: Boolean(until && until - now < STALE_MS) };
}

/**
 * The Guide tab's lineup: overall coverage, then every guide source. Picking
 * one opens its workbench on the right; "All guides" is the whole lineup.
 */
export function GuideSourceList({
  lineup,
  sources,
  selected,
  onSelect,
  now,
  query = "",
}: {
  /** The Live TV search: narrows the guide sources listed by name. */
  query?: string;
  lineup: ChannelSummary[];
  sources: Source[];
  /** The selected guide source id, or null for All guides. */
  selected: string | null;
  onSelect: (id: string | null) => void;
  now: number;
}) {
  const needle = query.trim().toLowerCase();
  const guides = sources
    .filter(isGuideSource)
    .filter((s) => !needle || sourceTitle(s).toLowerCase().includes(needle));

  return (
    <div className="space-y-4 p-3 sm:p-4">
      <div className="space-y-1.5">
        <div className="flex min-h-7 items-center gap-2 px-1">
          <h2 className="flex-1 text-[11px] font-semibold uppercase tracking-wider text-fg-subtle">
            {t("Guide sources")}
          </h2>
          {selected !== null ? (
            <Button size="sm" variant="ghost" onClick={() => onSelect(null)}>
              {t("Show all guides")}
            </Button>
          ) : null}
        </div>
        {guides.length === 0 ? (
          <p className="px-1 text-xs text-fg-muted">
            {t(
              "No guide sources yet. Add an XMLTV guide or your Plex guide to give channels listings.",
            )}
          </p>
        ) : null}
        {guides.map((s) => {
          const on = selected === s.id;
          const used = lineup.filter((c) => usesGuide(c, s)).length;
          const status = guideSourceStatus(s, used, now);
          return (
            <button
              key={s.id}
              type="button"
              onClick={() => onSelect(on ? null : s.id)}
              aria-current={on ? "true" : undefined}
              className={cn(
                "flex w-full items-start gap-3 rounded-xl border bg-surface p-3 text-left transition-colors hover:border-border-strong",
                on ? "border-primary/40 ring-1 ring-primary/30" : "border-border",
              )}
            >
              <span className="grid size-9 shrink-0 place-items-center rounded-lg bg-info/15 text-info">
                <CalendarRange size={17} />
              </span>
              <span className="min-w-0 flex-1 space-y-1">
                <span className="flex items-center gap-2">
                  <span className="truncate text-sm font-medium text-fg">{sourceTitle(s)}</span>
                  <Badge variant="secondary" className="shrink-0">
                    {s.kind === "plex" ? t("Plex") : s.hasPlaylist ? t("M3U guide") : t("XMLTV")}
                  </Badge>
                </span>
                <span
                  className={cn(
                    "flex items-center gap-1 text-xs",
                    status.warn ? "text-warning" : "text-fg-muted",
                  )}
                >
                  {status.warn ? <AlertTriangle size={12} className="shrink-0" /> : null}
                  <span className="truncate">{status.text}</span>
                </span>
              </span>
            </button>
          );
        })}
      </div>
    </div>
  );
}

/**
 * The guide workbench on the right of the Guide tab. For All guides: how
 * much of the lineup has listings, suggested matches, and every channel's
 * mapping. For one source: its health, the channels using it, and the
 * channels it could cover. Mapping a channel opens its guide drawer.
 */
export function GuideWorkbench({
  channels,
  sources,
  coverage,
  source,
  onMapChannel,
  onEditSource,
  onReload,
  now,
  query = "",
}: {
  channels: ChannelSummary[];
  sources: Source[];
  coverage: Map<string, Coverage> | null;
  /** The selected guide source, or undefined for All guides. */
  source?: Source;
  /** The Live TV search: narrows the channel mapping list (not the coverage figures). */
  query?: string;
  onMapChannel: (id: string) => void;
  onEditSource: (id: string) => void;
  onReload: () => Promise<void>;
  now: number;
}) {
  type Show = "missing" | "all" | "using";
  const [show, setShow] = useState<Show>("missing");
  const [limit, setLimit] = useState(ROW_PAGE);
  const [proposals, setProposals] = useState<MatchChange[] | null>(null);
  const [matchCounts, setMatchCounts] = useState({ ambiguous: 0, regionRejected: 0 });
  const [accepted, setAccepted] = useState<Set<string>>(new Set());
  const [busy, setBusy] = useState(false);
  const [refreshing, setRefreshing] = useState(false);
  const iptv = useMemo(() => channels.filter((c) => c.kind === "iptv"), [channels]);
  const live = useMemo(() => iptv.filter((c) => c.enabled), [iptv]);
  const missing = useMemo(
    () => live.filter((c) => (coverage?.get(c.id)?.programmes ?? 0) === 0),
    [live, coverage],
  );
  const using = useMemo(
    () => (source ? iptv.filter((c) => usesGuide(c, source)) : []),
    [iptv, source],
  );
  const has = (c: ChannelSummary) => (coverage?.get(c.id)?.programmes ?? 0) > 0;
  const needle = query.trim().toLowerCase();
  const matches = useMemo(() => {
    if (!needle) return null;
    return (list: ChannelSummary[]) =>
      list.filter((c) =>
        `${c.number} ${c.name} ${c.group ?? ""} ${c.guideId ?? ""}`.toLowerCase().includes(needle),
      );
  }, [needle]);
  const shown = useMemo(
    () => ({
      using: matches ? matches(using) : using,
      all: matches ? matches(iptv) : iptv,
      missing: matches ? matches(missing) : missing,
    }),
    [matches, using, iptv, missing],
  );
  useEffect(() => setLimit(ROW_PAGE), [needle]);
  const byId = useMemo(() => new Map(channels.map((c) => [c.id, c])), [channels]);
  const sourceName = (id?: string) => {
    const s = sources.find((x) => x.id === id);
    return s ? sourceTitle(s) : "";
  };
  const sourceId = source?.id;

  // A source opens on the channels it feeds, or on the gaps when it feeds none.
  useEffect(() => {
    setShow(source ? (iptv.some((c) => usesGuide(c, source)) ? "using" : "missing") : "missing");
    setLimit(ROW_PAGE);
    setProposals(null);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sourceId]);

  const rows = shown[show];
  const pct = live.length ? (live.length - missing.length) / live.length : 1;
  const status = source ? guideSourceStatus(source, using.length, now) : { text: "", warn: false };

  const propose = async () => {
    setBusy(true);
    try {
      const r = await tv.match({ dryRun: true });
      const changes = (r.changes ?? []).filter((c) => !sourceId || c.guideSourceId === sourceId);
      setProposals(changes);
      setMatchCounts({ ambiguous: r.ambiguous ?? 0, regionRejected: r.regionRejected ?? 0 });
      setAccepted(new Set(changes.filter((c) => !c.reviewRegion).map((c) => c.channelId)));
      if (!changes.length)
        toast.info(t("No new matches found"), {
          description: source
            ? t("Open a channel to search this guide by hand.")
            : t("Try adding another guide source."),
        });
    } catch (e) {
      toast.error(t("Couldn't match guide channels"), { description: errorMessage(e) });
    } finally {
      setBusy(false);
    }
  };
  const apply = async () => {
    setBusy(true);
    try {
      const r = await tv.match({ channelIds: [...accepted] });
      toast.success(
        tn(r.matched, "{count} channel matched to a guide", "{count} channels matched to a guide"),
      );
      setProposals(null);
      await onReload();
    } catch (e) {
      toast.error(t("Couldn't apply matches"), { description: errorMessage(e) });
    } finally {
      setBusy(false);
    }
  };
  const refresh = async () => {
    if (!source) return;
    setRefreshing(true);
    try {
      await tv.refreshSource(source.id);
      toast.success(t("{name} refreshed", { name: sourceTitle(source) }));
      await onReload();
    } catch (e) {
      toast.error(t("Couldn't refresh the guide"), { description: sourceImportError(e) });
    } finally {
      setRefreshing(false);
    }
  };

  return (
    <div className="mx-auto max-w-5xl space-y-5 p-3 sm:p-6">
      <section
        className={cn(
          "relative overflow-hidden rounded-2xl border border-border bg-surface shadow-xs",
          "px-4 py-3",
        )}
      >
        {source ? (
          <div className="relative flex flex-wrap items-center gap-4">
            <span className="grid size-9 shrink-0 place-items-center rounded-lg bg-info/15 text-info">
              <CalendarRange size={18} />
            </span>
            <div className="min-w-0 flex-1">
              <p className="text-xs font-semibold uppercase tracking-wider text-fg-subtle">
                {source.kind === "plex" ? t("Plex guide") : t("XMLTV guide")}
              </p>
              <h2 className="mt-0.5 truncate text-base font-semibold tracking-tight text-fg">
                {sourceTitle(source)}
              </h2>
              <p className={cn("mt-0.5 text-sm", status.warn ? "text-warning" : "text-fg-muted")}>
                {status.text}
                {source.updatedAt
                  ? ` · ${t("refreshed {when}", { when: relativeTime(source.updatedAt) })}`
                  : ""}
              </p>
            </div>
            <div className="flex flex-wrap gap-2">
              <Button variant="outline" onClick={() => void refresh()} disabled={refreshing}>
                <RefreshCw size={14} className={cn(refreshing && "animate-spin")} /> {t("Refresh")}
              </Button>
              <Button variant="outline" onClick={() => onEditSource(source.id)}>
                <Pencil size={14} /> {t("Edit")}
              </Button>
            </div>
            {source.lastError ? (
              <p
                role="alert"
                className="relative w-full rounded-lg border border-danger/30 bg-danger/10 px-3 py-2 text-sm text-danger"
              >
                {sourceImportError(source.lastError)}
              </p>
            ) : null}
          </div>
        ) : (
          <div className="relative flex flex-wrap items-center gap-x-4 gap-y-2">
            <p className="text-2xl font-semibold tracking-tight tabular-nums text-fg">
              {coverage === null ? "—" : `${Math.round(pct * 100)}%`}
            </p>
            <div className="min-w-0 flex-1 space-y-1.5">
              <p className="truncate text-sm text-fg-muted">
                <span className="text-fg">{t("of live IPTV channels have listings")}</span>
                {" · "}
                {coverage === null
                  ? t("Checking which channels have listings…")
                  : missing.length
                    ? t("{missing} of {total} need a guide. Virtual channels build their own.", {
                        missing: formatNumber(missing.length),
                        total: formatNumber(live.length),
                      })
                    : t("Every live channel has listings. Virtual channels build their own.")}
              </p>
              <div className="h-1.5 overflow-hidden rounded-full bg-bg-subtle">
                <div
                  className={cn(
                    "h-full rounded-full transition-all",
                    pct >= 0.999 ? "bg-success" : "bg-gradient-to-r from-primary to-info",
                  )}
                  style={{ width: `${pct * 100}%` }}
                />
              </div>
            </div>
          </div>
        )}
      </section>

      {proposals !== null ? (
        <section
          aria-label={t("Suggested matches")}
          className="space-y-3 rounded-2xl border border-primary/30 bg-primary-subtle/40 p-4"
        >
          <div className="flex flex-wrap items-center gap-2">
            <Wand2 size={15} className="text-primary" />
            <h2 className="text-sm font-semibold text-fg">
              {tn(proposals.length, "{count} suggested match", "{count} suggested matches")}
            </h2>
            <span className="text-xs text-fg-muted">
              {t("Matched by guide id, then by channel name.")}
            </span>
            <div className="ml-auto flex gap-2">
              <Button variant="ghost" size="sm" onClick={() => setProposals(null)}>
                {t("Dismiss")}
              </Button>
              <Button size="sm" disabled={busy || accepted.size === 0} onClick={() => void apply()}>
                <CheckCircle2 size={13} /> {t("Apply {count}", { count: accepted.size })}
              </Button>
            </div>
          </div>
          <p className="text-xs text-fg-muted">
            {t("Ambiguous matches: {count}", { count: formatNumber(matchCounts.ambiguous) })}
            {" · "}
            {t("Region conflicts: {count}", { count: formatNumber(matchCounts.regionRejected) })}
          </p>
          {proposals.length > 1 ? (
            <label className="flex w-fit cursor-pointer items-center gap-3 px-3 text-xs font-medium text-fg-muted">
              <input
                type="checkbox"
                checked={accepted.size === proposals.length}
                ref={(el) => {
                  if (el) el.indeterminate = accepted.size > 0 && accepted.size < proposals.length;
                }}
                onChange={() =>
                  setAccepted(
                    accepted.size === proposals.length
                      ? new Set()
                      : new Set(proposals.map((p) => p.channelId)),
                  )
                }
              />
              {t("Select all")}
            </label>
          ) : null}
          <ul className="max-h-80 space-y-1 overflow-y-auto">
            {proposals.map((p) => {
              const c = byId.get(p.channelId);
              const on = accepted.has(p.channelId);
              return (
                <li key={p.channelId}>
                  <label className="grid cursor-pointer grid-cols-[auto_minmax(0,1fr)] items-center gap-x-3 gap-y-2 sm:grid-cols-[auto_minmax(0,1fr)_auto_minmax(0,1fr)] rounded-lg bg-surface px-3 py-2 text-sm">
                    <input
                      type="checkbox"
                      checked={on}
                      onChange={() =>
                        setAccepted((s) => {
                          const n = new Set(s);
                          if (on) n.delete(p.channelId);
                          else n.add(p.channelId);
                          return n;
                        })
                      }
                      aria-label={t("Accept match for {channel}", {
                        channel: c?.name ?? p.channelId,
                      })}
                    />
                    <span className="min-w-0 break-words text-fg">
                      {c?.name ?? p.channelName ?? p.channelId}
                    </span>
                    <ArrowRight size={13} className="hidden shrink-0 text-fg-subtle sm:block" />
                    <span className="col-start-2 min-w-0 break-words text-fg-muted sm:col-start-auto">
                      {p.guideName}{" "}
                      <span className="text-fg-subtle">
                        · {p.guideSourceName || sourceName(p.guideSourceId)}
                        {p.method === "name" ? ` · ${t("by name")}` : ""}
                      </span>
                    </span>
                    {p.reviewRegion ? (
                      <span className="col-start-2 text-xs text-fg-muted sm:col-span-3">
                        <Badge variant="warning" className="mr-2">
                          {t("Check region")}
                        </Badge>
                        {t("A region is missing from the channel or guide. Confirm they match.")}
                      </span>
                    ) : null}
                  </label>
                </li>
              );
            })}
          </ul>
        </section>
      ) : null}

      <section aria-label={t("Channel guide mapping")} className="space-y-3">
        <div className="flex flex-wrap items-center gap-3">
          <div className="min-w-0 basis-full sm:basis-auto sm:flex-1">
            <h2 className="text-sm font-semibold text-fg">{t("Channel mapping")}</h2>
            <div className="mt-3 flex flex-wrap gap-2">
              <Button size="sm" onClick={() => void propose()} disabled={busy || coverage === null}>
                <Wand2 size={14} />
                {t("Auto-match")}
              </Button>
              <GuideAIAction
                channels={channels}
                sources={sources}
                coverage={coverage}
                sourceId={sourceId}
                onApplied={onReload}
              />
            </div>
            <p className="mt-1 text-xs text-fg-muted">
              {t("Map a channel from Missing or All IPTV, or run Auto-match.")}
            </p>
          </div>
          <Segmented
            ariaLabel={t("Show channels")}
            size="sm"
            value={show}
            onChange={(v) => {
              setShow(v);
              setLimit(ROW_PAGE);
            }}
            options={[
              ...(source
                ? [
                    {
                      value: "using" as Show,
                      label: t("Using this guide ({count})", { count: shown.using.length }),
                    },
                  ]
                : []),
              {
                value: "missing" as Show,
                label: t("Missing ({count})", { count: shown.missing.length }),
              },
              {
                value: "all" as Show,
                label: t("All IPTV ({count})", { count: shown.all.length }),
              },
            ]}
          />
        </div>
        {rows.length === 0 ? (
          <div className="rounded-xl border border-dashed border-border p-8 text-center">
            <CheckCircle2 className="mx-auto text-success" size={26} />
            <p className="mt-2 text-sm font-medium text-fg">
              {needle
                ? t("No channels match your search")
                : show === "using"
                  ? t("No channels use this guide yet")
                  : iptv.length
                    ? t("Nothing missing")
                    : t("No IPTV channels yet")}
            </p>
            <p className="text-xs text-fg-muted">
              {needle
                ? t("Try another name, number or group.")
                : show === "using"
                  ? t("Map a channel from Missing or All IPTV, or run Auto-match.")
                  : iptv.length
                    ? t("Every live channel is getting listings.")
                    : t("Add an M3U source to bring in IPTV channels.")}
            </p>
          </div>
        ) : (
          <ul className="divide-y divide-border overflow-hidden rounded-xl border border-border bg-surface">
            {rows.slice(0, limit).map((c) => {
              const cov = coverage?.get(c.id);
              const ok = has(c);
              const guideSrc = c.guideSourceId || c.sourceId;
              return (
                <li
                  key={c.id}
                  className="flex flex-wrap items-center gap-3 px-3 py-2 sm:flex-nowrap"
                >
                  <button
                    type="button"
                    onClick={() => onMapChannel(c.id)}
                    className="flex min-w-0 basis-full items-center gap-2.5 text-left sm:basis-auto sm:flex-1"
                  >
                    <ChannelLogo
                      name={c.name}
                      logo={c.logo}
                      seed={c.id}
                      channelId={c.id}
                      size={30}
                    />
                    <span className="min-w-0">
                      <span className="block truncate text-sm font-medium text-fg">
                        <span className="mr-1.5 font-mono text-xs text-fg-subtle">{c.number}</span>
                        {c.name}
                      </span>
                      <span className="block truncate text-[11px] text-fg-muted">
                        {sourceName(c.sourceId)}
                        {!c.enabled ? ` · ${t("disabled")}` : ""}
                      </span>
                    </span>
                  </button>
                  <span className="hidden min-w-0 flex-1 items-center gap-2 md:flex">
                    {c.guideId ? (
                      <>
                        {ok ? (
                          <Link2 size={13} className="shrink-0 text-success" />
                        ) : (
                          <Link2Off size={13} className="shrink-0 text-warning" />
                        )}
                        <span className="min-w-0 truncate text-xs text-fg-muted">
                          <span className="font-mono">{c.guideId}</span>
                          {guideSrc ? ` · ${sourceName(guideSrc)}` : ""}
                        </span>
                      </>
                    ) : (
                      <span className="text-xs text-fg-subtle">{t("Not mapped")}</span>
                    )}
                  </span>
                  <span className="ml-auto shrink-0 text-right text-[11px] text-fg-muted sm:ml-0 sm:w-36">
                    {ok ? (
                      <Badge variant="success" className="whitespace-nowrap">
                        {cov?.until ? t("to {time}", { time: dayClock(cov.until) }) : t("Listed")}
                      </Badge>
                    ) : (
                      <Badge variant="warning">{t("No listings")}</Badge>
                    )}
                  </span>
                  <Button
                    size="sm"
                    variant={ok ? "ghost" : "outline"}
                    aria-label={t("Map guide for {channel}", { channel: c.name })}
                    onClick={() => onMapChannel(c.id)}
                  >
                    <Link2 size={13} /> {c.guideId ? t("Change") : t("Map")}
                  </Button>
                </li>
              );
            })}
            {rows.length > limit ? (
              <li className="p-2">
                <Button
                  variant="ghost"
                  size="sm"
                  className="w-full"
                  onClick={() => setLimit((l) => l + 200)}
                >
                  {t("Show {count} more", {
                    count: formatNumber(Math.min(200, rows.length - limit)),
                  })}
                </Button>
              </li>
            ) : null}
          </ul>
        )}
      </section>
    </div>
  );
}

const PREVIEW_HOURS = 12;

/** What a channel would show with one guide channel: now, next and later. */
function ListingsPreview({ channel, pick }: { channel: Channel; pick: GuideChannel | null }) {
  const [items, setItems] = useState<Programme[] | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    if (!pick) {
      setItems([]);
      return;
    }
    setItems(null);
    const ctl = new AbortController();
    const from = new Date();
    const until = new Date(from.getTime() + PREVIEW_HOURS * 60 * 60 * 1000);
    tv.preview({ ...channel, guideSourceId: pick.sourceId, guideId: pick.guideId }, ctl.signal, {
      from,
      until,
    })
      .then((r) => {
        setItems((r ?? []).filter((p) => Date.parse(p.end) > from.getTime()).slice(0, 8));
        setError("");
      })
      .catch((e) => {
        if (!isAbort(e)) setError(errorMessage(e));
      });
    return () => ctl.abort();
  }, [channel, pick]);
  if (error) return <p className="text-xs text-danger">{error}</p>;
  if (items === null)
    return <p className="py-6 text-center text-xs text-fg-muted">{t("Loading listings…")}</p>;
  if (!items.length)
    return (
      <p className="rounded-lg border border-dashed border-border px-3 py-6 text-center text-xs text-fg-muted">
        {pick
          ? t("This guide channel has nothing scheduled in the next {count} hours.", {
              count: PREVIEW_HOURS,
            })
          : t("Pick a guide channel to see what it lists.")}
      </p>
    );
  const at = Date.now();
  return (
    <ol className="divide-y divide-border overflow-hidden rounded-xl border border-border bg-surface">
      {items.map((p) => {
        const live = Date.parse(p.start) <= at && Date.parse(p.end) > at;
        return (
          <li key={p.id} className="flex items-start gap-3 px-3 py-2">
            <span className="w-16 shrink-0 pt-0.5 text-xs tabular-nums text-fg-muted">
              {clock(p.start)}
            </span>
            <span className="min-w-0 flex-1">
              <span className="flex items-center gap-1.5">
                <span className="truncate text-sm font-medium text-fg">{p.item.title}</span>
                {live ? <Badge variant="destructive">{t("Live")}</Badge> : null}
              </span>
              {p.item.summary ? (
                <span className="line-clamp-1 text-xs text-fg-muted">{p.item.summary}</span>
              ) : null}
              {live ? (
                <span className="mt-1 block h-0.5 overflow-hidden rounded-full bg-border">
                  <span
                    className="block h-full rounded-full bg-primary/70"
                    style={{ width: `${progress(p, at) * 100}%` }}
                  />
                </span>
              ) : null}
            </span>
          </li>
        );
      })}
    </ol>
  );
}

const sameGuide = (c: Channel, g: GuideChannel) =>
  g.guideId === c.guideId && g.sourceId === (c.guideSourceId || c.sourceId);

/**
 * One channel's guide, in a drawer: the best matching guide channels with
 * their listings previewed before anything is saved.
 */
export function GuideMatchPanel({
  channel,
  sourceName,
  guideSourceKind,
  coverage,
  onUse,
  onOpenChannel,
  sources,
  onReload,
  setFooter,
  scopeSourceId,
  scopeName,
}: {
  channel: Channel;
  sourceName: string;
  guideSourceKind?: string;
  coverage?: Coverage;
  onUse: (c: Channel) => Promise<void>;
  onOpenChannel: () => void;
  sources: Source[];
  onReload: () => Promise<void>;
  /** When opened from a guide source, search that source first. */
  scopeSourceId?: string;
  scopeName?: string;
  setFooter: (footer: React.ReactNode) => void;
}) {
  const [q, setQ] = useState(channel.name);
  const debounced = useDebouncedValue(q, 200);
  const [scope, setScope] = useState<"this" | "all">(scopeSourceId ? "this" : "all");
  const [widened, setWidened] = useState(false);
  const [items, setItems] = useState<GuideChannel[] | null>(null);
  const [error, setError] = useState("");
  const [pick, setPick] = useState<GuideChannel | null>(null);
  const [busy, setBusy] = useState(false);
  const listed = (coverage?.programmes ?? 0) > 0;

  useEffect(() => {
    const ctl = new AbortController();
    const query = debounced.trim();
    const scoped = scope === "this" ? scopeSourceId : undefined;
    tv.guideChannels(query, ctl.signal, 12, scoped)
      .then(async (r) => {
        let list = r ?? [];
        // Nothing in the selected guide: look across every guide rather than dead-end.
        const widen = Boolean(scoped && query && list.length === 0);
        if (widen) list = (await tv.guideChannels(query, ctl.signal, 12)) ?? [];
        setWidened(widen && list.length > 0);
        setItems(list);
        setError("");
        setPick(
          (p) =>
            p ??
            list.find((g) => sameGuide(channel, g)) ??
            list.find((g) => g.programmes > 0) ??
            null,
        );
      })
      .catch((e) => {
        if (!isAbort(e)) setError(errorMessage(e));
      });
    return () => ctl.abort();
  }, [debounced, channel, scope, scopeSourceId]);

  const changed = pick !== null && !sameGuide(channel, pick);
  const use = async (next: Channel) => {
    setBusy(true);
    try {
      await onUse(next);
    } finally {
      setBusy(false);
    }
  };
  useEffect(() => {
    setFooter(
      <div className="flex w-full flex-wrap items-center gap-2">
        {channel.guideId ? (
          <Button
            variant="ghost"
            disabled={busy}
            onClick={() => void use({ ...channel, guideId: "", guideSourceId: "" })}
          >
            <Link2Off size={14} /> {t("Remove guide")}
          </Button>
        ) : null}
        <Button variant="ghost" onClick={onOpenChannel}>
          {t("Open channel")}
        </Button>
        <Button
          className="ml-auto"
          disabled={busy || !changed}
          onClick={() =>
            pick && void use({ ...channel, guideSourceId: pick.sourceId, guideId: pick.guideId })
          }
        >
          <CheckCircle2 size={14} /> {t("Use this guide")}
        </Button>
      </div>,
    );
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [channel, pick, busy, changed]);

  return (
    <div className="space-y-5">
      <div className="flex items-center gap-3 rounded-xl border border-border bg-surface p-3">
        <ChannelLogo
          name={channel.name}
          logo={channel.logo}
          seed={channel.id}
          channelId={channel.id}
          size={40}
        />
        <div className="min-w-0 flex-1">
          <p className="truncate text-sm font-semibold text-fg">
            <span className="mr-1.5 font-mono text-xs text-fg-subtle">{channel.number}</span>
            {channel.name}
          </p>
          <p className="truncate text-xs text-fg-muted">{sourceName}</p>
        </div>
        {listed ? (
          <Badge variant="success">
            {coverage?.until ? t("to {time}", { time: dayClock(coverage.until) }) : t("Listed")}
          </Badge>
        ) : channel.guideId && guideSourceKind === "plex" ? (
          <Badge variant="secondary">{t("Plex guide · listings load when used")}</Badge>
        ) : (
          <Badge variant="warning">{t("No listings")}</Badge>
        )}
      </div>

      <section className="space-y-2">
        <div className="flex flex-wrap items-center gap-2">
          <h3 className="flex-1 text-xs font-semibold uppercase tracking-wider text-fg-subtle">
            {t("Guide channels")}
          </h3>
          <GuideAIAction
            channels={[channel]}
            sources={sources}
            coverage={new Map([[channel.id, coverage ?? { channelId: channel.id, programmes: 0 }]])}
            sourceId={scopeSourceId}
            onApplied={onReload}
          />
          {scopeSourceId ? (
            <Segmented
              ariaLabel={t("Search in")}
              size="sm"
              value={scope}
              onChange={setScope}
              options={[
                { value: "this", label: scopeName || t("This guide") },
                { value: "all", label: t("All guides") },
              ]}
            />
          ) : null}
        </div>
        <div className="relative">
          <Search
            size={14}
            className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-fg-subtle"
            aria-hidden
          />
          <Input
            className="pl-8"
            value={q}
            onChange={(e) => setQ(e.currentTarget.value)}
            placeholder={t("Search guide channels by name or id…")}
            aria-label={t("Search guide channels")}
          />
        </div>
        {error ? <p className="text-xs text-danger">{error}</p> : null}
        {widened ? (
          <p className="text-xs text-fg-muted">
            {t("Nothing in {guide} matches, so these are from every guide.", {
              guide: scopeName || t("this guide"),
            })}
          </p>
        ) : null}
        {items === null ? (
          <p className="py-3 text-center text-xs text-fg-muted">{t("Searching guides…")}</p>
        ) : items.length === 0 ? (
          <p className="rounded-lg border border-dashed border-border px-3 py-4 text-center text-xs text-fg-muted">
            {t("No guide channels match. Try fewer words, or add an XMLTV guide source.")}
          </p>
        ) : (
          <ul className="space-y-1" aria-label={t("Guide channels")}>
            {items.map((g) => {
              const on =
                pick !== null && pick.sourceId === g.sourceId && pick.guideId === g.guideId;
              const current = sameGuide(channel, g);
              return (
                <li key={`${g.sourceId}/${g.guideId}`}>
                  <button
                    type="button"
                    aria-pressed={on}
                    onClick={() => setPick(g)}
                    className={cn(
                      "flex w-full items-center gap-2.5 rounded-lg border p-2 text-left transition-colors",
                      on
                        ? "border-primary/40 bg-primary-subtle ring-1 ring-primary/30"
                        : "border-transparent hover:bg-surface-hover",
                    )}
                  >
                    <ChannelLogo name={g.name} logo={g.logo} seed={g.guideId} size={30} />
                    <span className="min-w-0 flex-1">
                      <span className="flex items-center gap-1.5">
                        <span className="truncate text-sm font-medium text-fg">{g.name}</span>
                        {current ? <Badge variant="secondary">{t("Current")}</Badge> : null}
                      </span>
                      <span className="block truncate text-[11px] text-fg-muted">
                        {g.sourceName}
                      </span>
                    </span>
                    <span
                      className={cn(
                        "shrink-0 text-right text-[11px]",
                        g.programmes || g.sourceKind === "plex" ? "text-fg-muted" : "text-warning",
                      )}
                    >
                      {g.programmes
                        ? g.until
                          ? t("to {time}", { time: dayClock(g.until) })
                          : tn(g.programmes, "{count} listing", "{count} listings")
                        : g.sourceKind === "plex"
                          ? t("Plex guide · listings load when used")
                          : t("No listings")}
                    </span>
                  </button>
                </li>
              );
            })}
          </ul>
        )}
      </section>

      <section className="space-y-2">
        <h3 className="text-xs font-semibold uppercase tracking-wider text-fg-subtle">
          {pick ? t("What {guide} lists", { guide: pick.name }) : t("Listings")}
        </h3>
        <ListingsPreview channel={channel} pick={pick} />
      </section>
    </div>
  );
}
