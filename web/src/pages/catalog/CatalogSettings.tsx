import { SettingsSection, Spinner, SubtabNav, toast } from "@braedonsaunders/appkit-ui";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useCallback, useState } from "react";
import { useApp } from "../../app-context";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { ErrorPanel } from "../../components/feedback";
import { secretConfigured } from "../../components/IntegrationAccount";
import { t, tx } from "../../i18n";
import { api, ApiError, errorMessage } from "../../lib/api";
import { useAsync } from "../../lib/hooks";
import { isRuleComplete } from "../../lib/rules";
import { asObj, SchemaField, type FieldGroup } from "../settings/SchemaForm";
import { CatalogPickers, isCatalogPicker } from "./CatalogPickers";
import { CATALOG } from "../settings/schema";
import { HomeReleaseSettings, HomeReleaseStatus, UpcomingEpisodeSettings } from "./HomeReleaseSettings";
import { RulesPanel, type VisibilityRule } from "./LibraryVisibility";
import { TrendingStatus } from "./TrendingStatus";
import { LevelBadge } from "../../components/status";
import { SettingsRow } from "../../components/SettingsRow";

type Obj = Record<string, unknown>;

function TmdbAccount({ apiKey }: { apiKey: unknown }) {
  if (secretConfigured(apiKey)) {
    return (
      <p className="text-sm">
        <Link to="/services/tmdb" className="text-primary underline">
          {t("TMDB connected")}
        </Link>
      </p>
    );
  }
  return (
    <p className="text-sm text-fg-muted">
      {tx("Connect TMDB in {link}.", {
        link: (
          <Link to="/services/tmdb" className="text-primary underline">
            {t("Integrations")}
          </Link>
        ),
      })}
    </p>
  );
}

function briefGroup(g: FieldGroup): FieldGroup {
  return {
    ...g,
    description: undefined,
    fields: g.fields.map((f) =>
      "help" in f && typeof f.help === "string" && f.help.length > 90 ? { ...f, help: undefined } : f,
    ),
  };
}

export function CatalogSettings() {
  const { tab } = useParams();
  const navigate = useNavigate();
  const section = tab === "visibility" ? "rules" : "library";
  const { health, refreshDashboard } = useApp();
  const config = useAsync(() => api.getConfig(), []);
  const stored = config.data ? asObj(config.data.catalog) : null;
  const { draft, setDraft, dirty, reset } = useDraft<Obj>(stored);
  const [error, setError] = useState<unknown>(null);
  const [saving, setSaving] = useState(false);
  const rules = (Array.isArray(draft?.exclusions) ? draft.exclusions : []) as VisibilityRule[];
  const fallback = draft?.visibilityDefault === "hide" ? "hide" : "show";
  const rulesReady = rules.every((g) => g.rules.length > 0 && g.rules.every(isRuleComplete));
  const save = useCallback(async () => {
    if (!draft) return;
    setSaving(true);
    setError(null);
    try {
      await api.putSection("catalog", draft);
      toast.success(t("Settings saved"));
      await config.reload();
      void refreshDashboard();
    } catch (e) {
      setError(e);
      if (!(e instanceof ApiError)) toast.error(errorMessage(e));
    } finally {
      setSaving(false);
    }
  }, [config, draft, refreshDashboard]);
  const groups = [...CATALOG, HomeReleaseSettings, UpcomingEpisodeSettings].map(briefGroup);
  return (
    <div className="catalog-scroll app-scroll">
      <div className="space-y-5 p-5 lg:p-7">
        <SubtabNav
          ariaLabel={t("Catalog settings")}
          active={section}
          onSelect={(key) => navigate(key === "rules" ? "/catalog/visibility" : "/catalog/settings")}
          tabs={[
            { key: "library", label: t("General") },
            { key: "rules", label: t("Visibility") },
          ]}
        />
        {config.error ? <ErrorPanel error={config.error} onRetry={() => void config.reload()} /> : null}
        {!draft ? (
          <div className="py-10 text-center">
            <Spinner size={20} label={t("Loading settings")} />
          </div>
        ) : section === "rules" ? (
          <RulesPanel
            rules={rules}
            fallback={fallback}
            region={String(draft.region ?? "")}
            onRules={(next) => setDraft({ ...draft, exclusions: next })}
            onFallback={(value) => setDraft({ ...draft, visibilityDefault: value })}
          />
        ) : (
          <div className="grid gap-5 lg:grid-cols-[minmax(0,1fr)_300px]">
            <div className="space-y-5">
              <TmdbAccount apiKey={draft.tmdbKey} />
              {groups.map((g, i) => (
                <SettingsSection key={g.title ?? i} title={g.title}>
                  {g.fields.map((f, j) =>
                    f.kind === "group" || isCatalogPicker(f.key) ? null : (
                      <SchemaField
                        key={`${f.key}-${j}`}
                        def={f}
                        value={draft[f.key]}
                        path={`catalog.${f.key}`}
                        errors={{}}
                        onChange={(v) => setDraft({ ...draft, [f.key]: v })}
                      />
                    ),
                  )}
                  {g.fields.some((f) => isCatalogPicker(f.key)) ? (
                    <CatalogPickers
                      language={draft.language}
                      region={draft.region}
                      refreshInterval={draft.refreshInterval}
                      onChange={(key, value) => setDraft({ ...draft, [key]: value })}
                    />
                  ) : null}
                </SettingsSection>
              ))}
            </div>
            <aside className="space-y-5">
              <HomeReleaseStatus />
              <TrendingStatus />
              {health.find((h) => h.id === "catalog") ? (
                <SettingsSection title={t("Catalog health")}>
                  <SettingsRow
                    title={t("TMDB catalog")}
                    control={
                      <LevelBadge
                        level={health.find((h) => h.id === "catalog")!.level}
                        label={health.find((h) => h.id === "catalog")!.summary}
                      />
                    }
                  />
                </SettingsSection>
              ) : null}
            </aside>
          </div>
        )}
        <SaveBar
          dirty={dirty}
          saving={saving}
          error={error}
          disabled={dirty && !rulesReady}
          onSave={() => void save()}
          onDiscard={() => {
            reset();
            setError(null);
          }}
        />
      </div>
    </div>
  );
}
