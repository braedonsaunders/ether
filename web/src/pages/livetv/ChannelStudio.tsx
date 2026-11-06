import { ProgrammingEditor } from "./ProgrammingEditor";
import { programmeArtwork } from "./model";
import { styleCopy } from "./onair/naming";
import { rundownPreview } from "./onair/rundown";
import { LookTab } from "./onair/LookTab";
import { RecipeTab } from "./onair/RecipeTab";
import { apiFetch } from "../../lib/api";
import { SubtabNav } from "../../components/layout";
import { Avatar, Badge, Button, Input, Switch, cn } from "@braedonsaunders/appkit-ui";
import {
  KeyRound,
  Play,
  Plus,
  Radio,
  Sparkles,
  Square,
  Star,
  Trash2,
  Users,
  X,
} from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { Artwork } from "../../components/Artwork";
import { ToolbarPicker } from "../../components/ToolbarPicker";
import { DragHandle, SortableList } from "../../components/Sortable";
import { Field } from "../../components/fields";
import { errorMessage, isAbort } from "../../lib/api";
import type { CollectionSpec } from "../../lib/contract";
import type { EtherUser } from "../../lib/types";
import { localized, t, tn, tx } from "../../i18n";
import { relativeTime } from "../../lib/format";
import { useDebouncedValue } from "../../lib/hooks";
import { ChannelLogo } from "./ChannelLogo";
import { MapGuideButton } from "./GuideData";
import { LivePreview } from "./LivePreview";
import { ScheduleTimeline, type TimelineElement } from "./ScheduleTimeline";
import { Drawer } from "../../components/Drawer";
import {
  MINUTE,
  clock,
  duration,
  hueOf,
  onNow,
  programmingMode,
  progress,
  tv,
  upNext,
  policyPermits,
  type Channel,
  type ChannelSummary,
  type Coverage,
  type LiveTVPolicy,
  type Programme,
  type Source,
  type SourceStatus,
  type StreamSource,
} from "./model";
import { useLocale } from "../../i18n/react";

export type Mode = "movies" | "collection" | "genre" | "mix";
export type StudioDraft = {
  channel: Channel;
  mode: Mode;
  genre: string;
  /** Unsaved Viewers-tab switches by viewer id; saved into those viewers' policies. */
  viewers?: Record<string, boolean>;
};
export type CollectionChoice = CollectionSpec & { ratingKey?: string };

export function studioDraft(c: Channel): StudioDraft {
  return {
    channel: structuredClone(c),
    mode: programmingMode(c),
    genre: Array.isArray(c.filter?.genres) ? (c.filter.genres as string[]).join(", ") : "",
  };
}

/** The channel the server would receive, or an error explaining what's missing. */
export function compose(d: StudioDraft): Channel {
  const c = d.channel;
  if (c.kind === "virtual") {
    if (
      !c.personal &&
      !c.blocks?.length &&
      !c.pins?.length &&
      !c.sources?.length &&
      !c.items.length &&
      !c.collection &&
      !c.filter &&
      !c.offAir &&
      !c.fallback?.length
    )
      throw new Error(t("Add at least one programming source."));
    const blocks = [...(c.blocks ?? [])].sort((a, b) => a.start - b.start);
    for (let i = 0; i < blocks.length; i++) {
      const block = blocks[i]!;
      if (
        !Number.isFinite(block.start) ||
        !Number.isFinite(block.end) ||
        block.start < 0 ||
        block.end <= block.start ||
        block.end > 1440
      )
        throw new Error(t("Time blocks must have valid start and end times without overlapping."));
      const days = block.days?.length ? block.days : [0, 1, 2, 3, 4, 5, 6];
      for (let j = 0; j < i; j++) {
        const other = blocks[j]!;
        const otherDays = other.days?.length ? other.days : [0, 1, 2, 3, 4, 5, 6];
        const shareDay = days.some((day) => otherDays.includes(day));
        if (shareDay && block.start < other.end && block.end > other.start)
          throw new Error(
            t("Time blocks must have valid start and end times without overlapping."),
          );
      }
      const sourced = block.filter || block.collection || block.sources?.length;
      if (!c.personal && !block.items.length && !sourced && !c.offAir && !c.fallback?.length)
        throw new Error(t("Add at least one movie to each time block, or remove the empty block."));
    }
    return { ...c, blocks: c.blocks ?? [] };
  }
  return c;
}

/**
 * One channel, edited in place: a live hero (what's on, what's next), its
 * schedule as a ribbon, and tabs for programming, schedule, guide, access
 * and details.
 */
