import {
  Badge,
  Button,
  Input,
  Popover,
  SubtabNav,
  confirmDialog,
  cn,
  toast,
} from "@braedonsaunders/appkit-ui";
import {
  CalendarRange,
  LayoutGrid,
  ListVideo,
  Plus,
  Save,
  Search,
  Sparkles,
  Tv,
  Undo2,
} from "lucide-react";
import { useCallback, useEffect, useMemo, useState } from "react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";
import { Drawer } from "../../components/Drawer";
import { ErrorPanel } from "../../components/feedback";
import { ToolbarPicker } from "../../components/ToolbarPicker";
import { PanelHead, Workspace } from "../../components/workspace";
import { api, errorMessage } from "../../lib/api";
import { localized, t, tn } from "../../i18n";
import { formatNumber } from "../../lib/format";
import { useInterval } from "../../lib/hooks";
import type { EtherUser } from "../../lib/types";
import {
  ChannelStudio,
  compose,
  studioDraft,
  type CollectionChoice,
  type StudioDraft,
} from "./ChannelStudio";
import { GuideCoverage } from "./GuideData";
import { GuideGrid, OnNowStrip } from "./GuideGrid";
import { ChannelList, SourceList, type Selection } from "./Lineup";
import { SourceEditor, type SourceKind } from "./SourceEditor";
import {
  MINUTE,
  TEMPLATES,
  browserTimeZone,
  byNumber,
  emptyChannel,
  groupBy,
  nextNumber,
  tv,
  type Channel,
  type Coverage,
  type Programme,
  type Source,
} from "./model";

type Filter = "live" | "favorites" | "virtual" | "iptv" | "missing" | "off" | "all";
const FILTERS = localized<{ value: Filter; label: string }[]>(() => [
  { value: "live", label: t("On air") },
  { value: "favorites", label: t("Favourites") },
  { value: "virtual", label: t("Virtual") },
  { value: "iptv", label: "IPTV" },
  { value: "missing", label: t("Needs a guide") },
  { value: "off", label: t("Off air") },
  { value: "all", label: t("Everything") },
]);

const HOUR = 60 * MINUTE;
function windowStart(shiftHours: number): Date {
  const d = new Date();
  d.setMinutes(0, 0, 0);
  return new Date(d.getTime() - HOUR + shiftHours * HOUR);
}

function selectionFrom(params: URLSearchParams, tab?: string): Selection {
  const c = params.get("channel");
  const s = params.get("source");
  if (c === "new") return { kind: "new" };
  if (c) return { kind: "channel", id: c };
  if (s) return { kind: "source", id: s };
  if (tab === "guide-data") return { kind: "coverage" };
  return { kind: "guide" };
}

/**
 * Live TV studio: the lineup on the left third (channels, sources, guide
 * health), and on the right the guide, a channel, a source or the guide-data
 * workbench, all edited in place.
 */
