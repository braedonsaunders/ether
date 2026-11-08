import Library, { LibraryAside } from "./library/Library";
import { styleCopy } from "./onair/naming";
import { stylesName } from "./onair/naming";
import { useLooks, LooksAside, LooksGallery, LookSurfaces } from "./onair/LooksGallery";
import { SubtabNav } from "../../components/layout";
import {
  Badge,
  Button,
  Input,
  Popover,
  cn,
  confirmDialog,
  toast,
} from "@braedonsaunders/appkit-ui";
import {
  CalendarRange,
  ListVideo,
  Plus,
  Puzzle,
  Save,
  Search,
  Sparkles,
  Tv,
  UserRound,
  Palette,
  Upload,
  Undo2,
} from "lucide-react";
import { useCallback, useEffect, useId, useLayoutEffect, useMemo, useRef, useState } from "react";
import { useLocale } from "../../i18n/react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";
import { Drawer } from "../../components/Drawer";
import { ErrorPanel } from "../../components/feedback";
import { ToolbarPicker } from "../../components/ToolbarPicker";
import { PanelHead, Workspace } from "../../components/workspace";
import { api, errorMessage } from "../../lib/api";
import { localized, t, tn } from "../../i18n";
import { formatNumber } from "../../lib/format";
import { useDebouncedValue, useInterval, useMediaQuery } from "../../lib/hooks";
import type { EtherUser } from "../../lib/types";
import {
  ChannelStudio,
  compose,
  studioDraft,
  type CollectionChoice,
  type StudioDraft,
} from "./ChannelStudio";
import { GuideMatchPanel, GuideSourceList, GuideWorkbench } from "./GuideData";
import { ChannelBulkBar, type ChannelPatch } from "./ChannelBulkBar";
import { ChannelDrawerBody } from "./ChannelDrawerBody";
import { GuideGrid } from "./GuideGrid";
import { LivePreview } from "./LivePreview";
import { ProgrammeInspector, type InspectedProgramme } from "./ProgrammeInspector";
import { SourcesDashboard } from "./SourcesDashboard";
import { SourceList, type Selection } from "./Lineup";
import type { SourceKind } from "./SourceEditor";
import {
  CHANNELS_PER_PAGE,
  MINUTE,
  TEMPLATES,
  browserTimeZone,
  emptyChannel,
  groupBy,
  isChannelSource,
  plexLineupName,
  tv,
  type Channel,
  type ChannelPage,
  type ChannelSummary,
  type Coverage,
  type LiveTVPolicy,
  type LiveUsage,
  type Programme,
  type Source,
} from "./model";

// Stable event props let the guide skip editor-only updates without retaining
// stale draft/selection closures in bulk actions or navigation.
function useCurrentCallback<A extends unknown[], R>(callback: (...args: A) => R) {
  const latest = useRef(callback);
  useLayoutEffect(() => {
    latest.current = callback;
  });
  return useCallback((...args: A) => latest.current(...args), []);
}

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

function selectionFrom(params: URLSearchParams): Selection {
  const c = params.get("channel");
  const s = params.get("source");
  if (c === "new") return { kind: "new" };
  if (c) return { kind: "channel", id: c };
  if (s) return { kind: "source", id: s };
  const g = params.get("guide");
  if (g) return { kind: "map", id: g };
  return { kind: "guide" };
}

/**
 * Live TV studio: the lineup on the left third (channels, sources, guide
 * health), and on the right the guide, a channel, a source or the guide-data
 * workbench, all edited in place.
 */
