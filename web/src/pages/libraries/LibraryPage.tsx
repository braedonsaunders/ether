import { Button, toast } from "@braedonsaunders/appkit-ui";
import { ArrowLeft, Plus } from "lucide-react";
import { useCallback, useMemo, useState } from "react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";
import { useApp } from "../../app-context";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { SubtabNav } from "../../components/layout";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { LevelBadge } from "../../components/status";
import { Workspace } from "../../components/workspace";
import { t } from "../../i18n";
import { api, ApiError, errorMessage } from "../../lib/api";
import { useAsync } from "../../lib/hooks";
import { isRuleComplete } from "../../lib/rules";
import { asObj } from "../settings/SchemaForm";
import { librariesApi, type Library } from "./api";
import { CatalogBrowser } from "./browser/Catalog";
import { RulesPanel, type VisibilityRule } from "./browser/LibraryVisibility";
import { LibraryEditor } from "./LibraryEditor";
import { LibrarySettings } from "./LibrarySettings";
import { PluginBrowse } from "./PluginBrowse";

interface RulesDraft {
  rules: VisibilityRule[];
  visibilityDefault: string;
}

function LibraryRulesTab({
  library,
  mediaType,
  onSaved,
}: {
  library: Library;
  mediaType: "movie" | "show";
  onSaved: () => void;
}) {
  const stored = useMemo<RulesDraft>(
    () => ({
      rules: library.rules ?? [],
      visibilityDefault: library.visibilityDefault === "hide" ? "hide" : "show",
    }),
    [library.rules, library.visibilityDefault],
  );
  const { draft, setDraft, dirty, reset } = useDraft<RulesDraft>(stored);
  const config = useAsync(() => api.getConfig(), []);
  const region = String(asObj(config.data?.catalog).region ?? "");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);

  const rules = draft?.rules ?? stored.rules;
  const fallback = draft?.visibilityDefault ?? stored.visibilityDefault;
  const rulesReady = rules.every((g) => g.rules.length > 0 && g.rules.every(isRuleComplete));

  const save = useCallback(async () => {
    if (!draft) return;
    setSaving(true);
    setError(null);
    try {
      await librariesApi.patch(library.id, {
        rules: draft.rules,
        visibilityDefault: draft.visibilityDefault,
      });
      toast.success(t("Settings saved"));
      onSaved();
    } catch (e) {
      setError(e);
      if (!(e instanceof ApiError)) toast.error(errorMessage(e));
    } finally {
      setSaving(false);
    }
  }, [draft, library.id, onSaved]);

  return (
    <div className="catalog-scroll app-scroll">
      <div className="mx-auto max-w-6xl space-y-4 px-5 pb-5 pt-4 lg:px-7">
        <RulesPanel
          pluginLibrary={library.source === "plugin" ? library : undefined}
          libraryId={library.id}
          mediaType={library.source === "plugin" ? undefined : mediaType}
          rules={rules}
          fallback={fallback}
          region={region}
          onRules={(next) => setDraft({ rules: next, visibilityDefault: fallback })}
          onFallback={(value) => setDraft({ rules, visibilityDefault: value })}
        />
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

/** One library: Browse · Rules · Settings. */
export function LibraryPage() {
  const { id: rawId, tab: rawTab } = useParams();
  const navigate = useNavigate();
  const { health, refreshDashboard } = useApp();
  const [params, setParams] = useSearchParams();
  const [savingAsLibrary, setSavingAsLibrary] = useState(false);
  const id = Number(rawId);
  const library = useAsync((signal) => librariesApi.get(id, signal), [id]);
  const lib = library.data;
  const catalogHealth = health.find((h) => h.id === "catalog");

  if (!Number.isSafeInteger(id) || id <= 0)
    return (
      <Workspace title={t("Libraries")}>
        <ErrorPanel error={new Error(t("That library does not exist"))} />
      </Workspace>
    );
  if (library.loading && !lib)
    return (
      <Workspace title={t("Libraries")}>
        <LoadingRows />
      </Workspace>
    );
  if (!lib)
    return (
      <Workspace title={t("Libraries")}>
        <ErrorPanel error={library.error} onRetry={() => void library.reload()} />
      </Workspace>
    );

  const tabs = [
    { key: "browse", label: t("Browse") },
    { key: "rules", label: t("Rules") },
    { key: "settings", label: t("Settings") },
  ];
  const tab = tabs.some((x) => x.key === rawTab) ? rawTab! : "browse";
  const catalogType = lib.mediaType || "";

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <Workspace
        leading={
          <Button size="sm" variant="ghost" onClick={() => navigate("/libraries")}>
            <ArrowLeft size={14} /> {t("Libraries")}
          </Button>
        }
        title={lib.name}
        actions={
          <>
            {lib.source === "builtin" && catalogHealth ? (
              <LevelBadge
                level={catalogHealth.level}
                label={catalogHealth.level === "ok" ? t("Healthy") : catalogHealth.summary}
              />
            ) : lib.sync?.lastError ? (
              <LevelBadge level="error" label={t("Sync failed")} />
            ) : null}
            {tab === "browse" && lib.source !== "plugin" ? (
              <Button size="sm" variant="outline" onClick={() => setSavingAsLibrary(true)}>
                <Plus size={14} /> {t("Save view as library")}
              </Button>
            ) : null}
          </>
        }
      >
        <div className={tab === "settings" ? "[&>div]:!mb-0" : undefined}>
          <SubtabNav
            ariaLabel={t("Library sections")}
            active={tab}
            onSelect={(key) =>
              navigate(key === "browse" ? `/libraries/${id}` : `/libraries/${id}/${key}`)
            }
            tabs={tabs}
          />
        </div>
        {tab === "browse" ? (
          lib.source === "plugin" ? (
            <div className="app-scroll min-h-0 flex-1 overflow-y-auto">
              <div className="space-y-5 px-5 py-5 lg:px-7">
                <PluginBrowse library={lib} />
              </div>
            </div>
          ) : (
            <CatalogBrowser
              params={params}
              setParams={setParams}
              refreshHealth={refreshDashboard}
              libraryScope={{
                id: lib.id,
                type: catalogType,
              }}
            />
          )
        ) : tab === "rules" ? (
          <LibraryRulesTab
            library={lib}
            mediaType={catalogType === "show" ? "show" : "movie"}
            onSaved={() => {
              void library.reload();
              void refreshDashboard();
            }}
          />
        ) : (
          <div className="app-scroll min-h-0 flex-1 overflow-y-auto">
            <div className="space-y-5 px-3 py-4">
              <LibrarySettings library={lib} onSaved={() => void library.reload()} />
            </div>
          </div>
        )}
      </Workspace>
      {savingAsLibrary ? (
        <LibraryEditor
          sources={lib.sources}
          seed={new URLSearchParams({ ...Object.fromEntries(params), type: catalogType })}
          onClose={() => setSavingAsLibrary(false)}
          onCreated={(created) => {
            setSavingAsLibrary(false);
            navigate(`/libraries/${created.id}`);
          }}
        />
      ) : null}
    </div>
  );
}