export function ChannelStudio({
  draft,
  onChange,
  isNew,
  dirty,
  guide,
  coverage,
  sources,
  users,
  policies,
  collections,
  now,
  onDelete,
  onMapGuide,
}: {
  draft: StudioDraft;
  onChange: (d: StudioDraft) => void;
  isNew: boolean;
  /** Unsaved edits: the player still tunes the saved channel. */
  dirty?: boolean;
  guide: Programme[];
  coverage?: Coverage;
  sources: Source[];
  users: EtherUser[];
  policies: Map<number, LiveTVPolicy>;
  collections: CollectionChoice[];
  now: number;
  onDelete: () => void;
  onMapGuide: (c: ChannelSummary) => Promise<void>;
}) {
  const c = draft.channel;
  const set = (patch: Partial<Channel>) => onChange({ ...draft, channel: { ...c, ...patch } });
  const virtual = c.kind === "virtual";
  const [tab, setTab] = useState(virtual ? "programming" : "stream");
  const [watching, setWatching] = useState(false);
  const [selectedElement, setSelectedElement] = useState<TimelineElement | null>(null);
  const [editRequest, setEditRequest] = useState<
    { start?: number; blockId?: string; pinId?: string } | undefined
  >();
  const [horizon, setHorizon] = useState<1 | 7>(1);
  // Another channel (or a new one becoming saved) starts on its first tab with
  // the player stopped. Adjusted during render, not in an effect: an effect
  // also runs after mount and could undo a tab picked before it flushed.
  const [shown, setShown] = useState({ id: c.id, isNew });
  if (shown.id !== c.id || shown.isNew !== isNew) {
    setShown({ id: c.id, isNew });
    setTab(virtual ? "programming" : "stream");
    if (shown.id !== c.id) setWatching(false);
  }

  const { locale } = useLocale();
  // Virtual channels preview their draft schedule live; saved IPTV channels
  // show the guide the server already has.
  const composed = useMemo(() => {
    try {
      return { channel: compose(draft), error: "" };
    } catch (e) {
      return { channel: null, error: errorMessage(e) };
    }
  }, [draft, locale]);
  const previewKey = useDebouncedValue(
    virtual && composed.channel ? JSON.stringify(composed.channel) : "",
    500,
  );
  const [preview, setPreview] = useState<Programme[] | null>(null);
  const [previewError, setPreviewError] = useState("");
  useEffect(() => {
    if (!previewKey) {
      setPreview(null);
      return;
    }
    const ctl = new AbortController();
    const from = new Date(Date.now() - 30 * MINUTE);
    const until = new Date(from.getTime() + horizon * 24 * 60 * MINUTE);
    tv.preview(JSON.parse(previewKey) as Channel, ctl.signal, { from, until })
      .then((p) => {
        setPreview(p);
        setPreviewError("");
      })
      .catch((e) => {
        if (!isAbort(e)) setPreviewError(errorMessage(e));
      });
    return () => ctl.abort();
  }, [previewKey, horizon]);

  const [viewer, setViewer] = useState(String(users[0]?.id ?? "1"));
  const [rundown, setRundown] = useState<Programme[]>([]);
  useEffect(() => {
    if (!c.personal) return;
    const ctl = new AbortController();
    setRundown([]);
    apiFetch<Programme[]>(
      `/livetv/channels/${encodeURIComponent(c.id || "draft")}/rundown?viewer=${viewer}`,
      { signal: ctl.signal },
    )
      .then(setRundown)
      .catch((e) => {
        if (!isAbort(e)) setPreviewError(errorMessage(e));
      });
    return () => ctl.abort();
  }, [c.id, Boolean(c.personal), viewer, previewKey]);
  const schedule = c.personal
    ? rundownPreview(rundown, c.look)
    : virtual
      ? (preview ?? (isNew ? [] : guide))
      : guide;
  const live =
    onNow(
      schedule.filter((p) => !p.slate),
      now,
    ) ??
    (onNow(schedule, now)?.slate
      ? schedule.filter((p) => !p.slate && Date.parse(p.end) <= now).at(-1)
      : undefined);
  const next = upNext(
    schedule.filter((p) => !p.slate),
    now,
  );
  const windowFrom = now - 30 * MINUTE;
  const windowUntil = virtual ? now + horizon * 24 * 60 * MINUTE : now + 8 * 60 * MINUTE;
  const hue = hueOf(c.id || c.name || "new");
  const source = sources.find((s) => s.id === c.sourceId);

  const tabs = virtual
    ? [
        { key: "programming", label: c.personal ? t("Recipe") : t("Programming") },
        { key: "schedule", label: t("Schedule"), count: c.blocks?.length || undefined },
        { key: "look", label: styleCopy("Style") },
        { key: "viewers", label: t("Viewers") },
        { key: "details", label: t("Details") },
      ]
    : [
        { key: "stream", label: t("Stream") },
        { key: "guide", label: t("Guide") },
        { key: "look", label: styleCopy("Style") },
        { key: "viewers", label: t("Viewers") },
        { key: "details", label: t("Details") },
      ];

  return (
    <div className="@container min-w-0 pb-10">
      {watching && c.id ? (
        <LivePreview
          channel={c}
          viewer={c.personal ? viewer : undefined}
          onClose={() => setWatching(false)}
        />
      ) : null}
      {/* hero */}
      <section
        className="relative overflow-hidden border-b border-border"
        style={{
          background: `radial-gradient(120% 140% at 0% 0%, hsl(${hue} 70% 45% / 0.28), transparent 60%), radial-gradient(120% 140% at 100% 100%, hsl(${(hue + 60) % 360} 70% 40% / 0.18), transparent 55%)`,
        }}
      >
        {live?.item.thumb ? (
          <Artwork
            src={programmeArtwork(live.item.thumb)}
            alt=""
            title=""
            className="pointer-events-none absolute inset-y-0 right-0 hidden w-1/2 opacity-25 blur-[1px] [mask-image:linear-gradient(to_left,black,transparent)] sm:block"
            imgClassName="object-cover"
          />
        ) : null}
        <div className="relative flex flex-wrap items-start gap-4 p-4 sm:p-6">
          <ChannelLogo
            name={c.name || t("New channel")}
            logo={c.logo}
            seed={c.id || c.name}
            size={72}
            className="rounded-2xl shadow-lg"
          />
          <div className="min-w-0 flex-1 space-y-2">
            <div className="flex flex-wrap items-center gap-2 text-xs">
              <span className="rounded-md bg-fg/90 px-1.5 py-0.5 font-mono font-semibold tabular-nums text-bg">
                {c.number || "—"}
              </span>
              <Badge variant="secondary" className="gap-1">
                {virtual ? <Sparkles size={11} /> : <Radio size={11} />}
                {virtual ? t("Virtual") : source?.name || "IPTV"}
              </Badge>
              {c.group ? <Badge variant="secondary">{c.group}</Badge> : null}
              {!c.enabled ? <Badge variant="warning">{t("Off air")}</Badge> : null}
              {isNew ? <Badge variant="info">{t("Not saved yet")}</Badge> : null}
            </div>
            <input
              value={c.name}
              onChange={(e) => set({ name: e.currentTarget.value })}
              placeholder={t("Name this channel")}
              aria-label={t("Name")}
              className="w-full min-w-0 bg-transparent text-2xl font-semibold tracking-tight text-fg placeholder:text-fg-subtle focus:outline-none"
            />
            {live ? (
              <div className="max-w-xl space-y-1.5">
                <div className="flex items-center gap-2 text-sm">
                  <span className="inline-flex items-center gap-1 rounded bg-danger px-1.5 py-0.5 text-[10px] font-bold uppercase tracking-wider text-white">
                    <span className="size-1.5 animate-pulse rounded-full bg-white" /> {t("On now")}
                  </span>
                  <span className="min-w-0 truncate font-medium text-fg">{live.item.title}</span>
                  <span className="shrink-0 text-xs tabular-nums text-fg-muted">
                    {t("{time} left", { time: duration(Date.parse(live.end) - now) })}
                  </span>
                </div>
                <div className="h-1 overflow-hidden rounded-full bg-fg/10">
                  <div
                    className="h-full rounded-full bg-danger"
                    style={{ width: `${progress(live, now) * 100}%` }}
                  />
                </div>
                {next ? (
                  <p className="truncate text-xs text-fg-muted">
                    {tx("Up next at {time} · {title}", {
                      time: clock(next.start),
                      title: <span className="text-fg">{next.item.title}</span>,
                    })}
                  </p>
                ) : null}
              </div>
            ) : (
              <p className="text-sm text-fg-muted">
                {virtual
                  ? composed.error || previewError || t("Building the schedule…")
                  : coverage && coverage.programmes === 0
                    ? t("No listings for this channel yet. Map a guide channel on the Guide tab.")
                    : t("Live stream")}
              </p>
            )}
          </div>
          <div className="flex shrink-0 items-center gap-3">
            {!isNew ? (
              <button
                type="button"
                onClick={() => setWatching((w) => !w)}
                aria-pressed={watching}
                title={dirty ? t("Plays the saved channel") : undefined}
                className={cn(
                  "group inline-flex h-9 items-center gap-2 rounded-full pl-1 pr-3.5 text-sm font-semibold shadow-sm transition-all",
                  watching
                    ? "bg-danger text-white"
                    : "bg-fg text-bg hover:scale-[1.03] hover:shadow-md",
                )}
              >
                <span
                  className={cn(
                    "grid size-7 place-items-center rounded-full",
                    watching ? "bg-white/20" : "bg-bg/15",
                  )}
                >
                  {watching ? (
                    <Square size={11} className="fill-current" />
                  ) : (
                    <Play size={13} className="ml-0.5 fill-current" />
                  )}
                </span>
                {watching ? t("Stop") : t("Watch")}
              </button>
            ) : null}
            <button
              type="button"
              onClick={() => set({ favorite: !c.favorite })}
              aria-pressed={c.favorite}
              aria-label={t("Favourite")}
              className={cn(
                "grid size-9 place-items-center rounded-full border transition-colors",
                c.favorite
                  ? "border-warning/50 bg-warning/15 text-warning"
                  : "border-border bg-surface text-fg-subtle hover:text-warning",
              )}
            >
              <Star size={16} className={cn(c.favorite && "fill-warning")} />
            </button>
            <label className="flex items-center gap-2 rounded-full border border-border bg-surface py-1 pl-3 pr-1.5 text-sm font-medium">
              {t("On air")}
              <Switch
                aria-label={t("Enabled")}
                checked={c.enabled}
                onChange={(e) => set({ enabled: e.currentTarget.checked })}
              />
            </label>
          </div>
        </div>
        <div className="relative space-y-2 px-4 pb-4 sm:px-6">
          {virtual ? (
            <div
              className="flex flex-wrap items-center justify-end gap-2"
              role="group"
              aria-label={t("Preview window")}
            >
              {c.personal ? (
                <>
                  <span className="text-xs text-fg-muted">{t("Preview as")}</span>
                  <ToolbarPicker
                    value={viewer}
                    onChange={setViewer}
                    ariaLabel={t("Preview as")}
                    searchable
                    searchPlaceholder={t("Find a viewer…")}
                    options={users.map((u) => ({
                      value: String(u.id),
                      label: u.title || u.username,
                      icon: <Avatar name={u.title || u.username} src={u.thumb} size={22} />,
                    }))}
                  />
                </>
              ) : null}
              {(
                [
                  [1, t("Next 24 hours")],
                  [7, t("Next 7 days")],
                ] as const
              ).map(([days, label]) => (
                <button
                  key={days}
                  type="button"
                  aria-pressed={horizon === days}
                  onClick={() => setHorizon(days)}
                  className={cn(
                    "rounded-full px-2.5 py-1 text-xs font-medium",
                    horizon === days
                      ? "bg-primary text-primary-fg"
                      : "bg-surface text-fg-muted ring-1 ring-border",
                  )}
                >
                  {label}
                </button>
              ))}
            </div>
          ) : null}
          <ScheduleTimeline
            channelId={c.id || "draft"}
            range={{ from: windowFrom, to: windowUntil }}
            selectedId={selectedElement?.id}
            onSelect={setSelectedElement}
            onAdd={({ start, blockId }) => {
              setEditRequest({ start, blockId });
              setTab("programming");
            }}
          />
        </div>
      </section>

      {selectedElement && (
        <Drawer
          open
          onClose={() => setSelectedElement(null)}
          title={selectedElement.title || t("Programme")}
          size="md"
        >
          <div className="space-y-4">
            <p className="text-sm text-fg-muted">{selectedElement.subtitle}</p>
            <Field label={t("Start time")}>
              <p>{clock(new Date(selectedElement.start))}</p>
            </Field>
            <Field label={t("End time")}>
              <p>{clock(new Date(selectedElement.end))}</p>
            </Field>
            <Field label={t("Rule")}>
              <p className="text-sm">{selectedElement.rule}</p>
            </Field>
            {selectedElement.pinned && <Badge>{t("Pinned")}</Badge>}
            {selectedElement.breakPoints?.length ? (
              <Badge>
                {tn(
                  selectedElement.breakPoints.length,
                  "{count} detected break point",
                  "{count} detected break points",
                )}
              </Badge>
            ) : null}
            <Button
              onClick={() => {
                setSelectedElement(null);
                setTab("programming");
              }}
            >
              {t("Edit programming")}
            </Button>
          </div>
        </Drawer>
      )}
      <div className="sticky top-0 z-20 border-b border-border bg-bg/95 px-4 backdrop-blur sm:px-6">
        <SubtabNav ariaLabel={t("Channel settings")} active={tab} onSelect={setTab} tabs={tabs} />
      </div>

      <div className="space-y-5 p-4 sm:p-6">
        {tab === "programming" && virtual ? (
          c.personal ? (
            <RecipeTab
              recipe={c.personal}
              onChange={(personal) => set({ personal })}
              rundown={rundown.filter((p) => !p.slate)}
              collections={collections}
            />
          ) : (
            <ProgrammingEditor
              channel={c}
              onChange={set}
              collections={collections}
              editRequest={editRequest}
              onEditHandled={() => setEditRequest(undefined)}
            />
          )
        ) : null}
        {tab === "schedule" && virtual ? (
          <ProgrammingEditor channel={c} onChange={set} collections={collections} schedule />
        ) : null}
        {tab === "stream" && !virtual ? (
          <section className="max-w-2xl space-y-4">
            <StreamSources channel={c} sources={sources} onChange={set} />
            <Field label={t("Group")} htmlFor="ch-group">
              <Input
                id="ch-group"
                value={c.group || ""}
                onChange={(e) => set({ group: e.currentTarget.value })}
              />
            </Field>
            <label className="flex items-start gap-3 rounded-xl border border-border bg-surface p-3">
              <Switch
                checked={c.transcode || false}
                onChange={(e) => set({ transcode: e.currentTarget.checked })}
                aria-label={t("Transcode IPTV codecs for client compatibility")}
              />
              <span>
                <span className="block text-sm font-medium text-fg">
                  {t("Transcode for compatibility")}
                </span>
                <span className="block text-xs text-fg-muted">
                  {t(
                    "Convert unusual codecs so every Plex app can play this channel. Costs CPU while someone watches.",
                  )}
                </span>
              </span>
            </label>
          </section>
        ) : null}
        {tab === "guide" && !virtual ? (
          <GuideTab
            channel={c}
            guide={guide}
            coverage={coverage}
            sources={sources}
            now={now}
            onMap={onMapGuide}
            isNew={isNew}
          />
        ) : null}
        {tab === "look" ? (
          <LookTab
            channel={c}
            onChange={set}
            viewerName={users.find((u) => String(u.id) === viewer)?.title}
          />
        ) : null}
        {tab === "viewers" ? (
          <ViewerAccess
            channelId={c.id}
            users={users}
            policies={policies}
            isPrivate={Boolean(c.private)}
            onPrivate={(next) =>
              onChange({ ...draft, channel: { ...draft.channel, private: next || undefined } })
            }
            pending={draft.viewers ?? {}}
            onChange={(viewers) =>
              onChange({ ...draft, viewers: Object.keys(viewers).length ? viewers : undefined })
            }
          />
        ) : null}
        {tab === "details" ? (
          <section className="max-w-2xl space-y-4">
            <div className="grid gap-4 sm:grid-cols-3">
              <Field label={t("Number")} htmlFor="ch-number">
                <Input
                  id="ch-number"
                  value={c.number}
                  onChange={(e) => set({ number: e.currentTarget.value })}
                  className="font-mono tabular-nums"
                />
              </Field>
              <Field
                label={t("Content rating fallback")}
                htmlFor="ch-rating"
                className="sm:col-span-2"
              >
                <Input
                  id="ch-rating"
                  value={c.rating || ""}
                  onChange={(e) => set({ rating: e.currentTarget.value })}
                  placeholder="TV-PG"
                />
              </Field>
            </div>
            <Field
              label={t("Logo URL")}
              htmlFor="ch-logo"
              hint={t("Leave blank and Ether draws one from the name.")}
            >
              <div className="flex items-center gap-3">
                <ChannelLogo name={c.name} logo={c.logo} seed={c.id || c.name} size={36} />
                <Input
                  id="ch-logo"
                  value={c.logo || ""}
                  onChange={(e) => set({ logo: e.currentTarget.value })}
                  placeholder="https://…/logo.png"
                />
              </div>
            </Field>
            {virtual ? (
              <details className="rounded-xl border border-border bg-surface p-4">
                <summary className="cursor-pointer text-sm font-medium text-fg">
                  {t("Advanced schedule settings")}
                </summary>
                <div className="mt-4 grid gap-4 sm:grid-cols-2">
                  <Field
                    label={t("Schedule anchor (UTC)")}
                    htmlFor="ch-epoch"
                    hint={t("Start time of the repeating running order.")}
                  >
                    <Input
                      id="ch-epoch"
                      type="datetime-local"
                      value={c.epoch.slice(0, 16)}
                      onChange={(e) => set({ epoch: `${e.currentTarget.value}:00Z` })}
                    />
                  </Field>
                  <Field label={t("Shuffle seed")} htmlFor="ch-seed">
                    <Input
                      id="ch-seed"
                      type="number"
                      value={c.seed}
                      onChange={(e) => set({ seed: Number(e.currentTarget.value) })}
                    />
                  </Field>
                </div>
              </details>
            ) : null}
            {!isNew ? (
              <div className="flex items-center justify-between gap-3 rounded-xl border border-danger/30 bg-danger/5 p-4">
                <div>
                  <p className="text-sm font-medium text-fg">
                    {t("Delete {name}", { name: c.name || t("this channel") })}
                  </p>
                  <p className="text-xs text-fg-muted">
                    {virtual
                      ? t("Removes it from every viewer's guide.")
                      : t(
                          "It comes back on the next source refresh unless you turn it off instead.",
                        )}
                  </p>
                </div>
                <Button variant="outline" className="text-danger" onClick={onDelete}>
                  <Trash2 size={14} /> {t("Delete {name}", { name: c.name || t("this channel") })}
                </Button>
              </div>
            ) : null}
          </section>
        ) : null}
      </div>
    </div>
  );
}

