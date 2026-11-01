import {
  Alert,
  AlertDescription,
  Button,
  Checkbox,
  Input,
  SearchSelect,
  Spinner,
  Switch,
  WizardLayout,
  cn,
} from "@braedonsaunders/appkit-ui";
import { Check, ExternalLink, KeyRound, Newspaper, Plus, Trash2 } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useNavigate } from "react-router-dom";
import { useAppOptional } from "../app-context";
import { RestartButton } from "../components/RestartDrawer";
import { EtherMark } from "../components/brand";
import { IntegrationAppCard } from "../components/IntegrationAppCard";
import {
  EmbyMark,
  householdIcon,
  JellyfinMark,
  PlexMark,
  SiloMark,
} from "../components/integration-brands";
import { StremioMark } from "./sources/marks";
import { ErrorPanel, MutationError } from "../components/feedback";
import {
  Field,
  NumberInput,
  PasswordInput,
  SecretInput,
  TestButton,
  type TestState,
} from "../components/fields";
import { loadLocale, localized, rememberLocale, t, tn, tx } from "../i18n";
import { api, apiFetch, newId, setToken } from "../lib/api";
import { formatBytes } from "../lib/format";
import type { DebridProvider, DiskInfo, EtherConfig, Indexer, UsenetProvider } from "../lib/types";
import { appLabel, appPath, appPurpose, appRank } from "./apps/catalog";
import { CachePresetCards, cachePresetKey, presetForFreeBytes } from "./cache/presets";
import { ProwlarrImport } from "../components/ProwlarrImport";
import { DEBRID_KINDS } from "./sources/Debrid";
import { DEBRID_TOKEN_URL } from "./sources/cards";
import { addonUrlHasDebridKey, isStremio, normalizeAddonUrl } from "./sources/Indexers";
import { LanguageStep } from "./setup/LanguageStep";
import {
  householdLanguageFor,
  projectHouseholdRules,
  sameHouseholdLanguage,
  type HouseholdLanguage,
} from "./setup/language";
import { normalizeRules, type StreamRules } from "./viewers/shared";

export type WizardStepId =
  "language" | "apps" | "account" | "sources" | "indexers" | "cache" | "catalog";

/** Survives a remount after the owner account is created so the visit does not start over. */
export const SETUP_STEP_KEY = "ether.setup.step";

const STEP_COPY = localized(() => ({
  languageTitle: t("Language"),
  languageHeading: t("Choose a language"),
  appsTitle: t("Apps"),
  appsHeading: t("Where will your household watch?"),
  plexTitle: "Plex",
  plexHeading: t("Sign in to plex.tv and claim this server"),
  accountTitle: t("Account"),
  accountHeading: t("Create your owner account"),
  sourcesTitle: t("Sources"),
  sourcesHeading: t("Where Ether streams from"),
  indexersTitle: t("Indexers"),
  indexersHeading: t("Indexers and addons"),
  cacheTitle: t("Cache"),
  cacheHeading: t("Cache folder and disk budget"),
  catalogTitle: t("Catalogue"),
  catalogHeading: t("Catalogue and extras"),
}));

const FALLBACK_APPS = [
  { id: "plex", name: "Plex" },
  { id: "jellyfin", name: "Jellyfin" },
  { id: "emby", name: "Emby" },
  { id: "stremio", name: "Stremio" },
  { id: "silo", name: "Silo" },
  { id: "dlna", name: "DLNA" },
  { id: "webdav", name: "Files (WebDAV)" },
  { id: "m3u", name: "M3U & XMLTV" },
];

export interface SetupApp {
  id: string;
  name: string;
  description: string;
}

export function wizardSteps(
  plexClaimNow: boolean,
): { id: WizardStepId; title: string; heading: string }[] {
  return [
    { id: "language", title: STEP_COPY.languageTitle, heading: STEP_COPY.languageHeading },
    { id: "apps", title: STEP_COPY.appsTitle, heading: STEP_COPY.appsHeading },
    plexClaimNow
      ? { id: "account", title: STEP_COPY.plexTitle, heading: STEP_COPY.plexHeading }
      : { id: "account", title: STEP_COPY.accountTitle, heading: STEP_COPY.accountHeading },
    { id: "sources", title: STEP_COPY.sourcesTitle, heading: STEP_COPY.sourcesHeading },
    { id: "indexers", title: STEP_COPY.indexersTitle, heading: STEP_COPY.indexersHeading },
    { id: "cache", title: STEP_COPY.cacheTitle, heading: STEP_COPY.cacheHeading },
    { id: "catalog", title: STEP_COPY.catalogTitle, heading: STEP_COPY.catalogHeading },
  ];
}

function storedStep(): number {
  const n = Number(sessionStorage.getItem(SETUP_STEP_KEY));
  return Number.isInteger(n) && n >= 0 && n < wizardSteps(true).length ? n : 0;
}

export function appCatalog(
  list: { descriptor: { id: string; name: string } }[] | null,
): SetupApp[] {
  const rows = (
    list?.length
      ? list.map((item) => ({ id: item.descriptor.id, name: item.descriptor.name }))
      : FALLBACK_APPS
  )
    .slice()
    .sort((a, b) => appRank(a.id) - appRank(b.id));
  return rows.map((row) => {
    return {
      id: row.id,
      name: appLabel(row.id, row.name),
      description: appPurpose(row.id),
    };
  });
}

export function selectionFrom(cfg: EtherConfig, ids: string[]): Record<string, boolean> {
  if (cfg.frontends) {
    return Object.fromEntries(ids.map((id) => [id, Boolean(cfg.frontends?.[id]?.enabled)]));
  }
  // A finished or claimed server with no front-end map is the legacy Plex install.
  // A fresh install starts with nothing selected.
  const legacyPlex = cfg.setupComplete || cfg.plex.claimed;
  return Object.fromEntries(ids.map((id) => [id, legacyPlex && id === "plex"]));
}

/** Enabled flags change without dropping settings or front ends the wizard did not list. */
export function mergeFrontendSelection(
  cfg: EtherConfig,
  ids: string[],
  selected: Record<string, boolean>,
): EtherConfig {
  const prev = cfg.frontends ?? {};
  const frontends: NonNullable<EtherConfig["frontends"]> = { ...prev };
  for (const id of ids) {
    const prior = prev[id];
    frontends[id] = prior
      ? { ...prior, enabled: Boolean(selected[id]) }
      : { enabled: Boolean(selected[id]) };
  }
  return { ...cfg, frontends };
}

function blankProvider(): UsenetProvider {
  return {
    id: newId("prov"),
    name: "",
    host: "",
    port: 563,
    tls: true,
    username: "",
    password: "",
    connections: 20,
    priority: 0,
    backfill: false,
    enabled: true,
  };
}
function blankDebrid(): DebridProvider {
  return {
    id: newId("deb"),
    name: "",
    kind: "realdebrid",
    apiKey: "",
    priority: 0,
    enabled: true,
  };
}

/** How an indexer row is searched; maps onto Indexer.kind and protocol. */
type IndexerType = "newznab" | "torznab" | "stremio";
const INDEXER_TYPES: { value: IndexerType; label: string; hint: string }[] = localized(() => [
  {
    value: "stremio",
    label: t("Stremio addon (torrents)"),
    hint: t("A link from a torrent addon, such as Torrentio. No account."),
  },
  {
    value: "torznab",
    label: t("Torznab (torrents)"),
    hint: t("A torrent search from Prowlarr, Jackett, or another app."),
  },
  {
    value: "newznab",
    label: t("Newznab (Usenet)"),
    hint: t("A Usenet search. Use this with a Usenet provider."),
  },
]);
function indexerType(ix: Indexer): IndexerType {
  if (isStremio(ix)) return "stremio";
  return ix.protocol === "torrent" ? "torznab" : "newznab";
}
function withIndexerType(ix: Indexer, type: IndexerType): Indexer {
  if (type === "stremio") return { ...ix, kind: "stremio", protocol: "torrent", apiKey: "" };
  return { ...ix, kind: "", protocol: type === "torznab" ? "torrent" : "usenet" };
}

