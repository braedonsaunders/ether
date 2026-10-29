import { Badge, buttonVariants, SettingsSection, Spinner, toast } from "@braedonsaunders/appkit-ui";
import {
  ArrowRight,
  Captions,
  Clapperboard,
  Cloud,
  History,
  ListPlus,
  SkipForward,
  Sparkles,
  Star,
  Tv,
} from "lucide-react";
import { useCallback, useEffect, useState, type ReactNode } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useApp } from "../../app-context";
import { ErrorPanel } from "../../components/feedback";
import { TestButton, type TestState } from "../../components/fields";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { LevelBadge } from "../../components/status";
import { localized, t, tn } from "../../i18n";
import { api, apiFetch, ApiError, type TestFn } from "../../lib/api";
import { useAsync } from "../../lib/hooks";
import type { EtherConfig } from "../../lib/types";
import { SimklIntegration } from "./SimklIntegration";
import { LanguageFeatures } from "./LanguageFeatures";
import { LanguageModelSettings } from "./LanguageModel";
import { asObj, GenericFields, SchemaField, type FieldGroup } from "./SchemaForm";
import * as S from "./schema";

type Obj = Record<string, unknown>;

export type ServiceTab = "overview" | "trakt" | "llm" | "opensubtitles";

interface Service {
  /** The Integrations tab the form lives on. */
  tab: Exclude<ServiceTab, "overview">;
  configKey: "trakt" | "llm" | "openSubtitles";
  endpoint: string;
  label: string;
  groups: FieldGroup[];
  /** A dedicated form instead of schema groups. */
  form?: (props: {
    value: Obj;
    saved: Obj | undefined;
    onChange: (next: Obj) => void;
    errors: Record<string, string>;
    footer: ReactNode;
  }) => ReactNode;
  testLabel: string;
  test: (value: Obj) => ReturnType<TestFn>;
}

const SERVICES: Service[] = localized(() => [
  {
    tab: "trakt",
    configKey: "trakt",
    endpoint: "trakt",
    label: "Trakt",
    groups: S.TRAKT,
    testLabel: t("Test Trakt"),
    test: (v) => api.testTrakt(v as EtherConfig["trakt"]),
  },
  {
    tab: "llm",
    configKey: "llm",
    endpoint: "llm",
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
  },
  {
    tab: "opensubtitles",
    configKey: "openSubtitles",
    endpoint: "opensubtitles",
    label: "OpenSubtitles",
    groups: S.OPENSUBTITLES,
    testLabel: t("Test OpenSubtitles"),
    test: (v) => api.testOpenSubtitles(v as EtherConfig["openSubtitles"]),
  },
]);

/**
 * Integrations: an overview of every integration with its live status,
 * then one tab per credential Ether owns here (Trakt, language model,
 * OpenSubtitles). The three forms share one draft and one save bar, so edits
 * on several tabs save together.
 */