function newSourceId(): string {
  return Math.random().toString(36).slice(2, 10);
}

/**
 * Ordered stream sources: the first enabled one plays and the rest take over
 * in order when it fails. A single source keeps using the legacy streamUrl.
 */
function StreamSources({
  channel: c,
  sources,
  onChange,
}: {
  channel: Channel;
  sources: Source[];
  onChange: (p: Partial<Channel>) => void;
}) {
  // Each input signs in with the login it names. "Default" means the
  // playlist's own entry uses the channel's source and added backups go
  // anonymous, so one provider's login never reaches another's URLs.
  const logins = sources.filter((x) => x.auth?.type);
  const loginOptions = [
    {
      value: "",
      label: t("Default"),
      hint: t("Imported entry uses its source's login; added backups use none"),
    },
    ...logins.map((x) => ({
      value: x.id,
      label: x.name,
      hint: x.id === c.sourceId ? t("This channel's source") : t("Use this source's login"),
    })),
  ];
  const list: StreamSource[] = c.streamSources?.length
    ? [...c.streamSources].sort((a, b) => a.priority - b.priority)
    : [{ id: "primary", name: t("Primary"), url: c.streamUrl || "", enabled: true, priority: 0 }];
  const [status, setStatus] = useState<Map<string, SourceStatus>>(new Map());
  useEffect(() => {
    if (!c.id) return;
    const ctl = new AbortController();
    const load = () =>
      tv
        .sourceStatus(c.id, ctl.signal)
        .then((list) => setStatus(new Map(list.map((x) => [x.id, x]))))
        .catch(() => undefined);
    void load();
    const timer = setInterval(load, 30_000);
    return () => {
      ctl.abort();
      clearInterval(timer);
    };
  }, [c.id]);
  const commit = (next: StreamSource[]) => {
    const ranked = next.map((x, i) => ({ ...x, priority: i }));
    const first = ranked.find((x) => x.enabled && x.url.trim());
    onChange({
      // Once a channel has a source list, always send it (even 0/1 entries)
      // so removing a backup actually removes it server-side.
      streamSources: c.streamSources !== undefined || ranked.length > 1 ? ranked : undefined,
      streamUrl: first?.url ?? ranked[0]?.url ?? "",
    });
  };
  const update = (i: number, patch: Partial<StreamSource>) =>
    commit(list.map((x, n) => (n === i ? { ...x, ...patch } : x)));
  // Each source's role by position: 0 is the primary, -1 is off, n is backup n.
  const ranks = new Map<string, number>();
  let rank = 0;
  for (const x of list) {
    const live = x.enabled && x.url.trim() !== "";
    ranks.set(x.id, live ? rank : -1);
    if (live) rank++;
  }
  if (list.length === 1) ranks.set(list[0]!.id, 0);
  return (
    <section className="space-y-3" aria-label={t("Stream sources")}>
      <div className="flex flex-wrap items-end justify-between gap-2">
        <div>
          <h3 className="text-sm font-semibold text-fg">{t("Stream sources")}</h3>
          <p className="text-xs text-fg-muted">
            {t(
              "The first working source plays; if it drops, Ether moves down the list. Only you see these URLs.",
            )}
          </p>
        </div>
        <Button
          variant="outline"
          size="sm"
          onClick={() =>
            commit([
              ...list,
              {
                id: newSourceId(),
                name: t("Backup {number}", { number: list.length }),
                url: "",
                enabled: true,
                priority: list.length,
              },
            ])
          }
        >
          <Plus size={13} /> {t("Add backup")}
        </Button>
      </div>
      <SortableList
        items={list}
        getId={(x) => x.id}
        onReorder={(next) => commit(next)}
        className="space-y-2"
        renderItem={(x, handle, i) => {
          const role = ranks.get(x.id) ?? -1;
          const label =
            role < 0
              ? t("Off")
              : role === 0
                ? t("Primary")
                : t("Backup {number}", { number: role });
          return (
            <div
              className={cn(
                "flex items-center gap-2 rounded-xl border bg-surface p-2",
                role === 0 ? "border-primary/40" : "border-border",
                !x.enabled && "opacity-60",
              )}
            >
              {list.length > 1 ? (
                <DragHandle
                  handle={handle}
                  label={x.name ? t("Reorder {name}", { name: x.name }) : t("Reorder source")}
                  className="size-7"
                />
              ) : null}
              <span
                className={cn(
                  "w-[4.75rem] shrink-0 whitespace-nowrap rounded-md px-1.5 py-0.5 text-center text-[10.5px] font-semibold uppercase tracking-wider",
                  role === 0
                    ? "bg-primary-subtle text-primary"
                    : role < 0
                      ? "bg-bg-subtle text-fg-subtle"
                      : "bg-info/15 text-info",
                )}
              >
                {label}
              </span>
              {list.length > 1 ? (
                <Input
                  value={x.name}
                  onChange={(e) => update(i, { name: e.currentTarget.value })}
                  aria-label={t("Source {number} name", { number: i + 1 })}
                  className="h-8 w-32 shrink-0 text-sm"
                />
              ) : null}
              <SourceHealth status={status.get(x.id)} />
              {logins.length ? (
                <ToolbarPicker
                  ariaLabel={t("Login for source {number}", { number: i + 1 })}
                  value={x.authSourceId ?? ""}
                  onChange={(v) => update(i, { authSourceId: v || undefined })}
                  options={loginOptions.map((o) => ({ ...o, icon: <KeyRound size={13} /> }))}
                  className="h-8 max-w-[9rem] shrink-0 text-xs"
                  menuClassName="w-64"
                />
              ) : null}
              <Input
                value={x.url}
                onChange={(e) => update(i, { url: e.currentTarget.value })}
                aria-label={
                  list.length > 1 ? t("Source {number} URL", { number: i + 1 }) : t("Stream URL")
                }
                placeholder="https://…/stream.m3u8"
                className="h-8 min-w-0 flex-1 font-mono text-xs"
              />
              {list.length > 1 ? (
                <>
                  <Switch
                    checked={x.enabled}
                    onChange={(e) => update(i, { enabled: e.currentTarget.checked })}
                    aria-label={t("Use source {number}", { number: i + 1 })}
                  />
                  <Button
                    variant="ghost"
                    size="sm"
                    className="size-7 p-0 text-fg-subtle hover:text-danger"
                    aria-label={t("Remove source {number}", { number: i + 1 })}
                    onClick={() => commit(list.filter((_, n) => n !== i))}
                  >
                    <X size={14} />
                  </Button>
                </>
              ) : null}
            </div>
          );
        }}
      />
    </section>
  );
}