const TORRENTIO_URL = "https://torrentio.strem.fun/manifest.json";

function SourceChoice({
  pressed,
  onClick,
  logo,
  title,
  line,
  detail,
  note,
  children,
}: {
  pressed: boolean;
  onClick: () => void;
  logo: ReactNode;
  title: string;
  line: string;
  detail?: string;
  note?: string;
  children?: ReactNode;
}) {
  return (
    <section
      className={cn(
        "rounded-xl border",
        pressed ? "border-primary bg-primary/10" : "border-border bg-surface",
      )}
    >
      <button
        type="button"
        aria-pressed={pressed}
        onClick={onClick}
        className="flex w-full items-center gap-3 p-4 text-left"
      >
        <span className="grid size-7 shrink-0 place-items-center text-fg">{logo}</span>
        <span className="min-w-0 flex-1">
          <span className="block text-sm font-medium text-fg">{title}</span>
          <span className="block text-sm text-fg-muted">{line}</span>
          {detail ? <span className="mt-1 block text-sm text-fg-muted">{detail}</span> : null}
        </span>
        {pressed ? <Check size={18} className="shrink-0 text-primary" aria-hidden /> : null}
      </button>
      {pressed && note ? <p className="px-4 pb-3 text-sm text-fg">{note}</p> : null}
      {pressed && children ? <div className="space-y-4 px-4 pb-4">{children}</div> : null}
    </section>
  );
}
const READY_MS = 1700;

function prefersReducedMotion(): boolean {
  return (
    typeof window !== "undefined" && window.matchMedia("(prefers-reduced-motion: reduce)").matches
  );
}

function setupAppLogo(id: string) {
  switch (id) {
    case "plex":
      return <PlexMark />;
    case "jellyfin":
      return <JellyfinMark />;
    case "emby":
      return <EmbyMark />;
    case "silo":
      return <SiloMark />;
    case "stremio":
      return <StremioMark />;
    default:
      return householdIcon(id);
  }
}

const CONNECT_PANEL_APPS = ["jellyfin", "emby", "silo", "dlna"];

/** The finish chip opens the app that shows a server address, not the Apps overview. */
export function connectDevicePath(frontends: EtherConfig["frontends"]): string {
  const enabled = Object.entries(frontends ?? {})
    .filter(([, row]) => row?.enabled)
    .map(([id]) => id)
    .sort((a, b) => appRank(a) - appRank(b));
  const panel = enabled.find((id) => CONNECT_PANEL_APPS.includes(id));
  const id = panel ?? enabled[0];
  if (!id) return "/apps";
  if (["stremio", "webdav", "m3u"].includes(id)) return "/me";
  return `/apps/${appPath(id)}?connect=1`;
}

function SetupReady() {
  const navigate = useNavigate();
  const [cfg, setCfg] = useState<EtherConfig | null>(null);
  const [settled, setSettled] = useState(prefersReducedMotion);

  useEffect(() => {
    void api
      .getConfig()
      .then(setCfg)
      .catch(() => {});
  }, []);
  useEffect(() => {
    if (settled) return;
    const timer = window.setTimeout(() => setSettled(true), READY_MS);
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") setSettled(true);
    };
    window.addEventListener("keydown", onKey);
    return () => {
      window.clearTimeout(timer);
      window.removeEventListener("keydown", onKey);
    };
  }, [settled]);

  const debridOn = (cfg?.debrid?.providers ?? []).some((row) => row.enabled && row.apiKey.trim());
  const usenetOn = (cfg?.usenet.providers ?? []).some((row) => row.enabled && row.host.trim());
  const plexOpen = Boolean(cfg?.frontends?.plex?.enabled) && !cfg?.plex.claimed;
  const anyApp = cfg?.frontends ? Object.values(cfg.frontends).some((row) => row?.enabled) : false;
  const go = (to: string) => navigate(to);

  return (
    <div className="mx-auto flex w-full max-w-lg flex-col items-center px-6 py-16">
      {settled ? (
        <div className="ether-settle flex w-full flex-col items-center gap-5">
          <Button size="lg" className="h-12 min-w-56 px-10 text-base" onClick={() => go("/")}>
            {t("Get started")}
          </Button>
          <div className="flex flex-wrap justify-center gap-2">
            {anyApp ? (
              <Button
                variant="outline"
                size="sm"
                className="rounded-full"
                onClick={() => go(connectDevicePath(cfg?.frontends))}
              >
                {t("Connect a device")}
              </Button>
            ) : null}
            <Button
              variant="outline"
              size="sm"
              className="rounded-full"
              onClick={() => go("/viewers")}
            >
              {t("Add viewers")}
            </Button>
            {!debridOn && !usenetOn ? (
              <Button
                variant="outline"
                size="sm"
                className="rounded-full"
                onClick={() => go("/sources")}
              >
                {t("Add a debrid account")}
              </Button>
            ) : null}
            {plexOpen ? (
              <Button
                variant="outline"
                size="sm"
                className="rounded-full"
                onClick={() => go("/apps/plex/account")}
              >
                {t("Connect Plex")}
              </Button>
            ) : null}
          </div>
        </div>
      ) : (
        <div className="ether-settle flex flex-col items-center gap-3 text-primary" role="status">
          <EtherMark size={72} />
          <Button variant="link" onClick={() => setSettled(true)}>
            {t("Skip")}
          </Button>
        </div>
      )}
    </div>
  );
}

function indexerNameFromUrl(url: string): string {
  try {
    const host = new URL(url.includes("://") ? url : `https://${url}`).hostname;
    const label = host.split(".").find((part) => part && part !== "www") ?? "";
    if (!label) return "";
    return label.charAt(0).toUpperCase() + label.slice(1);
  } catch {
    return "";
  }
}

function blankIndexer(type: IndexerType = "newznab"): Indexer {
  return withIndexerType(
    {
      id: newId("idx"),
      name: "",
      url: "",
      apiKey: "",
      dailyLimit: 0,
      dailyGrabs: 0,
      priority: 0,
      enabled: true,
      movieCategories: null,
      tvCategories: null,
    },
    type,
  );
}

/** An indexer as saved and tested: addon links are read as https. */
function normalizedIndexer(ix: Indexer): Indexer {
  return isStremio(ix) ? { ...ix, url: normalizeAddonUrl(ix.url), protocol: "torrent" } : ix;
}

/**
 * The draft as saved: rows added but never filled in are dropped, so an
 * empty row can't fail validation and block the step.
 */
export function savedDraft(cfg: EtherConfig): EtherConfig {
  const blank = (...v: (string | undefined)[]) => v.every((x) => !x?.trim());
  return {
    ...cfg,
    usenet: {
      ...cfg.usenet,
      providers: (cfg.usenet.providers ?? []).filter(
        (p) => !blank(p.name, p.host, p.username, p.password),
      ),
    },
    indexers: (cfg.indexers ?? [])
      .filter((ix) => !blank(ix.name, ix.url, ix.apiKey))
      .map(normalizedIndexer),
    ...(cfg.debrid && {
      debrid: {
        ...cfg.debrid,
        providers: (cfg.debrid.providers ?? []).filter((d) => !blank(d.name, d.apiKey)),
      },
    }),
  };
}

