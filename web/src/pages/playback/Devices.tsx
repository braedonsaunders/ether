import { SubtabNav } from "../../components/layout";
import {
  Alert,
  AlertDescription,
  Badge,
  Button,
  EmptyState,
  FilterChips,
  Input,
  ListNavProvider,
  MultiSelect,
  PagedTable,
  SearchSelect,
  Select,
  type ListSearchParams,
} from "@braedonsaunders/appkit-ui";
import { Pencil, Plus, Search, SlidersHorizontal } from "lucide-react";
import { useMemo, useState, type ReactNode } from "react";
import { Link, useLocation, useNavigate, useParams } from "react-router-dom";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { SettingsRow } from "../../components/SettingsRow";
import { pagedTableLabels, searchSelectLabels } from "../../i18n/appkit-labels";
import { t, tx } from "../../i18n";
import { api, apiFetch } from "../../lib/api";
import { parseTime, relativeTime } from "../../lib/format";
import { useAsync } from "../../lib/hooks";
import { BoolRow, RecordDrawer, TextRow } from "../sources/form";
import { codecLabel } from "./hardwareCodecs";

type CompatApp = {
  protocol: string;
  name: string;
  product?: string;
  devices: string[];
  videoCodecs: string[] | null;
  audioCodecs?: string[] | null;
  containers?: string[] | null;
  subtitles: string[] | null;
  dolbyVisionProfiles?: number[] | null;
  maxHeight?: number;
  hdr10?: boolean;
  dolbyVision?: boolean;
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
  builtinAudioCodecs?: string[] | null;
  builtinContainers?: string[] | null;
  maxHeight?: number;
  hdr10?: boolean;
  dolbyVision?: boolean;
  builtinMaxHeight?: number;
  builtinHdr10?: boolean;
  builtinDolbyVision?: boolean;
};

type CapabilityOverride = {
  force?: boolean;
  id?: string;
  product?: string;
  productExact?: boolean;
  productMatch?: string;
  copiedFrom?: string;
  platform?: string;
  device?: string;
  dolbyVisionProfiles?: number[];
  subtitleCodecs?: string[];
  videoCodecs?: string[];
  audioCodecs?: string[];
  containers?: string[];
  maxHeight?: number;
  hdr10?: boolean;
  dolbyVision?: boolean;
  alwaysEncodeVideo?: boolean;
  notes?: string;
};

type ObservedClient = {
  protocol: string;
  name: string;
  lastSeen: string;
};

type SeenDevice = {
  clientIdentifier: string;
  name: string;
  platform: string;
  device: string;
  /** Hardware model the client reported (X-Plex-Model), e.g. "AppleTV5,3". */
  model?: string;
  /** Device profile record the hardware model selects, when it differs from the name's. */
  profile?: string;
  app: string;
  lastSeen: string;
  viewer: string;
  overrideIndex?: number;
};

type Catalog = {
  seenDevices?: SeenDevice[];
  apps: CompatApp[];
  devices: CompatDevice[];
  observed: ObservedClient[];
  overrides?: CapabilityOverride[];
};

type SectionView = "apps" | "devices" | "seen";
type SourceKind = "builtin" | "reported" | "override";

type Draft = {
  force: boolean;
  platform: string;
  pick: string;
  match: string;
  profiles: string[];
  subtitles: string[];
  video: string[];
  audio: string[];
  containers: string[];
  height: string;
  hdr: boolean;
  dolby: boolean;
  alwaysEncodeVideo: boolean;
  product: string;
};

const VIDEO_CHOICES = ["h264", "hevc", "vp9", "av1", "vp8", "mpeg2video", "vc1", "mpeg4"];
const AUDIO_CHOICES = [
  "aac",
  "ac3",
  "eac3",
  "truehd",
  "dts",
  "dtshd",
  "mp3",
  "flac",
  "alac",
  "opus",
  "vorbis",
  "pcm",
];
const CONTAINER_CHOICES = ["mp4", "mkv", "mov", "avi", "mpegts", "webm"];
const SUBTITLE_CHOICES = ["srt", "ass", "ssa", "vtt", "ttml", "pgs", "vobsub"];
const PROFILE_CHOICES = ["5", "7", "8", "4", "9"];

type Editor =
  | { mode: "create"; view: "apps" | "devices" }
  | {
      mode: "app";
      name: string;
      product: string;
      custom: boolean;
      builtin?: CompatApp;
      saved?: CapabilityOverride;
    }
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

function seenFor(app: CompatApp, observed: ObservedClient[]) {
  const product = appProduct(app);
  return observed.filter(
    (row) =>
      row.protocol === app.protocol &&
      (sameText(row.name, app.name) ||
        sameText(row.name, product) ||
        app.name.includes(row.name) ||
        row.name.includes(app.name)),
  );
}

function sameText(a?: string, b?: string) {
  return (a ?? "").trim().toLowerCase() === (b ?? "").trim().toLowerCase();
}

function appKey(app: CompatApp) {
  return `${app.protocol}:${app.name}`;
}

function appProduct(app: CompatApp) {
  return app.product || app.name;
}

// Several rows share a record id (Android TV, Google TV and Shield are all
// "android"), so the name keeps each row's key unique.
function deviceKey(row: CompatDevice) {
  return row.id ? `id:${row.id}:${row.name}` : `name:${row.name}`;
}

function isAppOverride(item: CapabilityOverride) {
  return (
    !item.id &&
    !item.device &&
    !item.platform &&
    item.productMatch !== "contains" &&
    Boolean(item.product)
  );
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
    force: false,
    platform: "",
    pick: "",
    match: "",
    profiles: [],
    subtitles: [],
    video: [],
    audio: [],
    containers: [],
    height: "",
    hdr: false,
    dolby: false,
    alwaysEncodeVideo: false,
    product: "",
  };
}