export default function LiveTV() {
  const { tab } = useParams();
  const { looks, reels, reload: reloadLooks } = useLooks();
  const navigate = useNavigate();
  const [params, setParams] = useSearchParams();
  const selection = selectionFrom(params);
  // The Guide tab's selected guide source; none means All guides.
  const guideSourceId = params.get("gsrc");
  useEffect(() => {
    if (selection.kind === "source")
      navigate(
        `/sources/iptv?${new URLSearchParams(selection.id === "new" ? { new: "playlist" } : { source: selection.id })}`,
        { replace: true },
      );
    else if (tab === "sources") navigate("/sources/iptv", { replace: true });
  }, [selection.kind, selection.kind === "source" ? selection.id : null, tab, navigate]);
  const [leftTab, setLeftTab] = useState(
    tab === "library"
      ? "library"
      : tab === "looks"
        ? "looks"
        : tab === "guide-data"
          ? "guide"
          : tab === "sources" || selection.kind === "source"
            ? "sources"
            : "channels",
  );

  useEffect(() => {
    // The Guide tab has no route of its own: leaving Looks for Guide lands on
    // /livetv, which must keep the Guide tab rather than fall back to Channels.
    setLeftTab((prev) =>
      tab === "library"
        ? "library"
        : tab === "looks"
          ? "looks"
          : tab === "guide-data"
            ? "guide"
            : tab === "sources"
              ? "sources"
              : prev === "guide"
                ? "guide"
                : "channels",
    );
  }, [tab]);
  useEffect(() => {
    if (selection.kind === "map") setLeftTab("guide");
  }, [selection.kind]);

  const [checkedChannels, setCheckedChannels] = useState<Set<string>>(new Set());
  const [bulkBusy, setBulkBusy] = useState(false);
  const bulkLock = useRef(false);
  const selectionAnchor = useRef<string | null>(null);
  const [page, setPage] = useState<ChannelPage | null>(null);
  const [rows, setRows] = useState<Map<number, Channel>>(new Map());
  const [coverageMap, setCoverageMap] = useState<Record<string, boolean>>({});
  const [allChannels, setAllChannels] = useState<ChannelSummary[] | null>(null);
  const [selectedChannel, setSelectedChannel] = useState<Channel | null>(null);
  const [selectionLoading, setSelectionLoading] = useState(false);
  const [visibleRange, setVisibleRange] = useState<[number, number]>([0, 40]);
  const [pageNonce, setPageNonce] = useState(0);
  const [guideNonce, setGuideNonce] = useState(0);
  const [coverageNonce, setCoverageNonce] = useState(0);
  const [sources, setSources] = useState<Source[]>([]);
  const [users, setUsers] = useState<EtherUser[]>([]);
  const [policies, setPolicies] = useState<Map<number, LiveTVPolicy>>(new Map());
  const [collections, setCollections] = useState<CollectionChoice[]>([]);
  const [programmes, setProgrammes] = useState<Programme[]>([]);
  const [coverageList, setCoverageList] = useState<Coverage[] | null | "unsupported">(null);
  const [rawUsage, setUsage] = useState<LiveUsage | null>(null);
  const usage = useMemo(
    () =>
      rawUsage && {
        ...rawUsage,
        sources: sources.filter(isChannelSource).map((s) => {
          const accountLimit = s.account?.maxConnections ?? 0;
          const limit =
            s.maxConnections && s.maxConnections > 0
              ? accountLimit > 0
                ? Math.min(accountLimit, s.maxConnections)
                : s.maxConnections
              : accountLimit;
          const counters = rawUsage.sources.find((u) => u.sourceId === s.id);
          return {
            sourceId: s.id,
            open: counters?.open ?? 0,
            startsLastHour: counters?.startsLastHour ?? 0,
            limit,
            perHour: s.tunesPerHour ?? 0,
            gapSeconds:
              (s.tuneGapSeconds ?? 0) > 0
                ? s.tuneGapSeconds!
                : (s.tuneGapSeconds ?? 0) < 0
                  ? 0
                  : limit > 0
                    ? 2
                    : 0,
          };
        }),
      },
    [rawUsage, sources],
  );
  const [error, setError] = useState<unknown>(null);
  const [guideLoading, setGuideLoading] = useState(false);
  const [now, setNow] = useState(() => Date.now());
  const [shift, setShift] = useState(0);
  const [q, setQ] = useState("");
  const [filter, setFilter] = useState<Filter>("live");
  // One lineup group (exact name); empty shows every group.
  const [group, setGroup] = useState("");
  const [inspected, setInspected] = useState<InspectedProgramme | null>(null);
  const [inspectOpen, setInspectOpen] = useState(false);
  const [previewChannel, setPreviewChannel] = useState<Channel | null>(null);
  const [draft, setDraft] = useState<StudioDraft | null>(null);
  const [baseline, setBaseline] = useState("");
  const [saving, setSaving] = useState(false);
  const [guideFooter, setGuideFooter] = useState<React.ReactNode>(null);

  useInterval(() => setNow(Date.now()), 30_000);
  // Local server usage only; polling never contacts an IPTV provider.
  const loadUsage = useCallback(async (signal?: AbortSignal) => {
    try {
      setUsage((await tv.usage(signal)) ?? null);
    } catch {
      // Older or unavailable servers must not leave stale usage on screen.
      if (!signal?.aborted) setUsage(null);
    }
  }, []);
  useEffect(() => {
    const ctl = new AbortController();
    void loadUsage(ctl.signal);
    return () => ctl.abort();
  }, [loadUsage]);
  useInterval(() => void loadUsage(), 15_000);
  const from = useMemo(() => windowStart(shift), [shift]);
  const until = useMemo(() => new Date(from.getTime() + 24 * HOUR), [from]);
  const search = useDebouncedValue(q, 200);
  const queryKey = `${search}\u0000${filter}\u0000${group}`;
  const pageState = useRef<{
    key: string;
    loaded: Set<number>;
    loading: Set<number>;
    ctl: AbortController | null;
  }>({ key: "", loaded: new Set(), loading: new Set(), ctl: null });

  useEffect(() => {
    const ctl = new AbortController();
    // Collections only feed the virtual-channel pickers: a failure there
    // leaves the list empty instead of taking the whole page down.
    Promise.all([
      tv.sources(ctl.signal),
      api.users(),
      api.collections().catch(() => []),
      tv.policies(ctl.signal),
    ])
      .then(([s, u, cs, ps]) => {
        if (ctl.signal.aborted) return;
        setSources(s ?? []);
        setUsers(u);
        setCollections(cs);
        setPolicies(new Map((ps ?? []).map((p) => [p.userId, p])));
      })
      .catch((e) => {
        if (!ctl.signal.aborted) setError(e);
      });
    return () => ctl.abort();
  }, []);

  // The lineup is paged and searched on the server; only rows near the
  // viewport (plus one page) are fetched.
  useEffect(() => {
    const ctl = new AbortController();
    const state = {
      key: queryKey,
      loaded: new Set<number>(),
      loading: new Set<number>([0]),
      ctl,
    };
    pageState.current = state;
    setRows(new Map());
    setPage(null);
    tv.channelPage({ offset: 0, limit: CHANNELS_PER_PAGE, q: search, filter, group }, ctl.signal)
      .then((p) => {
        if (ctl.signal.aborted || pageState.current !== state) return;
        state.loaded.add(0);
        setRows(new Map(p.channels.map((c, i) => [i, c])));
        setCoverageMap(p.coverage);
        setPage(p);
        setError(null);
      })
      .catch((e) => {
        if (!ctl.signal.aborted) setError(e);
      });
    return () => ctl.abort();
  }, [queryKey, pageNonce, search, filter, group]);

  const ensureRange = useCallback(
    (start: number, end: number) => {
      const state = pageState.current;
      for (
        let offset = Math.floor(Math.max(0, start) / CHANNELS_PER_PAGE) * CHANNELS_PER_PAGE;
        offset < end;
        offset += CHANNELS_PER_PAGE
      ) {
        if (state.loaded.has(offset) || state.loading.has(offset)) continue;
        state.loading.add(offset);
        tv.channelPage(
          { offset, limit: CHANNELS_PER_PAGE, q: search, filter, group },
          state.ctl?.signal,
        )
          .then((p) => {
            if (pageState.current !== state) return;
            state.loaded.add(offset);
            setRows((prev) => {
              const next = new Map(prev);
              p.channels.forEach((c, i) => next.set(offset + i, c));
              return next;
            });
            setCoverageMap((prev) => ({ ...prev, ...p.coverage }));
          })
          .catch((e) => {
            if (!state.ctl?.signal.aborted && pageState.current === state) setError(e);
          })
          .finally(() => state.loading.delete(offset));
      }
    },
    [search, filter, group],
  );

  const handleRange = useCallback(
    (first: number, last: number) => {
      setVisibleRange((prev) => (prev[0] === first && prev[1] === last ? prev : [first, last]));
      ensureRange(first, last);
    },
    [ensureRange],
  );

  const rowByID = useMemo(() => new Map(Array.from(rows.values()).map((c) => [c.id, c])), [rows]);
  const savedChannel =
    selection.kind === "channel"
      ? (rowByID.get(selection.id) ??
        (selectedChannel?.id === selection.id ? selectedChannel : undefined))
      : undefined;
  const dirty = draft !== null && JSON.stringify(draft) !== baseline;

  useEffect(() => {
    if (selection.kind !== "channel" && selection.kind !== "map") {
      setSelectedChannel(null);
      setSelectionLoading(false);
      return;
    }
    const local = rowByID.get(selection.id);
    if (local) {
      setSelectedChannel(local);
      setSelectionLoading(false);
      return;
    }
    const ctl = new AbortController();
    setSelectionLoading(true);
    tv.channel(selection.id, ctl.signal)
      .then((c) => {
        if (ctl.signal.aborted) return;
        setSelectedChannel(c);
        setSelectionLoading(false);
      })
      .catch(() => {
        if (ctl.signal.aborted) return;
        setSelectedChannel(null);
        setSelectionLoading(false);
      });
    return () => ctl.abort();
  }, [
    selection.kind,
    selection.kind === "channel" || selection.kind === "map" ? selection.id : "",
    rowByID,
  ]);

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

  const guide = useMemo(() => groupBy(programmes, (p) => p.channelId), [programmes]);
  const covered = useCallback(
    (c: Channel) => (c.kind === "virtual" ? true : coverageMap[c.id]),
    [coverageMap],
  );
  const missingCount = page?.counts.missing ?? null;
  const coverage = useMemo(
    () =>
      coverageList && coverageList !== "unsupported"
        ? new Map(coverageList.map((c) => [c.channelId, c]))
        : null,
    [coverageList],
  );
  const channels = allChannels;
  const sorted = useMemo(() => allChannels ?? [], [allChannels]);
  const channelGroups = useMemo(
    () => [...new Set(sorted.map((c) => c.group).filter((g): g is string => Boolean(g)))].sort(),
    [sorted],
  );
  // A renamed or emptied group must not leave the lineup filtered to nothing.
  useEffect(() => {
    if (group && allChannels && !channelGroups.includes(group)) setGroup("");
  }, [group, allChannels, channelGroups]);
  // When listings run out: virtual channels schedule any day on demand, so
  // one with programming lifts the bound; otherwise the last XMLTV listing.
  const guideUntil = useMemo(() => {
    if (!coverage) return undefined;
    let last = 0;
    for (const c of sorted) {
      const cov = c.enabled ? coverage.get(c.id) : undefined;
      if (!cov?.programmes) continue;
      if (c.kind === "virtual") return undefined;
      const until = cov.until ? Date.parse(cov.until) : 0;
      if (until > last) last = until;
    }
    return last;
  }, [coverage, sorted]);
  const query = search;
  const filtered = useMemo(() => {
    const needle = search.trim().toLowerCase();
    if (filter === "missing" && coverage === null) return [];
    return (allChannels ?? []).filter((c) => {
      if (group && c.group !== group) return false;
      if (filter === "live" && !c.enabled) return false;
      if (filter === "favorites" && !c.favorite) return false;
      if (filter === "virtual" && c.kind !== "virtual") return false;
      if (filter === "iptv" && c.kind !== "iptv") return false;
      if (filter === "off" && c.enabled) return false;
      if (
        filter === "missing" &&
        (!c.enabled || c.kind !== "iptv" || (coverage?.get(c.id)?.programmes ?? 0) > 0)
      )
        return false;
      return !needle || `${c.number} ${c.name} ${c.group ?? ""}`.toLowerCase().includes(needle);
    });
  }, [allChannels, search, filter, coverage, group]);
  const indexedChannels = useMemo(() => {
    const a = new Array<Channel | undefined>(page?.total ?? 0).fill(undefined);
    rows.forEach((c, i) => {
      a[i] = c;
    });
    return a;
  }, [rows, page?.total]);

  const guideIDs = useMemo(() => {
    const ids: string[] = [];
    const seen = new Set<string>();
    const add = (c: Channel | undefined) => {
      if (c && !seen.has(c.id)) {
        seen.add(c.id);
        ids.push(c.id);
      }
    };
    // Keep the picture strip supplied while fetching guide rows near the viewport.
    const first = Math.min(120, page?.total ?? 0);
    for (let i = 0; i < first; i++) add(rows.get(i));
    for (let i = visibleRange[0]; i < visibleRange[1]; i++) add(rows.get(i));
    if (selection.kind === "channel" && savedChannel) add(savedChannel);
    return ids;
  }, [rows, visibleRange, page?.total, selection.kind, savedChannel]);
  const debouncedGuideIDs = useDebouncedValue(guideIDs.join(","), 150);
  useEffect(() => {
    if (!debouncedGuideIDs) {
      setProgrammes([]);
      return;
    }
    const ctl = new AbortController();
    setGuideLoading(true);
    tv.guide(from, until, debouncedGuideIDs.split(","), ctl.signal)
      .then((ps) => {
        if (!ctl.signal.aborted) setProgrammes(ps ?? []);
      })
      .catch((e) => {
        if (!ctl.signal.aborted)
          toast.error(t("Couldn't load the guide"), { description: errorMessage(e) });
      })
      .finally(() => {
        if (!ctl.signal.aborted) setGuideLoading(false);
      });
    return () => ctl.abort();
  }, [debouncedGuideIDs, from, until, guideNonce]);

  const loadCoverage = useCallback(async (signal?: AbortSignal) => {
    try {
      setCoverageList((await tv.coverage(signal)) ?? []);
    } catch {
      if (!signal?.aborted) setCoverageList("unsupported");
    }
  }, []);

  useEffect(() => {
    const ctl = new AbortController();
    void loadCoverage(ctl.signal);
    tv.channelSummaries(ctl.signal)
      .then((all) => {
        if (!ctl.signal.aborted) setAllChannels(all ?? []);
      })
      .catch((e) => {
        if (!ctl.signal.aborted) setError(e);
      });
    return () => ctl.abort();
  }, [coverageNonce, loadCoverage]);

  const loadPolicies = useCallback(async (signal?: AbortSignal) => {
    const list = await tv.policies(signal);
    setPolicies(new Map((list ?? []).map((p) => [p.userId, p])));
  }, []);

  const reloadAll = useCallback(async () => {
    setPageNonce((n) => n + 1);
    setGuideNonce((n) => n + 1);
    setCoverageNonce((n) => n + 1);
    try {
      setSources((await tv.sources()) ?? []);
    } catch {
      // Keep the sources already on screen; the next save or reload retries.
    }
  }, [selection.kind]);

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
      if (next.kind === "source") {
        navigate(
          `/sources/iptv?${new URLSearchParams(next.id === "new" ? { new: "playlist" } : { source: next.id })}`,
        );
        return true;
      }
      const p = new URLSearchParams();
      if (next.kind === "channel") p.set("channel", next.id);
      if (next.kind === "new") p.set("channel", "new");
      if (next.kind === "map") p.set("guide", next.id);
      // Guide drawers open over the selected guide source's workbench.
      const gsrc = params.get("gsrc");
      if (gsrc && next.kind !== "channel" && next.kind !== "new") p.set("gsrc", gsrc);
      if (tab === "guide-data") navigate(`/livetv${p.size ? `?${p}` : ""}`);
      else setParams(p);
      return true;
    },
    [dirty, draft, navigate, params, setParams, tab],
  );

  const startNew = async (templateKey: string) => {
    const template = TEMPLATES.find((x) => x.key === templateKey) ?? TEMPLATES[0]!;
    const base = emptyChannel();
    base.number = page?.nextNumber ?? "1";
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
      // Who may watch lives in each viewer's policy, saved once the channel
      // exists.
      const viewers = draft.viewers;
      let viewersError: unknown = null;
      if (viewers && saved?.id) {
        const ids = Object.keys(viewers);
        try {
          await tv.setChannelViewers(saved.id, {
            allow: ids.filter((id) => viewers[id]).map(Number),
            deny: ids.filter((id) => !viewers[id]).map(Number),
          });
          await loadPolicies();
        } catch (e) {
          viewersError = e;
        }
      }
      toast.success(body.id ? t("Channel saved") : t("Channel created"), {
        description: t("Plex's lineup updates in a moment."),
      });
      if (viewersError)
        toast.error(t("Couldn't save who can watch this channel"), {
          description: errorMessage(viewersError),
        });
      if (saved) setSelectedChannel(saved);
      setPageNonce((n) => n + 1);
      setGuideNonce((n) => n + 1);
      setCoverageNonce((n) => n + 1);
      if (saved) {
        const d = studioDraft(saved);
        setDraft(d);
        setBaseline(JSON.stringify(d));
        if (!body.id && saved.id) await go({ kind: "channel", id: saved.id }, { force: true });
      }
    } catch (e) {
      toast.error(t("Couldn't save the channel"), { description: errorMessage(e) });
    } finally {
      setSaving(false);
    }
  };

  const quickSave = async (c: Channel | ChannelSummary, message?: string) => {
    try {
      // Guide-data rows are summaries; merge onto the stored record so a
      // mapping change never drops stream URLs or programming.
      const stored =
        "items" in c && c.items !== undefined ? (c as Channel) : await tv.channel(c.id);
      const saved = await tv.saveChannel({ ...stored, ...c } as Channel);
      setRows((prev) => {
        const next = new Map(prev);
        for (const [i, row] of next) if (row.id === saved.id) next.set(i, saved);
        return next;
      });
      if (selectedChannel?.id === saved.id) setSelectedChannel(saved);
      if (message) toast.success(message);
      setGuideNonce((n) => n + 1);
    } catch (e) {
      toast.error(t("Couldn't update the channel"), { description: errorMessage(e) });
    }
  };

  // Stable lineup callbacks, so memoised rows skip re-rendering on every page update.
  const latest = useRef({ go, quickSave });
  useLayoutEffect(() => {
    latest.current = { go, quickSave };
  });
  const selectChannel = useCallback(
    (id: string) => void latest.current.go({ kind: "channel", id }),
    [],
  );
  const inspectProgramme = useCallback((channel: Channel, programme: Programme) => {
    setInspected({ channel, programme });
    setInspectOpen(true);
  }, []);
  const mapGuide = useCallback(
    (c: Channel) => void latest.current.go({ kind: "map", id: c.id }),
    [],
  );
  const toggleFavorite = useCallback(
    (c: Channel) =>
      void latest.current.quickSave(
        { ...c, favorite: !c.favorite },
        c.favorite
          ? t("{name} unfavourited", { name: c.name })
          : t("{name} favourited", { name: c.name }),
      ),
    [],
  );

  const deleteChannel = async () => {
    if (!draft?.channel.id) return;
    const id = draft.channel.id;
    const name = draft.channel.name || t("this channel");
    const ok = await confirmDialog({
      title: t("Delete {name}?", { name }),
      message: t("It disappears from every viewer's guide."),
      confirmLabel: t("Delete {name}", { name }),
      tone: "danger",
    });
    if (!ok) return;
    try {
      await tv.deleteChannel(id);
      toast.success(t("Channel deleted"));
      await go({ kind: "guide" }, { force: true });
      await reloadAll();
    } catch (e) {
      toast.error(t("Couldn't delete the channel"), { description: errorMessage(e) });
    }
  };

  // ---- header actions ---------------------------------------------------------------

  const editing = (selection.kind === "channel" || selection.kind === "new") && !!draft;
  const addGuideSource = () => {
    void (async () => {
      if (await go({ kind: "guide" })) navigate("/sources/iptv?new=guide");
    })();
  };

  const actions = (
    <AddMenu
      onChannel={() => void go({ kind: "new" })}
      onPersonal={() =>
        void (async () => {
          if (await go({ kind: "new" })) await startNew("personal");
        })()
      }
      onSource={(k) => {
        if (k === "guide") {
          addGuideSource();
          return;
        }
        void (async () => {
          if (await go({ kind: "guide" })) navigate("/sources/iptv?new=playlist");
        })();
      }}
      onAddon={() => {
        void (async () => {
          if (await go({ kind: "guide" })) navigate("/sources/iptv?new=addon");
        })();
      }}
    />
  );

  // ---- render -----------------------------------------------------------------------------

  const state = error ? (
    <div className="p-6">
      <ErrorPanel error={error} />
    </div>
  ) : null;

  const { locale } = useLocale();
  const updateParams = useCurrentCallback(setParams);
  const libraryQuery = params.get("q");
  const librarySort = params.get("sort");
  const panelHead = useMemo(
    () => (
      <PanelHead className="@container flex h-[104px]! flex-col items-stretch! justify-between space-y-0! pt-3! pb-0! border-b! [&>div:last-child]:mb-0">
        {
          <div className="flex items-center gap-1.5">
            <div className="relative min-w-0 flex-1">
              <Search
                size={13}
                className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-fg-subtle"
                aria-hidden
              />
              <Input
                type="search"
                value={leftTab === "library" ? (libraryQuery ?? "") : q}
                onChange={(e) => {
                  const value = e.currentTarget.value;
                  if (leftTab !== "library") setQ(value);
                  else
                    updateParams((p) => {
                      if (value) p.set("q", value);
                      else p.delete("q");
                      return p;
                    });
                }}
                placeholder={
                  leftTab === "library"
                    ? t("Search Library")
                    : leftTab === "looks"
                      ? styleCopy("Search styles…")
                      : leftTab === "sources"
                        ? t("Find a channel, group or source…")
                        : leftTab === "guide"
                          ? t("Find a channel or guide…")
                          : t("Find a channel or group…")
                }
                aria-label={
                  leftTab === "library"
                    ? t("Search Library")
                    : leftTab === "looks"
                      ? styleCopy("Search styles")
                      : t("Search channels")
                }
                className="h-8 pl-7 text-sm"
              />
            </div>
            {leftTab === "library" ? (
              <ToolbarPicker
                ariaLabel={t("Sort assets")}
                value={librarySort ?? "newest"}
                onChange={(value) =>
                  updateParams((p) => {
                    if (value === "newest") p.delete("sort");
                    else p.set("sort", value);
                    return p;
                  })
                }
                options={[
                  { value: "newest", label: t("Newest") },
                  { value: "name", label: t("Name") },
                  { value: "size", label: t("Size") },
                  { value: "used", label: t("Most used") },
                ]}
                menuClassName="w-48"
              />
            ) : leftTab === "looks" ? null : (
              <>
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
                {leftTab === "channels" && channelGroups.length > 0 ? (
                  <ToolbarPicker
                    ariaLabel={t("Group")}
                    value={group}
                    onChange={setGroup}
                    className="max-w-[9rem]"
                    options={[
                      { value: "", label: t("All groups") },
                      ...channelGroups.map((g) => ({ value: g, label: g })),
                    ]}
                  />
                ) : null}
              </>
            )}
          </div>
        }
        <SubtabNav
          ariaLabel={t("Lineup")}
          className="justify-start"
          active={leftTab}
          onSelect={(next) => {
            setLeftTab(next);
            if (next === "library") navigate("/livetv/library");
            else if (next === "looks") navigate("/livetv/looks");
            else if (tab === "looks" || tab === "library")
              navigate(next === "sources" ? "/livetv/sources" : "/livetv");
          }}
          tabs={[
            { key: "channels", label: t("Channels") },
            { key: "guide", label: t("Guide") },
            { key: "looks", label: stylesName() },
            { key: "library", label: t("Library") },
          ]}
        />
      </PanelHead>
    ),
    [
      locale,
      leftTab,
      q,
      filter,
      missingCount,
      libraryQuery,
      librarySort,
      tab,
      navigate,
      updateParams,
      channelGroups,
      group,
    ],
  );
  const lineupEmpty = channels?.length ? (
    <p className="text-xs text-fg-muted">{t("No channels match.")}</p>
  ) : (
    <div className="mx-auto w-full max-w-sm space-y-3 text-center">
      <Tv size={28} className="mx-auto text-primary/70" aria-hidden />
      <p className="text-sm font-medium text-fg">{t("No channels yet")}</p>
      <p className="text-xs text-fg-muted">
        {t("Build a virtual channel from your catalogue, or bring in IPTV from an M3U playlist.")}
      </p>
    </div>
  );

  const aside = (
    <>
      {panelHead}
      <div className="app-scroll min-h-0 flex-1 lg:overflow-y-auto">
        {leftTab === "library" ? (
          <LibraryAside />
        ) : leftTab === "looks" ? (
          <LooksAside
            looks={looks}
            reels={reels}
            query={q}
            onSelect={(kind, id) => setParams(new URLSearchParams({ [kind]: id }))}
          />
        ) : leftTab === "channels" ? null : leftTab === "sources" ? (
          <SourceList
            sources={sources}
            selected={selection}
            onSelect={(s) => void go(s)}
            query={query}
          />
        ) : (
          <GuideSourceList
            query={query}
            lineup={sorted}
            sources={sources}
            selected={guideSourceId}
            onSelect={(id) =>
              setParams(id ? new URLSearchParams({ gsrc: id }) : new URLSearchParams())
            }
            now={now}
          />
        )}
      </div>
    </>
  );

  // Selection follows the current search/filter, never hidden matches or mounted rows only.
  const selectable = useMemo(() => new Set(filtered.map((c) => c.id)), [filtered]);
  useEffect(() => {
    setCheckedChannels((old) => {
      const next = new Set([...old].filter((id) => selectable.has(id)));
      return next.size === old.size ? old : next;
    });
  }, [selectable]);
  const checked = useMemo(
    () => new Set([...checkedChannels].filter((id) => selectable.has(id))),
    [checkedChannels, selectable],
  );
  const selectAllChannels = () => {
    if (!bulkLock.current) setCheckedChannels(new Set(selectable));
  };
  const clearChannels = () => {
    if (!bulkLock.current) {
      setCheckedChannels(new Set());
      selectionAnchor.current = null;
    }
  };
  const toggleChecked = (id: string, range = false) => {
    if (bulkLock.current) return;
    const anchor = selectionAnchor.current;
    selectionAnchor.current = id;
    setCheckedChannels((old) => {
      const next = new Set(old);
      const include = !old.has(id);
      const first = range && anchor ? filtered.findIndex((c) => c.id === anchor) : -1;
      const last = filtered.findIndex((c) => c.id === id);
      const ids =
        first >= 0 && last >= 0
          ? filtered.slice(Math.min(first, last), Math.max(first, last) + 1).map((c) => c.id)
          : [id];
      for (const key of ids) {
        if (include) next.add(key);
        else next.delete(key);
      }
      return next;
    });
  };
  const applyChannels = async (patch: ChannelPatch) => {
    if (bulkLock.current || !checked.size) return false;
    const ids = [...checked];
    const targets = new Set(ids);
    bulkLock.current = true;
    setBulkBusy(true);
    try {
      await tv.updateChannels(ids, patch);
      setAllChannels((cs) =>
        (cs ?? []).map((c) =>
          targets.has(c.id)
            ? { ...c, ...patch, ...(patch.group !== undefined ? { customGroup: true } : {}) }
            : c,
        ),
      );
      setCheckedChannels(new Set());
      selectionAnchor.current = null;
      toast.success(t("Channels updated"));
      setRows(
        (prev) =>
          new Map(
            [...prev].map(([i, c]) => [
              i,
              targets.has(c.id)
                ? { ...c, ...patch, ...(patch.group !== undefined ? { customGroup: true } : {}) }
                : c,
            ]),
          ),
      );
      await reloadAll();
      return true;
    } catch (e) {
      toast.error(t("Couldn't update the channels"), { description: errorMessage(e) });
      return false;
    } finally {
      bulkLock.current = false;
      setBulkBusy(false);
    }
  };

  const channelOpen = selection.kind === "new" || selection.kind === "channel";
  const guideOpen = selection.kind === "map";
  const guideChannel =
    selection.kind === "map"
      ? (rowByID.get(selection.id) ??
        (selectedChannel?.id === selection.id ? selectedChannel : undefined))
      : undefined;
  const missingChannel =
    selection.kind === "channel" && page !== null && !savedChannel && !draft && !selectionLoading;

  // Sources show provider health; Guide shows channel-to-listing mappings.
  const guideSource = guideSourceId ? sources.find((x) => x.id === guideSourceId) : undefined;
  const mainClassName = "";
  const main =
    leftTab === "library" ? (
      <div className="p-4 sm:p-6">
        <Library />
      </div>
    ) : leftTab === "looks" ? (
      <LooksGallery
        looks={looks}
        query={q}
        onNew={() => setParams(new URLSearchParams({ look: "new" }))}
        onSelect={(id) => setParams(new URLSearchParams({ look: id }))}
      />
    ) : leftTab === "guide" ? (
      <GuideWorkbench
        channels={sorted}
        query={query}
        sources={sources}
        coverage={coverage}
        source={guideSource}
        onMapChannel={(id) => void go({ kind: "map", id })}
        onEditSource={(id) => void go({ kind: "source", id })}
        onReload={reloadAll}
        now={now}
      />
    ) : (
      <SourcesDashboard
        sources={sources}
        channels={sorted}
        coverage={coverage}
        usage={usage}
        selected={selection.kind === "source" ? selection.id : null}
        now={now}
        onEdit={(id) => void go({ kind: "source", id })}
      />
    );

  // Channels: one guide whose left third is the lineup (search, tabs and
  // channel rows) and whose right two thirds are the listings, so every
  // channel is listed once and its programmes stay level with it.
  // Keep the full guide mounted at the same width behind the channel drawer.
  // Resizing it on selection would move the virtual rows and horizontal scroll.
  const toggleSelection = useCurrentCallback(toggleChecked);
  const selectMatches = useCurrentCallback(selectAllChannels);
  const clearSelection = useCurrentCallback(clearChannels);
  const applySelection = useCurrentCallback(applyChannels);
  const shiftGuide = useCallback((h: number) => setShift((s) => s + h), []);
  const jumpGuide = useCallback(() => {
    setShift(0);
    setNow(Date.now());
  }, []);
  const selectedID = selection.kind === "channel" ? selection.id : null;
  const channelsLoading = channels === null;
  const lineupGuide = useMemo(
    () => (
      <div className="flex min-h-0 flex-1 flex-col">
        <GuideGrid
          selection={{
            ids: checked,
            busy: bulkBusy,
            all: filtered.length > 0 && checked.size === filtered.length,
            toggle: toggleSelection,
            toggleAll: checked.size === filtered.length ? clearSelection : selectMatches,
          }}
          bulkActions={
            checked.size ? (
              <ChannelBulkBar
                count={checked.size}
                total={filtered.length}
                busy={bulkBusy}
                groups={channelGroups}
                onAll={selectMatches}
                onClear={clearSelection}
                onApply={applySelection}
              />
            ) : undefined
          }
          channels={indexedChannels}
          onRange={handleRange}
          guide={guide}
          from={from}
          until={until}
          now={now}
          selected={selectedID}
          onSelect={selectChannel}
          onInspect={inspectProgramme}
          onPlay={setPreviewChannel}
          onMapGuide={mapGuide}
          guideUntil={guideUntil}
          onShift={shiftGuide}
          onNow={jumpGuide}
          loading={guideLoading}
          head={panelHead}
          covered={covered}
          onToggleFavorite={toggleFavorite}
          emptyHint={
            channelsLoading ? (
              <p className="text-xs text-fg-muted">{t("Loading channels…")}</p>
            ) : (
              lineupEmpty
            )
          }
        />
      </div>
    ),
    [
      locale,
      checked,
      bulkBusy,
      filtered,
      channelGroups,
      indexedChannels,
      handleRange,
      guide,
      from,
      until,
      now,
      selectedID,
      selectChannel,
      inspectProgramme,
      mapGuide,
      guideUntil,
      guideLoading,
      panelHead,
      covered,
      toggleFavorite,
      channelsLoading,
      channels?.length,
      toggleSelection,
      selectMatches,
      clearSelection,
      applySelection,
      shiftGuide,
      jumpGuide,
    ],
  );

  const channelTitle =
    selection.kind === "new"
      ? t("New virtual channel")
      : draft?.channel.kind === "iptv"
        ? t("Edit channel")
        : t("Virtual channel");
  const live = page?.counts.live ?? 0;
  return (
    <>
      <Workspace
        title={t("Live TV")}
        subtitle={
          page
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
        asideClassName={cn(
          "[scrollbar-gutter:auto] lg:overflow-hidden",
          channelOpen && "lg:relative lg:z-[60]",
        )}
        mainLabel={t("Live TV workspace")}
        mainClassName={mainClassName}
        aside={aside}
        main={main}
      >
        {state ?? (leftTab === "channels" ? lineupGuide : undefined)}
      </Workspace>
      <Drawer
        open={guideOpen}
        onClose={() => void go({ kind: "guide" })}
        title={guideChannel ? t("Guide for {channel}", { channel: guideChannel.name }) : t("Guide")}
        description={t(
          "Pick the guide channel its listings come from. Nothing changes until you choose Use this guide.",
        )}
        size="md"
        footer={guideChannel ? guideFooter : undefined}
      >
        {guideChannel ? (
          <GuideMatchPanel
            key={guideChannel.id}
            channel={guideChannel}
            sourceName={sources.find((s) => s.id === guideChannel.sourceId)?.name ?? ""}
            guideSourceKind={
              sources.find((s) => s.id === (guideChannel.guideSourceId || guideChannel.sourceId))
                ?.kind
            }
            coverage={coverage?.get(guideChannel.id)}
            onUse={async (c) => {
              await quickSave(
                c,
                c.guideId
                  ? t("{name} now takes its listings from {guide}", {
                      name: c.name,
                      guide: c.guideId,
                    })
                  : t("Guide removed from {name}", { name: c.name }),
              );
              await go({ kind: "guide" }, { force: true });
            }}
            onOpenChannel={() => void go({ kind: "channel", id: guideChannel.id })}
            sources={sources}
            onReload={reloadAll}
            setFooter={setGuideFooter}
            scopeSourceId={guideSource?.id}
            scopeName={
              guideSource
                ? guideSource.kind === "plex"
                  ? plexLineupName(guideSource.name)
                  : guideSource.name
                : undefined
            }
          />
        ) : channels !== null ? (
          <p className="text-sm text-fg-muted">{t("This channel no longer exists.")}</p>
        ) : null}
      </Drawer>
      <Drawer
        open={channelOpen}
        onClose={() => void go({ kind: "guide" })}
        title={channelTitle}
        bodyClassName={draft ? "overflow-y-auto p-0" : undefined}
        description={
          draft?.channel.kind === "iptv"
            ? t("Stream, guide and who can watch.")
            : t("What plays, when it plays, and what happens when it runs out.")
        }
        size="xl"
        panelClassName="contain-layout contain-paint will-change-transform"
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
        <ChannelDrawerBody>
          {selection.kind === "new" && !draft ? (
            <TemplateGallery onPick={(k) => void startNew(k)} />
          ) : draft ? (
            <ChannelStudio
              draft={draft}
              onChange={setDraft}
              isNew={selection.kind === "new"}
              dirty={dirty}
              guide={(draft.channel.id && guide.get(draft.channel.id)) || []}
              coverage={
                draft.channel.id
                  ? { channelId: draft.channel.id, programmes: covered(draft.channel) ? 1 : 0 }
                  : undefined
              }
              sources={sources}
              users={users}
              policies={policies}
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
                setCoverageNonce((n) => n + 1);
              }}
            />
          ) : (
            <p className="text-sm text-fg-muted">
              {missingChannel ? t("That channel no longer exists.") : t("Loading…")}
            </p>
          )}
        </ChannelDrawerBody>
      </Drawer>
      <ProgrammeInspector
        open={inspectOpen}
        inspected={
          inspected && {
            ...inspected,
            // Follow saves (a favourite toggle) made while the drawer is open.
            channel: rowByID.get(inspected.channel.id) ?? inspected.channel,
          }
        }
        now={now}
        onClose={() => setInspectOpen(false)}
        onExitComplete={() => setInspected(null)}
        onPlay={(c) => {
          setInspectOpen(false);
          setPreviewChannel(c);
        }}
        onOpenChannel={(c) => {
          setInspectOpen(false);
          selectChannel(c.id);
        }}
        onToggleFavorite={toggleFavorite}
      />
      {previewChannel ? (
        <LivePreview
          key={previewChannel.id}
          channel={previewChannel}
          onClose={() => setPreviewChannel(null)}
        />
      ) : null}
      <LookSurfaces looks={looks} reload={reloadLooks} />
    </>
  );
}

