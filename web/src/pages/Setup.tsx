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
import { ErrorPanel, MutationError } from "../components/feedback";
import { Field, NumberInput, SecretInput, TestButton, type TestState } from "../components/fields";
import {
  findProvider,
  ModelPicker,
  providerOptions,
  useLLMProviders,
  withProvider,
} from "../components/llm";
import { api, newId } from "../lib/api";
import { formatBytes } from "../lib/format";
import type { DebridProvider, DiskInfo, EtherConfig, Indexer, UsenetProvider } from "../lib/types";
import { DEBRID_KINDS } from "./sources/Debrid";
import { addonUrlHasDebridKey, isStremio, normalizeAddonUrl } from "./sources/Indexers";

const STEPS = [
  { title: "Plex", heading: "Sign in to plex.tv and claim this server" },
  { title: "Sources", heading: "Where Ether streams from" },
  { title: "Indexers", heading: "Indexers and addons" },
  { title: "Cache", heading: "Cache folder and disk budget" },
  { title: "Catalogue", heading: "Catalogue and extras" },
];
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
const INDEXER_TYPES: { value: IndexerType; label: string }[] = [
  { value: "stremio", label: "Stremio addon (torrents)" },
  { value: "torznab", label: "Torznab (torrents)" },
  { value: "newznab", label: "Newznab (Usenet)" },
];
function indexerType(ix: Indexer): IndexerType {
  if (isStremio(ix)) return "stremio";
  return ix.protocol === "torrent" ? "torznab" : "newznab";
}
function withIndexerType(ix: Indexer, t: IndexerType): Indexer {
  if (t === "stremio") return { ...ix, kind: "stremio", protocol: "torrent", apiKey: "" };
  return { ...ix, kind: "", protocol: t === "torznab" ? "torrent" : "usenet" };
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
  step: number,
  opts: {
    plexDone: boolean;
    providers: UsenetProvider[];
    indexers: Indexer[];
    providerTests: Record<string, TestState>;
    indexerTests: Record<string, TestState>;
    /** The TMDB key was tested or is already saved (false blocks the last step). */
    tmdbReady?: boolean;
    /** Debrid accounts and their tests (absent on servers without debrid). */
    debrid?: DebridProvider[];
    debridTests?: Record<string, TestState>;
    skipped: Record<number, boolean>;
  },
): boolean {
  if (opts.skipped[step]) return true;
  switch (step) {
    case 0:
      return opts.plexDone;
    case 1: {
      // A debrid account, a Usenet provider, or both; every enabled one tested.
      const usenet = opts.providers.filter((p) => p.enabled);
      const debrid = (opts.debrid ?? []).filter((d) => d.enabled);
      return (
        usenet.length + debrid.length > 0 &&
        usenet.every((p) => p.host.trim() && opts.providerTests[p.id]?.status === "ok") &&
        debrid.every((d) => d.apiKey.trim() && opts.debridTests?.[d.id]?.status === "ok")
      );
    }
    case 2: {
      const enabled = opts.indexers.filter((i) => i.enabled);
      return (
        enabled.length > 0 &&
        enabled.every((i) => i.url.trim() && opts.indexerTests[i.id]?.status === "ok")
      );
    }
    case 4:
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
  const [step, setStep] = useState(0);
  const [error, setError] = useState<unknown>(null);
  const [saving, setSaving] = useState(false);
  const [cacheRestartRequired, setCacheRestartRequired] = useState(false);
  const [skipped, setSkipped] = useState<Record<number, boolean>>({});
  const [providerTests, setProviderTests] = useState<Record<string, TestState>>({});
  const [indexerTests, setIndexerTests] = useState<Record<string, TestState>>({});
  const [debridTests, setDebridTests] = useState<Record<string, TestState>>({});
  // A build with a bundled TMDB key needs none from the owner.
  const [tmdbBundled, setTmdbBundled] = useState(false);
  useEffect(() => {
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
  const [claimMsg, setClaimMsg] = useState("");
  const [disk, setDisk] = useState<DiskInfo | null>(null);
  const pollRef = useRef<number | null>(null);
  const mounted = useRef(false);
  const diskRun = useRef(0);
  const pinRun = useRef(0);
  const claimRun = useRef(0);
  const claiming = useRef(false);

  const load = useCallback(() => {
    api
      .getConfig()
      .then((c) => {
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
  useEffect(() => {
    if (step === 3 && cfg) refreshDisk(cfg.cache.dir);
    return () => {
      diskRun.current++;
    };
  }, [step, cfg?.cache.dir]); // eslint-disable-line react-hooks/exhaustive-deps

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
        setClaimMsg("Server claimed to your Plex account.");
      } catch (e) {
        if (!current()) return;
        setClaimMsg(`Signed in, but claiming failed: ${(e as Error).message}`);
      }
      // PIN approval and claiming update the server, not the wizard's draft.
      // Refresh only Plex so the next full-config save retains those credentials.
      const { plex } = await api.getConfig();
      if (!current()) return;
      setCfg((c) => (c ? { ...c, plex } : c));
    } catch (e) {
      if (current()) setError(e);
    } finally {
      if (current()) {
        claiming.current = false;
        setSaving(false);
      }
    }
  };

  const startPin = async () => {
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
          setClaimMsg("Signed in. Claiming this server…");
          await claimServer();
        } catch {
          /* keep polling */
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
          : { status: "fail", message: r.error ?? "failed" },
      }));
    } catch (e) {
      setMap((m) => ({ ...m, [key]: { status: "fail", message: (e as Error).message } }));
    }
  };

  const advanceOk = useMemo(
    () =>
      cfg
        ? canAdvance(step, {
            plexDone: Boolean(cfg.plex.claimed && cfg.plex.token),
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
    [cfg, step, providerTests, indexerTests, debridTests, optTests, tmdbBundled, skipped],
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
        <Spinner size={22} label="Loading configuration" />
      </div>
    );

  if (cacheRestartRequired)
    return (
      <div className="mx-auto w-full max-w-2xl space-y-4 p-6">
        <EtherMark size={40} />
        <h1 className="text-xl font-semibold">Restart Ether to use your cache folder</h1>
        <p>Setup is saved. Restart Ether, or restart its Docker container, before playing media.</p>
        <p className="text-sm text-fg-muted">
          Your selected media cache folder is <code>{cfg.cache.dir}</code>. The running server keeps
          using its previous folder until it restarts. Existing cached media stays in its original
          folder; it is not moved automatically.
        </p>
        <p className="text-sm text-fg-muted">
          Posters and other artwork remain in the configuration folder's cache. They are separate
          from the media cache.
        </p>
        <Button onClick={() => navigate("/cache")}>Open cache settings</Button>
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
      setCfg(await api.putConfig(savedDraft(cfg)));
      if (step < STEPS.length - 1) setStep(step + 1);
      else {
        const status = await api.setupComplete(Object.values(skipped).some(Boolean));
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
              <h1 className="text-lg font-semibold text-fg">Set up Ether</h1>
              <p className="text-sm text-fg-muted">
                Five quick steps and your Plex apps will find Ether like any other server.
              </p>
            </div>
          </div>
          <ol className="mt-4 flex gap-1.5" aria-label="Setup progress">
            {STEPS.map((s, i) => (
              <li key={s.title} className="flex-1">
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
            Back
          </Button>
          {/* Plex cannot be skipped: after setup only its owner can sign in. */}
          {step !== 0 && step !== 3 ? (
            <label className="flex items-center gap-2 text-xs text-fg-muted">
              <Checkbox
                checked={Boolean(skipped[step])}
                onChange={(e) => setSkipped({ ...skipped, [step]: e.currentTarget.checked })}
              />
              Skip test for this step (advanced)
            </label>
          ) : null}
          <span className="flex-1" />
          {!advanceOk && step !== 3 ? (
            <span className="hidden text-xs text-fg-muted sm:inline">
              Complete and test this step to continue.
            </span>
          ) : null}
          <Button disabled={!advanceOk || saving} onClick={() => void next()}>
            {saving ? "Saving…" : step === STEPS.length - 1 ? "Finish setup" : "Save and continue"}
          </Button>
        </div>
      }
    >
      {step === 0 ? (
        <Alert>
          <AlertDescription>
            Adding a node to an existing cluster?{" "}
            <Button variant="link" onClick={() => navigate("/cluster")}>
              Join a cluster instead
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}
      <h2 className="text-base font-semibold text-fg">{STEPS[step]!.heading}</h2>
      <MutationError error={error} />

      {step === 0 ? (
        <div className="space-y-4">
          <p className="text-sm text-fg-muted">
            Ether registers with your Plex account like a fresh Plex Media Server, so the official
            Plex apps can find it.
          </p>
          {plexSigned ? (
            <Alert variant="success">
              <AlertDescription>
                Signed in{cfg.plex.ownerUsername ? ` as ${cfg.plex.ownerUsername}` : ""}. {claimMsg}
              </AlertDescription>
            </Alert>
          ) : null}
          {pin ? (
            <div className="space-y-3 rounded-xl border border-border bg-surface p-4">
              <p className="text-sm text-fg">
                A plex.tv window opened. Sign in there and allow Ether; this page continues on its
                own.
              </p>
              <div className="flex flex-wrap items-center gap-3 text-sm text-fg-muted">
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => window.open(pin.authUrl, "_blank", "noopener,noreferrer")}
                >
                  Open plex.tv sign-in <ExternalLink size={13} />
                </Button>
                <span className="inline-flex items-center gap-2">
                  Waiting for approval <Spinner size={13} label="Waiting" />
                </span>
              </div>
              {/* Short PINs can also be typed at plex.tv/link; strong ones cannot. */}
              {pin.code.length <= 8 ? (
                <p className="text-sm text-fg-muted">
                  Or enter <span className="font-mono font-semibold text-fg">{pin.code}</span> at
                  plex.tv/link.
                </p>
              ) : null}
            </div>
          ) : !plexSigned ? (
            <Button disabled={saving} onClick={() => void startPin()}>
              Sign in to plex.tv
            </Button>
          ) : null}
          {claimMsg && !cfg.plex.claimed ? (
            <p className="text-sm text-fg-muted">{claimMsg}</p>
          ) : null}
          {plexSigned && !cfg.plex.claimed ? (
            <Button variant="outline" disabled={saving} onClick={() => void claimServer()}>
              Claim server
            </Button>
          ) : null}
        </div>
      ) : null}

      {step === 1 ? (
        <div className="space-y-6">
          <p className="text-sm text-fg-muted">
            Add a debrid account, a Usenet provider, or both, and test each one. Ether streams from
            your own accounts; it ships none.
          </p>
          {debridSupported ? (
            <section className="space-y-4" aria-labelledby="setup-debrid">
              <div>
                <h3 id="setup-debrid" className="text-sm font-semibold text-fg">
                  Debrid accounts
                </h3>
                <p className="text-sm text-fg-muted">
                  Torrents your service already holds stream over HTTPS. Use your account&apos;s API
                  token.
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
                    <Field label="Service">
                      <SearchSelect
                        aria-label="Debrid service"
                        value={d.kind}
                        options={DEBRID_KINDS}
                        onChange={(v) => patchDebrid(i, { kind: v as DebridProvider["kind"] })}
                      />
                    </Field>
                    <Field label="API token">
                      <SecretInput
                        ariaLabel="API token"
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
                      Enabled
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
                        <Trash2 size={14} /> Remove
                      </Button>
                    </span>
                  </div>
                </fieldset>
              ))}
              <Button variant="outline" onClick={() => setDebrid([...debrid, blankDebrid()])}>
                <Plus size={15} /> Add debrid account
              </Button>
            </section>
          ) : null}
          <section className="space-y-4" aria-labelledby="setup-usenet">
            <div>
              <h3 id="setup-usenet" className="text-sm font-semibold text-fg">
                Usenet providers
              </h3>
              <p className="text-sm text-fg-muted">Block accounts can be marked as backfill.</p>
            </div>
            {providers.map((p, i) => (
              <fieldset key={p.id || i} className="rounded-xl border border-border bg-surface p-4">
                <legend className="px-1 text-sm font-medium text-fg">
                  {p.name || `Provider ${i + 1}`}
                </legend>
                <div className="grid gap-3 sm:grid-cols-2">
                  <Field label="Name">
                    <Input
                      value={p.name}
                      onChange={(e) => patchProvider(i, { name: e.currentTarget.value })}
                    />
                  </Field>
                  <Field label="Host">
                    <Input
                      value={p.host}
                      placeholder="news.example.com"
                      onChange={(e) => patchProvider(i, { host: e.currentTarget.value })}
                    />
                  </Field>
                  <Field label="Port">
                    <NumberInput
                      aria-label="Port"
                      value={p.port}
                      min={1}
                      max={65535}
                      onChange={(n) => patchProvider(i, { port: n })}
                    />
                  </Field>
                  <Field label="Connections">
                    <NumberInput
                      aria-label="Connections"
                      value={p.connections}
                      min={0}
                      max={500}
                      onChange={(n) => patchProvider(i, { connections: n })}
                    />
                  </Field>
                  <Field label="Username">
                    <Input
                      value={p.username}
                      autoComplete="username"
                      onChange={(e) => patchProvider(i, { username: e.currentTarget.value })}
                    />
                  </Field>
                  <Field label="Password">
                    <SecretInput
                      ariaLabel="Password"
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
                    Backfill only
                  </label>
                  <label className="flex items-center gap-2">
                    <Switch
                      checked={p.enabled}
                      onChange={(e) => patchProvider(i, { enabled: e.currentTarget.checked })}
                    />{" "}
                    Enabled
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
                    <Trash2 size={14} /> Remove
                  </Button>
                </div>
              </fieldset>
            ))}
            <Button variant="outline" onClick={() => setProviders([...providers, blankProvider()])}>
              <Plus size={15} /> Add provider
            </Button>
          </section>
        </div>
      ) : null}

      {step === 2 ? (
        <div className="space-y-4">
          <p className="text-sm text-fg-muted">
            {hasDebrid
              ? "Add at least one torrent source for your debrid account: a Stremio stream addon's install link, or a Torznab URL from Prowlarr, Jackett or Zilean."
              : "Add at least one indexer."}{" "}
            {hasUsenet && hasDebrid ? "Add a Newznab indexer for Usenet too. " : ""}
            Ether handles service limits automatically.
          </p>
          {indexers.map((ix, i) => {
            const type = indexerType(ix);
            const addon = type === "stremio";
            return (
              <fieldset key={ix.id || i} className="rounded-xl border border-border bg-surface p-4">
                <legend className="px-1 text-sm font-medium text-fg">
                  {ix.name || `Indexer ${i + 1}`}
                </legend>
                <div className="grid gap-3 sm:grid-cols-2">
                  <Field label="Type">
                    <SearchSelect
                      aria-label="Indexer type"
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
                  <Field label="Name">
                    <Input
                      value={ix.name}
                      onChange={(e) => patchIndexer(i, { name: e.currentTarget.value })}
                    />
                  </Field>
                  {addon ? (
                    <Field
                      label="Addon link"
                      hint="The addon's install link (https:// or stremio://). Ether checks your debrid accounts itself, so the link needs no debrid key."
                    >
                      <Input
                        value={ix.url}
                        placeholder="https://addon.example/manifest.json"
                        onChange={(e) => patchIndexer(i, { url: e.currentTarget.value })}
                      />
                    </Field>
                  ) : (
                    <>
                      <Field label="Base URL" hint="/api is added automatically.">
                        <Input
                          value={ix.url}
                          placeholder="https://indexer.example.com"
                          onChange={(e) => patchIndexer(i, { url: e.currentTarget.value })}
                        />
                      </Field>
                      <Field label="API key">
                        <SecretInput
                          ariaLabel="API key"
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
                      This link includes a debrid API key. Ether checks your debrid accounts itself,
                      so the addon doesn&apos;t need it; a key only adds cached hints.
                    </AlertDescription>
                  </Alert>
                ) : null}
                <div className="mt-3 flex flex-wrap items-center justify-between gap-2">
                  <label className="flex items-center gap-2 text-sm text-fg">
                    <Switch
                      checked={ix.enabled}
                      onChange={(e) => patchIndexer(i, { enabled: e.currentTarget.checked })}
                    />{" "}
                    Enabled
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
                      <Trash2 size={14} /> Remove
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
            <Plus size={15} /> Add indexer
          </Button>
        </div>
      ) : null}

      {step === 3 ? (
        <div className="space-y-5 rounded-xl border border-border bg-surface p-4">
          <Field
            label="Cache folder"
            hint="Changing this folder requires restarting Ether after setup. Posters and artwork stay in the configuration folder's cache."
          >
            <div className="flex gap-2">
              <Input
                value={cfg.cache.dir}
                onChange={(e) =>
                  setCfg({ ...cfg, cache: { ...cfg.cache, dir: e.currentTarget.value } })
                }
              />
              <Button variant="outline" onClick={() => refreshDisk(cfg.cache.dir)}>
                Check space
              </Button>
            </div>
          </Field>
          {disk ? (
            <p className="text-sm text-fg-muted">
              {formatBytes(disk.freeBytes)} free of {formatBytes(disk.totalBytes)} on this disk.
            </p>
          ) : null}
          <Field
            label={`Disk budget: ${formatBytes(cfg.cache.budgetBytes)}`}
            hint="Older segments are evicted when the cache reaches this size."
          >
            <div className="flex items-center gap-3">
              <Slider
                min={GIB}
                max={maxBudget}
                step={GIB}
                value={Math.min(cfg.cache.budgetBytes, maxBudget)}
                aria-label="Disk budget"
                className="flex-1"
                onChange={(e) =>
                  setCfg({
                    ...cfg,
                    cache: { ...cfg.cache, budgetBytes: e.currentTarget.valueAsNumber },
                  })
                }
              />
              <NumberInput
                aria-label="Disk budget in GiB"
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
              Prefetch the next episode
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
              Keep in-progress titles warm
            </label>
          </div>
        </div>
      ) : null}

      {step === 4 ? (
        <div className="space-y-6">
          <section
            className="space-y-3 rounded-xl border border-border bg-surface p-4"
            aria-labelledby="setup-tmdb"
          >
            <div>
              <h3 id="setup-tmdb" className="text-sm font-semibold text-fg">
                Movie and TV catalogue
              </h3>
              <p className="text-sm text-fg-muted">
                Ether builds your Plex library from TMDB, so a TMDB API key is required. It is free:{" "}
                <a
                  href="https://www.themoviedb.org/settings/api"
                  target="_blank"
                  rel="noreferrer"
                  className="inline-flex items-center gap-1 font-medium text-primary hover:underline"
                >
                  get one at themoviedb.org <ExternalLink size={13} />
                </a>
                , then paste it here and test it.
              </p>
            </div>
            <Field label="TMDB API key" hint="Your v3 API key or v4 read access token.">
              <div className="space-y-2">
                <SecretInput
                  ariaLabel="TMDB API key"
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
            Optional. You can add these later under Integrations.
          </p>
          <div className="grid gap-4 rounded-xl border border-border bg-surface p-4 sm:grid-cols-2">
            <Field label="Trakt client ID">
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
            <Field label="OpenSubtitles API key">
              <SecretInput
                ariaLabel="OpenSubtitles API key"
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
              Allow CAM / telesync releases
            </label>
          </div>
          <Alert variant="info">
            <AlertDescription>
              Finishing setup locks this admin page to your Plex account. You can reopen setup later
              from Connectivity.
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
      <Field label="Language model provider" hint="Powers “Describe it” and themed Home rows.">
        <SearchSelect
          value={llm.provider}
          onChange={(id) => onChange({ ...withProvider(llm, undefined, id), enabled: false })}
          options={providerOptions(providers)}
          ariaLabel="Language model provider"
          placeholder={providers ? "None" : "Loading…"}
          searchable
        />
      </Field>
      {provider?.local ? (
        <Field label="Language model endpoint" htmlFor="setup-llm-url">
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
        <Field label="Language model API key">
          <SecretInput
            ariaLabel="Language model API key"
            value={llm.apiKey}
            placeholder={provider.keyHint}
            onChange={(apiKey) => onChange({ ...llm, apiKey })}
          />
        </Field>
      ) : null}
      {provider ? (
        <Field label="Language model" hint={provider.modelHint}>
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