function listsFrom(saved?: CapabilityOverride, builtinHeight = 0) {
  const height = saved?.maxHeight ?? 0;
  return {
    force: Boolean(saved?.force),
    platform: saved?.platform ?? "",
    profiles: saved?.dolbyVisionProfiles?.map(String) ?? [],
    subtitles: [...(saved?.subtitleCodecs ?? [])],
    video: [...(saved?.videoCodecs ?? [])],
    audio: [...(saved?.audioCodecs ?? [])],
    containers: [...(saved?.containers ?? [])],
    height: height > 0 && height !== builtinHeight ? String(height) : "",
    hdr: Boolean(saved?.hdr10),
    dolby: Boolean(saved?.dolbyVision),
    alwaysEncodeVideo: Boolean(saved?.alwaysEncodeVideo),
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

function hasBuiltinAppCaps(app: CompatApp) {
  return Boolean(
    (app.maxHeight ?? 0) > 0 ||
    (app.videoCodecs?.length ?? 0) > 0 ||
    app.hdr10 ||
    app.dolbyVision ||
    (app.dolbyVisionProfiles?.length ?? 0) > 0,
  );
}

function hasBuiltinDeviceCaps(row: CompatDevice) {
  return Boolean(
    (row.maxHeight ?? 0) > 0 ||
    (row.videoCodecs?.length ?? 0) > 0 ||
    row.hdr10 ||
    row.dolbyVision ||
    (row.dolbyVisionProfiles?.length ?? 0) > 0,
  );
}

function sourceLabel(kind: SourceKind) {
  switch (kind) {
    case "override":
      return t("Override");
    case "builtin":
      return t("Built in");
    case "reported":
      return t("Reports its own");
  }
}

function SourceBadge({ kind, copiedFrom }: { kind: SourceKind; copiedFrom?: string }) {
  if (kind === "override") {
    return (
      <span className="inline-flex flex-wrap items-center gap-1">
        <Badge variant="warning" className="whitespace-nowrap px-1.5 py-0.5 text-[11px]">
          {t("Override")}
        </Badge>
        {copiedFrom ? (
          <Badge
            variant="secondary"
            className="whitespace-nowrap px-1.5 py-0.5 text-[11px]"
            title={copiedFrom}
          >
            {t("Copied from a shared setting")}
          </Badge>
        ) : null}
      </span>
    );
  }
  if (kind === "builtin") {
    return (
      <Badge variant="secondary" className="whitespace-nowrap px-1.5 py-0.5 text-[11px]">
        {t("Built in")}
      </Badge>
    );
  }
  return (
    <Badge variant="outline" className="whitespace-nowrap px-1.5 py-0.5 text-[11px]">
      {t("Reports its own")}
    </Badge>
  );
}

function VideoBadges({ codecs }: { codecs?: string[] | null }) {
  if (!codecs?.length) {
    return <span className="text-fg-subtle">—</span>;
  }
  const labels = codecs.map((codec) => codecLabel(codec));
  const visible = labels.slice(0, 2);
  const rest = labels.slice(2);
  return (
    <span className="inline-flex flex-nowrap items-center gap-0.5" title={labels.join(", ")}>
      {visible.map((label) => (
        <Badge
          key={label}
          variant="secondary"
          className="whitespace-nowrap px-1.5 py-0.5 text-[11px]"
        >
          {label}
        </Badge>
      ))}
      {rest.length > 0 ? (
        <Badge
          variant="outline"
          className="whitespace-nowrap px-1.5 py-0.5 text-[11px]"
          title={rest.join(", ")}
        >
          +{rest.length}
        </Badge>
      ) : null}
    </span>
  );
}

function HdrDvBadges({
  hdr10,
  dolbyVision,
  profiles,
}: {
  hdr10?: boolean;
  dolbyVision?: boolean;
  profiles?: number[] | null;
}) {
  const badges = [
    hdr10 ? t("HDR") : "",
    profiles?.length
      ? t("Dolby Vision {profiles}", { profiles: profiles.join(" · ") })
      : dolbyVision
        ? t("Dolby Vision")
        : "",
  ].filter(Boolean);
  if (!badges.length) {
    return <span className="text-fg-subtle">—</span>;
  }
  return (
    <span className="inline-flex flex-nowrap items-center gap-0.5">
      {badges.map((label) => (
        <Badge
          key={label}
          variant="secondary"
          className="whitespace-nowrap px-1.5 py-0.5 text-[11px]"
        >
          {label}
        </Badge>
      ))}
    </span>
  );
}

function formatDevices(devices: string[]) {
  if (!devices.length) return t("Any");
  if (devices.length <= 2) return devices.join(", ");
  return `${devices.slice(0, 2).join(", ")} +${devices.length - 2}`;
}

function applyCapabilityDiff(
  override: CapabilityOverride,
  draft: Draft,
  baseline: Draft,
  builtinHeight: number,
  keepProduct: boolean,
) {
  if (draft.force) override.force = true;
  else delete override.force;
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
  if (!sameList(draft.audio, baseline.audio)) {
    if (draft.audio.length) override.audioCodecs = draft.audio;
    else delete override.audioCodecs;
  }
  if (!sameList(draft.containers, baseline.containers)) {
    if (draft.containers.length) override.containers = draft.containers;
    else delete override.containers;
  }
  const height = Number(draft.height);
  if (draft.height !== baseline.height) {
    if (Number.isInteger(height) && height > 0 && height !== builtinHeight)
      override.maxHeight = height;
    else delete override.maxHeight;
  }
  if (draft.hdr !== baseline.hdr) override.hdr10 = draft.hdr;
  if (draft.dolby !== baseline.dolby) override.dolbyVision = draft.dolby;
  if (draft.alwaysEncodeVideo) override.alwaysEncodeVideo = true;
  else delete override.alwaysEncodeVideo;
  if (!keepProduct && draft.product !== baseline.product) {
    if (draft.product.trim()) override.product = draft.product.trim();
    else delete override.product;
  }
}

function capabilityChanged(override: CapabilityOverride) {
  return Object.keys(override).some(
    (key) =>
      ![
        "id",
        "product",
        "productExact",
        "productMatch",
        "copiedFrom",
        "device",
        "platform",
      ].includes(key),
  );
}

function sameValues(a: string[], b: string[]) {
  if (a.length !== b.length) return false;
  const seen = new Set(a.map((v) => v.toLowerCase()));
  return b.every((v) => seen.has(v.toLowerCase()));
}

/**
 * A list the owner can override. With no override the built-in values show
 * ticked, so editing starts from them; choosing exactly the built-in list
 * again stores no override.
 */
function ChoiceRow({
  label,
  value,
  onChange,
  options,
  placeholder,
  help,
  builtin = [],
}: {
  label: string;
  value: string[];
  onChange: (value: string[]) => void;
  options: { value: string; label: string }[];
  placeholder: string;
  help?: ReactNode;
  builtin?: string[];
}) {
  const shown = value.length > 0 ? value : builtin;
  const change = (next: string[]) =>
    onChange(builtin.length > 0 && sameValues(next, builtin) ? [] : next);
  return (
    <SettingsRow
      title={label}
      description={help}
      control={
        <div className="w-full sm:w-80">
          <MultiSelect
            ariaLabel={label}
            value={shown}
            onChange={change}
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
  const view: SectionView =
    section === "devices" ? "devices" : section === "seen" ? "seen" : "apps";
  const params = queryMap(location.search);
  const protocol = selected(params, "protocol");
  const deviceFilter = selected(params, "device");
  const platformFilter = selected(params, "platform");
  const sourceFilter = selected(params, "source");
  const listNav = useMemo(
    () => ({
      pathname: location.pathname,
      search: location.search.replace(/^\?/, ""),
      replace: (href: string) => navigate(href, { replace: true }),
      push: (href: string) => navigate(href),
    }),
    [location.pathname, location.search, navigate],
  );
  const [query, setQuery] = useState("");
  const [editing, setEditing] = useState<Editor | null>(null);
  const [draft, setDraft] = useState<Draft>(blankDraft);
  const [baseline, setBaseline] = useState<Draft>(blankDraft);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<unknown>(null);
  const config = useAsync(() => api.getConfig(), []);
  const matrix = useAsync(() => apiFetch<Catalog>("/playback/policy"), []);
  const apps = matrix.data?.apps ?? [];
  const devices = matrix.data?.devices ?? [];
  const observed = matrix.data?.observed ?? [];
  const seenDevices = matrix.data?.seenDevices ?? [];
  const overrides = matrix.data?.overrides ?? [];

  const appOverride = (name: string) =>
    [...overrides].reverse().find((item) => isAppOverride(item) && sameText(item.product, name));
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
    const saved = appOverride(appProduct(app));
    const lists = listsFrom(saved, app.maxHeight || 0);
    openDraft(
      {
        mode: "app",
        name: app.name,
        product: appProduct(app),
        custom: false,
        builtin: app,
        saved,
      },
      {
        ...blankDraft(),
        pick: appKey(app),
        ...lists,
        hdr: saved?.hdr10 ?? Boolean(app.hdr10),
        dolby: saved?.dolbyVision ?? Boolean(app.dolbyVision),
      },
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

  const seenOverride = (row: SeenDevice) =>
    row.overrideIndex === undefined ? undefined : overrides[row.overrideIndex];

  const openSeenDevice = (row: SeenDevice) => {
    const matched = seenOverride(row);
    const saved = matched && isDeviceMatch(matched) ? matched : undefined;
    const defaultMatch = row.device || row.name;
    openDraft(
      { mode: "device", name: row.name || row.device, device: defaultMatch, custom: true, saved },
      {
        ...blankDraft(),
        ...listsFrom(saved),
        match: saved?.device || defaultMatch,
        platform: saved ? (saved.platform ?? "") : row.platform,
        product: saved?.product || "",
      },
    );
  };

  const openCreate = () => {
    openDraft({ mode: "create", view: view === "apps" ? "apps" : "devices" }, blankDraft());
  };

  const knownApp = (key: string) => apps.find((app) => appKey(app) === key);
  const knownDevice = (key: string) => devices.find((row) => deviceKey(row) === key);

  const resolved = (): { saved?: CapabilityOverride; override: CapabilityOverride } | null => {
    if (!editing) return null;
    if (editing.mode === "app") {
      const product = editing.custom ? draft.match.trim() : editing.product;
      if (!product) return null;
      const override: CapabilityOverride = { ...editing.saved, product, productExact: true };
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
        : editing.mode === "device"
          ? editing.saved
          : overrides.find(
              (item) =>
                isDeviceMatch(item) &&
                sameText(item.device, match) &&
                sameText(item.platform, draft.platform),
            );
      const override: CapabilityOverride = { ...saved };
      delete override.id;
      delete override.platform;
      if (!appsSide && draft.platform.trim()) override.platform = draft.platform.trim();
      if (appsSide) {
        delete override.device;
        override.product = match;
        override.productExact = true;
      } else {
        override.device = match;
      }
      return { saved, override };
    }
    if (editing.mode !== "create" || !draft.pick) return null;
    if (editing.view === "apps") {
      const app = knownApp(draft.pick);
      if (!app) return null;
      const saved = appOverride(appProduct(app));
      const override: CapabilityOverride = {
        ...saved,
        product: appProduct(app),
        productExact: true,
      };
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
    const appBuiltin =
      editing?.mode === "app"
        ? editing.builtin
        : editing?.mode === "create" && editing.view === "apps" && !draft.match.trim()
          ? knownApp(draft.pick)
          : undefined;
    applyCapabilityDiff(
      target.override,
      draft,
      baseline,
      builtin?.builtinMaxHeight ?? builtin?.maxHeight ?? appBuiltin?.maxHeight ?? 0,
      editing?.mode === "app" || (editing?.mode === "create" && editing.view === "apps"),
    );
    const renamed =
      (target.override.product ?? "") !== (target.saved?.product ?? "") ||
      (target.override.device ?? "") !== (target.saved?.device ?? "") ||
      (target.override.platform ?? "") !== (target.saved?.platform ?? "") ||
      Boolean(target.override.force) !== Boolean(target.saved?.force);
    const alwaysEncodeChanged =
      Boolean(target.override.alwaysEncodeVideo) !== Boolean(target.saved?.alwaysEncodeVideo);
    if (!capabilityChanged(target.override) && !renamed) {
      if (alwaysEncodeChanged && target.saved) {
        const others = overrides.filter((item) => item !== target.saved);
        await persist(others, true);
        return;
      }
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

  const customApps = overrides.filter(
    (item) => isAppOverride(item) && !apps.some((app) => sameText(appProduct(app), item.product)),
  );
  const customDevices = overrides.filter(
    (item) => isDeviceMatch(item) && !devices.some((row) => sameText(row.name, item.device)),
  );

  const allAppRows = [
    ...apps.map((app) => {
      const saved = appOverride(appProduct(app));
      const source: SourceKind = saved
        ? "override"
        : hasBuiltinAppCaps(app)
          ? "builtin"
          : "reported";
      return { kind: "known" as const, app, saved, source };
    }),
    ...customApps.map((saved) => ({
      kind: "custom" as const,
      saved,
      source: "override" as SourceKind,
    })),
  ];

  const allDeviceRows = [
    ...devices.map((row) => {
      const saved = deviceOverride(row);
      const changed =
        Boolean(saved) ||
        (row.maxHeight ?? 0) !== (row.builtinMaxHeight ?? 0) ||
        Boolean(row.hdr10) !== Boolean(row.builtinHdr10) ||
        Boolean(row.dolbyVision) !== Boolean(row.builtinDolbyVision);
      const source: SourceKind = changed
        ? "override"
        : hasBuiltinDeviceCaps(row)
          ? "builtin"
          : "reported";
      return { kind: "known" as const, row, saved, source };
    }),
    ...customDevices.map((saved) => ({
      kind: "custom" as const,
      saved,
      source: "override" as SourceKind,
    })),
  ];

  const allSeenRows = seenDevices.map((row) => {
    const matched = seenOverride(row);
    const matchedApp = apps.find(
      (app) => sameText(appProduct(app), row.app) || sameText(app.name, row.app),
    );
    const profileDevice = row.profile ? devices.find((dev) => dev.id === row.profile) : undefined;
    const matchedDevice =
      profileDevice ??
      devices.find(
        (dev) =>
          sameText(dev.name, row.device) ||
          sameText(dev.name, row.platform) ||
          (row.device && dev.name.toLowerCase().includes(row.device.toLowerCase())),
      );
    // A hardware-model profile (Apple TV HD) outranks the app's defaults.
    const preferred = profileDevice ?? matchedApp;
    const maxHeight = matched?.maxHeight ?? preferred?.maxHeight ?? matchedDevice?.maxHeight ?? 0;
    const videoCodecs =
      matched?.videoCodecs ??
      (preferred?.videoCodecs?.length ? preferred.videoCodecs : undefined) ??
      matchedDevice?.videoCodecs ??
      null;
    const hdr10 = matched?.hdr10 ?? preferred?.hdr10 ?? matchedDevice?.hdr10;
    const dolbyVision =
      matched?.dolbyVision ?? preferred?.dolbyVision ?? matchedDevice?.dolbyVision;
    const dolbyVisionProfiles =
      matched?.dolbyVisionProfiles ??
      preferred?.dolbyVisionProfiles ??
      matchedDevice?.dolbyVisionProfiles ??
      null;
    const hasBuiltin = Boolean(
      maxHeight > 0 ||
      (videoCodecs?.length ?? 0) > 0 ||
      hdr10 ||
      dolbyVision ||
      (dolbyVisionProfiles?.length ?? 0) > 0,
    );
    const source: SourceKind = matched ? "override" : hasBuiltin ? "builtin" : "reported";
    return {
      row,
      matched,
      maxHeight,
      videoCodecs,
      hdr10,
      dolbyVision,
      dolbyVisionProfiles,
      source,
    };
  });

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
  const seenPlatformOptions = [...new Set(seenDevices.map((row) => row.platform).filter(Boolean))]
    .sort()
    .map((name) => ({
      value: name,
      label: name,
      count: seenDevices.filter((row) => row.platform === name).length,
    }));

  const sourceCounts = (sources: SourceKind[]) =>
    (["builtin", "reported", "override"] as const)
      .map((value) => ({
        value,
        label: sourceLabel(value),
        count: sources.filter((item) => item === value).length,
      }))
      .filter((opt) => opt.count > 0);

  const appSourceOptions = sourceCounts(allAppRows.map((r) => r.source));
  const deviceSourceOptions = sourceCounts(allDeviceRows.map((r) => r.source));
  const seenSourceOptions = sourceCounts(allSeenRows.map((r) => r.source));

  const normalizedQuery = query.trim().toLocaleLowerCase();

  const appRows = allAppRows.filter((row) => {
    if (sourceFilter && row.source !== sourceFilter) return false;
    if (row.kind === "custom") {
      if (protocol || deviceFilter) return false;
      if (!normalizedQuery) return true;
      return `${row.saved.product ?? ""} ${sourceLabel(row.source)}`
        .toLocaleLowerCase()
        .includes(normalizedQuery);
    }
    if (protocol && row.app.protocol !== protocol) return false;
    if (deviceFilter && !row.app.devices.includes(deviceFilter)) return false;
    if (!normalizedQuery) return true;
    const haystack = [
      row.app.name,
      row.app.product ?? "",
      protocolLabel(row.app.protocol),
      row.app.protocol,
      ...row.app.devices,
      sourceLabel(row.source),
    ]
      .join(" ")
      .toLocaleLowerCase();
    return haystack.includes(normalizedQuery);
  });

  const deviceRows = allDeviceRows.filter((row) => {
    if (sourceFilter && row.source !== sourceFilter) return false;
    if (!normalizedQuery) return true;
    const haystack =
      row.kind === "known"
        ? `${row.row.name} ${row.row.id ?? ""} ${sourceLabel(row.source)}`
        : `${row.saved.device ?? ""} ${row.saved.platform ?? ""} ${row.saved.product ?? ""} ${sourceLabel(row.source)}`;
    return haystack.toLocaleLowerCase().includes(normalizedQuery);
  });

  const filteredSeenRows = allSeenRows.filter((item) => {
    if (platformFilter && item.row.platform !== platformFilter) return false;
    if (sourceFilter && item.source !== sourceFilter) return false;
    if (!normalizedQuery) return true;
    const haystack = [
      item.row.name,
      item.row.device,
      item.row.platform,
      item.row.app,
      item.row.viewer,
      sourceLabel(item.source),
    ]
      .join(" ")
      .toLocaleLowerCase();
    return haystack.includes(normalizedQuery);
  });

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
      ? (editing.builtin ?? apps.find((app) => sameText(app.name, editing.name)))
      : editing?.mode === "create" && editing.view === "apps" && !draft.match.trim()
        ? knownApp(draft.pick)
        : undefined;
  const dvBuiltin = builtin?.dolbyVisionProfiles ?? sourceApp?.dolbyVisionProfiles ?? [];
  const profileOptions = withChoices(PROFILE_CHOICES, [
    ...dvBuiltin.map(String),
    ...draft.profiles,
  ]).map((value) => ({ value, label: t("Profile {profile}", { profile: value }) }));
  const videoOptions = withChoices(VIDEO_CHOICES, [
    ...(builtin?.videoCodecs ?? sourceApp?.videoCodecs ?? []),
    ...draft.video,
  ]).map((value) => ({ value, label: codecLabel(value) }));
  const audioBuiltin = builtin?.builtinAudioCodecs ?? sourceApp?.audioCodecs ?? [];
  const containersBuiltin = builtin?.builtinContainers ?? sourceApp?.containers ?? [];
  const audioOptions = withChoices(AUDIO_CHOICES, [...audioBuiltin, ...draft.audio]).map(
    (value) => ({ value, label: value }),
  );
  const containerOptions = withChoices(CONTAINER_CHOICES, [
    ...containersBuiltin,
    ...draft.containers,
  ]).map((value) => ({ value, label: value }));
  const codecHelp = (
    <>
      {t("Replaces the built-in list, so include every codec the app supports.")}
      <span className="block">{t("Codecs the Plex app declares itself are still added.")}</span>
    </>
  );
  const subtitleOptions = withChoices(SUBTITLE_CHOICES, [
    ...(builtin?.subtitleCodecs ?? sourceApp?.subtitles ?? []),
    ...draft.subtitles,
  ]).map((value) => ({ value, label: value }));
  const profilePlaceholder = builtIn(
    dvBuiltin.map((profile) => t("Profile {profile}", { profile })),
  );
  const videoPlaceholder = builtIn(
    (builtin?.videoCodecs ?? sourceApp?.videoCodecs ?? []).map((codec) => codecLabel(codec)),
  );
  const subtitlePlaceholder = builtIn(builtin?.subtitleCodecs ?? sourceApp?.subtitles ?? []);
  const heightBuiltin =
    builtin?.builtinMaxHeight || builtin?.maxHeight || sourceApp?.maxHeight || 0;
  const resolutionPlaceholder = builtIn(heightBuiltin ? [resolutionName(heightBuiltin)] : []);
  const heightOptions = withChoices(["720", "1080", "2160"], [draft.height]);

  const searchPlaceholder =
    view === "apps"
      ? t("Search apps…")
      : view === "devices"
        ? t("Search device profiles…")
        : t("Search seen devices…");

  return (
    <ListNavProvider value={listNav}>
      <div className="space-y-3">
        <SubtabNav
          ariaLabel={t("App compatibility sections")}
          active={view}
          onSelect={(key) => {
            setQuery("");
            navigate(`/playback/devices/${key}`);
          }}
          tabs={[
            { key: "apps", label: t("Apps") },
            { key: "devices", label: t("Device profiles") },
            { key: "seen", label: t("Seen devices") },
          ]}
        />
        {matrix.error ? (
          <ErrorPanel error={matrix.error} onRetry={() => void matrix.reload()} />
        ) : !matrix.data ? (
          <LoadingRows />
        ) : (
          <>
            <div className="flex flex-wrap items-center gap-2">
              <div className="relative w-56 sm:w-64">
                <Search
                  size={14}
                  aria-hidden
                  className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-fg-subtle"
                />
                <Input
                  type="search"
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                  aria-label={searchPlaceholder}
                  placeholder={searchPlaceholder}
                  className="h-8 py-1 pl-8 pr-2.5 text-sm"
                />
              </div>
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
                  <FilterChips
                    basePath={location.pathname}
                    currentParams={params}
                    paramKey="source"
                    label={t("Source")}
                    allLabel={t("All sources")}
                    options={appSourceOptions}
                  />
                </>
              ) : view === "devices" ? (
                <FilterChips
                  basePath={location.pathname}
                  currentParams={params}
                  paramKey="source"
                  label={t("Source")}
                  allLabel={t("All sources")}
                  options={deviceSourceOptions}
                />
              ) : (
                <>
                  {seenPlatformOptions.length > 0 ? (
                    <FilterChips
                      basePath={location.pathname}
                      currentParams={params}
                      paramKey="platform"
                      label={t("Platform")}
                      allLabel={t("All platforms")}
                      options={seenPlatformOptions}
                    />
                  ) : null}
                  {seenSourceOptions.length > 0 ? (
                    <FilterChips
                      basePath={location.pathname}
                      currentParams={params}
                      paramKey="source"
                      label={t("Source")}
                      allLabel={t("All sources")}
                      options={seenSourceOptions}
                    />
                  ) : null}
                </>
              )}
              <Button size="sm" className="ml-auto" onClick={openCreate}>
                <Plus size={14} /> {t("Add")}
              </Button>
            </div>
            <div className="[&_table]:text-[13px] [&_tbody_td]:transition-colors [&_tbody_td]:duration-150 [&_tbody_tr:hover]:delay-0 [&_tbody_tr:hover>td]:bg-surface-hover [&_td]:px-1.5 [&_td]:py-2 [&_td:first-child]:pl-2.5 [&_td:last-child]:pr-2.5 [&_th]:h-9 [&_th]:px-1.5 [&_th]:text-[11px] [&_th]:tracking-normal [&_th:first-child]:pl-2.5 [&_th:last-child]:pr-2.5">
              {view === "apps" ? (
                <PagedTable
                  rows={appRows}
                  rowKey={(row) =>
                    row.kind === "known" ? appKey(row.app) : `custom:${row.saved.product}`
                  }
                  pageSize={25}
                  onRowClick={(row) =>
                    row.kind === "known" ? openApp(row.app) : openCustomApp(row.saved)
                  }
                  defaultSort={{ key: "name", dir: "asc" }}
                  empty={<EmptyState title={t("No apps")} />}
                  labels={pagedTableLabels({ searchPlaceholder: t("Search apps…") })}
                  columns={[
                    {
                      key: "name",
                      header: t("Name"),
                      sortValue: (row) =>
                        (row.kind === "known"
                          ? row.app.name
                          : (row.saved.product ?? "")
                        ).toLowerCase(),
                      cell: (row) => {
                        const name =
                          row.kind === "known" ? row.app.name : (row.saved.product ?? "");
                        return (
                          <div className="flex items-center gap-2 text-left">
                            <button
                              type="button"
                              title={name}
                              className="block max-w-36 truncate whitespace-nowrap text-left font-medium text-fg hover:underline"
                              aria-label={t("Edit {name}", { name })}
                              onClick={(e) => {
                                e.stopPropagation();
                                if (row.kind === "known") openApp(row.app);
                                else openCustomApp(row.saved);
                              }}
                            >
                              {name}
                            </button>
                          </div>
                        );
                      },
                    },
                    {
                      key: "platform",
                      header: t("Platform"),
                      sortValue: (row) => (row.kind === "known" ? row.app.devices.join(", ") : ""),
                      cell: (row) =>
                        row.kind === "known" ? (
                          <span
                            className="block max-w-36 truncate whitespace-nowrap text-xs text-fg-muted"
                            title={row.app.devices.join(", ") || undefined}
                          >
                            {formatDevices(row.app.devices)}
                          </span>
                        ) : (
                          <span className="text-xs text-fg-muted">{t("Any")}</span>
                        ),
                    },
                    {
                      key: "resolution",
                      header: t("Resolution"),
                      sortValue: (row) =>
                        row.saved?.maxHeight ??
                        (row.kind === "known" ? (row.app.maxHeight ?? 0) : 0),
                      cell: (row) => {
                        const height =
                          row.saved?.maxHeight ??
                          (row.kind === "known" ? (row.app.maxHeight ?? 0) : 0);
                        return height > 0 ? (
                          <span className="whitespace-nowrap tabular-nums">
                            {resolutionName(height)}
                          </span>
                        ) : (
                          <span className="text-fg-subtle">—</span>
                        );
                      },
                    },
                    {
                      key: "video",
                      header: t("Video"),
                      cell: (row) => {
                        const codecs = row.saved?.videoCodecs?.length
                          ? row.saved.videoCodecs
                          : row.kind === "known"
                            ? row.app.videoCodecs
                            : null;
                        return <VideoBadges codecs={codecs} />;
                      },
                    },
                    {
                      key: "hdr",
                      header: t("HDR / DV"),
                      cell: (row) => {
                        const hdr10 =
                          row.saved?.hdr10 ?? (row.kind === "known" ? row.app.hdr10 : undefined);
                        const dolbyVision =
                          row.saved?.dolbyVision ??
                          (row.kind === "known" ? row.app.dolbyVision : undefined);
                        const profiles =
                          row.saved?.dolbyVisionProfiles ??
                          (row.kind === "known" ? row.app.dolbyVisionProfiles : null);
                        return (
                          <HdrDvBadges
                            hdr10={hdr10}
                            dolbyVision={dolbyVision}
                            profiles={profiles}
                          />
                        );
                      },
                    },
                    {
                      key: "source",
                      header: t("Source"),
                      sortValue: (row) => row.source,
                      cell: (row) => (
                        <SourceBadge kind={row.source} copiedFrom={row.saved?.copiedFrom} />
                      ),
                    },
                    {
                      key: "lastSeen",
                      header: t("Last seen"),
                      sortValue: (row) => {
                        const seen = row.kind === "known" ? seenFor(row.app, observed) : [];
                        return seen[0] && parseTime(seen[0].lastSeen) ? seen[0].lastSeen : "";
                      },
                      cell: (row) => {
                        const seen = row.kind === "known" ? seenFor(row.app, observed) : [];
                        return seen.length > 0 && parseTime(seen[0]!.lastSeen) ? (
                          <span className="whitespace-nowrap text-xs text-fg-muted">
                            {relativeTime(seen[0]!.lastSeen)}
                          </span>
                        ) : (
                          <span className="text-fg-subtle">—</span>
                        );
                      },
                    },
                  ]}
                />
              ) : view === "devices" ? (
                <PagedTable
                  rows={deviceRows}
                  rowKey={(row) =>
                    row.kind === "known"
                      ? deviceKey(row.row)
                      : `custom:${row.saved.platform}:${row.saved.device}:${row.saved.product}`
                  }
                  pageSize={25}
                  onRowClick={(row) =>
                    row.kind === "known" ? openDevice(row.row) : openCustomDevice(row.saved)
                  }
                  defaultSort={{ key: "name", dir: "asc" }}
                  empty={<EmptyState title={t("No device profiles")} />}
                  labels={pagedTableLabels({ searchPlaceholder: t("Search device profiles…") })}
                  columns={[
                    {
                      key: "name",
                      header: t("Name"),
                      sortValue: (row) =>
                        (row.kind === "known"
                          ? row.row.name
                          : (row.saved.device ?? "")
                        ).toLowerCase(),
                      cell: (row) => {
                        const name = row.kind === "known" ? row.row.name : (row.saved.device ?? "");
                        return (
                          <div className="flex items-center gap-2 text-left">
                            <button
                              type="button"
                              className="whitespace-nowrap text-left font-medium text-fg hover:underline"
                              aria-label={t("Edit {name}", { name })}
                              onClick={(e) => {
                                e.stopPropagation();
                                if (row.kind === "known") openDevice(row.row);
                                else openCustomDevice(row.saved);
                              }}
                            >
                              {name}
                            </button>
                          </div>
                        );
                      },
                    },
                    {
                      key: "match",
                      header: t("Match"),
                      cell: (row) => {
                        if (row.kind === "custom") {
                          const parts = [
                            row.saved.platform,
                            row.saved.device,
                            row.saved.product ? t("App: {app}", { app: row.saved.product }) : "",
                          ].filter(Boolean);
                          return (
                            <span className="text-fg-muted">{parts.join(" · ") || t("Any")}</span>
                          );
                        }
                        if (row.saved?.product) {
                          return (
                            <span className="text-fg-muted">
                              {t("{id} · App: {app}", {
                                id: row.row.id || row.row.name,
                                app: row.saved.product,
                              })}
                            </span>
                          );
                        }
                        return (
                          <span className="whitespace-nowrap font-mono text-xs text-fg-muted">
                            {row.row.id || "dlna"}
                          </span>
                        );
                      },
                    },
                    {
                      key: "resolution",
                      header: t("Resolution"),
                      sortValue: (row) =>
                        row.kind === "known"
                          ? (row.row.maxHeight ?? 0)
                          : (row.saved.maxHeight ?? 0),
                      cell: (row) => {
                        const height =
                          row.kind === "known"
                            ? (row.row.maxHeight ?? 0)
                            : (row.saved.maxHeight ?? 0);
                        return height > 0 ? (
                          <span className="whitespace-nowrap tabular-nums">
                            {resolutionName(height)}
                          </span>
                        ) : (
                          <span className="text-fg-subtle">—</span>
                        );
                      },
                    },
                    {
                      key: "video",
                      header: t("Video"),
                      cell: (row) => (
                        <VideoBadges
                          codecs={
                            row.kind === "known" ? row.row.videoCodecs : row.saved.videoCodecs
                          }
                        />
                      ),
                    },
                    {
                      key: "hdr",
                      header: t("HDR / DV"),
                      cell: (row) =>
                        row.kind === "known" ? (
                          <HdrDvBadges
                            hdr10={row.row.hdr10}
                            dolbyVision={row.row.dolbyVision}
                            profiles={row.row.dolbyVisionProfiles}
                          />
                        ) : (
                          <HdrDvBadges
                            hdr10={row.saved.hdr10}
                            dolbyVision={row.saved.dolbyVision}
                            profiles={row.saved.dolbyVisionProfiles}
                          />
                        ),
                    },
                    {
                      key: "source",
                      header: t("Source"),
                      sortValue: (row) => row.source,
                      cell: (row) => <SourceBadge kind={row.source} />,
                    },
                  ]}
                />
              ) : (
                <PagedTable
                  rows={filteredSeenRows}
                  rowKey={(item) => item.row.clientIdentifier}
                  pageSize={25}
                  labels={pagedTableLabels({ searchPlaceholder: t("Search seen devices…") })}
                  empty={<EmptyState title={t("No seen devices yet")} />}
                  onRowClick={(item) => openSeenDevice(item.row)}
                  columns={[
                    {
                      key: "name",
                      header: t("Name"),
                      sortValue: (item) =>
                        (
                          item.row.name ||
                          item.row.device ||
                          item.row.app ||
                          item.row.clientIdentifier
                        ).toLowerCase(),
                      cell: (item) => {
                        const name =
                          item.row.name ||
                          item.row.device ||
                          item.row.app ||
                          item.row.clientIdentifier;
                        return (
                          <div className="flex items-center gap-2 text-left">
                            <button
                              type="button"
                              title={name}
                              className="block max-w-32 truncate whitespace-nowrap text-left font-medium text-fg hover:underline min-[1400px]:max-w-44"
                              onClick={(e) => {
                                e.stopPropagation();
                                openSeenDevice(item.row);
                              }}
                            >
                              {name}
                            </button>
                          </div>
                        );
                      },
                    },
                    {
                      key: "platform",
                      header: t("Platform / Match"),
                      sortValue: (item) => `${item.row.platform} ${item.row.app}`.toLowerCase(),
                      cell: (item) => {
                        const parts = [
                          item.row.platform,
                          item.row.app || t("Unknown"),
                          item.row.device && item.row.device !== item.row.name
                            ? item.row.device
                            : "",
                        ].filter(Boolean);
                        return (
                          <span
                            className="block max-w-32 truncate whitespace-nowrap text-xs text-fg-muted min-[1400px]:max-w-48"
                            title={parts.join(" · ")}
                          >
                            {parts.join(" · ")}
                          </span>
                        );
                      },
                    },
                    {
                      key: "resolution",
                      header: t("Resolution"),
                      sortValue: (item) => item.maxHeight,
                      cell: (item) =>
                        item.maxHeight > 0 ? (
                          <span className="whitespace-nowrap tabular-nums">
                            {resolutionName(item.maxHeight)}
                          </span>
                        ) : (
                          <span className="text-fg-subtle">—</span>
                        ),
                    },
                    {
                      key: "video",
                      header: t("Video"),
                      cell: (item) => <VideoBadges codecs={item.videoCodecs} />,
                    },
                    {
                      key: "hdr",
                      header: t("HDR / DV"),
                      cell: (item) => (
                        <HdrDvBadges
                          hdr10={item.hdr10}
                          dolbyVision={item.dolbyVision}
                          profiles={item.dolbyVisionProfiles}
                        />
                      ),
                    },
                    {
                      key: "source",
                      header: t("Source"),
                      sortValue: (item) => item.source,
                      cell: (item) => <SourceBadge kind={item.source} />,
                    },
                    {
                      key: "lastSeen",
                      header: t("Last seen"),
                      sortValue: (item) => (parseTime(item.row.lastSeen) ? item.row.lastSeen : ""),
                      cell: (item) =>
                        parseTime(item.row.lastSeen) ? (
                          <span className="whitespace-nowrap text-xs text-fg-muted">
                            {relativeTime(item.row.lastSeen)}
                          </span>
                        ) : (
                          <span className="text-fg-subtle">—</span>
                        ),
                    },
                    {
                      key: "edit",
                      header: "",
                      align: "right",
                      cell: (item) => {
                        const label = item.matched ? t("Edit override") : t("Create override");
                        return (
                          <Button
                            size="sm"
                            variant="outline"
                            aria-label={label}
                            title={label}
                            className="h-7 gap-1 whitespace-nowrap bg-transparent px-1.5 text-[11px] hover:bg-surface"
                            onClick={(e) => {
                              e.stopPropagation();
                              openSeenDevice(item.row);
                            }}
                          >
                            {item.matched ? (
                              <Pencil size={12} aria-hidden />
                            ) : (
                              <SlidersHorizontal size={12} aria-hidden />
                            )}
                            {item.matched ? t("Edit") : t("Override")}
                          </Button>
                        );
                      },
                    },
                  ]}
                />
              )}
            </div>
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
            editing?.mode === "create" ? t("Pick a known one, or type a match.") : undefined
          }
          onClose={() => setEditing(null)}
          onSave={() => void save()}
          saving={saving}
          saveLabel={t("Save")}
          saveDisabled={editing?.mode === "create" && !draft.pick && !draft.match.trim()}
          onRemove={savedNow ? () => void remove(savedNow) : undefined}
          removeLabel={
            editing && editing.mode !== "create" && editing.custom
              ? t("Remove")
              : t("Reset to defaults")
          }
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
            builtin={dvBuiltin.map(String)}
            onChange={(profiles) => setDraft({ ...draft, profiles })}
          />
          <ChoiceRow
            label={t("Subtitles")}
            value={draft.subtitles}
            options={subtitleOptions}
            placeholder={subtitlePlaceholder}
            builtin={builtin?.subtitleCodecs ?? sourceApp?.subtitles ?? []}
            onChange={(subtitles) => setDraft({ ...draft, subtitles })}
          />
          <ChoiceRow
            help={codecHelp}
            label={t("Video codecs")}
            value={draft.video}
            options={videoOptions}
            placeholder={videoPlaceholder}
            builtin={builtin?.videoCodecs ?? sourceApp?.videoCodecs ?? []}
            onChange={(video) => setDraft({ ...draft, video })}
          />
          <ChoiceRow
            label={t("Audio codecs")}
            help={codecHelp}
            value={draft.audio}
            options={audioOptions}
            placeholder={builtIn(audioBuiltin)}
            builtin={audioBuiltin}
            onChange={(audio) => setDraft({ ...draft, audio })}
          />
          <ChoiceRow
            label={t("Containers")}
            help={t("Replaces the built-in list, so include every container the app supports.")}
            value={draft.containers}
            options={containerOptions}
            placeholder={builtIn(containersBuiltin)}
            builtin={containersBuiltin}
            onChange={(containers) => setDraft({ ...draft, containers })}
          />
          <SettingsRow
            title={t("Highest resolution it can show")}
            control={
              <div className="w-full sm:w-80">
                <Select
                  aria-label={t("Highest resolution it can show")}
                  value={draft.height}
                  placeholder={resolutionPlaceholder}
                  onChange={(e) => setDraft({ ...draft, height: e.target.value })}
                >
                  <option value="">{resolutionPlaceholder}</option>
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
                : sourceApp
                  ? sourceApp.hdr10
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
                : sourceApp
                  ? sourceApp.dolbyVision
                    ? t("Built in: on.")
                    : t("Built in: off.")
                  : undefined
            }
            value={draft.dolby}
            onChange={(dolby) => setDraft({ ...draft, dolby })}
          />
          {editing?.mode === "device" && editing.custom ? (
            <TextRow
              label={t("Platform")}
              value={draft.platform}
              onChange={(platform) => setDraft({ ...draft, platform })}
            />
          ) : null}
          <BoolRow
            label={t("Always convert video")}
            help={t(
              "Encode video instead of copying it when streaming. Direct play of the original file is unaffected. Ignored for movies and shows when “No video transcoding” is on for the server or the viewer.",
            )}
            value={draft.alwaysEncodeVideo}
            onChange={(alwaysEncodeVideo) => setDraft({ ...draft, alwaysEncodeVideo })}
          />
          {draft.alwaysEncodeVideo && config.data?.transcode?.videoTranscoding === "off" ? (
            <Alert variant="warning" role="status">
              <AlertDescription>
                {t(
                  "Video transcoding is currently turned off for the whole server, so “No video transcoding” wins on movies and shows (Live TV is unaffected).",
                )}{" "}
                <Link to="/playback/transcoding" className="font-medium underline">
                  {t("Review Transcoding")}
                </Link>
              </AlertDescription>
            </Alert>
          ) : null}
          <BoolRow
            label={t("Use these settings even when the app reports otherwise")}
            help={t("Use this when your device can play something its app is too cautious about.")}
            value={draft.force}
            onChange={(force) => setDraft({ ...draft, force })}
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
