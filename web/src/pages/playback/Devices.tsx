import {
  Badge,
  Button,
  EmptyState,
  FilterChips,
  ListNavProvider,
  MultiSelect,
  PagedTable,
  SearchSelect,
  Select,
  SubtabNav,
  type ListSearchParams,
} from "@braedonsaunders/appkit-ui";
import { Plus } from "lucide-react";
import { useMemo, useState } from "react";
import { useLocation, useNavigate, useParams } from "react-router-dom";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { SettingsRow } from "../../components/SettingsRow";
import { StatusDot } from "../../components/status";
import { pagedTableLabels, searchSelectLabels } from "../../i18n/appkit-labels";
import { t, tx } from "../../i18n";
import { api, apiFetch } from "../../lib/api";
import { relativeTime } from "../../lib/format";
import { useAsync } from "../../lib/hooks";
import { BoolRow, RecordDrawer, TextRow } from "../sources/form";
import { codecLabel } from "./hardwareCodecs";

type CompatApp = {
  protocol: string;
  name: string;
  devices: string[];
  videoCodecs: string[] | null;
  subtitles: string[] | null;
  directPlay: string;
  note: string;
  certification: string;
};

type CompatDevice = {
  id?: string;
  name: string;
  dolbyVisionProfiles: number[] | null;
  subtitleCodecs: string[] | null;
  notes: string;
  videoCodecs?: string[] | null;
  maxHeight?: number;
  hdr10?: boolean;
  dolbyVision?: boolean;
  builtinMaxHeight?: number;
  builtinHdr10?: boolean;
  builtinDolbyVision?: boolean;
};

type CapabilityOverride = {
  id?: string;
  product?: string;
  platform?: string;
  device?: string;
  dolbyVisionProfiles?: number[];
  subtitleCodecs?: string[];
  videoCodecs?: string[];
  maxHeight?: number;
  hdr10?: boolean;
  dolbyVision?: boolean;
  notes?: string;
};

type ObservedClient = {
  protocol: string;
  name: string;
  lastSeen: string;
};

type Catalog = {
  apps: CompatApp[];
  devices: CompatDevice[];
  observed: ObservedClient[];
  overrides?: CapabilityOverride[];
};

type Draft = {
  pick: string;
  match: string;
  profiles: string[];
  subtitles: string[];
  video: string[];
  height: string;
  hdr: boolean;
  dolby: boolean;
  product: string;
};

const VIDEO_CHOICES = ["h264", "hevc", "vp9", "av1", "vp8", "mpeg2video", "vc1", "mpeg4"];
const SUBTITLE_CHOICES = ["srt", "ass", "ssa", "vtt", "ttml", "pgs", "vobsub"];
const PROFILE_CHOICES = ["5", "7", "8", "4", "9"];

type Editor =
  | { mode: "create"; view: "apps" | "devices" }
  | { mode: "app"; name: string; product: string; custom: boolean; saved?: CapabilityOverride }
  | {
      mode: "device";
      name: string;
      id?: string;
      device?: string;
      custom: boolean;
      builtin?: CompatDevice;
      saved?: CapabilityOverride;
    };

function protocolLabel(value: string) {
  switch (value) {
    case "plex":
      return "Plex";
    case "jellyfin":
      return "Jellyfin";
    case "emby":
      return "Emby";
    case "silo":
      return "Silo";
    case "stremio":
      return "Stremio";
    case "webdav":
      return t("Files");
    case "dlna":
      return "DLNA";
    default:
      return value;
  }
}

function certificationLabel(value: string) {
  switch (value) {
    case "pending":
      return t("Not certified yet");
    case "unsupported":
      return t("Not supported");
    case "builtin":
      return t("Built in");
    default:
      return value;
  }
}

function deviceNote(note: string) {
  switch (note) {
    case "Text is served separately; image subtitles require explicit client support.":
      return t("Text is served separately; image subtitles require explicit client support.");
    case "Dolby Vision profile 7 uses a compatible HDR base layer; TrueHD requires an audio transcode unless declared supported.":
      return t(
        "Dolby Vision profile 7 uses a compatible HDR base layer; TrueHD requires an audio transcode unless declared supported.",
      );
    case "Browser codec declarations govern playback; text subtitles remain sidecars.":
      return t("Browser codec declarations govern playback; text subtitles remain sidecars.");
    case "The TV or receiver advertises what it can play.":
      return t("The TV or receiver advertises what it can play.");
    default:
      return note;
  }
}

