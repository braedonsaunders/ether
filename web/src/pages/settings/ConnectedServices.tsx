import { Plus } from "lucide-react";
import { librariesApi } from "../libraries/api";
import {
  AddCatalogueSource,
  SourceIcon,
  sourceName,
  catalogueGroups,
} from "../libraries/CatalogueSources";
import { useStreamEvent } from "../../lib/stream";
import { isPluginDescriptor, setPluginEnabled, textOf, useRegistry } from "../../lib/extend";
import { InstalledPluginDrawer } from "../plugins/InstalledPluginDrawer";
import { AddSubtitleProvider } from "./AddSubtitleProvider";
import { SubtitleCheck } from "./SubtitleCheck";
import { IntegrationSetupFooter } from "../../components/IntegrationSetupFooter";
import { Badge, Button, SettingsSection, Spinner, toast } from "@braedonsaunders/appkit-ui";
import {
  Children,
  Fragment,
  isValidElement,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { useNavigate, useParams } from "react-router-dom";
import { useApp } from "../../app-context";
import { IntegrationTab } from "../../components/IntegrationTab";
import { Drawer } from "../../components/Drawer";
import { ErrorPanel, MutationError } from "../../components/feedback";
import { TestButton, type TestState } from "../../components/fields";
import { IntegrationAppCard } from "../../components/IntegrationAppCard";
import { secretConfigured } from "../../components/IntegrationAccount";
import { MDBListMark, SimklMark, TraktMark } from "../../components/integration-brands";
import { useOwnerViewerPath } from "../../components/pickers";
import { SaveBar } from "../../components/SaveBar";
import { LevelBadge } from "../../components/status";
import { localized, t, tn } from "../../i18n";
import { api, apiFetch, ApiError, errorMessage, type TestFn } from "../../lib/api";
import { useAsync } from "../../lib/hooks";
import type { EtherConfig } from "../../lib/types";
import { MetadataCatalogue } from "./MetadataCatalogue";
import { builtinMetadataSources, metadataSources } from "../../lib/metadataSources";
import { TmdbCatalogueSettings } from "./TmdbCatalogueSettings";
import { NewReleasesSettings } from "../libraries/browser/NewReleasesSettings";
import type { AvailabilityConfig } from "../../lib/types";
import { OwnerPosters } from "./OwnerPosters";
import { PosterAccounts } from "./PosterAccounts";
import { RatingSources, type RatingSettings } from "../metadata/Ratings";
import { SimklSettings } from "./SimklIntegration";
import { MDBListSettings } from "./MDBListIntegration";
import { PlexServers, PlexServersCard } from "./PlexServers";
import { LanguageFeatures } from "./LanguageFeatures";
import { LanguageModelSettings } from "./LanguageModel";
import { asObj, GenericFields, SchemaField, type FieldGroup } from "./SchemaForm";
import * as S from "./schema";

type Obj = Record<string, unknown>;

export type ServiceTab =
  | "oidc"
  | "postersplus"
  | "xrdb"
  | "betterposters"
  | "rpdb"
  | "custom"
  | "overview"
  | "tmdb"
  | "trakt"
  | "simkl"
  | "mdblist"
  | "plexservers"
  | "llm"
  | "opensubtitles"
  | "subdl"
  | "subsource"
  | "ratings"
  | "introdb";

const TAB_ORDER: ServiceTab[] = [
  "overview",
  "postersplus",
  "xrdb",
  "betterposters",
  "rpdb",
  "custom",
  "tmdb",
  "trakt",
  "simkl",
  "mdblist",
  "oidc",
  "llm",
  "opensubtitles",
  "subdl",
  "subsource",
  "ratings",
  "introdb",
];

/** Overview plus every enabled integration. A disabled URL does not grow a tab. */
export function integrationTabs(
  enabled: Partial<Record<ServiceTab, boolean>>,
): { key: ServiceTab; label: string }[] {
  const labels: Record<ServiceTab, string> = {
    postersplus: "PostersPlus",
    xrdb: "XRDB",
    betterposters: "BetterPosters",
    rpdb: "RPDB",
    custom: t("Custom posters"),
    overview: t("Overview"),
    tmdb: "TMDB",
    trakt: "Trakt",
    simkl: "Simkl",
    mdblist: "MDBList",
    oidc: t("Single sign-on"),
    plexservers: t("Plex Media Server Sync"),
    llm: t("Language model"),
    opensubtitles: "OpenSubtitles",
    subdl: "SubDL",
    subsource: "SubSource",
    ratings: t("Ratings"),
    introdb: "TheIntroDB",
  };
  return TAB_ORDER.filter(
    (key) => key === "overview" || key === "tmdb" || key === "introdb" || enabled[key],
  ).map((key) => ({
    key,
    label: labels[key],
  }));
}

interface Service {
  tab: Exclude<ServiceTab, "overview" | "ratings">;
  configKey:
    | "oidc"
    | "plexServers"
    | "trakt"
    | "simkl"
    | "mdblist"
    | "llm"
    | "openSubtitles"
    | "subdl"
    | "subsource"
    | "markers";
  endpoint: string;
  /** Boolean this card's switch writes. Omitted fields stay stored. */
  flag: "enabled" | "theIntroDb";
  label: string;
  groups: FieldGroup[];
  form?: (props: {
    value: Obj;
    saved: Obj | undefined;
    onChange: (next: Obj) => void;
    errors: Record<string, string>;
    footer: ReactNode;
  }) => ReactNode;
  testLabel?: string;
  test?: (value: Obj) => ReturnType<TestFn>;
  /** Why enabling must wait. The switch stays off and the setup drawer opens. */
  blockEnable?: (value: Obj) => string | null;
  /** Field the server requires before this integration may be enabled. */
  requiredField?: "clientId" | "model" | "issuer";
}

const SERVICES: Service[] = localized(() => [
  {
    tab: "plexservers",
    configKey: "plexServers",
    endpoint: "plexservers",
    flag: "enabled",
    label: t("Plex Media Server Sync"),
    groups: [],
  },
  {
    tab: "trakt",
    configKey: "trakt",
    endpoint: "trakt",
    flag: "enabled",
    label: "Trakt",
    groups: S.TRAKT,
    testLabel: t("Test Trakt"),
    test: (v) => api.testTrakt(v as EtherConfig["trakt"]),
    blockEnable: (v) => (String(v.clientId ?? "").trim() ? null : t("Needs a client ID")),
    requiredField: "clientId",
  },
  {
    tab: "simkl",
    configKey: "simkl",
    endpoint: "simkl",
    flag: "enabled",
    label: "Simkl",
    groups: [],
    form: ({ value, onChange, errors, footer }) => (
      <SimklSettings value={value} onChange={onChange} errors={errors} footer={footer} />
    ),
    blockEnable: (v) => (String(v.clientId ?? "").trim() ? null : t("Needs a client ID")),
    requiredField: "clientId",
  },
  {
    tab: "oidc",
    configKey: "oidc",
    endpoint: "oidc",
    flag: "enabled",
    label: t("Single sign-on"),
    groups: S.OIDC,
    testLabel: t("Test connection"),
    test: (v) => api.testOIDC(v as NonNullable<EtherConfig["oidc"]>),
    blockEnable: (v) =>
      !String(v.issuer ?? "").trim()
        ? t("Needs an issuer URL")
        : !String(v.clientId ?? "").trim()
          ? t("Needs a client ID")
          : null,
    requiredField: "issuer",
  },
  {
    tab: "mdblist",
    configKey: "mdblist",
    endpoint: "mdblist",
    flag: "enabled",
    label: "MDBList",
    groups: [],
    form: ({ footer }) => <MDBListSettings footer={footer} />,
  },
  {
    tab: "llm",
    configKey: "llm",
    endpoint: "llm",
    flag: "enabled",
    label: t("Language model"),
    groups: [],
    form: ({ value, saved, onChange, errors, footer }) => (
      <LanguageModelSettings
        value={value as unknown as EtherConfig["llm"]}
        saved={saved as unknown as EtherConfig["llm"] | undefined}
        onChange={(next) => onChange(next as unknown as Obj)}
        errors={errors}
        footer={footer}
      />
    ),
    testLabel: t("Test endpoint"),
    test: (v) => api.testLLM(v as EtherConfig["llm"]),
    blockEnable: (v) => (String(v.model ?? "").trim() ? null : t("Needs a model")),
    requiredField: "model",
  },
  {
    tab: "opensubtitles",
    configKey: "openSubtitles",
    endpoint: "opensubtitles",
    flag: "enabled",
    label: "OpenSubtitles",
    groups: S.OPENSUBTITLES,
    testLabel: t("Test OpenSubtitles"),
    test: (v) => api.testOpenSubtitles(v as EtherConfig["openSubtitles"]),
  },
  {
    tab: "subdl",
    configKey: "subdl",
    endpoint: "subdl",
    flag: "enabled",
    label: "SubDL",
    groups: S.SUBDL,
    testLabel: t("Test SubDL"),
    test: (v) => api.testSubDL(v as EtherConfig["subdl"]),
  },
  {
    tab: "subsource",
    configKey: "subsource",
    endpoint: "subsource",
    flag: "enabled",
    label: "SubSource",
    groups: S.SUBSOURCE,
    testLabel: t("Test SubSource"),
    test: (v) => api.testSubSource(v as EtherConfig["subsource"]),
  },
  {
    tab: "introdb",
    configKey: "markers",
    endpoint: "markers",
    flag: "theIntroDb",
    label: "TheIntroDB",
    groups: S.MARKERS,
    testLabel: t("Test TheIntroDB"),
    test: (v) => api.testIntroDB(String(v.apiKey ?? "")),
  },
]);

function sectionBases(stored: Record<string, Obj> | null): Record<string, string> {
  if (!stored) return {};
  return Object.fromEntries(
    Object.entries(stored).map(([key, value]) => [key, JSON.stringify(value)]),
  );
}

/**
 * Per-section draft. A saved update adopts only sections whose draft still
 * matched the previous save, so toggling one integration cannot wipe edits
 * on another. This is not a change to the shared useDraft hook.
 */
function useSectionDrafts(stored: Record<string, Obj> | null) {
  const storedKey = useMemo(() => JSON.stringify(stored ?? null), [stored]);
  const [adopted, setAdopted] = useState(storedKey);
  const [bases, setBases] = useState(() => sectionBases(stored));
  const [draft, setDraft] = useState<Record<string, Obj> | null>(() =>
    stored ? structuredClone(stored) : null,
  );
  if (adopted !== storedKey) {
    const next = (() => {
      if (!stored) return null;
      if (!draft) return structuredClone(stored);
      const reconciled: Record<string, Obj> = {};
      for (const [key, value] of Object.entries(stored)) {
        const now = JSON.stringify(draft[key] ?? null);
        if (bases[key] === undefined || now === bases[key])
          reconciled[key] = structuredClone(value);
        else reconciled[key] = draft[key]!;
      }
      return reconciled;
    })();
    setAdopted(storedKey);
    setBases(sectionBases(stored));
    setDraft(next);
  }
  const dirty = draft !== null && JSON.stringify(draft) !== storedKey;
  const reset = useCallback(() => {
    setDraft(stored ? structuredClone(stored) : null);
    setBases(sectionBases(stored));
    // storedKey stands in for the stored snapshot.
  }, [storedKey]);
  return { draft, setDraft, dirty, reset };
}

function flagOf(value: Obj | undefined, flag: Service["flag"]) {
  return value?.[flag] === true;
}

/** One persistence tail. A save that started first cannot finish over a later toggle. */
function useExclusive() {
  const tail = useRef(Promise.resolve());
  const depth = useRef(0);
  const [locked, setLocked] = useState(false);
  const enqueue = useCallback(<T,>(task: () => Promise<T>): Promise<T> => {
    depth.current += 1;
    setLocked(true);
    const run = tail.current.then(task, task);
    const finish = () => {
      depth.current -= 1;
      if (depth.current === 0) setLocked(false);
    };
    tail.current = run.then(finish, finish);
    return run;
  }, []);
  return { locked, enqueue };
}

function textField(section: Obj, key: string, fallback: string): string {
  const value = section[key];
  return typeof value === "string" ? value : fallback;
}

/** A saved language model can translate and answer search. */
function languageModelReady(section: Obj | undefined): boolean {
  if (!section || section.enabled !== true) return false;
  const model = textField(section, "model", "").trim();
  if (!model) return false;
  const provider = textField(section, "provider", "");
  const base = textField(section, "baseUrl", "").trim();
  return base !== "" || (provider !== "" && provider !== "custom");
}

/** Speech-to-text is a separate endpoint from the chat model. */
function speechToTextReady(section: Obj | undefined): boolean {
  if (!section) return false;
  return (
    textField(section, "speechBaseUrl", "").trim() !== "" &&
    textField(section, "speechModel", "").trim() !== ""
  );
}

function boolField(section: Obj, key: string, fallback: boolean): boolean {
  const value = section[key];
  return typeof value === "boolean" ? value : fallback;
}

/** Overlay a saved section onto the loaded config. Missing fields keep the loaded values. */
function mergeSection(
  base: EtherConfig,
  key: Service["configKey"] | "catalog" | "availability",
  section: Obj,
): EtherConfig {
  switch (key) {
    case "plexServers":
      return {
        ...base,
        plexServers: { enabled: boolField(section, "enabled", base.plexServers?.enabled ?? true) },
      };
    case "trakt":
      return {
        ...base,
        trakt: {
          enabled: boolField(section, "enabled", base.trakt.enabled),
          clientId: textField(section, "clientId", base.trakt.clientId),
          clientSecret: textField(section, "clientSecret", base.trakt.clientSecret),
        },
      };
    case "simkl": {
      const prev = base.simkl ?? {
        enabled: false,
        clientId: "",
        clientSecret: "",
      };
      return {
        ...base,
        simkl: {
          enabled: boolField(section, "enabled", prev.enabled),
          clientId: textField(section, "clientId", prev.clientId),
          clientSecret: textField(section, "clientSecret", prev.clientSecret),
        },
      };
    }
    case "oidc": {
      const prev = base.oidc ?? {
        enabled: false,
        displayName: "",
        issuer: "",
        clientId: "",
        clientSecret: "",
      };
      return {
        ...base,
        oidc: {
          enabled: boolField(section, "enabled", prev.enabled),
          displayName: textField(section, "displayName", prev.displayName ?? ""),
          issuer: textField(section, "issuer", prev.issuer),
          clientId: textField(section, "clientId", prev.clientId),
          clientSecret: textField(section, "clientSecret", prev.clientSecret),
          autoLinkEmail: textField(section, "autoLinkEmail", prev.autoLinkEmail ?? "") as
            "" | "verified" | "any",
          autoLinkUsername: boolField(section, "autoLinkUsername", prev.autoLinkUsername ?? false),
          autoProvision: boolField(section, "autoProvision", prev.autoProvision ?? false),
          requiredGroup: textField(section, "requiredGroup", prev.requiredGroup ?? ""),
        },
      };
    }
    case "mdblist":
      return {
        ...base,
        mdblist: { enabled: boolField(section, "enabled", base.mdblist?.enabled ?? false) },
      };
    case "llm":
      return {
        ...base,
        llm: {
          enabled: boolField(section, "enabled", base.llm.enabled),
          provider: textField(section, "provider", base.llm.provider),
          baseUrl: textField(section, "baseUrl", base.llm.baseUrl),
          apiKey: textField(section, "apiKey", base.llm.apiKey),
          model: textField(section, "model", base.llm.model),
          speechBaseUrl: textField(section, "speechBaseUrl", base.llm.speechBaseUrl ?? ""),
          speechApiKey: textField(section, "speechApiKey", base.llm.speechApiKey ?? ""),
          speechModel: textField(section, "speechModel", base.llm.speechModel ?? ""),
        },
      };
    case "openSubtitles":
      return {
        ...base,
        openSubtitles: {
          enabled: boolField(section, "enabled", base.openSubtitles.enabled),
          apiKey: textField(section, "apiKey", base.openSubtitles.apiKey),
          username: textField(section, "username", base.openSubtitles.username),
          password: textField(section, "password", base.openSubtitles.password),
          automaticPreferred: boolField(
            section,
            "automaticPreferred",
            base.openSubtitles.automaticPreferred ?? false,
          ),
        },
      };
    case "subdl":
      return {
        ...base,
        subdl: {
          enabled: boolField(section, "enabled", base.subdl?.enabled ?? false),
          apiKey: textField(section, "apiKey", base.subdl?.apiKey ?? ""),
        },
      };
    case "subsource":
      return {
        ...base,
        subsource: {
          enabled: boolField(section, "enabled", base.subsource?.enabled ?? false),
          apiKey: textField(section, "apiKey", base.subsource?.apiKey ?? ""),
        },
      };
    case "markers": {
      const prev = base.markers ?? { theIntroDb: false, apiKey: "" };
      return {
        ...base,
        markers: {
          ...prev,
          theIntroDb: boolField(section, "theIntroDb", prev.theIntroDb),
          apiKey: textField(section, "apiKey", prev.apiKey),
        },
      };
    }
    case "availability":
      return {
        ...base,
        availability: { ...base.availability, ...section } as unknown as AvailabilityConfig,
      };
    case "catalog":
      return {
        ...base,
        catalog: {
          ...base.catalog,
          ...section,
          tmdbKey: textField(section, "tmdbKey", base.catalog.tmdbKey),
        },
      };
  }
}

/** On, off, or switched on but missing what it needs. */
function switchStatus(enabled: boolean, ready: boolean, missing: string) {
  if (!enabled) return <Badge variant="secondary">{t("Off")}</Badge>;
  return ready ? (
    <Badge variant="success">{t("On")}</Badge>
  ) : (
    <Badge variant="warning">{missing}</Badge>
  );
}

/**
 * Integrations overview and one configuration tab per enabled service.
 * Card switches persist the enable flag. A dirty form keeps its other unsaved
 * fields; turning it off stores only the flag. Tabs and card status follow the
 * saved flag. Forms share one draft, reconciled per section.
 */
export function ConnectedServices({
  tab,
  onEnabled,
}: {
  tab: string;
  /** Enabled flags only. The page derives translated labels while rendering. */
  onEnabled: (enabled: Partial<Record<ServiceTab, boolean>>) => void;
}) {
  const { refreshDashboard } = useApp();
  const navigate = useNavigate();
  const ownerPath = useOwnerViewerPath();
  const { tab: requested } = useParams();
  const config = useAsync(() => api.getConfig(), []);
  const ratings = useAsync(() => apiFetch<RatingSettings>("/discovery/ratings/settings"), []);
  const cfg = config.data;
  const sources = useAsync(metadataSources, []);
  const tmdb = sources.data?.find((source) => source.id === "tmdb");
  // i18n: memo holds no copy — only stable config keys and stored values.
  const stored = useMemo(() => {
    if (!cfg) return null;
    const raw = cfg as unknown as Obj;
    return {
      ...Object.fromEntries(SERVICES.map((s) => [s.configKey, asObj(raw[s.configKey])])),
      catalog: asObj(cfg.catalog),
      availability: asObj(cfg.availability),
    } as Record<string, Obj>;
  }, [cfg]);
  const { draft, setDraft, dirty, reset } = useSectionDrafts(stored);
  const [ratingsSetup, setRatingsSetup] = useState(false);
  const [posterFlags, setPosterFlags] = useState<Record<string, boolean>>({});
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [error, setError] = useState<unknown>(null);
  const [saving, setSaving] = useState(false);
  const [tests, setTests] = useState<Record<string, TestState>>({});
  const [busyId, setBusyId] = useState<string | null>(null);
  const [visited, setVisited] = useState<Set<string>>(() => new Set([tab]));
  const [sectionSetup, setSectionSetup] = useState<{
    service: Service;
    draft: Obj;
    errors: Record<string, string>;
    failure: unknown;
  } | null>(null);
  const { locked, enqueue } = useExclusive();
  const skipSetupOffer = useRef(false);
  const model = useRef({
    draft: null as Record<string, Obj> | null,
    stored: null as Record<string, Obj> | null,
    ratings: undefined as RatingSettings | undefined,
    tab,
  });
  model.current.draft = draft;
  model.current.stored = stored;
  model.current.ratings = ratings.data;
  model.current.tab = tab;

  useEffect(() => {
    setVisited((current) => (current.has(tab) ? current : new Set(current).add(tab)));
  }, [tab]);

  // i18n: memo holds no copy — only stable tab keys and boolean flags.
  const enabled = useMemo(() => {
    const flags: Partial<Record<ServiceTab, boolean>> = { ...posterFlags };
    for (const service of SERVICES) {
      flags[service.tab] = flagOf(stored?.[service.configKey], service.flag);
    }
    flags.ratings = true;
    flags.tmdb = true;
    flags.introdb = true;
    flags.plexservers = true;
    return flags;
  }, [stored, posterFlags]);

  const flagSig = TAB_ORDER.map((key) => (enabled[key] ? "1" : "0")).join("");
  const reported = useRef("");
  useEffect(() => {
    if (reported.current === flagSig) return;
    reported.current = flagSig;
    onEnabled(enabled);
  }, [flagSig, enabled, onEnabled]);

  useEffect(() => {
    if (!draft || Object.keys(posterFlags).length === 0) return;
    if (!requested || requested === "overview") {
      skipSetupOffer.current = false;
      return;
    }
    // The server registry is a drawer route, not a service tab.
    if (requested === "plexservers") return;
    const key = TAB_ORDER.find((item) => item === requested);
    if (key && enabled[key]) return;
    navigate("/services/overview", { replace: true });
    if (skipSetupOffer.current) return;
    const service = SERVICES.find((item) => item.tab === key);
    if (service) {
      openSectionSetup(service, draft[service.configKey] ?? {});
    }
    // Setup openers are stable setState wrappers. Re-running would reset a drawer mid-edit.
  }, [draft, requested, enabled, navigate, posterFlags]);

  const runTest = useCallback((s: Service, value: Obj) => {
    if (!s.test) return;
    setTests((prev) => ({ ...prev, [s.configKey]: { status: "running" } }));
    s.test(value).then(
      (r) =>
        setTests((prev) => ({
          ...prev,
          [s.configKey]: r.ok
            ? { status: "ok", durationMs: r.durationMs }
            : { status: "fail", message: r.error ?? t("failed") },
        })),
      (e) =>
        setTests((prev) => ({
          ...prev,
          [s.configKey]: { status: "fail", message: (e as Error).message },
        })),
    );
  }, []);

  const applySection = (
    key: Service["configKey"] | "catalog" | "availability",
    section: Obj,
    draftNext: Record<string, Obj>,
  ) => {
    const currentConfig = config.data;
    if (!currentConfig) return;
    const storedNext = { ...(model.current.stored ?? {}), [key]: section };
    model.current.stored = storedNext;
    model.current.draft = draftNext;
    config.setData((prev) => mergeSection(prev ?? currentConfig, key, section));
    setDraft(draftNext);
  };

  const openSectionSetup = (service: Service, seed: Obj) => {
    setSectionSetup({
      service,
      draft: { ...seed, [service.flag]: true },
      errors: {},
      failure: null,
    });
  };

  const save = () =>
    enqueue(async () => {
      const draftNow = model.current.draft;
      const storedNow = model.current.stored;
      if (!draftNow || !storedNow) return;
      const changed = SERVICES.filter(
        (s) => JSON.stringify(draftNow[s.configKey]) !== JSON.stringify(storedNow[s.configKey]),
      );
      const catalogChanged = JSON.stringify(draftNow.catalog) !== JSON.stringify(storedNow.catalog);
      const availabilityChanged =
        JSON.stringify(draftNow.availability) !== JSON.stringify(storedNow.availability);
      if (!changed.length && !catalogChanged && !availabilityChanged) return;
      setSaving(true);
      setError(null);
      setErrors({});
      const savedLabels: string[] = [];
      try {
        if (catalogChanged && config.data) {
          const sent = draftNow.catalog ?? {};
          const res = await api.putSection("catalog", { ...config.data.catalog, ...sent });
          const latest = model.current.draft ?? draftNow;
          const section = {
            ...sent,
            ...asObj(res.value),
            tmdbKey: textField(asObj(res.value), "tmdbKey", String(sent.tmdbKey ?? "")),
          };
          const editedSinceSend = JSON.stringify(latest.catalog) !== JSON.stringify(sent);
          const draftNext = editedSinceSend ? latest : { ...latest, catalog: section };
          applySection("catalog", section, draftNext);
          savedLabels.push("TMDB");
        }
        if (availabilityChanged) {
          const sent = draftNow.availability ?? {};
          const res = await api.putSection("availability", sent);
          const section = { ...sent, ...asObj(res.value) };
          const latest = model.current.draft ?? draftNow;
          applySection(
            "availability",
            section,
            JSON.stringify(latest.availability) !== JSON.stringify(sent)
              ? latest
              : { ...latest, availability: section },
          );
        }
        for (const s of changed) {
          const sent = draftNow[s.configKey]!;
          const res = await api.putSection(s.endpoint, sent);
          const latest = model.current.draft ?? draftNow;
          const storedLatest = model.current.stored ?? storedNow;
          const section = { ...asObj(storedLatest[s.configKey]), ...asObj(res.value) };
          const editedSinceSend = JSON.stringify(latest[s.configKey]) !== JSON.stringify(sent);
          // The server accepted this body. Adopt its normalized document when
          // the user has not typed over it; otherwise keep the newer draft dirty.
          const draftNext = editedSinceSend ? latest : { ...latest, [s.configKey]: section };
          applySection(s.configKey, section, draftNext);
          savedLabels.push(s.label);
        }
        toast.success(t("Settings saved"), { description: savedLabels.join(", ") });
        void refreshDashboard();
      } catch (e) {
        setError(e);
        if (e instanceof ApiError && e.fields) {
          setErrors(Object.fromEntries(e.fields.map((f) => [f.field, f.message])));
          const failed = SERVICES.find((svc) =>
            e.fields!.some((f) => f.field.startsWith(`${svc.configKey}.`)),
          );
          if (failed) navigate(`/services/${failed.tab}`);
        }
      } finally {
        setSaving(false);
      }
    });

  const toggleService = (service: Service, next: boolean) =>
    enqueue(async () => {
      const draftNow = model.current.draft;
      const storedNow = model.current.stored;
      if (!draftNow || !storedNow) return;
      const current = draftNow[service.configKey] ?? {};
      if (!next) skipSetupOffer.current = true;

      if (next && service.tab !== "plexservers") {
        openSectionSetup(service, current);
        return;
      }
      const saved = storedNow[service.configKey] ?? {};
      const sectionDirty = JSON.stringify(current) !== JSON.stringify(saved);
      const enablingDraft = next && sectionDirty;
      // Off persists the flag alone so unsaved fields stay in the draft.
      const sent = enablingDraft ? { ...current, [service.flag]: true } : { [service.flag]: next };
      setBusyId(service.tab);
      try {
        const res = await api.putSection(service.endpoint, sent);
        const storedLatest = model.current.stored ?? storedNow;
        const draftLatest = model.current.draft ?? draftNow;
        const section = { ...asObj(storedLatest[service.configKey]), ...asObj(res.value) };
        section[service.flag] = next;
        const baseline = enablingDraft ? current : saved;
        const unchanged =
          JSON.stringify(draftLatest[service.configKey] ?? null) === JSON.stringify(baseline);
        const draftNext =
          !next && sectionDirty
            ? {
                ...draftLatest,
                [service.configKey]: {
                  ...asObj(draftLatest[service.configKey]),
                  [service.flag]: false,
                },
              }
            : unchanged
              ? { ...draftLatest, [service.configKey]: section }
              : draftLatest;
        applySection(service.configKey, section, draftNext);
        if (!next && model.current.tab === service.tab) navigate("/services/overview");
      } catch (e) {
        if (next) {
          const fields =
            e instanceof ApiError && e.fields
              ? Object.fromEntries(e.fields.map((f) => [f.field, f.message]))
              : {};
          setSectionSetup({
            service,
            draft: { ...current, [service.flag]: true },
            errors: fields,
            failure: e,
          });
        } else {
          toast.error(t("Couldn't save the change"), {
            description: (e as Error).message,
          });
        }
      } finally {
        setBusyId(null);
      }
    });

  const saveSectionSetup = () => {
    const setup = sectionSetup;
    if (!setup) return;
    // MDBList credentials belong to each viewer; owner setup only enables it.
    const body =
      setup.service.tab === "mdblist"
        ? { enabled: true }
        : { ...setup.draft, [setup.service.flag]: true };
    const blocked = setup.service.blockEnable?.(body);
    if (blocked) {
      const field = setup.service.requiredField ?? "clientId";
      setSectionSetup({
        ...setup,
        draft: body,
        failure: null,
        errors: { [`${setup.service.configKey}.${field}`]: blocked },
      });
      return;
    }
    void enqueue(async () => {
      setBusyId(setup.service.tab);
      try {
        const res = await api.putSection(setup.service.endpoint, body);
        const storedLatest = model.current.stored ?? {};
        const draftLatest = model.current.draft ?? {};
        const section = {
          ...asObj(storedLatest[setup.service.configKey]),
          ...asObj(res.value),
        };
        section[setup.service.flag] = true;
        applySection(setup.service.configKey, section, {
          ...draftLatest,
          [setup.service.configKey]: section,
        });
        setSectionSetup(null);
      } catch (e) {
        const fields =
          e instanceof ApiError && e.fields
            ? Object.fromEntries(e.fields.map((f) => [f.field, f.message]))
            : {};
        setSectionSetup((current) =>
          current && current.service.tab === setup.service.tab
            ? { ...current, errors: fields, failure: e }
            : current,
        );
      } finally {
        setBusyId(null);
      }
    });
  };

  if (config.error) return <ErrorPanel error={config.error} onRetry={() => void config.reload()} />;
  if (!cfg || !draft)
    return (
      <div className="py-10 text-center">
        <Spinner size={20} label={t("Loading settings")} />
      </div>
    );

  const setupService = sectionSetup?.service;
  const toggleRatings = (next: boolean) => {
    if (next) {
      setRatingsSetup(true);
      return;
    }
    void enqueue(async () => {
      setBusyId("ratings");
      try {
        const current = model.current.ratings ?? ratings.data;
        const res = await apiFetch<RatingSettings>("/discovery/ratings/settings", {
          method: "PUT",
          body: JSON.stringify({ ...current, enabled: false, apiKey: "" }),
        });
        model.current.ratings = res;
        ratings.setData(res);
      } catch (e) {
        setError(e);
        toast.error(t("Couldn't save the change"), {
          description: errorMessage(e),
        });
      } finally {
        setBusyId(null);
      }
    });
  };
  return (
    <>
      <Drawer
        open={ratingsSetup}
        onClose={() => setRatingsSetup(false)}
        title={t("Set up {app}", { app: t("OMDb and MDBList") })}
        description={t("Save and enable turns this on. Cancel leaves it off.")}
        footer={
          <IntegrationSetupFooter
            disabled={locked}
            onCancel={() => setRatingsSetup(false)}
            form="ratings-integration-setup"
          />
        }
      >
        <RatingSources
          setup
          keysOnly
          saved={ratings.data ?? null}
          persist={enqueue}
          onPersisted={(next) => {
            model.current.ratings = next;
            ratings.setData(next);
            setRatingsSetup(false);
          }}
        />
      </Drawer>
      <Drawer
        open={sectionSetup !== null}
        onClose={() => {
          if (busyId) return;
          setSectionSetup(null);
        }}
        title={t("Set up {app}", { app: setupService?.label ?? "" })}
        description={t("Save and enable turns this on. Cancel leaves it off.")}
        footer={
          <IntegrationSetupFooter
            disabled={busyId !== null || locked}
            onCancel={() => setSectionSetup(null)}
            onSave={() => void saveSectionSetup()}
          />
        }
      >
        {sectionSetup && setupService ? (
          <div className="space-y-4">
            {sectionSetup.failure ? <MutationError error={sectionSetup.failure} /> : null}
            {setupService.form ? (
              <setupService.form
                value={sectionSetup.draft}
                saved={stored?.[setupService.configKey]}
                onChange={(next) =>
                  setSectionSetup({
                    ...sectionSetup,
                    draft: { ...next, [setupService.flag]: true },
                  })
                }
                errors={sectionSetup.errors}
                footer={null}
              />
            ) : (
              <SchemaGroups
                service={setupService}
                value={sectionSetup.draft}
                set={(next) => setSectionSetup({ ...sectionSetup, draft: next })}
                errors={sectionSetup.errors}
                footer={null}
              />
            )}
          </div>
        ) : null}
      </Drawer>
      {tab === "tmdb" || visited.has("tmdb") ? (
        <div hidden={tab !== "tmdb"}>
          <IntegrationTab id="tmdb" name="TMDB" enabled ready={Boolean(tmdb?.configured)}>
            <TmdbCatalogueSettings
              bundled={tmdb?.keySource === "built-in"}
              disabled={locked || saving}
              draft={draft.catalog ?? {}}
              onChange={(catalog) => setDraft({ ...draft, catalog })}
              errors={errors}
            />
            <div className="space-y-3">
              <h3 className="font-semibold">{t("Movies")}</h3>
              <NewReleasesSettings
                availability={draft.availability as unknown as AvailabilityConfig}
                onAvailability={(availability) =>
                  setDraft({ ...draft, availability: asObj(availability) })
                }
                mediaType="movie"
              />
            </div>
            <div className="space-y-3">
              <h3 className="font-semibold">{t("TV shows")}</h3>
              <NewReleasesSettings
                availability={draft.availability as unknown as AvailabilityConfig}
                onAvailability={(availability) =>
                  setDraft({ ...draft, availability: asObj(availability) })
                }
                mediaType="show"
              />
            </div>
          </IntegrationTab>
        </div>
      ) : null}
      <PosterAccounts locked={locked} tab={tab} onEnabled={setPosterFlags}>
        {(posterCards, posterSavedNote) => (
          <div hidden={tab !== "overview"}>
            <ServiceOverview
              posterCards={posterCards}
              posterSavedNote={posterSavedNote}
              tmdbBundled={tmdb?.keySource === "built-in"}
              onMetadataSave={(patch) =>
                enqueue(async () => {
                  const res = await api.putSection("catalog", patch);
                  const section = asObj(res.value);
                  const latest = model.current.draft;
                  if (latest)
                    applySection("catalog", section, {
                      ...latest,
                      catalog: {
                        ...section,
                        ...latest.catalog,
                        ...Object.fromEntries(
                          Object.keys(patch).map((field) => [field, section[field]]),
                        ),
                      },
                    });
                  await sources.reload();
                  void refreshDashboard();
                })
              }
              onRatingsSetup={toggleRatings}
              draft={draft}
              stored={stored}
              ratings={ratings.data}
              busyId={busyId}
              locked={locked}
              onToggle={(service, next) => void toggleService(service, next)}
            />
          </div>
        )}
      </PosterAccounts>
      {SERVICES.map((s) => {
        if (s.tab === "plexservers" || (tab !== s.tab && !visited.has(s.tab))) return null;
        const value = draft[s.configKey] ?? {};
        const set = (next: Obj) => setDraft({ ...draft, [s.configKey]: next });
        const testButton =
          s.test && s.testLabel ? (
            <TestButton
              label={s.testLabel}
              state={tests[s.configKey] ?? { status: "idle" }}
              onTest={() => runTest(s, value)}
            />
          ) : undefined;
        return (
          <div key={s.configKey} hidden={tab !== s.tab}>
            <IntegrationTab
              id={s.tab}
              name={s.label}
              enabled={flagOf(stored?.[s.configKey], s.flag)}
              ready={!s.blockEnable?.(stored?.[s.configKey] ?? {})}
              onToggle={(next) => void toggleService(s, next)}
              disabled={locked}
              test={testButton}
              href={
                s.tab === "introdb"
                  ? "/playback/extras"
                  : s.tab === "opensubtitles" || s.tab === "subdl" || s.tab === "subsource"
                    ? "/playback/subtitles"
                    : s.tab === "llm"
                      ? "/recommendations"
                      : s.tab === "oidc"
                        ? "/viewers"
                        : s.tab === "trakt"
                          ? ownerPath === "/viewers"
                            ? "/viewers"
                            : `${ownerPath}/sign-in?signin=sync`
                          : "/viewers"
              }
              usage={
                s.tab === "introdb"
                  ? t("Choose intro lookups in Playback › Extras.")
                  : s.tab === "opensubtitles" || s.tab === "subdl" || s.tab === "subsource"
                    ? t("Supplies subtitles during playback.")
                    : s.tab === "llm"
                      ? t("Powers search, collections and Home recommendations.")
                      : s.tab === "oidc"
                        ? t(
                            "People link their account under Sign-in methods at /ether/me, then sign in here and in the viewer portal with it.",
                          )
                        : s.tab === "trakt"
                          ? t(
                              "Connect Trakt under each viewer's Sign-in & devices tab, or let viewers connect at /ether/me.",
                            )
                          : t("Connect viewer accounts to sync watch history.")
              }
            >
              {s.form ? (
                <s.form
                  value={value}
                  saved={stored?.[s.configKey]}
                  onChange={set}
                  errors={errors}
                  footer={null}
                />
              ) : (
                <SchemaGroups service={s} value={value} set={set} errors={errors} footer={null} />
              )}
            </IntegrationTab>
            {s.tab === "llm" ? (
              <LanguageFeatures
                textModelReady={languageModelReady(stored?.llm)}
                speechReady={speechToTextReady(stored?.llm)}
              />
            ) : null}
          </div>
        );
      })}
      <Drawer
        open={requested === "plexservers"}
        onClose={() => navigate("/services/overview")}
        title={
          <span className="block whitespace-normal">
            {t("Sync watch state with your Plex servers")}
          </span>
        }
        description={t("Sync watched marks with other Plex Media Servers you run alongside Ether.")}
        size="xl"
      >
        {requested === "plexservers" ? (
          <PlexServers enabled={cfg?.plexServers?.enabled ?? true} />
        ) : null}
      </Drawer>
      {tab === "ratings" || visited.has("ratings") ? (
        <div hidden={tab !== "ratings"}>
          <IntegrationTab
            id="ratings"
            name={t("OMDb and MDBList")}
            enabled={secretConfigured(ratings.data?.apiKey)}
            onToggle={toggleRatings}
          >
            <RatingSources
              keysOnly
              saved={ratings.data ?? null}
              disabled={locked}
              persist={enqueue}
              onPersisted={(next) => {
                model.current.ratings = next;
                ratings.setData(next);
              }}
            />
          </IntegrationTab>
        </div>
      ) : null}
      <SaveBar
        dirty={dirty}
        saving={saving}
        disabled={locked}
        error={error}
        onSave={save}
        onDiscard={() => {
          reset();
          setErrors({});
          setError(null);
        }}
      />
    </>
  );
}

function SchemaGroups({
  service,
  value,
  set,
  errors,
  footer,
}: {
  service: Service;
  value: Obj;
  set: (next: Obj) => void;
  errors: Record<string, string>;
  footer?: ReactNode;
}) {
  const known = new Set(service.groups.flatMap((g) => g.fields.map((f) => f.key)));
  const extra =
    service.configKey === "markers"
      ? []
      : Object.keys(value).filter((k) => !known.has(k) && k !== service.flag);
  return service.groups.map((g, i) => {
    const last = i === service.groups.length - 1;
    return (
      <SettingsSection
        key={`${service.configKey}-${g.title ?? i}`}
        title={g.title && g.title !== service.label ? g.title : undefined}
        description={g.description}
        footer={last ? footer : undefined}
      >
        {g.fields.map((f) => (
          <SchemaField
            key={f.key}
            def={f}
            value={value[f.key]}
            path={`${service.configKey}.${f.key}`}
            error={errors[`${service.configKey}.${f.key}`]}
            onChange={(v) => set({ ...value, [f.key]: v })}
          />
        ))}
        {last && extra.length ? (
          <GenericFields
            value={value}
            keys={extra}
            onChange={set}
            errors={errors}
            prefix={service.configKey}
          />
        ) : null}
      </SettingsSection>
    );
  });
}

function ServiceOverview({
  stored,
  ratings,
  busyId,
  locked,
  onToggle,
  posterCards,
  posterSavedNote,
  tmdbBundled,
  onMetadataSave,
  onRatingsSetup,
}: {
  tmdbBundled: boolean;
  onMetadataSave: (patch: Record<string, string | boolean>) => Promise<void>;
  onRatingsSetup: (next: boolean) => void;
  posterCards: ReactNode;
  posterSavedNote?: ReactNode;
  draft: Record<string, Obj>;
  stored: Record<string, Obj> | null;
  ratings: RatingSettings | undefined;
  busyId: string | null;
  locked: boolean;
  onToggle: (service: Service, next: boolean) => void;
}) {
  const catalogue = useAsync(
    (signal) => librariesApi.catalogueSources(signal, { includeDisabled: true }),
    [],
  );
  const [addingSource, setAddingSource] = useState(false);
  const [managingCatalogue, setManagingCatalogue] = useState<string | null>(null);
  const [togglingCatalogue, setTogglingCatalogue] = useState<string | null>(null);
  const subtitlePlugins = useRegistry("subtitleProvider").descriptors.filter(isPluginDescriptor);
  const [addingSubtitles, setAddingSubtitles] = useState(false);
  const [checkingSubtitles, setCheckingSubtitles] = useState<{ id: string; name: string } | null>(
    null,
  );
  const [ownerPostersSaved, setOwnerPostersSaved] = useState(false);
  useStreamEvent("plugins.changed", () => {
    void catalogue.reload();
  });
  const service = (tab: Service["tab"]) => SERVICES.find((s) => s.tab === tab)!;
  const saved = (tab: Service["tab"]) => stored?.[service(tab).configKey] ?? {};
  const on = (tab: Service["tab"]) => flagOf(saved(tab), service(tab).flag);
  const switchesHeld = locked || busyId !== null;
  async function toggleCatalogue(id: string, enabled: boolean) {
    setTogglingCatalogue(id);
    try {
      await setPluginEnabled(id, enabled);
      await catalogue.reload();
    } catch (error) {
      toast.error(t("Couldn't save the change"), { description: errorMessage(error) });
    } finally {
      setTogglingCatalogue(null);
    }
  }
  return (
    <>
      <IntegrationGroup
        title={t("Catalogues")}
        footer={
          <Button variant="secondary" onClick={() => setAddingSource(true)}>
            <Plus size={14} />
            {t("Add catalogue source")}
          </Button>
        }
      >
        {(["tmdb", "tvdb"] as const).map((source) => (
          <MetadataCatalogue
            key={source}
            identity={{ id: source, name: builtinMetadataSources[source].name }}
            source={source}
            storedKey={String(stored?.catalog?.[builtinMetadataSources[source].key] ?? "")}
            bundled={source === "tmdb" && tmdbBundled}
            enabled={
              source === "tmdb"
                ? stored?.catalog?.tmdbDisabled !== true
                : secretConfigured(stored?.catalog?.tvdbKey)
            }
            onSetEnabled={async (enabled) => {
              await onMetadataSave(
                source === "tmdb" ? { tmdbDisabled: !enabled } : { tvdbKey: "" },
              );
              void catalogue.reload();
            }}
            locked={switchesHeld}
            onSave={async (key) => {
              await onMetadataSave({
                [builtinMetadataSources[source].key]: key,
                ...(source === "tmdb" ? { tmdbDisabled: false } : {}),
              });
              void catalogue.reload();
            }}
          />
        ))}
        {catalogueGroups((catalogue.data ?? []).filter((source) => source.pluginId)).map(
          (source) => (
            <IntegrationAppCard
              key={source.id}
              identity={{ id: source.id, name: source.name }}
              logo={<SourceIcon source={source} size={40} />}
              description={[
                tn(source.entries.length, "{count} catalogue", "{count} catalogues"),
                ...source.entries.map(sourceName),
                source.libraries.length
                  ? t("Used by: {libraries}", {
                      libraries: source.libraries.map((lib) => lib.name).join(", "),
                    })
                  : t("No libraries use this source yet."),
              ].join(" · ")}
              enabled={source.enabled}
              busy={togglingCatalogue === source.id}
              disabled={switchesHeld || togglingCatalogue !== null}
              onToggle={(enabled) => void toggleCatalogue(source.id, enabled)}
              status={
                <>
                  {!source.enabled ? <Badge variant="secondary">{t("Off")}</Badge> : null}
                  <LevelBadge
                    level={
                      source.health === "unknown"
                        ? "unknown"
                        : source.health === "ok"
                          ? "ok"
                          : "warn"
                    }
                    label={
                      source.health === "unknown"
                        ? t("Not loaded yet")
                        : source.health === "ok"
                          ? t("Healthy")
                          : source.lastError || t("Needs attention")
                    }
                  />
                </>
              }
              configureLabel={
                source.enabled
                  ? t("Manage {app}", { app: source.name })
                  : t("Set up {app}", { app: source.name })
              }
              onConfigure={() => setManagingCatalogue(source.id)}
              details={source.entries
                .filter((entry) => entry.lastError)
                .map((entry) => (
                  <p key={entry.id} className="text-danger">
                    {sourceName(entry)}: {entry.lastError}
                  </p>
                ))}
            />
          ),
        )}
      </IntegrationGroup>
      {catalogue.error ? (
        <ErrorPanel error={catalogue.error} onRetry={() => void catalogue.reload()} />
      ) : null}
      {managingCatalogue ? (
        <InstalledPluginDrawer
          pluginId={managingCatalogue}
          initialTab="setup"
          onClose={() => setManagingCatalogue(null)}
          onChanged={() => void catalogue.reload()}
          onRemoved={() => {
            setManagingCatalogue(null);
            void catalogue.reload();
          }}
        />
      ) : null}
      {addingSource ? (
        <AddCatalogueSource
          onClose={() => setAddingSource(false)}
          onAdded={() => void catalogue.reload()}
        />
      ) : null}
      <IntegrationGroup title={t("Watch tracking")}>
        <IntegrationAppCard
          identity={{ id: "trakt", name: "Trakt" }}
          logo={<TraktMark />}
          description={t("Sync watch history and lists with Trakt.")}
          status={switchStatus(
            on("trakt"),
            Boolean(saved("trakt").clientId),
            t("Needs a client ID"),
          )}
          enabled={on("trakt")}
          busy={locked || busyId === "trakt"}
          disabled={switchesHeld}
          onToggle={(next) => onToggle(service("trakt"), next)}
          configureHref="/services/trakt"
          configureLabel={on("trakt") ? t("Manage Trakt") : t("Set up Trakt")}
        />
        <IntegrationAppCard
          identity={{ id: "simkl", name: "Simkl" }}
          logo={<SimklMark />}
          description={t(
            "Connect a viewer's Simkl account to import watched history and send completed playback.",
          )}
          status={switchStatus(
            on("simkl"),
            Boolean(saved("simkl").clientId),
            t("Needs a client ID"),
          )}
          enabled={on("simkl")}
          busy={locked || busyId === "simkl"}
          disabled={switchesHeld}
          onToggle={(next) => onToggle(service("simkl"), next)}
          configureHref="/services/simkl"
          configureLabel={on("simkl") ? t("Manage Simkl") : t("Set up Simkl")}
        />
        <IntegrationAppCard
          identity={{ id: "mdblist", name: "MDBList" }}
          logo={<MDBListMark />}
          description={t("Sync watch history with MDBList. Each viewer pastes their own API key.")}
          status={switchStatus(on("mdblist"), true, "")}
          enabled={on("mdblist")}
          busy={locked || busyId === "mdblist"}
          disabled={switchesHeld}
          onToggle={(next) => onToggle(service("mdblist"), next)}
          configureHref="/services/mdblist"
          configureLabel={on("mdblist") ? t("Manage MDBList") : t("Set up MDBList")}
        />
        <PlexServersCard
          enabled={on("plexservers")}
          busy={locked || busyId === "plexservers"}
          disabled={switchesHeld}
          onToggle={(next) => onToggle(service("plexservers"), next)}
        />
      </IntegrationGroup>
      <IntegrationGroup title={t("Sign-in")}>
        <IntegrationAppCard
          identity={{ id: "oidc", name: t("Single sign-on") }}
          description={t(
            "Sign in with Pocket ID, Authelia, Authentik, Keycloak or another OpenID Connect provider.",
          )}
          status={switchStatus(
            on("oidc"),
            !service("oidc").blockEnable?.(saved("oidc")),
            service("oidc").blockEnable?.(saved("oidc")) ?? "",
          )}
          enabled={on("oidc")}
          busy={locked || busyId === "oidc"}
          disabled={switchesHeld}
          onToggle={(next) => onToggle(service("oidc"), next)}
          configureHref="/services/oidc"
          configureLabel={
            on("oidc")
              ? t("Manage {app}", { app: t("Single sign-on") })
              : t("Set up single sign-on")
          }
        />
      </IntegrationGroup>
      <>
        <IntegrationGroup
          title={t("Artwork and ratings")}
          footer={
            posterSavedNote ??
            (ownerPostersSaved ? (
              <p className="text-sm text-fg-muted" role="status">
                {t(
                  "Apps reload posters the next time they open a library; some may need to be restarted.",
                )}
              </p>
            ) : null)
          }
        >
          {posterCards}
          <OwnerPosters locked={locked} onSaved={() => setOwnerPostersSaved(true)} />
          <IntegrationAppCard
            identity={{ id: "ratings", name: t("OMDb and MDBList") }}
            description={t("IMDb, Rotten Tomatoes and other rating badges.")}
            status={ratingsStatus(ratings)}
            enabled={secretConfigured(ratings?.apiKey)}
            busy={locked || busyId === "ratings"}
            disabled={switchesHeld}
            onToggle={onRatingsSetup}
            configureHref="/services/ratings"
            configureLabel={t(secretConfigured(ratings?.apiKey) ? "Manage {app}" : "Set up {app}", {
              app: t("OMDb and MDBList"),
            })}
          />
        </IntegrationGroup>
      </>
      <IntegrationGroup
        title={t("Subtitle providers")}
        setting="subtitle-providers"
        footer={
          <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
            <Button variant="secondary" onClick={() => setAddingSubtitles(true)}>
              <Plus size={14} />
              {t("Add subtitle provider")}
            </Button>
            <p className="text-sm text-fg-muted">
              {t("Every provider that is on is searched, and the best match for the file is used.")}
            </p>
          </div>
        }
      >
        <IntegrationAppCard
          identity={{ id: "opensubtitles", name: "OpenSubtitles" }}
          description={t("Fetch subtitles when a release doesn't include them.")}
          status={
            <>
              <Badge variant="secondary">{t("Built-in")}</Badge>
              {switchStatus(
                on("opensubtitles"),
                Boolean(saved("opensubtitles").apiKey),
                t("Needs an API key"),
              )}
            </>
          }
          enabled={on("opensubtitles")}
          busy={locked || busyId === "opensubtitles"}
          disabled={switchesHeld}
          onToggle={(next) => onToggle(service("opensubtitles"), next)}
          configureHref="/services/opensubtitles"
          configureLabel={
            on("opensubtitles") ? t("Manage OpenSubtitles") : t("Set up OpenSubtitles")
          }
        />
        <IntegrationAppCard
          identity={{ id: "subdl", name: "SubDL" }}
          description={t("Fetch subtitles from SubDL when a release doesn't include them.")}
          status={
            <>
              <Badge variant="secondary">{t("Built-in")}</Badge>
              {switchStatus(on("subdl"), Boolean(saved("subdl").apiKey), t("Needs an API key"))}
            </>
          }
          enabled={on("subdl")}
          busy={locked || busyId === "subdl"}
          disabled={switchesHeld}
          onToggle={(next) => onToggle(service("subdl"), next)}
          configureHref="/services/subdl"
          configureLabel={on("subdl") ? t("Manage SubDL") : t("Set up SubDL")}
        />
        <IntegrationAppCard
          identity={{ id: "subsource", name: "SubSource" }}
          description={t("Fetch subtitles from SubSource when a release doesn't include them.")}
          status={
            <>
              <Badge variant="secondary">{t("Built-in")}</Badge>
              {switchStatus(
                on("subsource"),
                Boolean(saved("subsource").apiKey),
                t("Needs an API key"),
              )}
            </>
          }
          enabled={on("subsource")}
          busy={locked || busyId === "subsource"}
          disabled={switchesHeld}
          onToggle={(next) => onToggle(service("subsource"), next)}
          configureHref="/services/subsource"
          configureLabel={on("subsource") ? t("Manage SubSource") : t("Set up SubSource")}
        />
        {subtitlePlugins.map((provider) => {
          const name = textOf(provider.label) || provider.owner;
          const isStremio = Boolean((provider.meta as { stremio?: boolean } | undefined)?.stremio);
          return (
            <IntegrationAppCard
              key={provider.id}
              identity={{ id: provider.id, name }}
              logo={
                provider.icon && /^(https?:|\/|data:)/.test(provider.icon) ? (
                  <img src={provider.icon} alt="" />
                ) : undefined
              }
              description={t(
                "Offered beside your other subtitle providers; a file downloads only when picked.",
              )}
              status={
                <>
                  <Badge variant="secondary">{isStremio ? t("Stremio add-on") : t("Plugin")}</Badge>
                  <Badge variant="success">{t("On")}</Badge>
                </>
              }
              action={
                <Button
                  size="sm"
                  variant="outline"
                  aria-label={t("Check subtitles from {name}", { name })}
                  onClick={() => setCheckingSubtitles({ id: provider.owner, name })}
                >
                  {t("Check subtitles")}
                </Button>
              }
              configureHref={`/plugins/installed?plugin=${encodeURIComponent(provider.owner)}&tab=setup`}
              configureLabel={t("Open plugin")}
            />
          );
        })}
      </IntegrationGroup>
      {addingSubtitles ? <AddSubtitleProvider onClose={() => setAddingSubtitles(false)} /> : null}
      {checkingSubtitles ? (
        <SubtitleCheck provider={checkingSubtitles} onClose={() => setCheckingSubtitles(null)} />
      ) : null}
      <IntegrationGroup title={t("Playback extras")}>
        <IntegrationAppCard
          identity={{ id: "introdb", name: "TheIntroDB" }}
          description={t("Intro and credits markers so viewers can skip them.")}
          status={introStatus(on("introdb"), saved("introdb").apiKey)}
          enabled={on("introdb")}
          onToggle={(next) => onToggle(service("introdb"), next)}
          disabled={switchesHeld}
          configureHref="/services/introdb"
          configureLabel={t(on("introdb") ? "Manage {app}" : "Set up {app}", { app: "TheIntroDB" })}
        />
      </IntegrationGroup>
      <IntegrationGroup title={t("Smart features")}>
        <IntegrationAppCard
          identity={{ id: "llm", name: t("Language model") }}
          description={t(
            "Describe-it collections, themed Home rows, natural-language search and recaps.",
          )}
          status={switchStatus(on("llm"), Boolean(saved("llm").model), t("Needs a model"))}
          enabled={on("llm")}
          busy={locked || busyId === "llm"}
          disabled={switchesHeld}
          onToggle={(next) => onToggle(service("llm"), next)}
          configureHref="/services/llm"
          configureLabel={t(on("llm") ? "Manage {app}" : "Set up {app}", {
            app: t("Language model"),
          })}
        />
      </IntegrationGroup>
    </>
  );
}

function introStatus(enabled: boolean, apiKey: unknown) {
  if (!enabled) return <Badge variant="secondary">{t("Off")}</Badge>;
  if (secretConfigured(apiKey)) return <Badge variant="success">{t("Key saved")}</Badge>;
  return <Badge variant="secondary">{t("Public lookup")}</Badge>;
}

function ratingsStatus(ratings: RatingSettings | undefined) {
  if (!ratings) return undefined;
  if (!ratings.enabled) return <Badge variant="secondary">{t("Off")}</Badge>;
  if (!ratings.apiKey.trim()) return <Badge variant="warning">{t("Needs an API key")}</Badge>;
  return <Badge variant="success">{t("Configured")}</Badge>;
}

function flattenCards(children: ReactNode): ReactNode[] {
  const out: ReactNode[] = [];
  Children.forEach(children, (child) => {
    if (isValidElement<{ children?: ReactNode }>(child) && child.type === Fragment)
      out.push(...flattenCards(child.props.children));
    else out.push(child);
  });
  return out;
}

function IntegrationGroup({
  title,
  children,
  footer,
  setting,
}: {
  title: string;
  children: ReactNode;
  footer?: ReactNode;
  /** Stable key a link elsewhere in the console can point at (`settingHref`). */
  setting?: string;
}) {
  const cards = flattenCards(children).filter(isValidElement);
  return (
    <section aria-label={title} data-setting={setting} className="space-y-3 pt-6 first:pt-0">
      <h2 className="text-sm font-semibold text-fg">{title}</h2>
      <ul className="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
        {cards.map((child, index) => {
          const name = isValidElement<{ identity?: { name?: string } }>(child)
            ? child.props.identity?.name
            : undefined;
          return (
            <li
              key={isValidElement(child) ? (child.key ?? name ?? index) : index}
              aria-label={name}
              className="h-full"
            >
              {child}
            </li>
          );
        })}
      </ul>
      {footer}
    </section>
  );
}
