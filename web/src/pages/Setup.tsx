import {
  Alert,
  AlertDescription,
  Button,
  Checkbox,
  Input,
  SearchSelect,
  Slider,
  Spinner,
  Switch,
  WizardLayout,
  cn,
} from "@braedonsaunders/appkit-ui";
import { Check, ExternalLink, Plus, Trash2 } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useNavigate } from "react-router-dom";
import { useAppOptional } from "../app-context";
import { EtherMark } from "../components/brand";
import { IntegrationAppCard } from "../components/IntegrationAppCard";
import { EmbyMark, JellyfinMark, PlexMark } from "../components/integration-brands";
import { ErrorPanel, MutationError } from "../components/feedback";
import { Field, NumberInput, SecretInput, TestButton, type TestState } from "../components/fields";
import {
  findProvider,
  ModelPicker,
  providerOptions,
  useLLMProviders,
  withProvider,
} from "../components/llm";
import { localized, t, tx } from "../i18n";
import { api, apiFetch, newId, setToken } from "../lib/api";
import { formatBytes } from "../lib/format";
import type { DebridProvider, DiskInfo, EtherConfig, Indexer, UsenetProvider } from "../lib/types";
import { appLabel, appPurpose } from "./apps/catalog";
import { DEBRID_KINDS } from "./sources/Debrid";
import { addonUrlHasDebridKey, isStremio, normalizeAddonUrl } from "./sources/Indexers";

export type WizardStepId = "apps" | "account" | "sources" | "indexers" | "cache" | "catalog";

/** Survives a remount after the owner account is created so the visit does not start over. */
export const SETUP_STEP_KEY = "ether.setup.step";