const HEALTH = localized<Record<string, { label: string; className: string }>>(() => ({
  available: { label: t("Healthy"), className: "bg-success/15 text-success" },
  cooling_down: { label: t("Cooling down"), className: "bg-warning/15 text-warning" },
  retry_ready: { label: t("Will retry"), className: "bg-info/15 text-info" },
  disabled: { label: t("Off"), className: "bg-bg-subtle text-fg-subtle" },
  unchecked: { label: t("Untested"), className: "bg-bg-subtle text-fg-subtle" },
}));

/** Go's zero time ("0001-01-01T00:00:00Z") means "never". */
const when = (iso?: string) => (iso && Date.parse(iso) > 0 ? iso : undefined);

/** Live health of one stream source, from the server's tune history. */
function SourceHealth({ status }: { status?: SourceStatus }) {
  if (!status) return null;
  const played = when(status.lastPlayed);
  const retry = when(status.retryAfter);
  const h = HEALTH[status.state] ?? {
    label: status.state,
    className: "bg-bg-subtle text-fg-subtle",
  };
  const detail = [
    status.failures ? tn(status.failures, "{count} failure", "{count} failures") : "",
    played ? t("played {when}", { when: relativeTime(played) }) : "",
    retry && status.state === "cooling_down"
      ? t("retry {when}", { when: relativeTime(retry) })
      : "",
    status.lastError ?? "",
  ]
    .filter(Boolean)
    .join(" · ");
  return (
    <span
      title={detail || undefined}
      className={cn(
        "shrink-0 whitespace-nowrap rounded-full px-2 py-0.5 text-[11px] font-medium",
        h.className,
      )}
    >
      {h.label}
    </span>
  );
}

