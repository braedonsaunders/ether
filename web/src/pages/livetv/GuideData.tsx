import { Badge, Button, Input, Popover, cn, toast } from "@braedonsaunders/appkit-ui";
import {
  ArrowRight,
  CalendarPlus,
  CheckCircle2,
  Link2,
  Link2Off,
  Search,
  Wand2,
} from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { Segmented } from "../../components/fields";
import { errorMessage, isAbort } from "../../lib/api";
import { t, tn, tx } from "../../i18n";
import { formatNumber } from "../../lib/format";
import { useDebouncedValue } from "../../lib/hooks";
import { ChannelLogo } from "./ChannelLogo";
import {
  dayClock,
  tv,
  type Channel,
  type Coverage,
  type GuideChannel,
  type MatchChange,
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
  channel: Channel;
  onMapped: (c: Channel) => void | Promise<void>;
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

/**
 * Guide data at a glance: how much of the lineup has listings, one-click
 * matching for the rest, and a row per channel to map by hand.
 */
export function GuideCoverage({
  channels,
  sources,
  coverage,
  onSaveChannel,
  onAddGuideSource,
  onSelectChannel,
  onReload,
}: {
  channels: Channel[];
  sources: Source[];
  coverage: Map<string, Coverage> | null;
  onSaveChannel: (c: Channel) => Promise<void>;
  onAddGuideSource: () => void;
  onSelectChannel: (id: string) => void;
  onReload: () => Promise<void>;
}) {
  const [show, setShow] = useState<"missing" | "all">("missing");
  const [limit, setLimit] = useState(80);
  const [proposals, setProposals] = useState<MatchChange[] | null>(null);
  const [accepted, setAccepted] = useState<Set<string>>(new Set());
  const [busy, setBusy] = useState(false);
  const iptv = useMemo(() => channels.filter((c) => c.kind === "iptv"), [channels]);
  const live = iptv.filter((c) => c.enabled);
  const has = (c: Channel) => (coverage?.get(c.id)?.programmes ?? 0) > 0;
  const missing = live.filter((c) => !has(c));
  const rows = show === "missing" ? missing : iptv;
  const pct = live.length ? (live.length - missing.length) / live.length : 1;
  const sourceName = (id?: string) => sources.find((s) => s.id === id)?.name ?? "";
  const byId = useMemo(() => new Map(channels.map((c) => [c.id, c])), [channels]);

  const propose = async () => {
    setBusy(true);
    try {
      const r = await tv.match({ dryRun: true });
      const changes = r.changes ?? [];
      setProposals(changes);
      setAccepted(new Set(changes.map((c) => c.channelId)));
      if (!changes.length)
        toast.info(t("No new matches found"), {
          description: t("Try adding another guide source."),
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

  return (
    <div className="mx-auto max-w-5xl space-y-5 p-3 sm:p-6">
      <section className="relative overflow-hidden rounded-2xl border border-border bg-surface p-5 shadow-xs">
        <div
          className="pointer-events-none absolute -right-16 -top-24 size-72 rounded-full bg-primary/10 blur-3xl"
          aria-hidden
        />
        <div className="relative flex flex-wrap items-center gap-5">
          <div>
            <p className="text-xs font-semibold uppercase tracking-wider text-fg-subtle">
              {t("Guide data")}
            </p>
            <p className="mt-1 text-3xl font-semibold tracking-tight tabular-nums text-fg">
              {coverage === null ? "—" : `${Math.round(pct * 100)}%`}
              <span className="ml-2 text-sm font-normal text-fg-muted">
                {t("of live IPTV channels have listings")}
              </span>
            </p>
            <p className="mt-1 text-sm text-fg-muted">
              {coverage === null
                ? t("Checking which channels have listings…")
                : missing.length
                  ? t("{missing} of {total} need a guide. Virtual channels build their own.", {
                      missing: formatNumber(missing.length),
                      total: formatNumber(live.length),
                    })
                  : t("Every live channel has listings. Virtual channels build their own.")}
            </p>
          </div>
          <div className="ml-auto flex flex-wrap gap-2">
            <Button variant="outline" onClick={onAddGuideSource}>
              <CalendarPlus size={14} /> {t("Add guide source")}
            </Button>
            <Button onClick={() => void propose()} disabled={busy || coverage === null}>
              <Wand2 size={14} /> {t("Auto-match")}
            </Button>
          </div>
        </div>
        <div className="relative mt-4 h-2 overflow-hidden rounded-full bg-bg-subtle">
          <div
            className={cn(
              "h-full rounded-full transition-all",
              pct >= 0.999 ? "bg-success" : "bg-gradient-to-r from-primary to-info",
            )}
            style={{ width: `${pct * 100}%` }}
          />
        </div>
      </section>

      {proposals && proposals.length ? (
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
          <ul className="max-h-80 space-y-1 overflow-y-auto">
            {proposals.map((p) => {
              const c = byId.get(p.channelId);
              const on = accepted.has(p.channelId);
              return (
                <li key={p.channelId}>
                  <label className="flex cursor-pointer items-center gap-3 rounded-lg bg-surface px-3 py-2 text-sm">
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
                    <span className="min-w-0 flex-1 truncate text-fg">
                      {c?.name ?? p.channelName ?? p.channelId}
                    </span>
                    <ArrowRight size={13} className="shrink-0 text-fg-subtle" />
                    <span className="min-w-0 flex-1 truncate text-fg-muted">
                      {p.guideName}{" "}
                      <span className="text-fg-subtle">
                        · {p.guideSourceName || sourceName(p.guideSourceId)}
                        {p.method === "name" ? ` · ${t("by name")}` : ""}
                      </span>
                    </span>
                  </label>
                </li>
              );
            })}
          </ul>
        </section>
      ) : null}

      <section aria-label={t("Channel guide mapping")} className="space-y-3">
        <div className="flex flex-wrap items-center gap-3">
          <h2 className="text-sm font-semibold text-fg">{t("Channel mapping")}</h2>
          <Segmented
            ariaLabel={t("Show channels")}
            size="sm"
            value={show}
            onChange={(v) => {
              setShow(v);
              setLimit(80);
            }}
            options={[
              { value: "missing", label: t("Missing ({count})", { count: missing.length }) },
              { value: "all", label: t("All IPTV ({count})", { count: iptv.length }) },
            ]}
          />
        </div>
        {rows.length === 0 ? (
          <div className="rounded-xl border border-dashed border-border p-8 text-center">
            <CheckCircle2 className="mx-auto text-success" size={26} />
            <p className="mt-2 text-sm font-medium text-fg">
              {iptv.length ? t("Nothing missing") : t("No IPTV channels yet")}
            </p>
            <p className="text-xs text-fg-muted">
              {iptv.length
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
                <li key={c.id} className="flex items-center gap-3 px-3 py-2">
                  <button
                    type="button"
                    onClick={() => onSelectChannel(c.id)}
                    className="flex min-w-0 flex-1 items-center gap-2.5 text-left"
                  >
                    <ChannelLogo name={c.name} logo={c.logo} seed={c.id} size={30} />
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
                  <span className="w-28 shrink-0 text-right text-[11px] text-fg-muted">
                    {ok ? (
                      <Badge variant="success">
                        {cov?.until ? t("to {time}", { time: dayClock(cov.until) }) : t("Listed")}
                      </Badge>
                    ) : (
                      <Badge variant="warning">{t("No listings")}</Badge>
                    )}
                  </span>
                  <MapGuideButton
                    channel={c}
                    label={c.guideId ? t("Change") : t("Map")}
                    variant={ok ? "ghost" : "outline"}
                    onMapped={onSaveChannel}
                  />
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