function seenFor(app: CompatApp, observed: ObservedClient[]) {
  return observed.filter(
    (row) =>
      row.protocol === app.protocol &&
      (row.name === app.name || app.name.includes(row.name) || row.name.includes(app.name)),
  );
}

function sameText(a?: string, b?: string) {
  return (a ?? "").trim().toLowerCase() === (b ?? "").trim().toLowerCase();
}

function appKey(app: CompatApp) {
  return `${app.protocol}:${app.name}`;
}

function deviceKey(row: CompatDevice) {
  return row.id ? `id:${row.id}` : `name:${row.name}`;
}

function isAppOverride(item: CapabilityOverride) {
  return !item.id && !item.device && !item.platform && Boolean(item.product);
}

function isDeviceMatch(item: CapabilityOverride) {
  return !item.id && Boolean(item.device);
}

function sameList(a: string[], b: string[]) {
  return a.length === b.length && a.every((item, i) => item === b[i]);
}

function withChoices(base: string[], extra: Array<string | undefined>) {
  const seen = new Set(base);
  const out = [...base];
  for (const value of extra) {
    if (!value || seen.has(value)) continue;
    seen.add(value);
    out.push(value);
  }
  return out;
}

function builtIn(parts: string[]) {
  const detail = parts.filter(Boolean).join(", ");
  return detail ? t("Built in: {detail}", { detail }) : t("Built in");
}

function resolutionName(height: number) {
  if (height >= 2160) return t("4K");
  if (height >= 1080) return t("1080p");
  if (height >= 720) return t("720p");
  return t("{height}p", { height });
}

function blankDraft(): Draft {
  return {
    pick: "",
    match: "",
    profiles: [],
    subtitles: [],
    video: [],
    height: "",
    hdr: false,
    dolby: false,
    product: "",
  };
}

function listsFrom(saved?: CapabilityOverride, builtinHeight = 0) {
  const height = saved?.maxHeight ?? 0;
  return {
    profiles: saved?.dolbyVisionProfiles?.map(String) ?? [],
    subtitles: [...(saved?.subtitleCodecs ?? [])],
    video: [...(saved?.videoCodecs ?? [])],
    height: height > 0 && height !== builtinHeight ? String(height) : "",
    hdr: Boolean(saved?.hdr10),
    dolby: Boolean(saved?.dolbyVision),
  };
}

function queryMap(search: string): ListSearchParams {
  const out: ListSearchParams = {};
  for (const [key, value] of new URLSearchParams(search)) out[key] = value;
  return out;
}

function selected(params: ListSearchParams, key: string) {
  const raw = params[key];
  const value = Array.isArray(raw) ? raw[0] : raw;
  return !value || value === "all" ? "" : value;
}

function CapabilityBadges({ row }: { row: CompatDevice }) {
  const badges = [
    ...(row.videoCodecs ?? []).map((codec) => codecLabel(codec)),
    row.maxHeight ? t("Up to {height}p", { height: row.maxHeight }) : "",
    row.hdr10 ? t("HDR") : "",
    row.dolbyVision ? t("Dolby Vision") : "",
    ...(row.dolbyVisionProfiles ?? []).map((profile) => t("Profile {profile}", { profile })),
  ].filter(Boolean);
  if (!badges.length) {
    return <span className="text-fg-muted">{t("Reported by the app")}</span>;
  }
  return (
    <span className="flex flex-wrap gap-1">
      {badges.map((label) => (
        <Badge key={label} variant="secondary">
          {label}
        </Badge>
      ))}
    </span>
  );
}

function CertificationMark({ value }: { value: string }) {
  if (value === "builtin") return <Badge variant="secondary">{t("Built in")}</Badge>;
  const unsupported = value === "unsupported";
  return (
    <span className="inline-flex items-center gap-1.5 whitespace-nowrap text-xs text-fg-muted">
      <StatusDot level={unsupported ? "warn" : "unknown"} />
      {unsupported
        ? t("Not supported")
        : value === "pending"
          ? t("Not certified yet")
          : certificationLabel(value)}
    </span>
  );
}