function GuideTab({
  channel: c,
  guide,
  coverage,
  sources,
  now,
  onMap,
  isNew,
}: {
  channel: Channel;
  guide: Programme[];
  coverage?: Coverage;
  sources: Source[];
  now: number;
  onMap: (c: ChannelSummary) => Promise<void>;
  isNew: boolean;
}) {
  const src = sources.find((s) => s.id === (c.guideSourceId || c.sourceId));
  const upcoming = guide.filter((p) => Date.parse(p.end) > now).slice(0, 8);
  const ok = (coverage?.programmes ?? guide.length) > 0;
  return (
    <div className="max-w-3xl space-y-4">
      <section className="flex flex-wrap items-center gap-4 rounded-xl border border-border bg-surface p-4">
        <div className="min-w-0 flex-1">
          <p className="text-xs font-semibold uppercase tracking-wider text-fg-subtle">
            {t("Listings from")}
          </p>
          {c.guideId ? (
            <p className="mt-1 truncate text-sm text-fg">
              <span className="font-mono">{c.guideId}</span>
              <span className="text-fg-muted">
                {" "}
                {t("in {source}", { source: src?.name ?? t("an unknown source") })}
              </span>
            </p>
          ) : (
            <p className="mt-1 text-sm text-fg-muted">{t("Not mapped to a guide channel.")}</p>
          )}
          <p className={cn("mt-1 text-xs", ok ? "text-success" : "text-warning")}>
            {ok
              ? tn(
                  coverage?.programmes ?? guide.length,
                  "{count} listing in the next day",
                  "{count} listings in the next day",
                )
              : t("No listings in the next day")}
          </p>
        </div>
        {isNew ? null : (
          <MapGuideButton
            channel={c}
            label={c.guideId ? t("Change guide channel") : t("Map guide channel")}
            variant="default"
            onMapped={onMap}
          />
        )}
      </section>
      {upcoming.length ? (
        <ol className="divide-y divide-border overflow-hidden rounded-xl border border-border bg-surface">
          {upcoming.map((p) => {
            const live = Date.parse(p.start) <= now;
            return (
              <li key={p.id} className="flex gap-3 px-4 py-2.5">
                <span
                  className={cn(
                    "w-20 shrink-0 text-xs tabular-nums",
                    live ? "font-semibold text-danger" : "text-fg-muted",
                  )}
                >
                  {live ? t("Now") : clock(p.start)}
                </span>
                <span className="min-w-0">
                  <span className="block truncate text-sm font-medium text-fg">{p.item.title}</span>
                  {p.item.summary ? (
                    <span className="line-clamp-2 text-xs text-fg-muted">{p.item.summary}</span>
                  ) : null}
                </span>
              </li>
            );
          })}
        </ol>
      ) : null}
    </div>
  );
}