const STEP_COPY = localized(() => ({
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
  { id: "silo", name: "Silo" },
  { id: "stremio", name: "Stremio" },
  { id: "webdav", name: "Files" },
];

export interface SetupApp {
  id: string;
  name: string;
  description: string;
}

export function wizardSteps(
  plexSelected: boolean,
): { id: WizardStepId; title: string; heading: string }[] {
  return [
    { id: "apps", title: STEP_COPY.appsTitle, heading: STEP_COPY.appsHeading },
    plexSelected
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
  const rows = list?.length
    ? list.map((item) => ({ id: item.descriptor.id, name: item.descriptor.name }))
    : FALLBACK_APPS;
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
  return Object.fromEntries(ids.map((id) => [id, id === "plex"]));
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

const GIB = 1024 ** 3;

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
  return { id: newId("deb"), name: "", kind: "realdebrid", apiKey: "", priority: 0, enabled: true };
}

/** How an indexer row is searched; maps onto Indexer.kind and protocol. */
type IndexerType = "newznab" | "torznab" | "stremio";
const INDEXER_TYPES: { value: IndexerType; label: string }[] = localized(() => [
  { value: "stremio", label: t("Stremio addon (torrents)") },
  { value: "torznab", label: t("Torznab (torrents)") },
  { value: "newznab", label: t("Newznab (Usenet)") },
]);
function indexerType(ix: Indexer): IndexerType {
  if (isStremio(ix)) return "stremio";
  return ix.protocol === "torrent" ? "torznab" : "newznab";
}
function withIndexerType(ix: Indexer, type: IndexerType): Indexer {
  if (type === "stremio") return { ...ix, kind: "stremio", protocol: "torrent", apiKey: "" };
  return { ...ix, kind: "", protocol: type === "torznab" ? "torrent" : "usenet" };
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
  },
): boolean {
  if (opts.skipped[step]) return true;
  switch (step) {
    case "apps":
    case "cache":
      return true;
    case "account":
      return opts.accountReady;
    case "sources": {
      // A debrid account, a Usenet provider, or both; every enabled one tested.
      const usenet = opts.providers.filter((p) => p.enabled);
      const debrid = (opts.debrid ?? []).filter((d) => d.enabled);
      return (
        usenet.length + debrid.length > 0 &&
        usenet.every((p) => p.host.trim() && opts.providerTests[p.id]?.status === "ok") &&
        debrid.every((d) => d.apiKey.trim() && opts.debridTests?.[d.id]?.status === "ok")
      );
    }
    case "indexers": {
      const enabled = opts.indexers.filter((i) => i.enabled);
      return (
        enabled.length > 0 &&
        enabled.every((i) => i.url.trim() && opts.indexerTests[i.id]?.status === "ok")
      );
    }
    case "catalog":
      // Without TMDB there is no catalogue to browse.
      return opts.tmdbReady !== false;
    default:
      return true;
  }
}

export default function Setup() {
  const navigate = useNavigate();
  const app = useAppOptional();
  const [cfg, setCfg] = useState<EtherConfig | null>(null);
  const [loadError, setLoadError] = useState<unknown>(null);
  const [step, setStepState] = useState(storedStep);
  const [apps, setApps] = useState<SetupApp[]>(() => appCatalog(null));
  const [frontends, setFrontends] = useState<Record<string, boolean>>({});
  const [error, setError] = useState<unknown>(null);
  const [saving, setSaving] = useState(false);
  const [cacheRestartRequired, setCacheRestartRequired] = useState(false);
  const [skipped, setSkipped] = useState<Record<string, boolean>>({});
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
    ])
      .then(([c, list]) => {
        const catalog = appCatalog(list);
        setApps(catalog);
        setFrontends(
          selectionFrom(
            c,
            catalog.map((item) => item.id),
          ),
        );
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
  const steps = wizardSteps(plexSelected);
  const stepId = steps[step]?.id ?? "apps";
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
            accountReady: plexSelected
              ? Boolean(cfg.plex.claimed && cfg.plex.token) && !plexRecovery && !claimFailed
              : localDone,
            providers: cfg.usenet.providers ?? [],
            indexers: cfg.indexers ?? [],
            providerTests,
            indexerTests,
            debrid: cfg.debrid?.providers ?? [],
            debridTests,
            tmdbReady:
              optTests.tmdb?.status === "ok" || cfg.catalog.tmdbKey === "••••" || tmdbBundled,
            skipped,
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
      plexRecovery,
      claimFailed,
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
        <p>
          {t(
            "Setup is saved. Restart Ether, or restart its Docker container, before playing media.",
          )}
        </p>
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
        <Button onClick={() => navigate("/cache")}>{t("Open cache settings")}</Button>
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

  const next = async () => {
    setSaving(true);
    setError(null);
    try {
      setCfg(
        await api.putConfig(
          savedDraft(
            mergeFrontendSelection(
              cfg,
              apps.map((item) => item.id),
              frontends,
            ),
          ),
        ),
      );
      if (step < steps.length - 1) setStep(step + 1);
      else {
        const status = await api.setupComplete(false);
        sessionStorage.removeItem(SETUP_STEP_KEY);
        await app?.reloadBoot();
        if (status.cacheRestartRequired) setCacheRestartRequired(true);
        else navigate("/");
      }
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  };

  const plexSigned = pinDone || Boolean(cfg.plex.claimed && cfg.plex.token);
  const maxBudget = disk ? Math.max(GIB, disk.freeBytes + cfg.cache.budgetBytes) : 500 * GIB;

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
            {steps.map((s, i) => (
              <li key={i} className="flex-1">
                <button
                  type="button"
                  disabled={i > step}
                  onClick={() => setStep(i)}
                  aria-current={i === step ? "step" : undefined}
                  className={cn(
                    "w-full text-left disabled:cursor-not-allowed",
                    i <= step ? "text-fg" : "text-fg-subtle",
                  )}
                >
                  <span
                    className={cn(
                      "block h-1 rounded-full",
                      i < step ? "bg-primary" : i === step ? "bg-primary/60" : "bg-border",
                    )}
                  />
                  <span className="mt-1.5 flex items-center gap-1 text-xs font-medium">
                    {i < step ? (
                      <Check size={12} className="text-primary" />
                    ) : (
                      <span className="tabular-nums">{i + 1}.</span>
                    )}{" "}
                    {s.title}
                  </span>
                </button>
              </li>
            ))}
          </ol>
        </div>
      }
      footer={
        <div className="flex flex-wrap items-center gap-3">
          <Button variant="ghost" disabled={step === 0 || saving} onClick={() => setStep(step - 1)}>
            {t("Back")}
          </Button>
          {/* After setup, only the owner account can sign in. */}
          {stepId === "sources" || stepId === "indexers" || stepId === "catalog" ? (
            <label className="flex items-center gap-2 text-xs text-fg-muted">
              <Checkbox
                checked={Boolean(skipped[stepId])}
                onChange={(e) => setSkipped({ ...skipped, [stepId]: e.currentTarget.checked })}
              />
              {t("Skip test for this step (advanced)")}
            </label>
          ) : null}
          <span className="flex-1" />
          {!advanceOk && stepId !== "cache" ? (
            <span className="hidden text-xs text-fg-muted sm:inline">
              {t("Complete and test this step to continue.")}
            </span>
          ) : null}
          <Button disabled={!advanceOk || saving} onClick={() => void next()}>
            {saving
              ? t("Saving…")
              : step === steps.length - 1
                ? t("Finish setup")
                : t("Save and continue")}
          </Button>
        </div>
      }
    >
      {stepId === "account" ? (
        <Alert>
          <AlertDescription>
            {tx("Adding a node to an existing cluster? {join}", {
              join: (
                <Button variant="link" onClick={() => navigate("/cluster")}>
                  {t("Join a cluster instead")}
                </Button>
              ),
            })}
          </AlertDescription>
        </Alert>
      ) : null}
      <h2 className="text-base font-semibold text-fg">{steps[step]?.heading}</h2>
      <MutationError error={error} />

      {stepId === "apps" ? (
        <div className="grid gap-4 md:grid-cols-2">
          {apps.map((item) => (
            <IntegrationAppCard
              key={item.id}
              identity={{ id: item.id, name: item.name }}
              description={item.description}
              logo={
                item.id === "plex" ? (
                  <PlexMark />
                ) : item.id === "jellyfin" ? (
                  <JellyfinMark />
                ) : item.id === "emby" ? (
                  <EmbyMark />
                ) : undefined
              }
              enabled={Boolean(frontends[item.id])}
              disabled={saving}
              busy={saving}
              onToggle={(enabled) => setFrontends({ ...frontends, [item.id]: enabled })}
            />
          ))}
        </div>
      ) : null}
      {stepId === "account" && !plexSelected ? (
        <form
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault();
            setSaving(true);
            setError(null);
            void api
              .localSetup(localUsername, localPassword)
              .then(({ token }) => {
                setToken(token);
                setLocalPassword("");
                setLocalDone(true);
              })
              .catch(setError)
              .finally(() => setSaving(false));
          }}
        >
          <Field label={t("Username")} htmlFor="setup-owner-username">
            <Input
              id="setup-owner-username"
              autoComplete="username"
              value={localUsername}
              onChange={(e) => setLocalUsername(e.currentTarget.value)}
            />
          </Field>
          <Field label={t("Password")} htmlFor="setup-owner-password">
            <Input
              id="setup-owner-password"
              type="password"
              autoComplete="new-password"
              value={localPassword}
              onChange={(e) => setLocalPassword(e.currentTarget.value)}
            />
          </Field>
          <Button type="submit" disabled={saving || localDone || !localUsername || !localPassword}>
            {localDone ? t("Owner created") : t("Create owner")}
          </Button>
        </form>
      ) : null}
      {stepId === "account" && plexSelected ? (
        <div className="space-y-4">
          <p className="text-sm text-fg-muted">
            {t(
              "Ether registers with your Plex account like a fresh Plex Media Server, so the official Plex apps can find it.",
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
        </div>
      ) : null}

      {stepId === "sources" ? (
        <div className="space-y-6">
          <p className="text-sm text-fg-muted">
            {t(
              "Add a debrid account, a Usenet provider, or both, and test each one. Ether streams from your own accounts; it ships none.",
            )}
          </p>
          {debridSupported ? (
            <section className="space-y-4" aria-labelledby="setup-debrid">
              <div>
                <h3 id="setup-debrid" className="text-sm font-semibold text-fg">
                  {t("Debrid accounts")}
                </h3>
                <p className="text-sm text-fg-muted">
                  {t(
                    "Torrents your service already holds stream over HTTPS. Use your account's API token.",
                  )}
                </p>
              </div>
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
                        aria-label={t("Debrid service")}
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
                    <Field label={t("API token")}>
                      <SecretInput
                        ariaLabel={t("API token")}
                        value={d.apiKey}
                        onChange={(v) => patchDebrid(i, { apiKey: v })}
                      />
                    </Field>
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
            </section>
          ) : null}
          <section className="space-y-4" aria-labelledby="setup-usenet">
            <div>
              <h3 id="setup-usenet" className="text-sm font-semibold text-fg">
                {t("Usenet providers")}
              </h3>
              <p className="text-sm text-fg-muted">
                {t("Block accounts can be marked as backfill.")}
              </p>
            </div>
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
          </section>
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
                      />
                    </Field>
                  ) : (
                    <>
                      <Field label={t("Base URL")} hint={t("/api is added automatically.")}>
                        <Input
                          value={ix.url}
                          placeholder="https://indexer.example.com"
                          onChange={(e) => patchIndexer(i, { url: e.currentTarget.value })}
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
        </div>
      ) : null}

      {stepId === "cache" ? (
        <div className="space-y-5 rounded-xl border border-border bg-surface p-4">
          <Field
            label={t("Cache folder")}
            hint={t(
              "Changing this folder requires restarting Ether after setup. Posters and artwork stay in the configuration folder's cache.",
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
            </div>
          </Field>
          {disk ? (
            <p className="text-sm text-fg-muted">
              {t("{free} free of {total} on this disk.", {
                free: formatBytes(disk.freeBytes),
                total: formatBytes(disk.totalBytes),
              })}
            </p>
          ) : null}
          <Field
            label={t("Disk budget: {size}", { size: formatBytes(cfg.cache.budgetBytes) })}
            hint={t("Older segments are evicted when the cache reaches this size.")}
          >
            <div className="flex items-center gap-3">
              <Slider
                min={GIB}
                max={maxBudget}
                step={GIB}
                value={Math.min(cfg.cache.budgetBytes, maxBudget)}
                aria-label={t("Disk budget")}
                className="flex-1"
                onChange={(e) =>
                  setCfg({
                    ...cfg,
                    cache: { ...cfg.cache, budgetBytes: e.currentTarget.valueAsNumber },
                  })
                }
              />
              <NumberInput
                aria-label={t("Disk budget in GiB")}
                className="w-24"
                value={Math.round(cfg.cache.budgetBytes / GIB)}
                min={1}
                onChange={(n) =>
                  setCfg({
                    ...cfg,
                    cache: { ...cfg.cache, budgetBytes: Math.max(1, Math.round(n)) * GIB },
                  })
                }
              />
              <span className="text-sm text-fg-muted">GiB</span>
            </div>
          </Field>
          <div className="flex flex-wrap gap-5 text-sm text-fg">
            <label className="flex items-center gap-2">
              <Switch
                checked={cfg.cache.prefetchNextEpisode}
                onChange={(e) =>
                  setCfg({
                    ...cfg,
                    cache: { ...cfg.cache, prefetchNextEpisode: e.currentTarget.checked },
                  })
                }
              />{" "}
              {t("Prefetch the next episode")}
            </label>
            <label className="flex items-center gap-2">
              <Switch
                checked={cfg.cache.keepInProgressWarm}
                onChange={(e) =>
                  setCfg({
                    ...cfg,
                    cache: { ...cfg.cache, keepInProgressWarm: e.currentTarget.checked },
                  })
                }
              />{" "}
              {t("Keep in-progress titles warm")}
            </label>
          </div>
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
            {t("Optional. You can add these later under Integrations.")}
          </p>
          <div className="grid gap-4 rounded-xl border border-border bg-surface p-4 sm:grid-cols-2">
            <Field label={t("Trakt client ID")}>
              <Input
                value={cfg.trakt.clientId}
                onChange={(e) =>
                  setCfg({
                    ...cfg,
                    trakt: {
                      ...cfg.trakt,
                      clientId: e.currentTarget.value,
                      enabled: Boolean(e.currentTarget.value),
                    },
                  })
                }
              />
            </Field>
            <Field label={t("OpenSubtitles API key")}>
              <SecretInput
                ariaLabel={t("OpenSubtitles API key")}
                value={cfg.openSubtitles.apiKey}
                onChange={(v) =>
                  setCfg({
                    ...cfg,
                    openSubtitles: { ...cfg.openSubtitles, apiKey: v, enabled: Boolean(v) },
                  })
                }
              />
            </Field>
            <LanguageModelFields
              llm={cfg.llm}
              onChange={(llm) => setCfg({ ...cfg, llm })}
              test={
                <TestButton
                  state={optTests.llm ?? { status: "idle" }}
                  onTest={() => void runTest("opt", "llm", () => api.testLLM(cfg.llm))}
                />
              }
            />
            <label className="flex items-center gap-2 self-end text-sm text-fg">
              <Switch
                checked={cfg.resolver.allowCam}
                onChange={(e) =>
                  setCfg({
                    ...cfg,
                    resolver: { ...cfg.resolver, allowCam: e.currentTarget.checked },
                  })
                }
              />{" "}
              {t("Allow CAM / telesync releases")}
            </label>
          </div>
          <Alert variant="info">
            <AlertDescription>
              {t(
                "Finishing setup locks this admin page to your owner account. You can reopen setup later from Settings.",
              )}
            </AlertDescription>
          </Alert>
        </div>
      ) : null}
    </WizardLayout>
  );
}

/** Wizard step 4's language model: provider, key or endpoint, then model. */
function LanguageModelFields({
  llm,
  onChange,
  test,
}: {
  llm: EtherConfig["llm"];
  onChange: (llm: EtherConfig["llm"]) => void;
  test: ReactNode;
}) {
  const { providers } = useLLMProviders();
  // "None" selected: no endpoint, key or model fields (findProvider reads an
  // empty id as the custom endpoint).
  const provider = llm.provider ? findProvider(providers, llm.provider) : undefined;
  return (
    <>
      <Field
        label={t("Language model provider")}
        hint={t("Powers “Describe it” and themed Home rows.")}
      >
        <SearchSelect
          value={llm.provider}
          onChange={(id) => onChange({ ...withProvider(llm, undefined, id), enabled: false })}
          options={providerOptions(providers)}
          ariaLabel={t("Language model provider")}
          placeholder={providers ? t("None") : t("Loading…")}
          searchPlaceholder={t("Search…")}
          noMatchesLabel={t("No matches")}
          selectLabel={t("Select")}
          closeLabel={t("Close")}
          searchable
        />
      </Field>
      {provider?.local ? (
        <Field label={t("Language model endpoint")} htmlFor="setup-llm-url">
          <Input
            id="setup-llm-url"
            value={llm.baseUrl}
            placeholder={provider.baseUrl || "https://example.com/v1"}
            onChange={(e) => onChange({ ...llm, baseUrl: e.currentTarget.value })}
            spellCheck={false}
          />
        </Field>
      ) : null}
      {provider ? (
        <Field label={t("Language model API key")}>
          <SecretInput
            ariaLabel={t("Language model API key")}
            value={llm.apiKey}
            placeholder={provider.keyHint}
            onChange={(apiKey) => onChange({ ...llm, apiKey })}
          />
        </Field>
      ) : null}
      {provider ? (
        <Field label={t("Language model")} hint={provider.modelHint}>
          <div className="space-y-2">
            <ModelPicker
              cfg={llm}
              provider={provider}
              value={llm.model}
              onChange={(model) => onChange({ ...llm, model, enabled: Boolean(model) })}
            />
            {test}
          </div>
        </Field>
      ) : null}
    </>
  );
}