function applyCapabilityDiff(
  override: CapabilityOverride,
  draft: Draft,
  baseline: Draft,
  builtinHeight: number,
  keepProduct: boolean,
) {
  if (!sameList(draft.profiles, baseline.profiles)) {
    const profiles = draft.profiles
      .map((part) => Number(part))
      .filter((n) => Number.isInteger(n) && n >= 0);
    if (profiles.length) override.dolbyVisionProfiles = profiles;
    else delete override.dolbyVisionProfiles;
  }
  if (!sameList(draft.subtitles, baseline.subtitles)) {
    if (draft.subtitles.length) override.subtitleCodecs = draft.subtitles;
    else delete override.subtitleCodecs;
  }
  if (!sameList(draft.video, baseline.video)) {
    if (draft.video.length) override.videoCodecs = draft.video;
    else delete override.videoCodecs;
  }
  const height = Number(draft.height);
  if (draft.height !== baseline.height) {
    if (Number.isInteger(height) && height > 0 && height !== builtinHeight)
      override.maxHeight = height;
    else delete override.maxHeight;
  }
  if (draft.hdr !== baseline.hdr) override.hdr10 = draft.hdr;
  if (draft.dolby !== baseline.dolby) override.dolbyVision = draft.dolby;
  if (!keepProduct && draft.product !== baseline.product) {
    if (draft.product.trim()) override.product = draft.product.trim();
    else delete override.product;
  }
}

function capabilityChanged(override: CapabilityOverride) {
  return Object.keys(override).some(
    (key) => !["id", "product", "device", "platform"].includes(key),
  );
}

function ChoiceRow({
  label,
  value,
  onChange,
  options,
  placeholder,
}: {
  label: string;
  value: string[];
  onChange: (value: string[]) => void;
  options: { value: string; label: string }[];
  placeholder: string;
}) {
  return (
    <SettingsRow
      title={label}
      control={
        <div className="w-full sm:w-80">
          <MultiSelect
            ariaLabel={label}
            value={value}
            onChange={onChange}
            options={options}
            placeholder={placeholder}
            searchPlaceholder={t("Search…")}
            noMatchesLabel={t("No matches")}
            clearLabel={t("Clear")}
            selectAllLabel={t("Select all")}
            clearable
            maxVisibleValues={4}
          />
        </div>
      }
    />
  );
}