function ViewerAccess({
  channelId,
  users,
  policies,
  isPrivate,
  onPrivate,
  pending,
  onChange,
}: {
  channelId: string;
  users: EtherUser[];
  policies: Map<number, LiveTVPolicy>;
  isPrivate: boolean;
  onPrivate: (next: boolean) => void;
  pending: Record<string, boolean>;
  onChange: (pending: Record<string, boolean>) => void;
}) {
  // Owners always see every channel; the switches edit each viewer's own
  // allowed channels, the same list as their Live TV limits. A private
  // channel shows only to viewers given it, so the baseline follows the
  // Private switch as drafted.
  const viewers = users.filter((u) => !u.isOwner);
  const saved = (id: number) => policyPermits(policies.get(id), channelId, isPrivate);
  const isOn = (id: number) => pending[id] ?? saved(id);
  const off = (id: number) => policies.get(id)?.disabled ?? false;
  const toggle = (next: Record<string, boolean>, id: number, on: boolean) => {
    if (on === saved(id)) delete next[id];
    else next[id] = on;
  };
  const eligible = viewers.filter((u) => !off(u.id));
  const allowed = eligible.filter((u) => isOn(u.id)).length;
  const everyone = allowed === eligible.length;
  return (
    <section className="max-w-3xl space-y-3">
      <label className="flex cursor-pointer items-center gap-3 rounded-xl border border-border bg-surface p-3">
        <span className="min-w-0 flex-1">
          <span className="block text-sm font-medium text-fg">{t("Private")}</span>
          <span className="block text-xs text-fg-muted">
            {t("Only viewers you give it to can watch, including people you add later.")}
          </span>
        </span>
        <Switch
          aria-label={t("Private")}
          checked={isPrivate}
          onChange={(e) => onPrivate(e.currentTarget.checked)}
        />
      </label>
      <div className="flex flex-wrap items-center gap-3 rounded-xl border border-border bg-surface p-3">
        <span className="grid size-9 place-items-center rounded-full bg-primary-subtle text-primary">
          <Users size={16} />
        </span>
        <div className="min-w-0 flex-1">
          <p className="text-sm font-medium text-fg">
            {everyone
              ? t("Everyone with Live TV access")
              : tn(allowed, "Only {count} viewer", "Only {count} viewers")}
          </p>
          <p className="text-xs text-fg-muted">
            {t("Changes each viewer's allowed channels. Their parental limits still apply.")}
          </p>
        </div>
        {!everyone ? (
          <Button
            variant="ghost"
            size="sm"
            onClick={() => {
              const next = { ...pending };
              for (const u of eligible) toggle(next, u.id, true);
              onChange(next);
            }}
          >
            {t("Allow everyone")}
          </Button>
        ) : null}
      </div>
      <div className="grid gap-2 sm:grid-cols-2">
        {viewers.map((user) => {
          const disabled = off(user.id);
          const on = !disabled && isOn(user.id);
          const name = user.title || user.username;
          return (
            <label
              key={user.id}
              className={cn(
                "flex items-center gap-3 rounded-xl border p-2.5 transition-colors",
                disabled ? "cursor-default" : "cursor-pointer",
                on
                  ? "border-primary/40 bg-primary-subtle"
                  : "border-border bg-surface hover:border-border-strong",
              )}
            >
              <Avatar src={user.thumb} name={name} size={30} />
              <span className="min-w-0 flex-1">
                <span className="block truncate text-sm font-medium text-fg">{name}</span>
                {disabled ? (
                  <span className="block text-xs text-fg-muted">{t("Live TV is off")}</span>
                ) : null}
              </span>
              <Switch
                aria-label={name}
                checked={on}
                disabled={disabled}
                onChange={(e) => {
                  const next = { ...pending };
                  toggle(next, user.id, e.currentTarget.checked);
                  onChange(next);
                }}
              />
            </label>
          );
        })}
      </div>
    </section>
  );
}