// AppKit-style sections; upstream labelled grouping and arrow navigation.
function AddMenu({
  onChannel,
  onPersonal,
  onSource,
  onAddon,
}: {
  onChannel: () => void;
  onPersonal: () => void;
  onSource: (k: SourceKind) => void;
  onAddon: () => void;
}) {
  const [open, setOpen] = useState(false);
  const phone = useMediaQuery("(max-width: 639px)");
  const navigate = useNavigate();
  const id = useId();
  const menu = useRef<HTMLDivElement>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    if (!open) return;
    const frame = requestAnimationFrame(() =>
      menu.current?.querySelector<HTMLButtonElement>('[role="menuitem"]')?.focus(),
    );
    return () => cancelAnimationFrame(frame);
  }, [open]);
  const item = (Icon: typeof Sparkles, title: string, hint: string, fn: () => void) => (
    <button
      type="button"
      role="menuitem"
      tabIndex={-1}
      onClick={() => {
        setOpen(false);
        fn();
      }}
      className="flex w-full items-start gap-3 rounded-md px-2.5 py-2 text-left transition-colors hover:bg-surface-hover focus-visible:bg-surface-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring/50"
    >
      <Icon size={16} className="mt-0.5 shrink-0 text-primary" aria-hidden />
      <span>
        <span className="block text-sm font-medium text-fg">{title}</span>
        <span className="block text-xs text-fg-muted">{hint}</span>
      </span>
    </button>
  );
  const section = (key: string, label: string, children: React.ReactNode) => (
    <div role="group" aria-labelledby={`${id}-${key}`} className="py-1">
      <p id={`${id}-${key}`} className="px-2.5 py-1.5 text-xs font-medium text-fg-subtle">
        {label}
      </p>
      {children}
    </div>
  );
  return (
    <Popover
      open={open}
      onOpenChange={setOpen}
      align={phone ? "start" : "end"}
      className="w-80 max-w-[calc(100vw-1.5rem)] p-1"
      trigger={
        <Button
          ref={trigger}
          size="sm"
          onClick={() => setOpen((o) => !o)}
          aria-haspopup="menu"
          aria-expanded={open}
          aria-controls={`${id}-menu`}
          onKeyDown={(e) => {
            if (e.key === "ArrowDown" || e.key === "ArrowUp") {
              e.preventDefault();
              setOpen(true);
            }
          }}
        >
          <Plus size={14} />
          {t("Add")}
        </Button>
      }
    >
      <div
        ref={menu}
        id={`${id}-menu`}
        role="menu"
        aria-label={t("Add to Live TV")}
        className="app-scroll max-h-[calc(100dvh-10rem)] overflow-y-auto"
        onKeyDown={(e) => {
          const items = [
            ...(menu.current?.querySelectorAll<HTMLButtonElement>('[role="menuitem"]') ?? []),
          ];
          const current = items.indexOf(document.activeElement as HTMLButtonElement);
          let next: number | undefined;
          if (e.key === "ArrowDown") next = (current + 1) % items.length;
          if (e.key === "ArrowUp") next = (current - 1 + items.length) % items.length;
          if (e.key === "Home") next = 0;
          if (e.key === "End") next = items.length - 1;
          if (next !== undefined) {
            e.preventDefault();
            items[next]?.focus();
          }
          if (e.key === "Escape") {
            e.preventDefault();
            setOpen(false);
            trigger.current?.focus();
          }
          if (e.key === "Tab") setOpen(false);
        }}
      >
        {section(
          "yours",
          t("Your channels"),
          <>
            {item(
              Sparkles,
              t("Virtual channel"),
              t("Programme your own from the catalogue"),
              onChannel,
            )}
            {item(
              UserRound,
              t("Personal channel"),
              t("A channel made for each viewer"),
              onPersonal,
            )}
            {item(Palette, styleCopy("Style"), t("Design your channel’s on-air graphics"), () =>
              navigate("/livetv/looks?look=new"),
            )}
            {item(Upload, t("Upload to library"), t("Bumpers, stings, logos and fonts"), () =>
              navigate("/livetv/library?upload=1"),
            )}
            {item(ListVideo, t("Break reel"), t("Clips to play between programmes"), () =>
              navigate("/livetv/looks?reel=new"),
            )}
          </>,
        )}
        <div role="separator" className="mx-2 border-t border-border" />
        {section(
          "bring",
          t("Bring in channels"),
          <>
            {item(Tv, t("IPTV source"), t("Channels from M3U or Xtream"), () =>
              onSource("playlist"),
            )}
            {item(Puzzle, t("Stremio plugin"), t("Live channels from a Stremio plugin"), onAddon)}
          </>,
        )}
        <div role="separator" className="mx-2 border-t border-border" />
        {section(
          "guide",
          t("Program guide"),
          item(CalendarRange, t("Guide source"), t("XMLTV listings for any channel"), () =>
            onSource("guide"),
          ),
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