/** Whether a wizard step may be left: required steps must be complete and tested (or explicitly skipped). */
export function canAdvance(
  step: WizardStepId,
  opts: {
    /** Plex claim, or the local owner when Plex was not selected. */
    accountReady: boolean;
    providers: UsenetProvider[];
    indexers: Indexer[];
    providerTests: Record<string, TestState>;
    indexerTests: Record<string, TestState>;
    /** The TMDB key was tested or is already saved (false blocks the last step). */
    tmdbReady?: boolean;
    /** Debrid accounts and their tests (absent on servers without debrid). */
    debrid?: DebridProvider[];
    debridTests?: Record<string, TestState>;
    skipped: Record<string, boolean>;
    /** The owner chose a torrent addon and will paste the link on the next step. */
    readyMade?: boolean;
  },
): boolean {
  switch (step) {
    case "language":
    case "apps":
    case "cache":
      return true;
    case "account":
      return opts.accountReady || Boolean(opts.skipped[step]);
    case "sources": {
      // A debrid account, a Usenet provider, or a torrent addon. Skip only skips the test.
      const usenet = opts.providers.filter((p) => p.enabled);
      const debrid = (opts.debrid ?? []).filter((d) => d.enabled && d.kind && d.apiKey.trim());
      const addons = opts.indexers.filter((i) => i.enabled && isStremio(i) && i.url.trim());
      const present = usenet.length + debrid.length + addons.length > 0;
      if (!present) return Boolean(opts.readyMade);
      if (opts.skipped[step]) return true;
      // Addon links are tested on the indexer step, where their fields are.
      return (
        usenet.every((p) => p.host.trim() && opts.providerTests[p.id]?.status === "ok") &&
        debrid.every((d) => opts.debridTests?.[d.id]?.status === "ok")
      );
    }
    case "indexers": {
      const enabled = opts.indexers.filter((i) => i.enabled);
      if (enabled.length === 0) return false;
      if (opts.skipped[step]) return true;
      return enabled.every((i) => i.url.trim() && opts.indexerTests[i.id]?.status === "ok");
    }
    case "catalog":
      // Without TMDB there is no catalogue to browse. Finishing later is a separate button.
      return opts.tmdbReady !== false;
    default:
      return true;
  }
}