/** Built-in app and device limits, with owner overrides edited in one drawer. */
export function DeviceCapabilities() {
  const navigate = useNavigate();
  const location = useLocation();
  const { section } = useParams();
  const view = section === "devices" ? "devices" : "apps";
  const params = queryMap(location.search);
  const protocol = selected(params, "protocol");
  const deviceFilter = selected(params, "device");
  const listNav = useMemo(
    () => ({
      pathname: location.pathname,
      search: location.search.replace(/^\?/, ""),
      replace: (href: string) => navigate(href, { replace: true }),
      push: (href: string) => navigate(href),
    }),
    [location.pathname, location.search, navigate],
  );
  const [editing, setEditing] = useState<Editor | null>(null);
  const [draft, setDraft] = useState<Draft>(blankDraft);
  const [baseline, setBaseline] = useState<Draft>(blankDraft);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<unknown>(null);
  const matrix = useAsync(() => apiFetch<Catalog>("/playback/policy"), []);
  const apps = matrix.data?.apps ?? [];
  const devices = matrix.data?.devices ?? [];
  const observed = matrix.data?.observed ?? [];
  const overrides = matrix.data?.overrides ?? [];

  const appOverride = (name: string) =>
    overrides.find((item) => isAppOverride(item) && sameText(item.product, name));
  const deviceOverride = (row: CompatDevice) =>
    row.id
      ? overrides.find((item) => item.id === row.id)
      : overrides.find((item) => isDeviceMatch(item) && sameText(item.device, row.name));

  const openDraft = (editor: Editor, next: Draft) => {
    setDraft(next);
    setBaseline(next);
    setSaveError(null);
    setEditing(editor);
  };

  const openApp = (app: CompatApp) => {
    const saved = appOverride(app.name);
    openDraft(
      { mode: "app", name: app.name, product: app.name, custom: false, saved },
      { ...blankDraft(), pick: appKey(app), ...listsFrom(saved) },
    );
  };

  const openCustomApp = (saved: CapabilityOverride) => {
    const name = saved.product ?? "";
    openDraft(
      { mode: "app", name, product: name, custom: true, saved },
      { ...blankDraft(), match: name, ...listsFrom(saved) },
    );
  };

  const openDevice = (row: CompatDevice) => {
    const saved = deviceOverride(row);
    const lists = listsFrom(saved, row.builtinMaxHeight || 0);
    if (
      !lists.height &&
      row.maxHeight &&
      row.builtinMaxHeight &&
      row.maxHeight !== row.builtinMaxHeight
    ) {
      lists.height = String(row.maxHeight);
    }
    openDraft(
      {
        mode: "device",
        name: row.name,
        id: row.id,
        device: row.id ? undefined : row.name,
        custom: false,
        builtin: row,
        saved,
      },
      {
        pick: deviceKey(row),
        match: "",
        ...lists,
        hdr: Boolean(row.hdr10),
        dolby: Boolean(row.dolbyVision),
        product: saved?.product ?? "",
      },
    );
  };

  const openCustomDevice = (saved: CapabilityOverride) => {
    const name = saved.device ?? "";
    openDraft(
      { mode: "device", name, device: name, custom: true, saved },
      {
        ...blankDraft(),
        match: name,
        ...listsFrom(saved),
        product: saved.product ?? "",
      },
    );
  };

  const openCreate = () => {
    openDraft({ mode: "create", view }, blankDraft());
  };

  const knownApp = (key: string) => apps.find((app) => appKey(app) === key);
  const knownDevice = (key: string) => devices.find((row) => deviceKey(row) === key);

  const resolved = (): { saved?: CapabilityOverride; override: CapabilityOverride } | null => {
    if (!editing) return null;
    if (editing.mode === "app") {
      const product = editing.custom ? draft.match.trim() : editing.product;
      if (!product) return null;
      const override: CapabilityOverride = { ...editing.saved, product };
      delete override.id;
      delete override.device;
      delete override.platform;
      return { saved: editing.saved, override };
    }
    if (editing.mode === "device" && !draft.match.trim()) {
      const override: CapabilityOverride = { ...editing.saved };
      if (editing.id) override.id = editing.id;
      else if (editing.device) override.device = editing.device;
      else return null;
      return { saved: editing.saved, override };
    }
    const match = draft.match.trim();
    if (match) {
      const appsSide = editing.mode === "create" && editing.view === "apps";
      const saved = appsSide
        ? appOverride(match)
        : overrides.find((item) => isDeviceMatch(item) && sameText(item.device, match));
      const override: CapabilityOverride = { ...saved };
      delete override.id;
      delete override.platform;
      if (appsSide) {
        delete override.device;
        override.product = match;
      } else {
        override.device = match;
      }
      return { saved, override };
    }
    if (editing.mode !== "create" || !draft.pick) return null;
    if (editing.view === "apps") {
      const app = knownApp(draft.pick);
      if (!app) return null;
      const saved = appOverride(app.name);
      const override: CapabilityOverride = { ...saved, product: app.name };
      delete override.id;
      delete override.device;
      delete override.platform;
      return { saved, override };
    }
    const row = knownDevice(draft.pick);
    if (!row) return null;
    const saved = deviceOverride(row);
    const override: CapabilityOverride = { ...saved };
    if (row.id) override.id = row.id;
    else override.device = row.name;
    return { saved, override };
  };

  const persist = async (next: CapabilityOverride[], close: boolean) => {
    setSaving(true);
    setSaveError(null);
    try {
      await api.putSection("clients", { overrides: next });
      if (close) setEditing(null);
      await matrix.reload();
    } catch (err) {
      setSaveError(err);
    } finally {
      setSaving(false);
    }
  };

  const save = async () => {
    const target = resolved();
    if (!target) return;
    const builtin =
      editing?.mode === "device"
        ? editing.builtin
        : editing?.mode === "create" && editing.view === "devices" && !draft.match.trim()
          ? knownDevice(draft.pick)
          : undefined;
    applyCapabilityDiff(
      target.override,
      draft,
      baseline,
      builtin?.builtinMaxHeight ?? builtin?.maxHeight ?? 0,
      editing?.mode === "app" || (editing?.mode === "create" && editing.view === "apps"),
    );
    const renamed =
      (target.override.product ?? "") !== (target.saved?.product ?? "") ||
      (target.override.device ?? "") !== (target.saved?.device ?? "");
    if (!capabilityChanged(target.override) && !renamed) {
      setEditing(null);
      return;
    }
    const others = overrides.filter((item) => item !== target.saved);
    await persist([...others, target.override], true);
  };

  const remove = async (saved: CapabilityOverride) => {
    await persist(
      overrides.filter((item) => item !== saved),
      true,
    );
  };

  const protocolOptions = [...new Set(apps.map((app) => app.protocol))].map((value) => ({
    value,
    label: protocolLabel(value),
    count: apps.filter((app) => app.protocol === value).length,
  }));
  const deviceOptions = [...new Set(apps.flatMap((app) => app.devices))].sort().map((name) => ({
    value: name,
    label: name,
    count: apps.filter((app) => app.devices.includes(name)).length,
  }));

  const visibleApps = apps.filter(
    (app) =>
      (!protocol || app.protocol === protocol) &&
      (!deviceFilter || app.devices.includes(deviceFilter)),
  );
  const customApps = overrides.filter(
    (item) => isAppOverride(item) && !apps.some((app) => sameText(app.name, item.product)),
  );
  const customDevices = overrides.filter(
    (item) => isDeviceMatch(item) && !devices.some((row) => sameText(row.name, item.device)),
  );

  const appRows = [
    ...visibleApps.map((app) => ({ kind: "known" as const, app, saved: appOverride(app.name) })),
    ...(protocol || deviceFilter
      ? []
      : customApps.map((saved) => ({ kind: "custom" as const, saved }))),
  ];
  const deviceRows = [
    ...devices.map((row) => ({ kind: "known" as const, row, saved: deviceOverride(row) })),
    ...customDevices.map((saved) => ({ kind: "custom" as const, saved })),
  ];

  const builtin =
    editing?.mode === "device"
      ? editing.builtin
      : editing?.mode === "create" && editing.view === "devices" && !draft.match.trim()
        ? knownDevice(draft.pick)
        : undefined;
  const savedNow = resolved()?.saved;
  const showProduct =
    editing?.mode === "device" || (editing?.mode === "create" && editing.view === "devices");
  const showMatch = editing?.mode === "create" || editing?.custom === true;
  const sourceApp =
    editing?.mode === "app" && !editing.custom
      ? apps.find((app) => sameText(app.name, editing.name))
      : editing?.mode === "create" && editing.view === "apps" && !draft.match.trim()
        ? knownApp(draft.pick)
        : undefined;
  const profileOptions = withChoices(PROFILE_CHOICES, [
    ...(builtin?.dolbyVisionProfiles ?? []).map(String),
    ...draft.profiles,
  ]).map((value) => ({ value, label: t("Profile {profile}", { profile: value }) }));
  const videoOptions = withChoices(VIDEO_CHOICES, [
    ...(builtin?.videoCodecs ?? sourceApp?.videoCodecs ?? []),
    ...draft.video,
  ]).map((value) => ({ value, label: codecLabel(value) }));
  const subtitleOptions = withChoices(SUBTITLE_CHOICES, [
    ...(builtin?.subtitleCodecs ?? sourceApp?.subtitles ?? []),
    ...draft.subtitles,
  ]).map((value) => ({ value, label: value }));
  const profilePlaceholder = builtIn(
    (builtin?.dolbyVisionProfiles ?? []).map((profile) => t("Profile {profile}", { profile })),
  );
  const videoPlaceholder = builtIn(
    (builtin?.videoCodecs ?? sourceApp?.videoCodecs ?? []).map((codec) => codecLabel(codec)),
  );
  const subtitlePlaceholder = builtIn(builtin?.subtitleCodecs ?? sourceApp?.subtitles ?? []);
  const heightBuiltin = builtin?.builtinMaxHeight || builtin?.maxHeight || 0;
  const heightPlaceholder = builtIn(heightBuiltin ? [resolutionName(heightBuiltin)] : []);
  const heightOptions = withChoices(["720", "1080", "2160"], [draft.height]);

  return (
    <ListNavProvider value={listNav}>
      <div className="space-y-4">
        <SubtabNav
          ariaLabel={t("App compatibility sections")}
          active={view}
          onSelect={(key) => navigate(`/playback/devices/${key}`)}
          tabs={[
            { key: "apps", label: t("Apps") },
            { key: "devices", label: t("Devices") },
          ]}
        />
        {matrix.error ? (
          <ErrorPanel error={matrix.error} onRetry={() => void matrix.reload()} />
        ) : !matrix.data ? (
          <LoadingRows />
        ) : (
          <>
            <div className="flex flex-wrap items-center gap-2">
              <Button size="sm" onClick={openCreate}>
                <Plus size={14} /> {t("Add")}
              </Button>
              {view === "apps" ? (
                <>
                  <FilterChips
                    basePath={location.pathname}
                    currentParams={params}
                    paramKey="protocol"
                    label={t("Protocol")}
                    allLabel={t("All protocols")}
                    options={protocolOptions}
                  />
                  <FilterChips
                    basePath={location.pathname}
                    currentParams={params}
                    paramKey="device"
                    label={t("Device")}
                    allLabel={t("All devices")}
                    options={deviceOptions}
                  />
                </>
              ) : null}
            </div>
            {view === "apps" ? (
              <PagedTable
                rows={appRows}
                rowKey={(row) =>
                  row.kind === "known" ? appKey(row.app) : `custom:${row.saved.product}`
                }
                searchable
                pageSize={15}
                onRowClick={(row) =>
                  row.kind === "known" ? openApp(row.app) : openCustomApp(row.saved)
                }
                defaultSort={{ key: "name", dir: "asc" }}
                empty={<EmptyState title={t("No apps")} />}
                labels={pagedTableLabels({ searchPlaceholder: t("Search apps…") })}
                columns={[
                  {
                    key: "name",
                    header: t("App"),
                    search: (row) =>
                      row.kind === "known"
                        ? `${row.app.name} ${row.app.protocol} ${row.app.devices.join(" ")}`
                        : (row.saved.product ?? ""),
                    sortValue: (row) =>
                      (row.kind === "known"
                        ? row.app.name
                        : (row.saved.product ?? "")
                      ).toLowerCase(),
                    cell: (row) => {
                      const name = row.kind === "known" ? row.app.name : (row.saved.product ?? "");
                      const seen = row.kind === "known" ? seenFor(row.app, observed) : [];
                      return (
                        <span>
                          <button
                            type="button"
                            className="font-medium"
                            aria-label={t("Edit {name}", { name })}
                            onClick={() =>
                              row.kind === "known" ? openApp(row.app) : openCustomApp(row.saved)
                            }
                          >
                            {name}
                          </button>
                          {row.saved ? (
                            <Badge variant="warning" className="ml-2">
                              {t("Changed")}
                            </Badge>
                          ) : null}
                          {seen.length > 0 ? (
                            <span className="mt-1 block text-xs font-normal text-fg-muted">
                              {tx("Seen on this server, last {when}", {
                                when: relativeTime(seen[0]!.lastSeen),
                              })}
                            </span>
                          ) : null}
                        </span>
                      );
                    },
                  },
                  {
                    key: "protocol",
                    header: t("Protocol"),
                    sortValue: (row) => (row.kind === "known" ? row.app.protocol : ""),
                    cell: (row) => (row.kind === "known" ? protocolLabel(row.app.protocol) : ""),
                  },
                  {
                    key: "devices",
                    header: t("Typical devices"),
                    cell: (row) =>
                      row.kind === "known" ? row.app.devices.join(", ") || t("Any") : t("Any"),
                  },
                  {
                    key: "plays",
                    header: t("Plays directly"),
                    cell: (row) => {
                      if (row.saved?.videoCodecs?.length) {
                        return (
                          <CapabilityBadges
                            row={{
                              name: "",
                              dolbyVisionProfiles: row.saved.dolbyVisionProfiles ?? null,
                              subtitleCodecs: row.saved.subtitleCodecs ?? null,
                              notes: "",
                              videoCodecs: row.saved.videoCodecs,
                              maxHeight: row.saved.maxHeight,
                              hdr10: row.saved.hdr10,
                              dolbyVision: row.saved.dolbyVision,
                            }}
                          />
                        );
                      }
                      if (
                        row.kind === "known" &&
                        row.app.directPlay === "listed" &&
                        row.app.videoCodecs?.length
                      ) {
                        return (
                          <span className="flex flex-wrap gap-1">
                            {row.app.videoCodecs.map((codec) => (
                              <Badge key={codec} variant="secondary">
                                {codecLabel(codec)}
                              </Badge>
                            ))}
                          </span>
                        );
                      }
                      return (
                        <Badge variant="secondary" className="whitespace-nowrap">
                          {row.kind === "known" && row.app.directPlay === "stream"
                            ? t("Stream link")
                            : t("Reports its own")}
                        </Badge>
                      );
                    },
                  },
                  {
                    key: "subtitles",
                    header: t("Subtitles"),
                    cell: (row) =>
                      (
                        row.saved?.subtitleCodecs ??
                        (row.kind === "known" ? row.app.subtitles : null)
                      )?.join(", ") || t("Reported by the app"),
                  },
                  {
                    key: "certification",
                    header: t("Certification"),
                    cell: (row) =>
                      row.kind === "known" ? (
                        <CertificationMark value={row.app.certification} />
                      ) : null,
                  },
                ]}
              />
            ) : (
              <PagedTable
                rows={deviceRows}
                rowKey={(row) =>
                  row.kind === "known" ? deviceKey(row.row) : `custom:${row.saved.device}`
                }
                searchable
                pageSize={15}
                onRowClick={(row) =>
                  row.kind === "known" ? openDevice(row.row) : openCustomDevice(row.saved)
                }
                defaultSort={{ key: "name", dir: "asc" }}
                empty={<EmptyState title={t("No devices")} />}
                labels={pagedTableLabels({ searchPlaceholder: t("Search devices…") })}
                columns={[
                  {
                    key: "name",
                    header: t("Device"),
                    search: (row) =>
                      row.kind === "known" ? row.row.name : (row.saved.device ?? ""),
                    sortValue: (row) =>
                      (row.kind === "known"
                        ? row.row.name
                        : (row.saved.device ?? "")
                      ).toLowerCase(),
                    cell: (row) => {
                      const name = row.kind === "known" ? row.row.name : (row.saved.device ?? "");
                      const changed =
                        row.kind === "custom" ||
                        Boolean(row.saved) ||
                        (row.kind === "known" &&
                          ((row.row.maxHeight ?? 0) !== (row.row.builtinMaxHeight ?? 0) ||
                            Boolean(row.row.hdr10) !== Boolean(row.row.builtinHdr10) ||
                            Boolean(row.row.dolbyVision) !== Boolean(row.row.builtinDolbyVision)));
                      return (
                        <span className="inline-flex flex-wrap items-center gap-2">
                          <button
                            type="button"
                            className="font-medium"
                            aria-label={t("Edit {name}", { name })}
                            onClick={() =>
                              row.kind === "known"
                                ? openDevice(row.row)
                                : openCustomDevice(row.saved)
                            }
                          >
                            {name}
                          </button>
                          {changed ? <Badge variant="warning">{t("Changed")}</Badge> : null}
                        </span>
                      );
                    },
                  },
                  {
                    key: "plays",
                    header: t("Plays"),
                    cell: (row) =>
                      row.kind === "known" ? (
                        <CapabilityBadges row={row.row} />
                      ) : (
                        <CapabilityBadges
                          row={{
                            name: "",
                            dolbyVisionProfiles: row.saved.dolbyVisionProfiles ?? null,
                            subtitleCodecs: row.saved.subtitleCodecs ?? null,
                            notes: "",
                            videoCodecs: row.saved.videoCodecs,
                            maxHeight: row.saved.maxHeight,
                            hdr10: row.saved.hdr10,
                            dolbyVision: row.saved.dolbyVision,
                          }}
                        />
                      ),
                  },
                  {
                    key: "subtitles",
                    header: t("Subtitles"),
                    cell: (row) =>
                      (row.kind === "known"
                        ? row.row.subtitleCodecs
                        : row.saved.subtitleCodecs
                      )?.join(", ") || t("Reported by the app"),
                  },
                  {
                    key: "notes",
                    header: t("Notes"),
                    cell: (row) => (
                      <span className="text-fg-muted">
                        {row.kind === "known" ? deviceNote(row.row.notes) : ""}
                      </span>
                    ),
                  },
                ]}
              />
            )}
          </>
        )}
        <RecordDrawer
          open={editing != null}
          title={
            editing?.mode === "create"
              ? editing.view === "apps"
                ? t("Add an app")
                : t("Add a device")
              : editing
                ? tx("Edit {name}", { name: editing.name })
                : ""
          }
          description={
            editing?.mode === "create"
              ? t("Pick a known one, or type a match.")
              : t("What the app reports at playback replaces this.")
          }
          onClose={() => setEditing(null)}
          onSave={() => void save()}
          saving={saving}
          saveLabel={t("Save")}
          saveDisabled={editing?.mode === "create" && !draft.pick && !draft.match.trim()}
          onRemove={savedNow ? () => void remove(savedNow) : undefined}
          error={saveError}
        >
          {editing?.mode === "create" ? (
            <SearchSelect
              {...searchSelectLabels()}
              ariaLabel={editing.view === "apps" ? t("Known app") : t("Known device")}
              placeholder={editing.view === "apps" ? t("Known app") : t("Known device")}
              value={draft.pick}
              onChange={(pick) => setDraft({ ...draft, pick })}
              options={
                editing.view === "apps"
                  ? apps.map((app) => ({
                      value: appKey(app),
                      label: app.name,
                      hint: protocolLabel(app.protocol),
                    }))
                  : devices.map((row) => ({ value: deviceKey(row), label: row.name }))
              }
              clearable
              searchable
            />
          ) : null}
          {showMatch ? (
            <TextRow
              label={t("Match")}
              help={t("Ether matches this text against the name.")}
              value={draft.match}
              onChange={(match) => setDraft({ ...draft, match })}
            />
          ) : null}
          <ChoiceRow
            label={t("Dolby Vision profiles")}
            value={draft.profiles}
            options={profileOptions}
            placeholder={profilePlaceholder}
            onChange={(profiles) => setDraft({ ...draft, profiles })}
          />
          <ChoiceRow
            label={t("Subtitles")}
            value={draft.subtitles}
            options={subtitleOptions}
            placeholder={subtitlePlaceholder}
            onChange={(subtitles) => setDraft({ ...draft, subtitles })}
          />
          <ChoiceRow
            label={t("Video codecs")}
            value={draft.video}
            options={videoOptions}
            placeholder={videoPlaceholder}
            onChange={(video) => setDraft({ ...draft, video })}
          />
          <SettingsRow
            title={t("Maximum resolution")}
            control={
              <div className="w-full sm:w-80">
                <Select
                  aria-label={t("Maximum resolution")}
                  value={draft.height}
                  placeholder={heightPlaceholder}
                  onChange={(e) => setDraft({ ...draft, height: e.target.value })}
                >
                  <option value="">{heightPlaceholder}</option>
                  {heightOptions.map((value) => (
                    <option key={value} value={value}>
                      {resolutionName(Number(value))}
                    </option>
                  ))}
                </Select>
              </div>
            }
          />
          <BoolRow
            label={t("HDR")}
            help={
              builtin
                ? builtin.builtinHdr10
                  ? t("Built in: on.")
                  : t("Built in: off.")
                : undefined
            }
            value={draft.hdr}
            onChange={(hdr) => setDraft({ ...draft, hdr })}
          />
          <BoolRow
            label={t("Dolby Vision")}
            help={
              builtin
                ? builtin.builtinDolbyVision
                  ? t("Built in: on.")
                  : t("Built in: off.")
                : undefined
            }
            value={draft.dolby}
            onChange={(dolby) => setDraft({ ...draft, dolby })}
          />
          {showProduct ? (
            <TextRow
              label={t("Only when the app name contains")}
              help={t("Leave this blank to change the device for every app.")}
              value={draft.product}
              onChange={(product) => setDraft({ ...draft, product })}
            />
          ) : null}
        </RecordDrawer>
      </div>
    </ListNavProvider>
  );
}