export function ConnectedServices({ tab }: { tab: ServiceTab }) {
  const { refreshDashboard } = useApp();
  const navigate = useNavigate();
  const config = useAsync(() => api.getConfig(), []);
  const cfg = config.data;
  const stored: Record<string, Obj> | null = cfg
    ? Object.fromEntries(
        SERVICES.map((s) => [s.configKey, asObj((cfg as unknown as Obj)[s.configKey])]),
      )
    : null;
  const { draft, setDraft, dirty, reset } = useDraft<Record<string, Obj>>(stored);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [error, setError] = useState<unknown>(null);
  const [saving, setSaving] = useState(false);
  const [tests, setTests] = useState<Record<string, TestState>>({});

  const runTest = useCallback((s: Service, value: Obj) => {
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

  const save = async () => {
    if (!draft || !stored) return;
    const changed = SERVICES.filter(
      (s) => JSON.stringify(draft[s.configKey]) !== JSON.stringify(stored[s.configKey]),
    );
    setSaving(true);
    setError(null);
    setErrors({});
    try {
      // Each service saves through its own endpoint so the server validates it alone.
      for (const s of changed) await api.putSection(s.endpoint, draft[s.configKey]);
      toast.success(t("Settings saved"), { description: changed.map((s) => s.label).join(", ") });
      await config.reload();
      void refreshDashboard();
    } catch (e) {
      setError(e);
      if (e instanceof ApiError && e.fields) {
        setErrors(Object.fromEntries(e.fields.map((f) => [f.field, f.message])));
        const failed = SERVICES.find((s) =>
          e.fields!.some((f) => f.field.startsWith(`${s.configKey}.`)),
        );
        if (failed) navigate(`/services/${failed.tab}`);
      }
      // Sections saved before the failure stay in the draft; saving again is harmless.
    } finally {
      setSaving(false);
    }
  };

  if (config.error) return <ErrorPanel error={config.error} onRetry={() => void config.reload()} />;
  if (!cfg || !draft)
    return (
      <div className="py-10 text-center">
        <Spinner size={20} label={t("Loading settings")} />
      </div>
    );

  return (
    <>
      {tab === "overview" ? (
        <>
          <ServiceOverview cfg={cfg} />
          <SimklIntegration />
        </>
      ) : null}
      {SERVICES.filter((s) => s.tab === tab).map((s) => {
        const value = draft[s.configKey] ?? {};
        const set = (next: Obj) => setDraft({ ...draft, [s.configKey]: next });
        const testButton = (
          <TestButton
            label={s.testLabel}
            state={tests[s.configKey] ?? { status: "idle" }}
            onTest={() => runTest(s, value)}
          />
        );
        if (s.form)
          return (
            <s.form
              key={s.configKey}
              value={value}
              saved={stored?.[s.configKey]}
              onChange={set}
              errors={errors}
              footer={testButton}
            />
          );
        const known = new Set(s.groups.flatMap((g) => g.fields.map((f) => f.key)));
        const extra = Object.keys(value).filter((k) => !known.has(k));
        return s.groups.map((g, i) => {
          const last = i === s.groups.length - 1;
          return (
            <SettingsSection
              key={`${s.configKey}-${g.title ?? i}`}
              title={g.title ?? s.label}
              description={g.description}
              footer={last ? testButton : undefined}
            >
              {g.fields.map((f) => (
                <SchemaField
                  key={f.key}
                  def={f}
                  value={value[f.key]}
                  path={`${s.configKey}.${f.key}`}
                  error={errors[`${s.configKey}.${f.key}`]}
                  onChange={(v) => set({ ...value, [f.key]: v })}
                />
              ))}
              {last && extra.length ? (
                <GenericFields
                  value={value}
                  keys={extra}
                  onChange={set}
                  errors={errors}
                  prefix={s.configKey}
                />
              ) : null}
            </SettingsSection>
          );
        });
      })}
      {/* Mounted on every tab so its unsaved edits survive switching. */}
      <div hidden={tab !== "llm"}>
        <LanguageFeatures />
      </div>
      <SaveBar
        dirty={dirty}
        saving={saving}
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

const linkClass = buttonVariants({ variant: "outline", size: "sm" });

/** On, off, or switched on but missing what it needs. */
function switchStatus(enabled: boolean, ready: boolean, missing: string) {
  if (!enabled) return <Badge variant="secondary">{t("Off")}</Badge>;
  return ready ? (
    <Badge variant="success">{t("On")}</Badge>
  ) : (
    <Badge variant="warning">{missing}</Badge>
  );
}

interface Integration {
  name: string;
  icon: ReactNode;
  description: string;
  status: ReactNode;
  href: string;
  action: string;
}

/** Every integration with its live status, most important first. */
function ServiceOverview({ cfg }: { cfg: EtherConfig }) {
  const { health } = useApp();
  const [ratings, setRatings] = useState<{ enabled?: boolean; provider?: string } | null>(null);
  useEffect(() => {
    let live = true;
    apiFetch<{ enabled?: boolean; provider?: string }>("/discovery/ratings/settings").then(
      (r) => {
        if (live) setRatings(r);
      },
      () => {
        /* status is optional here; the Ratings page reports errors */
      },
    );
    return () => {
      live = false;
    };
  }, []);
  const catalog = health.find((h) => h.id === "catalog");
  const remote = health.find((h) => h.id === "remote");
  const markers = asObj((cfg as unknown as Obj).markers);
  const debridAccounts = (cfg.debrid?.providers ?? []).filter((p) => p.enabled && p.apiKey).length;
  const providerName = ratings?.provider === "mdblist" ? "MDBList" : "OMDb";
  const icon = 18;

  const essentials: Integration[] = [
    {
      name: "Plex",
      icon: <Tv size={icon} />,
      description: t(
        "Your Plex account, sign-in and remote access. How every Plex app finds Ether.",
      ),
      status:
        cfg.plex.claimed && cfg.plex.token ? (
          remote ? (
            <LevelBadge level={remote.level} label={remote.summary} />
          ) : (
            <Badge variant="success">{t("Signed in")}</Badge>
          )
        ) : (
          <Badge variant="warning">{t("Not signed in")}</Badge>
        ),
      href: "/remote-access",
      action: t("Connectivity"),
    },
    {
      name: "TMDB",
      icon: <Clapperboard size={icon} />,
      description: t("Titles, artwork and details for the whole catalogue."),
      status: catalog ? <LevelBadge level={catalog.level} label={catalog.summary} /> : null,
      href: "/catalog/settings",
      action: t("Catalog settings"),
    },
    {
      name: "Debrid",
      icon: <Cloud size={icon} />,
      description: t("Stream torrent releases through your own debrid account."),
      status: debridAccounts ? (
        <Badge variant="success">{tn(debridAccounts, "{count} account", "{count} accounts")}</Badge>
      ) : (
        <Badge variant="secondary">{t("Not connected")}</Badge>
      ),
      href: "/sources/debrid",
      action: t("Sources › Debrid"),
    },
  ];
  const optional: Integration[] = [
    {
      name: "Trakt",
      icon: <History size={icon} />,
      description: t("Sync watch history and lists with Trakt."),
      status: switchStatus(cfg.trakt.enabled, Boolean(cfg.trakt.clientId), t("Needs a client ID")),
      href: "/services/trakt",
      action: cfg.trakt.enabled ? t("Manage Trakt") : t("Set up Trakt"),
    },
    {
      name: t("Language model"),
      icon: <Sparkles size={icon} />,
      description: t(
        "Describe-it collections, themed Home rows, natural-language search and recaps.",
      ),
      status: switchStatus(cfg.llm.enabled, Boolean(cfg.llm.model), t("Needs a model")),
      href: "/services/llm",
      action: cfg.llm.enabled ? t("Manage language model") : t("Set up a language model"),
    },
    {
      name: "OpenSubtitles",
      icon: <Captions size={icon} />,
      description: t("Fetch subtitles when a release doesn't include them."),
      status: switchStatus(
        cfg.openSubtitles.enabled,
        Boolean(cfg.openSubtitles.apiKey),
        t("Needs an API key"),
      ),
      href: "/services/opensubtitles",
      action: cfg.openSubtitles.enabled ? t("Manage OpenSubtitles") : t("Set up OpenSubtitles"),
    },
    {
      name: t("OMDb and MDBList"),
      icon: <Star size={icon} />,
      description: t("IMDb, Rotten Tomatoes and other rating badges in Plex."),
      status: ratings ? (
        ratings.enabled ? (
          <Badge variant="success">{t("Connected · {provider}", { provider: providerName })}</Badge>
        ) : (
          <Badge variant="secondary">{t("Not connected")}</Badge>
        )
      ) : null,
      href: "/metadata/ratings",
      action: t("Metadata › Ratings"),
    },
    {
      name: "TheIntroDB",
      icon: <SkipForward size={icon} />,
      description: t("Intro and credits markers so viewers can skip them."),
      status:
        "theIntroDb" in markers ? (
          markers.theIntroDb ? (
            <Badge variant="success">{t("On")}</Badge>
          ) : (
            <Badge variant="secondary">{t("Off")}</Badge>
          )
        ) : null,
      href: "/playback/markers",
      action: t("Playback › Markers"),
    },
    {
      name: "Prowlarr",
      icon: <ListPlus size={icon} />,
      description: t("Import the Usenet and torrent indexers you already manage."),
      status: null,
      href: "/sources/indexers",
      action: t("Sources › Indexers"),
    },
  ];
  return (
    <>
      <IntegrationGroup
        title={t("Essentials")}
        description={t("How Plex apps reach Ether, where titles come from, and torrent streaming.")}
        items={essentials}
      />
      <IntegrationGroup
        title={t("Optional")}
        description={t("Extras that make Ether smarter. Everything works without them.")}
        items={optional}
      />
    </>
  );
}

function IntegrationGroup({
  title,
  description,
  items,
}: {
  title: string;
  description: string;
  items: Integration[];
}) {
  return (
    <section aria-label={title} className="space-y-3">
      <div>
        <h2 className="text-sm font-semibold text-fg">{title}</h2>
        <p className="text-sm text-fg-muted">{description}</p>
      </div>
      <ul className="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
        {items.map((it) => (
          <li
            key={it.name}
            className="flex flex-col rounded-xl border border-border bg-surface p-5"
            aria-label={it.name}
          >
            <div className="flex items-start gap-3">
              <span className="flex size-10 shrink-0 items-center justify-center rounded-lg bg-bg-subtle text-fg-muted">
                {it.icon}
              </span>
              <div className="min-w-0 space-y-1">
                <h3 className="font-semibold text-fg">{it.name}</h3>
                {it.status ? <div className="flex flex-wrap">{it.status}</div> : null}
              </div>
            </div>
            <p className="mt-3 flex-1 text-sm text-fg-muted">{it.description}</p>
            <div className="mt-4">
              <Link to={it.href} className={linkClass}>
                {it.action} <ArrowRight size={14} />
              </Link>
            </div>
          </li>
        ))}
      </ul>
    </section>
  );
}