export default function Setup() {
  const navigate = useNavigate();
  const app = useAppOptional();
  const [cfg, setCfg] = useState<EtherConfig | null>(null);
  const [languageChoice, setLanguageChoice] = useState<HouseholdLanguage | null>(null);
  const languageBaseline = useRef<HouseholdLanguage | null>(null);
  const [householdRules, setHouseholdRules] = useState<StreamRules | null>(null);
  const [loadError, setLoadError] = useState<unknown>(null);
  const [step, setStepState] = useState(storedStep);
  const [apps, setApps] = useState<SetupApp[]>(() => appCatalog(null));
  const [frontends, setFrontends] = useState<Record<string, boolean>>({});
  const [error, setError] = useState<unknown>(null);
  const [saving, setSaving] = useState(false);
  const [cacheRestartRequired, setCacheRestartRequired] = useState(false);
  const [skipped, setSkipped] = useState<Record<string, boolean>>(() => {
    try {
      const raw = sessionStorage.getItem("ether.setup.skipped");
      return raw ? (JSON.parse(raw) as Record<string, boolean>) : {};
    } catch {
      return {};
    }
  });
  const setStep = (n: number) => {
    const nextStep = Math.max(0, Math.min(n, wizardSteps(true).length - 1));
    sessionStorage.setItem(SETUP_STEP_KEY, String(nextStep));
    setStepState(nextStep);
  };
  const [providerTests, setProviderTests] = useState<Record<string, TestState>>({});
  const [indexerTests, setIndexerTests] = useState<Record<string, TestState>>({});
  const [debridTests, setDebridTests] = useState<Record<string, TestState>>({});
  // A build with a bundled TMDB key needs none from the owner.
  const [tmdbBundled, setTmdbBundled] = useState(false);
  useEffect(() => {
    void api
      .bootstrap()
      .then((boot) => setLocalDone(Boolean(boot.ownerConfigured)))
      .catch(() => {});
    api
      .setupStatus()
      .then((s) => {
        const catalog = s.steps.find((x) => x.id === "catalog");
        setTmdbBundled(Boolean(catalog?.complete));
        setCacheVolume(s.cacheVolume ?? "");
        setServerSteps(Object.fromEntries(s.steps.map((step) => [step.id, step.complete])));
        if (s.setupComplete && s.cacheRestartRequired) setCacheRestartRequired(true);
      })
      .catch(() => {});
  }, []);
  const [optTests, setOptTests] = useState<Record<string, TestState>>({});
  const [pin, setPin] = useState<{ id: string; code: string; authUrl: string } | null>(null);
  const [pinDone, setPinDone] = useState(false);
  const [localDone, setLocalDone] = useState(false);
  const [localUsername, setLocalUsername] = useState("");
  const [localPassword, setLocalPassword] = useState("");
  const [localPasswordAgain, setLocalPasswordAgain] = useState("");
  const [plexLater, setPlexLater] = useState(false);
  const [readyMade, setReadyMade] = useState(false);
  const [cacheVolume, setCacheVolume] = useState("");
  const [serverSteps, setServerSteps] = useState<Record<string, boolean>>({});
  const [finished, setFinished] = useState(false);
  const [claimMsg, setClaimMsg] = useState("");
  const [plexRecovery, setPlexRecovery] = useState(false);
  const [claimFailed, setClaimFailed] = useState(false);
  const [disk, setDisk] = useState<DiskInfo | null>(null);
  const pollRef = useRef<number | null>(null);
  const mounted = useRef(false);
  const diskRun = useRef(0);
  const pinRun = useRef(0);
  const claimRun = useRef(0);
  const claiming = useRef(false);

  const load = useCallback(() => {
    Promise.all([
      api.getConfig(),
      apiFetch<{ descriptor: { id: string; name: string } }[]>("/frontends").catch(() => null),
      apiFetch<Partial<StreamRules>>("/stream-preferences/0").catch(() => ({})),
    ])
      .then(([c, list, rules]) => {
        const catalog = appCatalog(list);
        setApps(catalog);
        setFrontends(
          selectionFrom(
            c,
            catalog.map((item) => item.id),
          ),
        );
        const normalized = normalizeRules(rules);
        setHouseholdRules(normalized);
        if (!languageBaseline.current) {
          const choice = householdLanguageFor({
            setupComplete: c.setupComplete,
            catalogLanguage: c.catalog.language,
            catalogRegion: c.catalog.region,
            audioLanguages: normalized.audioLanguages,
            subtitleLanguages: normalized.subtitleLanguages,
            preferOriginal: normalized.preferOriginal,
          });
          languageBaseline.current = choice;
          setLanguageChoice(choice);
        }
        setCfg(c);
        setPinDone(Boolean(c.plex.token));
        setLoadError(null);
      })
      .catch(setLoadError);
  }, []);
  useEffect(() => {
    mounted.current = true;
    load();
    return () => {
      mounted.current = false;
      diskRun.current++;
      pinRun.current++;
      claimRun.current++;
      claiming.current = false;
      if (pollRef.current) window.clearInterval(pollRef.current);
      pollRef.current = null;
    };
  }, [load]);

  const refreshDisk = useCallback((path: string) => {
    const run = ++diskRun.current;
    setDisk(null);
    api
      .disk(path)
      .then((info) => {
        if (run === diskRun.current) setDisk(info);
      })
      .catch(() => {
        if (run === diskRun.current) setDisk(null);
      });
  }, []);
  const plexSelected = Boolean(frontends.plex);
  const showLocalOwner = !plexSelected || plexLater;
  const steps = wizardSteps(plexSelected && !plexLater);
  const stepId = steps[step]?.id ?? "apps";
  useEffect(() => {
    sessionStorage.setItem("ether.setup.skipped", JSON.stringify(skipped));
  }, [skipped]);
  useEffect(() => {
    if (stepId !== "cache" || !cfg || !disk) return;
    if (cachePresetKey(cfg.cache)) return;
    const untouched = !cfg.cache.mode && cfg.cache.budgetBytes === 50 * 1024 ** 3;
    if (!untouched) return;
    const preset = presetForFreeBytes(disk.freeBytes);
    if (!preset) return;
    setCfg({ ...cfg, cache: { ...cfg.cache, ...preset.values } });
  }, [stepId, disk, cfg]);
  useEffect(() => {
    if (stepId === "cache" && cfg) refreshDisk(cfg.cache.dir);
    return () => {
      diskRun.current++;
    };
  }, [stepId, cfg?.cache.dir]); // eslint-disable-line react-hooks/exhaustive-deps

  const claimServer = async () => {
    if (!mounted.current || claiming.current) return;
    claiming.current = true;
    const run = ++claimRun.current;
    const current = () => mounted.current && run === claimRun.current;
    setSaving(true);
    setError(null);
    try {
      try {
        await api.claim();
        if (!current()) return;
        setClaimMsg(t("Server claimed to your Plex account."));
        setClaimFailed(false);
      } catch (e) {
        if (!current()) return;
        setClaimMsg(t("Signed in, but claiming failed: {error}", { error: (e as Error).message }));
        setClaimFailed(true);
        setError(e);
      }
      // PIN approval and claiming update the server, not the wizard's draft.
      // Refresh only Plex so the next full-config save retains those credentials.
      const { plex } = await api.getConfig();
      if (!current()) return;
      setCfg((c) => (c ? { ...c, plex } : c));
      // Do not allow the old draft token to be saved if refreshing the
      // newly stored credentials failed.
      setPlexRecovery(false);
    } catch (e) {
      if (current()) {
        setClaimFailed(true);
        setError(e);
      }
    } finally {
      if (current()) {
        claiming.current = false;
        setSaving(false);
      }
    }
  };

  const startPin = async () => {
    // A saved token may have been revoked at plex.tv. Reauthorize without
    // clearing the owner, machine identity, or unrelated setup settings.
    setPlexRecovery(true);
    setClaimFailed(false);
    setClaimMsg("");
    const run = ++pinRun.current;
    const current = () => mounted.current && run === pinRun.current;
    if (pollRef.current) window.clearInterval(pollRef.current);
    pollRef.current = null;
    setSaving(true);
    setError(null);
    try {
      const p = await api.pinStart();
      if (!current()) return;
      setPin(p);
      window.open(p.authUrl, "_blank", "noopener,noreferrer");
      let polling = false;
      pollRef.current = window.setInterval(async () => {
        if (!current() || polling) return;
        polling = true;
        try {
          const { done } = await api.pinPoll(p.id);
          if (!current() || !done) return;
          if (pollRef.current) window.clearInterval(pollRef.current);
          pollRef.current = null;
          setPin(null);
          setPinDone(true);
          setClaimMsg(t("Signed in. Claiming this server…"));
          await claimServer();
        } catch (e) {
          if (!current()) return;
          if (pollRef.current) window.clearInterval(pollRef.current);
          pollRef.current = null;
          setPin(null);
          setError(e);
        } finally {
          polling = false;
        }
      }, 2000);
    } catch (e) {
      if (current()) setError(e);
    } finally {
      if (current()) setSaving(false);
    }
  };

  const runTest = async (
    kind: "provider" | "indexer" | "debrid" | "opt",
    key: string,
    fn: () => Promise<{ ok: boolean; error?: string; durationMs: number }>,
  ) => {
    const setMap =
      kind === "provider"
        ? setProviderTests
        : kind === "indexer"
          ? setIndexerTests
          : kind === "debrid"
            ? setDebridTests
            : setOptTests;
    setMap((m) => ({ ...m, [key]: { status: "running" } }));
    try {
      const r = await fn();
      setMap((m) => ({
        ...m,
        [key]: r.ok
          ? { status: "ok", durationMs: r.durationMs }
          : { status: "fail", message: r.error ?? t("failed") },
      }));
    } catch (e) {
      setMap((m) => ({ ...m, [key]: { status: "fail", message: (e as Error).message } }));
    }
  };

  const advanceOk = useMemo(
    () =>
      cfg
        ? canAdvance(stepId, {
            accountReady: showLocalOwner
              ? localDone ||
                (localUsername.trim() !== "" &&
                  localPassword !== "" &&
                  localPassword === localPasswordAgain)
              : Boolean(cfg.plex.claimed && cfg.plex.token) && !plexRecovery && !claimFailed,
            providers: cfg.usenet.providers ?? [],
            indexers: cfg.indexers ?? [],
            providerTests,
            indexerTests,
            debrid: cfg.debrid?.providers ?? [],
            debridTests,
            tmdbReady:
              optTests.tmdb?.status === "ok" || cfg.catalog.tmdbKey === "••••" || tmdbBundled,
            skipped,
            readyMade,
          })
        : false,
    [
      cfg,
      stepId,
      plexSelected,
      providerTests,
      indexerTests,
      debridTests,
      optTests,
      tmdbBundled,
      skipped,
      localDone,
      localUsername,
      localPassword,
      localPasswordAgain,
      plexLater,
      plexRecovery,
      claimFailed,
      readyMade,
    ],
  );

  if (loadError && !cfg)
    return (
      <div className="p-6">
        <ErrorPanel error={loadError} onRetry={load} />
      </div>
    );
  if (!cfg)
    return (
      <div className="grid flex-1 place-items-center">
        <Spinner size={22} label={t("Loading configuration")} />
      </div>
    );

  if (cacheRestartRequired)
    return (
      <div className="mx-auto w-full max-w-2xl space-y-4 p-6">
        <EtherMark size={40} />
        <h1 className="text-xl font-semibold">{t("Restart Ether to use your cache folder")}</h1>
        <p>{t("Setup is saved. Restart Ether once to start using the cache folder you chose.")}</p>
        <p className="text-sm text-fg-muted">
          {tx(
            "Your selected media cache folder is {folder}. The running server keeps using its previous folder until it restarts. Existing cached media stays in its original folder; it is not moved automatically.",
            { folder: <code>{cfg.cache.dir}</code> },
          )}
        </p>
        <p className="text-sm text-fg-muted">
          {t(
            "Posters and other artwork remain in the configuration folder's cache. They are separate from the media cache.",
          )}
        </p>
        <div className="flex flex-wrap gap-2 pt-2">
          <RestartButton
            variant="default"
            size="md"
            context={t("Restarting puts your new cache folder to work.")}
          />
          <Button variant="outline" onClick={() => navigate("/cache")}>
            {t("Open cache settings")}
          </Button>
        </div>
      </div>
    );

  const providers = cfg.usenet.providers ?? [];
  const indexers = cfg.indexers ?? [];
  const setProviders = (list: UsenetProvider[]) =>
    setCfg({ ...cfg, usenet: { providers: list.map((p, i) => ({ ...p, priority: i })) } });
  const setIndexers = (list: Indexer[]) =>
    setCfg({ ...cfg, indexers: list.map((x, i) => ({ ...x, priority: i })) });
  const patchProvider = (i: number, p: Partial<UsenetProvider>) =>
    setProviders(providers.map((x, j) => (j === i ? { ...x, ...p } : x)));
  const patchIndexer = (i: number, p: Partial<Indexer>) =>
    setIndexers(indexers.map((x, j) => (j === i ? { ...x, ...p } : x)));
  const debridSupported = cfg.debrid !== undefined;
  const debrid = cfg.debrid?.providers ?? [];
  const setDebrid = (list: DebridProvider[]) =>
    setCfg({
      ...cfg,
      debrid: {
        ...(cfg.debrid ?? { cachedOnly: true, removeUncached: true, providers: [] }),
        providers: list.map((d, i) => ({ ...d, priority: i })),
      },
    });
  const patchDebrid = (i: number, d: Partial<DebridProvider>) =>
    setDebrid(debrid.map((x, j) => (j === i ? { ...x, ...d } : x)));
  // Debrid streams torrents, Usenet providers stream Newznab results.
  const hasDebrid = debrid.some((d) => d.enabled);
  const hasUsenet = providers.some((p) => p.enabled);
  const newIndexerType: IndexerType = hasDebrid && !hasUsenet ? "stremio" : "newznab";

  const next = async (opts?: { force?: boolean }) => {
    setSaving(true);
    setError(null);
    try {
      if (stepId === "account" && showLocalOwner && !localDone) {
        const { token } = await api.localSetup(localUsername.trim(), localPassword);
        setToken(token);
        setLocalPassword("");
        setLocalPasswordAgain("");
        setLocalDone(true);
        const status = await api.setupStatus();
        setServerSteps(Object.fromEntries(status.steps.map((item) => [item.id, item.complete])));
      }
      let draft = cfg;
      if (stepId === "language" && languageChoice) {
        const baseline = languageBaseline.current;
        const edited = !baseline || !sameHouseholdLanguage(languageChoice, baseline);
        const audioEdited =
          !baseline ||
          baseline.audio !== languageChoice.audio ||
          baseline.subtitle !== languageChoice.subtitle;
        if (
          !cfg.setupComplete ||
          edited ||
          languageChoice.metadata !== cfg.catalog.language ||
          languageChoice.region !== cfg.catalog.region
        ) {
          draft = {
            ...cfg,
            catalog: {
              ...cfg.catalog,
              language: languageChoice.metadata,
              region: languageChoice.region,
            },
          };
        }
        if (!cfg.setupComplete || audioEdited) {
          const saved = normalizeRules(
            await apiFetch<Partial<StreamRules>>("/stream-preferences/0", {
              method: "PUT",
              body: JSON.stringify(
                projectHouseholdRules(householdRules ?? normalizeRules(null), languageChoice),
              ),
            }),
          );
          setHouseholdRules(saved);
        }
        languageBaseline.current = languageChoice;
        rememberLocale(languageChoice.console);
        await loadLocale(languageChoice.console);
      }
      const saved = await api.putConfig(
        savedDraft(
          mergeFrontendSelection(
            draft,
            apps.map((item) => item.id),
            frontends,
          ),
        ),
      );
      // The empty addon is not stored until it has an address. Keep the row so the next step can take it.
      if (readyMade && !(saved.indexers ?? []).some((item) => isStremio(item))) {
        saved.indexers = [...(saved.indexers ?? []), blankIndexer("stremio")];
      }
      setCfg(saved);
      if (opts?.force) await finish(true);
      else if (step < steps.length - 1) setStep(step + 1);
      else await finish(false);
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  };

  const finish = async (force: boolean) => {
    const status = await api.setupComplete(force);
    setServerSteps(Object.fromEntries(status.steps.map((item) => [item.id, item.complete])));
    sessionStorage.removeItem(SETUP_STEP_KEY);
    sessionStorage.removeItem("ether.setup.skipped");
    await app?.reloadBoot();
    if (status.cacheRestartRequired) setCacheRestartRequired(true);
    else setFinished(true);
  };

  const plexSigned = pinDone || Boolean(cfg.plex.claimed && cfg.plex.token);
  const passwordsMatch = localPassword !== "" && localPassword === localPasswordAgain;

  if (finished) return <SetupReady />;

  return (
    <WizardLayout
      header={
        <div>
          <div className="flex items-center gap-3">
            <EtherMark size={32} />
            <div>
              <h1 className="text-lg font-semibold text-fg">{t("Set up Ether")}</h1>
              <p className="text-sm text-fg-muted">
                {t("Complete the steps below to set up your household.")}
              </p>
            </div>
          </div>
          <ol className="mt-4 flex gap-1.5" aria-label={t("Setup progress")}>
            {steps.map((s, i) => {
              const later = s.id === "account" && plexSelected && plexLater && !plexSigned;
              const label = later ? `${s.title} · ${t("Plex comes later")}` : s.title;
              return (
                <li key={i} className="min-w-0 flex-1">
                  <button
                    type="button"
                    disabled={i > step}
                    onClick={() => setStep(i)}
                    aria-current={i === step ? "step" : undefined}
                    title={label}
                    className={cn(
                      "w-full min-w-0 text-left disabled:cursor-not-allowed",
                      i <= step ? "text-fg" : "text-fg-subtle",
                    )}
                  >
                    <span
                      className={cn(
                        "block h-1 rounded-full",
                        i < step ? "bg-primary" : i === step ? "bg-primary/60" : "bg-border",
                      )}
                    />
                    <span className="mt-1.5 flex min-w-0 items-center gap-1 text-xs font-medium">
                      {i < step &&
                      !(s.id === "account" && plexSelected && plexLater && !plexSigned) &&
                      (!((s.id === "account" ? "plex" : s.id) in serverSteps) ||
                        serverSteps[s.id === "account" ? "plex" : s.id]) ? (
                        <Check size={12} className="shrink-0 text-primary" />
                      ) : (
                        <span className="shrink-0 tabular-nums">{i + 1}.</span>
                      )}{" "}
                      <span className="truncate">{label}</span>
                    </span>
                  </button>
                </li>
              );
            })}
          </ol>
        </div>
      }
      footer={
        <div className="flex flex-col gap-1">
          <div className="flex items-center gap-3">
            <Button
              variant="ghost"
              disabled={step === 0 || saving}
              onClick={() => setStep(step - 1)}
            >
              {t("Back")}
            </Button>
            {stepId === "sources" || stepId === "indexers" || stepId === "catalog" ? (
              <label className="flex items-center gap-2 text-xs text-fg-muted">
                <Checkbox
                  checked={Boolean(skipped[stepId])}
                  onChange={(e) => setSkipped({ ...skipped, [stepId]: e.currentTarget.checked })}
                />
                {t("Skip the connection test")}
              </label>
            ) : null}
            <span className="flex-1" />
            <Button
              className="shrink-0"
              disabled={!advanceOk || saving}
              onClick={() => void next()}
            >
              {saving
                ? t("Saving…")
                : step === steps.length - 1
                  ? t("Finish setup")
                  : t("Save and continue")}
            </Button>
          </div>
          {(stepId === "sources" || stepId === "indexers" || stepId === "catalog") && !advanceOk ? (
            <div className="text-right">
              <Button
                variant="link"
                disabled={saving || (!localDone && !plexSigned)}
                onClick={() => void next({ force: true })}
              >
                {stepId === "catalog"
                  ? t("Finish now, add the TMDB key later")
                  : t("Finish now, add sources later")}
              </Button>
            </div>
          ) : null}
        </div>
      }
    >
      <h2 className="text-base font-semibold text-fg">{steps[step]?.heading}</h2>
      <MutationError error={error} />

      {stepId === "language" && languageChoice ? (
        <LanguageStep choice={languageChoice} onChange={setLanguageChoice} />
      ) : null}
      {stepId === "apps" ? (
        <div className="space-y-4">
          <p className="text-sm text-fg-muted">
            {t(
              "Most households pick Plex or Jellyfin. You will need a debrid token or a Usenet login to play anything. A Plex account is only needed if Plex is on. You can finish and add sources later.",
            )}
          </p>
          <p className="text-sm text-fg-muted">
            {t(
              "People at home use the address on each app. People away from home use Plex, or a private network tool (Tailscale).",
            )}
          </p>
          <div className="grid gap-4 md:grid-cols-2">
            {apps.map((item) => (
              <IntegrationAppCard
                key={item.id}
                identity={{ id: item.id, name: item.name }}
                description={item.description}
                logo={setupAppLogo(item.id)}
                enabled={Boolean(frontends[item.id])}
                disabled={saving}
                busy={saving}
                onToggle={(enabled) => setFrontends({ ...frontends, [item.id]: enabled })}
              />
            ))}
          </div>
        </div>
      ) : null}
      {stepId === "account" && showLocalOwner ? (
        <div className="space-y-4">
          {plexSelected && plexLater ? (
            <p className="text-sm text-fg-muted">
              {t(
                "Plex comes later. This login is only for Ether. You will connect Plex from Apps.",
              )}
            </p>
          ) : null}
          {localDone ? (
            <p className="text-sm text-fg-muted">{t("Owner created")}</p>
          ) : (
            <>
              <Field label={t("Username")} htmlFor="setup-owner-username">
                <Input
                  id="setup-owner-username"
                  autoComplete="username"
                  value={localUsername}
                  onChange={(e) => setLocalUsername(e.currentTarget.value)}
                />
              </Field>
              <Field label={t("Password")} htmlFor="setup-owner-password">
                <PasswordInput
                  id="setup-owner-password"
                  autoComplete="new-password"
                  value={localPassword}
                  onChange={(e) => setLocalPassword(e.currentTarget.value)}
                />
              </Field>
              <Field label={t("Password again")} htmlFor="setup-owner-password-again">
                <PasswordInput
                  id="setup-owner-password-again"
                  autoComplete="new-password"
                  value={localPasswordAgain}
                  onChange={(e) => setLocalPasswordAgain(e.currentTarget.value)}
                />
              </Field>
              <p className="text-sm text-fg-muted">
                {tn(
                  3 -
                    [localUsername.trim(), localPassword, passwordsMatch ? "ok" : ""].filter(
                      Boolean,
                    ).length,
                  "{n} required field left",
                  "{n} required fields left",
                  {
                    n:
                      3 -
                      [localUsername.trim(), localPassword, passwordsMatch ? "ok" : ""].filter(
                        Boolean,
                      ).length,
                  },
                )}
              </p>
            </>
          )}
        </div>
      ) : null}
      {stepId === "account" && plexSelected && !plexLater ? (
        <div className="space-y-4">
          <p className="text-sm text-fg-muted">
            {t("Signs this server in to your Plex account so Plex apps can find it.")}
          </p>
          <p className="text-sm text-fg-muted">
            {t(
              "This is a separate server. It does not change an existing Plex Media Server or the people who share it.",
            )}
          </p>
          {plexSigned && !plexRecovery && !claimFailed ? (
            <Alert variant="success">
              <AlertDescription>
                {cfg.plex.ownerUsername
                  ? t("Signed in as {user}.", { user: cfg.plex.ownerUsername })
                  : t("Signed in.")}{" "}
                {claimMsg}
              </AlertDescription>
            </Alert>
          ) : null}
          {pin ? (
            <div className="space-y-3 rounded-xl border border-border bg-surface p-4">
              <p className="text-sm text-fg">
                {t(
                  "A plex.tv window opened. Sign in there and allow Ether; this page continues on its own.",
                )}
              </p>
              <div className="flex flex-wrap items-center gap-3 text-sm text-fg-muted">
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => window.open(pin.authUrl, "_blank", "noopener,noreferrer")}
                >
                  {t("Open plex.tv sign-in")} <ExternalLink size={13} />
                </Button>
                <span className="inline-flex items-center gap-2">
                  {t("Waiting for approval")} <Spinner size={13} label={t("Waiting")} />
                </span>
              </div>
              {/* Short PINs can also be typed at plex.tv/link; strong ones cannot. */}
              {pin.code.length <= 8 ? (
                <p className="text-sm text-fg-muted">
                  {tx("Or enter {code} at plex.tv/link.", {
                    code: <span className="font-mono font-semibold text-fg">{pin.code}</span>,
                  })}
                </p>
              ) : null}
            </div>
          ) : (
            <Button disabled={saving} onClick={() => void startPin()}>
              {plexSigned ? t("Reconnect") : t("Sign in to plex.tv")}
            </Button>
          )}
          {claimMsg && (!cfg.plex.claimed || claimFailed) ? (
            <p className="text-sm text-fg-muted">{claimMsg}</p>
          ) : null}
          {plexSigned && (!cfg.plex.claimed || claimFailed) && !pin ? (
            <Button variant="outline" disabled={saving} onClick={() => void claimServer()}>
              {t("Claim server")}
            </Button>
          ) : null}
          <div className="flex flex-wrap gap-3">
            <Button variant="outline" disabled={saving} onClick={() => setPlexLater(true)}>
              {t("Connect Plex later")}
            </Button>
          </div>
        </div>
      ) : null}

      {stepId === "sources" ? (
        <div className="space-y-3">
          {debridSupported ? (
            <SourceChoice
              pressed={debrid.length > 0}
              logo={<KeyRound size={18} />}
              title={t("Debrid service")}
              line={t("Real-Debrid, TorBox, AllDebrid… (fast, paid)")}
              onClick={() => {
                if (debrid.length === 0) {
                  setDebrid([blankDebrid()]);
                  return;
                }
                if (debrid.every((row) => row.apiKey.trim() === "")) setDebrid([]);
              }}
            >
              {debrid.map((d, i) => (
                <fieldset
                  key={d.id || i}
                  className="rounded-xl border border-border bg-surface p-4"
                >
                  <legend className="px-1 text-sm font-medium text-fg">
                    {DEBRID_KINDS.find((k) => k.value === d.kind)?.label ?? d.kind}
                  </legend>
                  <div className="grid gap-3 sm:grid-cols-2">
                    <Field label={t("Service")}>
                      <SearchSelect
                        ariaLabel={t("Debrid service")}
                        placeholder={t("Select…")}
                        searchPlaceholder={t("Search…")}
                        noMatchesLabel={t("No matches")}
                        selectLabel={t("Select")}
                        closeLabel={t("Close")}
                        value={d.kind}
                        options={DEBRID_KINDS}
                        onChange={(v) => patchDebrid(i, { kind: v as DebridProvider["kind"] })}
                      />
                    </Field>
                    {d.kind ? (
                      <Field label={t("API token")}>
                        <SecretInput
                          ariaLabel={t("API token")}
                          value={d.apiKey}
                          onChange={(v) => patchDebrid(i, { apiKey: v })}
                        />
                        {DEBRID_TOKEN_URL[d.kind] ? (
                          <a
                            className="text-sm text-primary underline"
                            href={DEBRID_TOKEN_URL[d.kind]}
                            target="_blank"
                            rel="noreferrer"
                          >
                            {t("Find your {service} API token", {
                              service:
                                DEBRID_KINDS.find((k) => k.value === d.kind)?.label ?? d.kind,
                            })}
                          </a>
                        ) : null}
                      </Field>
                    ) : null}
                  </div>
                  <div className="mt-3 flex flex-wrap items-center justify-between gap-2">
                    <label className="flex items-center gap-2 text-sm text-fg">
                      <Switch
                        checked={d.enabled}
                        onChange={(e) => patchDebrid(i, { enabled: e.currentTarget.checked })}
                      />{" "}
                      {t("Enabled")}
                    </label>
                    <span className="flex items-center gap-2">
                      <TestButton
                        state={debridTests[d.id] ?? { status: "idle" }}
                        onTest={() => void runTest("debrid", d.id, () => api.testDebrid(d))}
                      />
                      <Button
                        variant="ghost"
                        size="sm"
                        onClick={() => setDebrid(debrid.filter((_, j) => j !== i))}
                      >
                        <Trash2 size={14} /> {t("Remove")}
                      </Button>
                    </span>
                  </div>
                </fieldset>
              ))}
              <Button variant="outline" onClick={() => setDebrid([...debrid, blankDebrid()])}>
                <Plus size={15} /> {t("Add debrid account")}
              </Button>
            </SourceChoice>
          ) : null}
          <SourceChoice
            pressed={providers.length > 0}
            logo={<Newspaper size={18} />}
            title={t("Usenet provider")}
            line={t("NZB login from a provider you pay")}
            onClick={() => {
              if (providers.length === 0) {
                setProviders([blankProvider()]);
                return;
              }
              if (
                providers.every(
                  (row) => !row.host.trim() && !row.username.trim() && !row.name.trim(),
                )
              ) {
                setProviders([]);
              }
            }}
          >
            <h3 className="text-sm font-semibold text-fg">{t("Usenet providers")}</h3>
            {providers.map((p, i) => (
              <fieldset key={p.id || i} className="rounded-xl border border-border bg-surface p-4">
                <legend className="px-1 text-sm font-medium text-fg">
                  {p.name || t("Provider {number}", { number: i + 1 })}
                </legend>
                <div className="grid gap-3 sm:grid-cols-2">
                  <Field label={t("Name")}>
                    <Input
                      value={p.name}
                      onChange={(e) => patchProvider(i, { name: e.currentTarget.value })}
                    />
                  </Field>
                  <Field label={t("Host")}>
                    <Input
                      value={p.host}
                      placeholder="news.example.com"
                      onChange={(e) => patchProvider(i, { host: e.currentTarget.value })}
                    />
                  </Field>
                  <Field label={t("Port")}>
                    <NumberInput
                      aria-label={t("Port")}
                      value={p.port}
                      min={1}
                      max={65535}
                      onChange={(n) => patchProvider(i, { port: n })}
                    />
                  </Field>
                  <Field label={t("Connections")}>
                    <NumberInput
                      aria-label={t("Connections")}
                      value={p.connections}
                      min={0}
                      max={500}
                      onChange={(n) => patchProvider(i, { connections: n })}
                    />
                  </Field>
                  <Field label={t("Username")}>
                    <Input
                      value={p.username}
                      autoComplete="username"
                      onChange={(e) => patchProvider(i, { username: e.currentTarget.value })}
                    />
                  </Field>
                  <Field label={t("Password")}>
                    <SecretInput
                      ariaLabel={t("Password")}
                      value={p.password}
                      onChange={(v) => patchProvider(i, { password: v })}
                    />
                  </Field>
                </div>
                <div className="mt-3 flex flex-wrap gap-5 text-sm text-fg">
                  <label className="flex items-center gap-2">
                    <Switch
                      checked={p.tls}
                      onChange={(e) => patchProvider(i, { tls: e.currentTarget.checked })}
                    />{" "}
                    SSL / TLS
                  </label>
                  <label className="flex items-center gap-2">
                    <Switch
                      checked={p.backfill}
                      onChange={(e) => patchProvider(i, { backfill: e.currentTarget.checked })}
                    />{" "}
                    {t("Backfill only")}
                  </label>
                  <label className="flex items-center gap-2">
                    <Switch
                      checked={p.enabled}
                      onChange={(e) => patchProvider(i, { enabled: e.currentTarget.checked })}
                    />{" "}
                    {t("Enabled")}
                  </label>
                </div>
                <div className="mt-3 flex flex-wrap items-center justify-between gap-2">
                  <TestButton
                    state={providerTests[p.id] ?? { status: "idle" }}
                    onTest={() => void runTest("provider", p.id, () => api.testUsenet(p))}
                  />
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => setProviders(providers.filter((_, j) => j !== i))}
                  >
                    <Trash2 size={14} /> {t("Remove")}
                  </Button>
                </div>
              </fieldset>
            ))}
            <Button variant="outline" onClick={() => setProviders([...providers, blankProvider()])}>
              <Plus size={15} /> {t("Add provider")}
            </Button>
          </SourceChoice>
          <SourceChoice
            pressed={
              readyMade ||
              indexers.some((item) => isStremio(item) && item.url.trim() === TORRENTIO_URL)
            }
            logo={<StremioMark />}
            title="Torrentio"
            line={t("finds releases; needs debrid to play")}
            detail={t(
              "Lookups go to torrentio.strem.fun. Prefer your own? Use Torznab (Prowlarr, Jackett, Zilean).",
            )}
            note={
              readyMade ||
              indexers.some((item) => isStremio(item) && item.url.trim() === TORRENTIO_URL)
                ? t("Added. You'll test it on the next step.")
                : undefined
            }
            onClick={() => {
              const chosen = indexers.some(
                (item) => isStremio(item) && item.url.trim() === TORRENTIO_URL,
              );
              if (readyMade || chosen) {
                const removed = indexers.filter(
                  (item) => isStremio(item) && item.url.trim() === TORRENTIO_URL,
                );
                setIndexers(indexers.filter((item) => !removed.some((row) => row.id === item.id)));
                setIndexerTests((current) => {
                  const nextTests = { ...current };
                  for (const row of removed) delete nextTests[row.id];
                  return nextTests;
                });
                setReadyMade(false);
                return;
              }
              const row = { ...blankIndexer("stremio"), name: "Torrentio", url: TORRENTIO_URL };
              const rest = indexers.filter((item) => !(isStremio(item) && !item.url.trim()));
              setIndexers([...rest, row]);
              setReadyMade(true);
              void runTest("indexer", row.id, () => api.testIndexer(normalizedIndexer(row)));
            }}
          />
        </div>
      ) : null}

      {stepId === "indexers" ? (
        <div className="space-y-4">
          <p className="text-sm text-fg-muted">
            {hasDebrid
              ? t(
                  "Add at least one torrent source for your debrid account: a Stremio stream addon's install link, or a Torznab URL from Prowlarr, Jackett or Zilean.",
                )
              : t("Add at least one indexer.")}{" "}
            {hasUsenet && hasDebrid ? `${t("Add a Newznab indexer for Usenet too.")} ` : ""}
            {t("Ether handles service limits automatically.")}
          </p>
          {indexers.map((ix, i) => {
            const type = indexerType(ix);
            const addon = type === "stremio";
            return (
              <fieldset key={ix.id || i} className="rounded-xl border border-border bg-surface p-4">
                <legend className="px-1 text-sm font-medium text-fg">
                  {ix.name || t("Indexer {number}", { number: i + 1 })}
                </legend>
                <div className="grid gap-3 sm:grid-cols-2">
                  <Field label={t("Type")}>
                    <SearchSelect
                      aria-label={t("Indexer type")}
                      placeholder={t("Select…")}
                      searchPlaceholder={t("Search…")}
                      noMatchesLabel={t("No matches")}
                      selectLabel={t("Select")}
                      closeLabel={t("Close")}
                      value={type}
                      options={INDEXER_TYPES}
                      onChange={(v) => {
                        setIndexers(
                          indexers.map((x, j) =>
                            j === i ? withIndexerType(x, v as IndexerType) : x,
                          ),
                        );
                      }}
                    />
                  </Field>
                  <p className="text-sm text-fg-muted sm:col-span-2">
                    {INDEXER_TYPES.find((item) => item.value === type)?.hint}
                  </p>
                  <Field label={t("Name")}>
                    <Input
                      value={ix.name}
                      onChange={(e) => patchIndexer(i, { name: e.currentTarget.value })}
                    />
                  </Field>
                  {addon ? (
                    <Field
                      label={t("Addon link")}
                      hint={t(
                        "The addon's install link (https:// or stremio://). It must expose torrent results, not only direct or proxied playback links. Ether checks your debrid accounts itself, so the link needs no debrid key.",
                      )}
                    >
                      <Input
                        value={ix.url}
                        placeholder="https://addon.example/manifest.json"
                        onChange={(e) => patchIndexer(i, { url: e.currentTarget.value })}
                        onBlur={() => {
                          if (ix.name.trim()) return;
                          const name = indexerNameFromUrl(ix.url);
                          if (name) patchIndexer(i, { name });
                        }}
                      />
                      {ix.url.includes("torrentio.strem.fun") ? (
                        <p className="text-sm text-fg-muted">
                          {t(
                            "Lookups go to torrentio.strem.fun. Prefer your own? Use Torznab (Prowlarr, Jackett, Zilean).",
                          )}
                        </p>
                      ) : null}
                    </Field>
                  ) : (
                    <>
                      <Field label={t("Base URL")} hint={t("/api is added automatically.")}>
                        <Input
                          value={ix.url}
                          placeholder="https://indexer.example.com"
                          onChange={(e) => patchIndexer(i, { url: e.currentTarget.value })}
                          onBlur={() => {
                            if (ix.name.trim()) return;
                            const name = indexerNameFromUrl(ix.url);
                            if (name) patchIndexer(i, { name });
                          }}
                        />
                      </Field>
                      <Field label={t("API key")}>
                        <SecretInput
                          ariaLabel={t("API key")}
                          value={ix.apiKey}
                          onChange={(v) => patchIndexer(i, { apiKey: v })}
                        />
                      </Field>
                    </>
                  )}
                </div>
                {addon && addonUrlHasDebridKey(ix.url) ? (
                  <Alert variant="warning" role="status" className="mt-3">
                    <AlertDescription>
                      {t(
                        "This link includes a debrid API key. Ether checks your debrid accounts itself, so the addon doesn't need it; a key only adds cached hints.",
                      )}
                    </AlertDescription>
                  </Alert>
                ) : null}
                <div className="mt-3 flex flex-wrap items-center justify-between gap-2">
                  <label className="flex items-center gap-2 text-sm text-fg">
                    <Switch
                      checked={ix.enabled}
                      onChange={(e) => patchIndexer(i, { enabled: e.currentTarget.checked })}
                    />{" "}
                    {t("Enabled")}
                  </label>
                  <span className="flex items-center gap-2">
                    <TestButton
                      state={indexerTests[ix.id] ?? { status: "idle" }}
                      onTest={() =>
                        void runTest("indexer", ix.id, () => api.testIndexer(normalizedIndexer(ix)))
                      }
                    />
                    <Button
                      variant="ghost"
                      size="sm"
                      onClick={() => setIndexers(indexers.filter((_, j) => j !== i))}
                    >
                      <Trash2 size={14} /> {t("Remove")}
                    </Button>
                  </span>
                </div>
              </fieldset>
            );
          })}
          <Button
            variant="outline"
            onClick={() => setIndexers([...indexers, blankIndexer(newIndexerType)])}
          >
            <Plus size={15} /> {t("Add indexer")}
          </Button>
          <details>
            <summary className="cursor-pointer text-sm font-medium text-fg">
              {t("Import from Prowlarr")}
            </summary>
            <div className="mt-4">
              <ProwlarrImport
                onImported={() => {
                  void api.getConfig().then((next) => setIndexers(next.indexers ?? []));
                }}
              />
            </div>
          </details>
        </div>
      ) : null}

      {stepId === "cache" ? (
        <div className="space-y-5 rounded-xl border border-border bg-surface p-4">
          <Field
            label={t("Cache folder")}
            hint={t(
              "Posters and artwork stay in the configuration folder. A mounted /cache volume is already in use when this page shows it.",
            )}
          >
            <div className="flex gap-2">
              <Input
                value={cfg.cache.dir}
                onChange={(e) =>
                  setCfg({ ...cfg, cache: { ...cfg.cache, dir: e.currentTarget.value } })
                }
              />
              <Button variant="outline" onClick={() => refreshDisk(cfg.cache.dir)}>
                {t("Check space")}
              </Button>
              {cacheVolume && cfg.cache.dir !== cacheVolume ? (
                <Button
                  variant="outline"
                  onClick={() => {
                    setCfg({ ...cfg, cache: { ...cfg.cache, dir: cacheVolume } });
                    refreshDisk(cacheVolume);
                  }}
                >
                  {t("Use {path}", { path: cacheVolume })}
                </Button>
              ) : null}
            </div>
          </Field>
          {disk && cfg.cache.budgetBytes > disk.freeBytes ? (
            <Alert variant="warning">
              <AlertDescription>
                {t("This budget is larger than the free space on this disk.")}
              </AlertDescription>
            </Alert>
          ) : null}
          {disk ? (
            <p className="text-sm text-fg-muted">
              {t("{free} free of {total} on this disk.", {
                free: formatBytes(disk.freeBytes),
                total: formatBytes(disk.totalBytes),
              })}{" "}
              {cachePresetKey(cfg.cache) === "everyday"
                ? t("Everyday (32 GiB) is plenty to start; you have {free} free.", {
                    free: formatBytes(disk.freeBytes),
                  })
                : t("Suggested because only {free} is free on this disk.", {
                    free: formatBytes(disk.freeBytes),
                  })}
            </p>
          ) : null}
          <CachePresetCards
            selected={cachePresetKey(cfg.cache)}
            onSelect={(preset) => setCfg({ ...cfg, cache: { ...cfg.cache, ...preset.values } })}
          />
        </div>
      ) : null}

      {stepId === "catalog" ? (
        <div className="space-y-6">
          <section
            className="space-y-3 rounded-xl border border-border bg-surface p-4"
            aria-labelledby="setup-tmdb"
          >
            <div>
              <h3 id="setup-tmdb" className="text-sm font-semibold text-fg">
                {t("Movie and TV catalogue")}
              </h3>
              <p className="text-sm text-fg-muted">
                {tx(
                  "Ether builds your library from TMDB, so a TMDB API key is required. It is free: {link}, then paste it here and test it.",
                  {
                    link: (
                      <a
                        href="https://www.themoviedb.org/settings/api"
                        target="_blank"
                        rel="noreferrer"
                        className="inline-flex items-center gap-1 font-medium text-primary hover:underline"
                      >
                        {t("get one at themoviedb.org")} <ExternalLink size={13} />
                      </a>
                    ),
                  },
                )}
              </p>
            </div>
            <Field label={t("TMDB API key")} hint={t("Your v3 API key or v4 read access token.")}>
              <div className="space-y-2">
                <SecretInput
                  ariaLabel={t("TMDB API key")}
                  value={cfg.catalog.tmdbKey}
                  onChange={(v) => setCfg({ ...cfg, catalog: { ...cfg.catalog, tmdbKey: v } })}
                />
                <TestButton
                  state={optTests.tmdb ?? { status: "idle" }}
                  onTest={() =>
                    void runTest("opt", "tmdb", () => api.testTMDB(cfg.catalog.tmdbKey))
                  }
                />
              </div>
            </Field>
          </section>
          <p className="text-sm text-fg-muted">
            {tn(
              cfg.catalog.tmdbKey.trim() && cfg.catalog.tmdbKey !== "••••"
                ? 0
                : tmdbBundled
                  ? 0
                  : 1,
              "{n} required field left",
              "{n} required fields left",
              {
                n:
                  cfg.catalog.tmdbKey.trim() && cfg.catalog.tmdbKey !== "••••"
                    ? 0
                    : tmdbBundled
                      ? 0
                      : 1,
              },
            )}
          </p>
          <Alert variant="info">
            <AlertDescription>
              {t(
                "Finishing setup locks this admin page to your owner account. You can reopen setup later from General.",
              )}
            </AlertDescription>
          </Alert>
        </div>
      ) : null}
      {stepId === "language" ? (
        <p className="pt-2 text-center text-xs text-fg-muted">
          <Button variant="link" onClick={() => navigate("/cluster")}>
            {t("Join a cluster instead")}
          </Button>
        </p>
      ) : null}
    </WizardLayout>
  );
}