export default function LiveTV() {
  const { tab } = useParams();
  const navigate = useNavigate();
  const [params, setParams] = useSearchParams();
  const selection = selectionFrom(params, tab);
  const [leftTab, setLeftTab] = useState(
    tab === "sources" || tab === "guide-data" ? "sources" : "channels",
  );

  useEffect(() => {
    if (selection.kind === "source" || selection.kind === "coverage") setLeftTab("sources");
  }, [selection.kind]);

  const [channels, setChannels] = useState<Channel[] | null>(null);
  const [sources, setSources] = useState<Source[]>([]);
  const [users, setUsers] = useState<EtherUser[]>([]);
  const [collections, setCollections] = useState<CollectionChoice[]>([]);
  const [programmes, setProgrammes] = useState<Programme[]>([]);
  const [coverageList, setCoverageList] = useState<Coverage[] | null | "unsupported">(null);
  const [error, setError] = useState<unknown>(null);
  const [guideLoading, setGuideLoading] = useState(false);
  const [now, setNow] = useState(() => Date.now());
  const [shift, setShift] = useState(0);
  const [q, setQ] = useState("");
  const [filter, setFilter] = useState<Filter>("live");
  const [draft, setDraft] = useState<StudioDraft | null>(null);
  const [baseline, setBaseline] = useState("");
  const [saving, setSaving] = useState(false);
  const [newKind, setNewKind] = useState<SourceKind>("playlist");

  useInterval(() => setNow(Date.now()), 30_000);
  const from = useMemo(() => windowStart(shift), [shift]);
  const until = useMemo(() => new Date(from.getTime() + 24 * HOUR), [from]);

  const loadCore = useCallback(async (signal?: AbortSignal) => {
    const [c, s] = await Promise.all([tv.channels(signal), tv.sources(signal)]);
    setChannels(c ?? []);
    setSources(s ?? []);
  }, []);
  const loadCoverage = useCallback(async (signal?: AbortSignal) => {
    try {
      setCoverageList((await tv.coverage(signal)) ?? []);
    } catch {
      if (!signal?.aborted) setCoverageList("unsupported");
    }
  }, []);
  const loadGuide = useCallback(
    async (signal?: AbortSignal) => {
      setGuideLoading(true);
      try {
        setProgrammes((await tv.guide(from, until, signal)) ?? []);
      } catch (e) {
        if (!signal?.aborted)
          toast.error(t("Couldn't load the guide"), { description: errorMessage(e) });
      } finally {
        if (!signal?.aborted) setGuideLoading(false);
      }
    },
    [from, until],
  );

  useEffect(() => {
    const ctl = new AbortController();
    Promise.all([loadCore(ctl.signal), api.users(), api.collections()])
      .then(([, u, cs]) => {
        if (ctl.signal.aborted) return;
        setUsers(u);
        setCollections(cs);
      })
      .catch((e) => {
        if (!ctl.signal.aborted) setError(e);
      });
    void loadCoverage(ctl.signal);
    return () => ctl.abort();
  }, [loadCore, loadCoverage]);
  useEffect(() => {
    const ctl = new AbortController();
    void loadGuide(ctl.signal);
    return () => ctl.abort();
  }, [loadGuide]);

  const reloadAll = useCallback(async () => {
    await Promise.all([loadCore(), loadGuide(), loadCoverage()]);
  }, [loadCore, loadGuide, loadCoverage]);

  const guide = useMemo(() => groupBy(programmes, (p) => p.channelId), [programmes]);
  const coverage = useMemo(() => {
    if (coverageList === null) return null;
    if (coverageList === "unsupported") {
      // Older servers: infer from the guide window (enabled channels only).
      const m = new Map<string, Coverage>();
      for (const c of channels ?? [])
        m.set(c.id, { channelId: c.id, programmes: guide.get(c.id)?.length ?? 0 });
      return m;
    }
    return new Map(coverageList.map((c) => [c.channelId, c]));
  }, [coverageList, channels, guide]);
  const covered = useCallback(
    (c: Channel) => {
      if (c.kind === "virtual") return true;
      const v = coverage?.get(c.id);
      return v ? v.programmes > 0 : undefined;
    },
    [coverage],
  );

  const sorted = useMemo(() => [...(channels ?? [])].sort(byNumber), [channels]);
  const filtered = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return sorted.filter((c) => {
      switch (filter) {
        case "live":
          if (!c.enabled) return false;
          break;
        case "favorites":
          if (!c.favorite) return false;
          break;
        case "virtual":
          if (c.kind !== "virtual") return false;
          break;
        case "iptv":
          if (c.kind !== "iptv") return false;
          break;
        case "missing":
          if (!c.enabled || covered(c) !== false) return false;
          break;
        case "off":
          if (c.enabled) return false;
          break;
      }
      if (!needle) return true;
      return `${c.number} ${c.name} ${c.group ?? ""}`.toLowerCase().includes(needle);
    });
  }, [sorted, filter, q, covered]);
  const missingCount = useMemo(
    () =>
      coverage === null
        ? null
        : sorted.filter((c) => c.enabled && c.kind === "iptv" && covered(c) === false).length,
    [sorted, coverage, covered],
  );

  // ---- selection + studio draft -------------------------------------------------

  const savedChannel =
    selection.kind === "channel" ? (channels ?? []).find((c) => c.id === selection.id) : undefined;
  const dirty = draft !== null && JSON.stringify(draft) !== baseline;

  useEffect(() => {
    if (selection.kind === "channel" && savedChannel) {
      const d = studioDraft(savedChannel);
      setDraft(d);
      setBaseline(JSON.stringify(d));
    } else if (selection.kind !== "new") {
      setDraft(null);
      setBaseline("");
    }
    // Only re-seed when the selected record (or its saved copy) changes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selection.kind, selection.kind === "channel" ? selection.id : "", savedChannel]);

  const go = useCallback(
    async (next: Selection, opts?: { force?: boolean }) => {
      if (dirty && !opts?.force) {
        const ok = await confirmDialog({
          title: t("Discard unsaved changes?"),
          message: draft?.channel.name
            ? t("Your changes to {name} haven't been saved.", { name: draft.channel.name })
            : t("Your changes to this channel haven't been saved."),
          confirmLabel: t("Discard"),
          tone: "danger",
        });
        if (!ok) return;
      }
      if (next.kind === "coverage") {
        navigate("/livetv/guide-data");
        return;
      }
      const p = new URLSearchParams();
      if (next.kind === "channel") p.set("channel", next.id);
      if (next.kind === "new") p.set("channel", "new");
      if (next.kind === "source") p.set("source", next.id);
      if (tab === "guide-data") navigate(`/livetv${p.size ? `?${p}` : ""}`);
      else setParams(p);
    },
    [dirty, draft, navigate, setParams, tab],
  );

  const startNew = async (templateKey: string) => {
    const template = TEMPLATES.find((x) => x.key === templateKey) ?? TEMPLATES[0]!;
    const base = emptyChannel();
    base.number = nextNumber(channels ?? []);
    base.timezone = browserTimeZone();
    const d = studioDraft(template.apply(base));
    setDraft(d);
    setBaseline("");
  };

  const saveDraft = async () => {
    if (!draft) return;
    let body: Channel;
    try {
      body = compose(draft);
    } catch (e) {
      toast.error(t("Can't save yet"), { description: errorMessage(e) });
      return;
    }
    if (!body.name.trim()) {
      toast.error(t("Can't save yet"), { description: t("Give the channel a name.") });
      return;
    }
    setSaving(true);
    try {
      const saved = await tv.saveChannel(body);
      toast.success(body.id ? t("Channel saved") : t("Channel created"), {
        description: t("Plex picks it up on the next guide refresh."),
      });
      await loadCore();
      void loadGuide();
      void loadCoverage();
      if (!body.id && saved?.id) await go({ kind: "channel", id: saved.id }, { force: true });
      else if (saved) {
        const d = studioDraft(saved);
        setDraft(d);
        setBaseline(JSON.stringify(d));
      }
    } catch (e) {
      toast.error(t("Couldn't save the channel"), { description: errorMessage(e) });
    } finally {
      setSaving(false);
    }
  };

  const quickSave = async (c: Channel, message?: string) => {
    try {
      await tv.saveChannel(c);
      setChannels((cs) => (cs ?? []).map((x) => (x.id === c.id ? c : x)));
      if (message) toast.success(message);
      void loadGuide();
      void loadCoverage();
    } catch (e) {
      toast.error(t("Couldn't update the channel"), { description: errorMessage(e) });
    }
  };

  const deleteChannel = async () => {
    if (!draft?.channel.id) return;
    const ok = await confirmDialog({
      title: t("Delete {name}?", { name: draft.channel.name }),
      message: t("It disappears from every viewer's guide."),
      confirmLabel: t("Delete channel"),
      tone: "danger",
    });
    if (!ok) return;
    try {
      await tv.deleteChannel(draft.channel.id);
      toast.success(t("Channel deleted"));
      await go({ kind: "guide" }, { force: true });
      await reloadAll();
    } catch (e) {
      toast.error(t("Couldn't delete the channel"), { description: errorMessage(e) });
    }
  };

  // ---- header actions ---------------------------------------------------------------

  const editing = (selection.kind === "channel" || selection.kind === "new") && !!draft;
  const actions = (
    <AddMenu
      onChannel={() => void go({ kind: "new" })}
      onSource={(k) => {
        setNewKind(k);
        setLeftTab("sources");
        void go({ kind: "source", id: "new" });
      }}
    />
  );

  // ---- render -----------------------------------------------------------------------------

  const state = error ? (
    <div className="p-6">
      <ErrorPanel error={error} />
    </div>
  ) : null;

  const aside = (
    <>
      <PanelHead>
        <div className="flex items-center gap-1.5">
          <div className="relative min-w-0 flex-1">
            <Search
              size={13}
              className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-fg-subtle"
              aria-hidden
            />
            <Input
              type="search"
              value={q}
              onChange={(e) => setQ(e.currentTarget.value)}
              placeholder={t("Find a channel or group…")}
              aria-label={t("Search channels")}
              className="h-8 pl-7 text-sm"
            />
          </div>
          <ToolbarPicker
            ariaLabel={t("Show")}
            value={filter}
            onChange={setFilter}
            options={FILTERS.map((f) => ({
              ...f,
              hint:
                f.value === "missing" && missingCount !== null
                  ? tn(missingCount, "{count} channel", "{count} channels")
                  : undefined,
            }))}
            menuClassName="w-48"
          />
        </div>
        <SubtabNav
          ariaLabel={t("Lineup")}
          active={leftTab}
          onSelect={setLeftTab}
          tabs={[
            { key: "channels", label: t("Channels"), count: filtered.length },
            { key: "sources", label: t("Sources"), count: sources.length || undefined },
          ]}
        />
      </PanelHead>
      {leftTab === "channels" ? (
        <>
          <div className="px-2 pt-2">
            <button
              type="button"
              onClick={() => void go({ kind: "guide" })}
              aria-current={selection.kind === "guide" ? "true" : undefined}
              className={cn(
                "flex w-full items-center gap-2.5 rounded-lg px-2 py-2 text-left text-sm font-medium transition-colors",
                selection.kind === "guide"
                  ? "bg-primary-subtle text-primary ring-1 ring-inset ring-primary/30"
                  : "text-fg hover:bg-surface-hover",
              )}
            >
              <span className="grid size-[34px] place-items-center rounded-lg bg-surface text-fg-muted ring-1 ring-border">
                <LayoutGrid size={16} />
              </span>
              <span className="min-w-0 flex-1">
                <span className="block">{t("Guide")}</span>
                <span className="block text-xs font-normal text-fg-muted">
                  {t("The whole lineup, live")}
                </span>
              </span>
            </button>
          </div>
          {channels === null ? (
            <p className="px-4 py-8 text-center text-xs text-fg-muted">{t("Loading channels…")}</p>
          ) : (
            <ChannelList
              channels={filtered}
              sources={sources}
              guide={guide}
              covered={covered}
              selected={selection.kind === "channel" ? selection.id : null}
              onSelect={(id) => void go({ kind: "channel", id })}
              onToggleFavorite={(c) =>
                void quickSave(
                  { ...c, favorite: !c.favorite },
                  c.favorite
                    ? t("{name} unfavourited", { name: c.name })
                    : t("{name} favourited", { name: c.name }),
                )
              }
              now={now}
              emptyHint={
                channels.length ? (
                  <p className="text-xs text-fg-muted">{t("No channels match.")}</p>
                ) : (
                  <div className="space-y-3">
                    <p className="text-sm font-medium text-fg">{t("No channels yet")}</p>
                    <p className="text-xs text-fg-muted">
                      {t(
                        "Build a virtual channel from your catalogue, or bring in IPTV from an M3U playlist.",
                      )}
                    </p>
                    <div className="flex justify-center gap-2">
                      <Button size="sm" onClick={() => void go({ kind: "new" })}>
                        <Sparkles size={13} /> {t("New channel")}
                      </Button>
                    </div>
                  </div>
                )
              }
            />
          )}
        </>
      ) : (
        <SourceList
          sources={sources}
          channels={sorted}
          missing={missingCount}
          selected={selection}
          onSelect={(s) => void go(s)}
        />
      )}
    </>
  );

  const channelOpen = selection.kind === "new" || selection.kind === "channel";
  const sourceOpen = selection.kind === "source";
  const sourceRecord =
    selection.kind === "source" && selection.id !== "new"
      ? (sources.find((s) => s.id === selection.id) ?? null)
      : null;
  const missingSource = selection.kind === "source" && selection.id !== "new" && !sourceRecord;
  const missingChannel =
    selection.kind === "channel" && channels !== null && !savedChannel && !draft;

  let main: React.ReactNode = null;
  let mainClassName = "";
  if (selection.kind === "coverage") {
    main = (
      <GuideCoverage
        channels={sorted}
        sources={sources}
        coverage={coverage}
        onSaveChannel={(c) =>
          quickSave(c, t("{name} mapped to {guide}", { name: c.name, guide: c.guideId ?? "" }))
        }
        onAddGuideSource={() => {
          setNewKind("guide");
          void go({ kind: "source", id: "new" });
        }}
        onSelectChannel={(id) => void go({ kind: "channel", id })}
        onReload={reloadAll}
      />
    );
  } else {
    mainClassName = "flex flex-col lg:overflow-hidden";
    main = (
      <>
        <OnNowStrip
          channels={filtered}
          guide={guide}
          now={now}
          onSelect={(id) => void go({ kind: "channel", id })}
        />
        <div className="min-h-0 flex-1">
          <GuideGrid
            channels={filtered}
            guide={guide}
            from={from}
            until={until}
            now={now}
            selected={selection.kind === "channel" ? selection.id : null}
            onSelect={(id) => void go({ kind: "channel", id })}
            onShift={(h) => setShift((s) => s + h)}
            onNow={() => {
              setShift(0);
              setNow(Date.now());
            }}
            loading={guideLoading}
          />
        </div>
      </>
    );
  }

  const channelTitle =
    selection.kind === "new"
      ? t("New virtual channel")
      : draft?.channel.kind === "iptv"
        ? t("Edit channel")
        : t("Virtual channel");
  const sourceTitle =
    newKind === "guide" || (sourceRecord && sourceRecord.hasPlaylist === false)
      ? sourceRecord
        ? t("Edit guide source")
        : t("Guide source")
      : sourceRecord
        ? t("Edit IPTV source")
        : t("IPTV source");

  const live = sorted.filter((c) => c.enabled).length;
  return (
    <>
      <Workspace
        title={t("Live TV")}
        subtitle={
          channels
            ? missingCount
              ? tn(
                  live,
                  "{count} channel on air · {missing} need a guide · Plex Pass required in Plex",
                  "{count} channels on air · {missing} need a guide · Plex Pass required in Plex",
                  { missing: formatNumber(missingCount) },
                )
              : tn(
                  live,
                  "{count} channel on air · Plex Pass required in Plex",
                  "{count} channels on air · Plex Pass required in Plex",
                )
            : undefined
        }
        actions={actions}
        asideLabel={t("Lineup")}
        mainLabel={t("Live TV workspace")}
        mainClassName={mainClassName}
        aside={aside}
        main={main}
      >
        {state}
      </Workspace>
      <Drawer
        open={channelOpen}
        onClose={() => void go({ kind: "guide" })}
        title={channelTitle}
        description={
          draft?.channel.kind === "iptv"
            ? t("Stream, guide and who can watch.")
            : t("What plays, when it plays, and what happens when it runs out.")
        }
        size="lg"
        footer={
          editing ? (
            <div className="flex w-full flex-wrap items-center gap-2">
              {dirty ? <Badge variant="warning">{t("Unsaved changes")}</Badge> : null}
              <div className="ml-auto flex gap-2">
                <Button
                  variant="outline"
                  size="sm"
                  disabled={(!dirty && selection.kind !== "new") || saving}
                  onClick={() => {
                    if (selection.kind === "new") void go({ kind: "guide" }, { force: true });
                    else setDraft(JSON.parse(baseline) as StudioDraft);
                  }}
                >
                  <Undo2 size={14} /> {selection.kind === "new" ? t("Cancel") : t("Discard")}
                </Button>
                <Button
                  size="sm"
                  disabled={(!dirty && selection.kind !== "new") || saving}
                  onClick={() => void saveDraft()}
                >
                  <Save size={14} />{" "}
                  {saving
                    ? t("Saving…")
                    : selection.kind === "new"
                      ? t("Create channel")
                      : t("Save channel")}
                </Button>
              </div>
            </div>
          ) : null
        }
      >
        {selection.kind === "new" && !draft ? (
          <TemplateGallery onPick={(k) => void startNew(k)} />
        ) : draft ? (
          <ChannelStudio
            draft={draft}
            onChange={setDraft}
            isNew={selection.kind === "new"}
            dirty={dirty}
            guide={(draft.channel.id && guide.get(draft.channel.id)) || []}
            coverage={draft.channel.id ? coverage?.get(draft.channel.id) : undefined}
            sources={sources}
            users={users}
            collections={collections}
            now={now}
            onDelete={() => void deleteChannel()}
            onMapGuide={async (c) => {
              await quickSave(
                c,
                t("{name} now takes listings from {guide}", {
                  name: c.name,
                  guide: c.guideId ?? "",
                }),
              );
              await loadCore();
            }}
          />
        ) : (
          <p className="text-sm text-fg-muted">
            {missingChannel ? t("That channel no longer exists.") : t("Loading…")}
          </p>
        )}
      </Drawer>
      <Drawer
        open={sourceOpen}
        onClose={() => void go({ kind: "guide" })}
        title={sourceTitle}
        description={
          newKind === "guide"
            ? t("XMLTV listings for any channel.")
            : t("An M3U playlist, optionally with a guide.")
        }
        size="lg"
      >
        {missingSource ? (
          <p className="text-sm text-fg-muted">{t("That source no longer exists.")}</p>
        ) : (
          <SourceEditor
            source={selection.kind === "source" && selection.id !== "new" ? sourceRecord : null}
            kind={newKind}
            onSaved={async (s) => {
              await reloadAll();
              if (selection.kind === "source" && selection.id === "new" && s?.id)
                await go({ kind: "source", id: s.id }, { force: true });
            }}
            onDeleted={async () => {
              await go({ kind: "guide" }, { force: true });
              await reloadAll();
            }}
          />
        )}
      </Drawer>
    </>
  );
}

function AddMenu({
  onChannel,
  onSource,
}: {
  onChannel: () => void;
  onSource: (k: SourceKind) => void;
}) {
  const [open, setOpen] = useState(false);
  const item = (icon: React.ReactNode, title: string, hint: string, fn: () => void) => (
    <button
      type="button"
      role="menuitem"
      onClick={() => {
        setOpen(false);
        fn();
      }}
      className="flex w-full items-start gap-3 rounded-md px-2.5 py-2 text-left hover:bg-surface-hover"
    >
      <span className="mt-0.5 text-primary">{icon}</span>
      <span>
        <span className="block text-sm font-medium text-fg">{title}</span>
        <span className="block text-xs text-fg-muted">{hint}</span>
      </span>
    </button>
  );
  return (
    <Popover
      open={open}
      onOpenChange={setOpen}
      align="end"
      className="w-72 p-1"
      trigger={
        <Button
          size="sm"
          onClick={() => setOpen((o) => !o)}
          aria-haspopup="menu"
          aria-expanded={open}
        >
          <Plus size={14} /> {t("Add")}
        </Button>
      }
    >
      <div role="menu" aria-label={t("Add to Live TV")}>
        {item(
          <Sparkles size={16} />,
          t("Virtual channel"),
          t("Programme your own from the catalogue"),
          onChannel,
        )}
        {item(
          <ListVideo size={16} />,
          t("IPTV source"),
          t("An M3U playlist, optionally with a guide"),
          () => onSource("playlist"),
        )}
        {item(
          <CalendarRange size={16} />,
          t("Guide source"),
          t("XMLTV listings for any channel"),
          () => onSource("guide"),
        )}
      </div>
    </Popover>
  );
}

function TemplateGallery({ onPick }: { onPick: (key: string) => void }) {
  return (
    <div className="@container mx-auto max-w-4xl space-y-6">
      <div className="space-y-1">
        <p className="text-xs font-semibold uppercase tracking-wider text-primary">
          {t("New channel")}
        </p>
        <h2 className="text-2xl font-semibold tracking-tight text-fg">
          {t("What should it play?")}
        </h2>
      </div>
      <div className="grid gap-3 @md:grid-cols-2 @4xl:grid-cols-3">
        {TEMPLATES.map((template) => (
          <button
            key={template.key}
            type="button"
            onClick={() => onPick(template.key)}
            aria-label={t("{name} template", { name: template.name })}
            className="group relative overflow-hidden rounded-2xl border border-border p-4 text-left shadow-xs transition-all hover:-translate-y-0.5 hover:shadow-lg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
            style={{
              background: `linear-gradient(140deg, hsl(${template.hue} 70% 50% / 0.18), transparent 60%), rgb(var(--ch-surface))`,
            }}
          >
            <span
              className="mb-6 grid size-10 place-items-center rounded-xl text-white shadow-md"
              style={{
                background: `linear-gradient(135deg, hsl(${template.hue} 70% 55%), hsl(${(template.hue + 40) % 360} 70% 40%))`,
              }}
            >
              {template.key === "blank" ? <Plus size={18} /> : <Tv size={18} />}
            </span>
            <span className="block text-base font-semibold text-fg">{template.name}</span>
            <span className="block text-xs text-fg-muted">{template.tagline}</span>
          </button>
        ))}
      </div>
    </div>
  );
}
