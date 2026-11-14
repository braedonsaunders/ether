import { conditionsFromBrowse } from "./browser/RuleCondition";
import { compileQuery, readQuery } from "./browser/catalogQuery";
import { isRuleComplete } from "../../lib/rules";
import { sourceName, sourceLabel } from "./CatalogueSources";
import { Button, Checkbox, toast } from "@braedonsaunders/appkit-ui";
import { ArrowLeft, Check, Layers, Plus, ListFilter, LibraryBig } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";
import { Drawer } from "../../components/Drawer";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { Segmented } from "../../components/fields";
import { t, tn } from "../../i18n";
import { errorMessage } from "../../lib/api";
import { useAsync } from "../../lib/hooks";
import { librariesApi, type CollectionSource, type Library } from "./api";
import { RulesPanel, type VisibilityRule } from "./browser/LibraryVisibility";
import { AddCatalogueSource, SourceIcon, sourceOwner } from "./CatalogueSources";
import { useStreamEvent } from "../../lib/stream";
import { SetupWizard } from "./SetupWizard";

/**
 * Create a library from the Libraries page: first choose where titles come
 * from (TMDB catalogue or an installed content-kind plugin), then either
 * add visible rules or walk the plugin's setup wizard.
 */
export function NewLibraryDrawer({
  onClose,
  onCreated,
  onPluginOpened,
}: {
  onClose: () => void;
  onCreated: (lib: Library) => void;
  onPluginOpened: (libId?: number) => void;
}) {
  const [step, setStep] = useState<
    "source" | "narrow" | "tmdb" | "all" | "collection" | { pluginId: string; libId?: number }
  >("source");
  const [selected, setSelected] = useState<string[]>([]);
  const [adding, setAdding] = useState(false);
  const [busy, setBusy] = useState(false);
  const registry = useAsync((signal) => librariesApi.catalogueSources(signal), []);
  useStreamEvent("plugins.changed", () => {
    void registry.reload();
  });
  const chosen = registry.data?.filter((source) => selected.includes(source.id)) ?? [];
  if (adding)
    return (
      <AddCatalogueSource onClose={() => setAdding(false)} onAdded={() => void registry.reload()} />
    );
  if (step === "tmdb" || step === "all")
    return (
      <LibraryEditor
        sources={selected}
        sourceNames={chosen.map(sourceLabel)}
        allTitles={step === "all"}
        seed={new URLSearchParams({ type: chosen[0]?.mediaTypes[0] ?? "movie" })}
        onBack={() => setStep("narrow")}
        onClose={onClose}
        onCreated={onCreated}
      />
    );
  if (step === "collection")
    return (
      <CollectionLibraryEditor
        onBack={() => setStep("source")}
        onClose={onClose}
        onCreated={onCreated}
      />
    );
  if (typeof step === "object")
    return <SetupWizard pluginId={step.pluginId} onClose={() => onPluginOpened(step.libId)} />;
  const next = async () => {
    const source = chosen[0];
    if (!source) return;
    if (source.group !== "plugin" || !source.pluginId) {
      setStep("narrow");
      return;
    }
    setBusy(true);
    try {
      const setup = await librariesApi.ensurePluginSetup(source.pluginId);
      const libId = setup.libraries.find((lib) => lib.kindKey === source.kindKey)?.id;
      if (!setup.steps.length) {
        onPluginOpened(libId);
        return;
      }
      setStep({ pluginId: source.pluginId, libId });
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  const card =
    "flex w-full items-center gap-3 rounded-xl border border-border p-4 text-left hover:bg-surface-hover";
  return (
    <Drawer
      open
      onClose={onClose}
      title={t("New library")}
      size="lg"
      footer={
        <div className="flex items-center justify-between gap-3">
          {step === "narrow" ? (
            <Button variant="ghost" onClick={() => setStep("source")}>
              <ArrowLeft size={14} />
              {t("Back")}
            </Button>
          ) : (
            <span />
          )}
          <div className="flex gap-2">
            <Button variant="ghost" onClick={onClose}>
              {t("Cancel")}
            </Button>
            {step === "source" ? (
              <Button disabled={busy || !chosen.length} onClick={() => void next()}>
                {t("Continue")}
              </Button>
            ) : null}
          </div>
        </div>
      }
    >
      {step === "narrow" ? (
        <div className="space-y-3">
          <p className="font-medium">{t("What goes in it")}</p>
          <p className="text-sm text-fg-muted">{chosen.map(sourceLabel).join(", ")}</p>
          {[
            {
              key: "all" as const,
              label: tn(
                chosen.length,
                "Everything from this source",
                "Everything from these sources",
              ),
              description: tn(
                chosen.length,
                "Include every title this source provides.",
                "Include every title these sources provide.",
              ),
              icon: <LibraryBig size={20} />,
            },
            {
              key: "tmdb" as const,
              label: t("Add rules"),
              description: t("Narrow it with genres, years, ratings, or keywords."),
              icon: <ListFilter size={20} />,
            },
          ].map((option) => (
            <button
              key={option.key}
              type="button"
              className={card}
              onClick={() => setStep(option.key)}
            >
              {option.icon}
              <span>
                <span className="block font-medium">{option.label}</span>
                <span className="block text-sm text-fg-muted">{option.description}</span>
              </span>
            </button>
          ))}
        </div>
      ) : (
        <div className="space-y-4">
          <p className="font-medium">{t("Where do titles come from?")}</p>
          <Button variant="outline" onClick={() => setStep("collection")}>
            <Layers size={20} />
            {t("From a collection")}
          </Button>
          {registry.loading && !registry.data ? <LoadingRows rows={2} /> : null}
          {registry.error ? (
            <ErrorPanel error={registry.error} onRetry={() => void registry.reload()} />
          ) : null}
          {[
            ...new Set(
              (registry.data ?? []).map((source) =>
                source.group === "stremio" ? source.pluginId! : source.group,
              ),
            ),
          ].map((group) => (
            <div key={group} className="space-y-2">
              <h3 className="text-sm text-fg-muted">
                {group === "builtin"
                  ? t("Included with Ether")
                  : group === "plugin"
                    ? t("Plugins")
                    : registry.data?.find((source) => source.pluginId === group)?.owner}
              </h3>
              {registry.data
                ?.filter(
                  (source) =>
                    source.group === group ||
                    (source.group === "stremio" && source.pluginId === group),
                )
                .map((source) => {
                  const incompatible =
                    chosen.length > 0 &&
                    !selected.includes(source.id) &&
                    (chosen[0]?.group === "plugin" ||
                      source.group === "plugin" ||
                      chosen[0]?.mediaTypes[0] !== source.mediaTypes[0]);
                  return (
                    <label
                      key={source.id}
                      className={`${card} ${incompatible ? "opacity-50" : "cursor-pointer"}`}
                    >
                      <SourceIcon source={source} />
                      <Checkbox
                        className={source.group === "plugin" ? "rounded-full" : ""}
                        checked={selected.includes(source.id)}
                        disabled={busy || incompatible}
                        onChange={(event) =>
                          setSelected(
                            event.target.checked
                              ? [...selected, source.id]
                              : selected.filter((id) => id !== source.id),
                          )
                        }
                      />
                      <span className="flex-1">
                        <span className="block font-medium">{sourceName(source)}</span>
                        <span className="block text-sm text-fg-muted">
                          {incompatible
                            ? source.group === "plugin" || chosen[0]?.group === "plugin"
                              ? t("Plugin sources are used on their own.")
                              : t("Can't mix movies and TV shows.")
                            : sourceOwner(source)}
                        </span>
                      </span>
                    </label>
                  );
                })}
            </div>
          ))}
          <Button variant="secondary" onClick={() => setAdding(true)}>
            <Plus size={14} />
            {t("Add catalogue source")}
          </Button>
        </div>
      )}
    </Drawer>
  );
}

/** Create a source library with the same visible rules editor used on its Rules tab. */
export function LibraryEditor({
  allTitles,
  sourceNames,
  sources,
  seed,
  onBack,
  onClose,
  onCreated,
}: {
  sourceNames?: string[];
  allTitles?: boolean;
  sources?: string[];
  seed?: URLSearchParams;
  onBack?: () => void;
  onClose: () => void;
  onCreated: (lib: Library) => void;
}) {
  const [seededRules] = useState<VisibilityRule[]>(() => {
    const predicate = {
      ...compileQuery(readQuery(seed?.get("filters") ?? null), seed?.get("viewer") ?? ""),
      ...(seed?.get("genre") ? { genres: [seed.get("genre")] } : {}),
      ...(seed?.get("decade")
        ? { yearFrom: Number(seed.get("decade")), yearTo: Number(seed.get("decade")) + 9 }
        : {}),
      ...(seed?.get("q")?.trim() ? { query: seed.get("q")!.trim() } : {}),
    };
    // A type-only source choice is the full source pool. Only an actual
    // saved browse predicate needs an initial Show rule and a Hide default.
    const conditions = conditionsFromBrowse(predicate);
    if (allTitles || conditions.length === 0) return [];
    return [
      {
        name: "",
        action: "show",
        mediaType: seed?.get("type") === "show" ? "show" : "movie",
        match: "all",
        rules: conditions,
      },
    ];
  });
  const [rules, setRules] = useState(seededRules);
  const [fallback, setFallback] = useState(seededRules.length ? "hide" : "show");
  const [name, setName] = useState("");
  const [type, setType] = useState(seed?.get("type") === "show" ? "show" : "movie");
  const [saving, setSaving] = useState(false);
  const save = async () => {
    setSaving(true);
    try {
      const lib = await librariesApi.create(name.trim(), type, rules, fallback, undefined, sources);
      toast.success(t("Library created"));
      onCreated(lib);
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Drawer
      open
      onClose={onClose}
      title={t("New library")}
      size="lg"
      footer={
        <div className="flex items-center justify-between gap-3">
          {onBack ? (
            <Button variant="ghost" size="sm" onClick={onBack}>
              <ArrowLeft size={14} /> {t("Back")}
            </Button>
          ) : (
            <span className="text-xs text-fg-muted">
              {t("Starts console-only. Turn on “Show in apps” in its Settings when ready.")}
            </span>
          )}
          <span className="flex gap-2">
            <Button variant="ghost" onClick={onClose}>
              {t("Cancel")}
            </Button>
            <Button
              disabled={
                saving || !name.trim() || !rules.every((r) => r.rules.every(isRuleComplete))
              }
              onClick={() => void save()}
            >
              {t("Create library")}
            </Button>
          </span>
        </div>
      }
    >
      <div className="flex flex-col gap-4">
        <label className="library-field">
          <span>{t("Library name")}</span>
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder={t("Comedies the kids can watch")}
            aria-label={t("Library name")}
          />
        </label>
        {allTitles && sourceNames?.length ? (
          <p className="text-sm text-fg-muted">
            {t("Titles from: {sources}", { sources: sourceNames.join(", ") })}
          </p>
        ) : null}
        {!allTitles ? (
          <>
            {sourceNames?.length ? (
              <p className="text-sm text-fg-muted">
                {t("Titles from: {sources}", { sources: sourceNames.join(", ") })}
              </p>
            ) : (
              <>
                {" "}
                <div className="library-field">
                  <span>{t("What goes in it")}</span>
                  <Segmented
                    ariaLabel={t("What goes in it")}
                    value={type}
                    onChange={
                      sources?.length
                        ? () => {}
                        : (next) => {
                            setType(next);
                            setRules(rules.map((rule) => ({ ...rule, mediaType: next })));
                          }
                    }
                    options={[
                      { value: "movie", label: t("Movies") },
                      { value: "show", label: t("TV shows") },
                    ]}
                  />
                </div>
              </>
            )}
            <RulesPanel
              sources={sources ?? [type === "show" ? "tmdb-tv" : "tmdb-movies"]}
              rules={rules}
              fallback={fallback}
              onRules={setRules}
              onFallback={setFallback}
              region=""
              mediaType={type as "movie" | "show"}
            />
          </>
        ) : null}
      </div>
    </Drawer>
  );
}

/**
 * Create a library from one owner collection (an MDBList or Trakt list, a
 * smart rule, a hand-made list…): the library shows exactly that
 * collection's titles, typed movie or show like a filter library. New
 * libraries are console-only until "Show in apps" is turned on (§15.3).
 * The collection can change later; the library always follows it, and an
 * empty or deleted collection simply shows nothing.
 */
export function CollectionLibraryEditor({
  sourceIds,
  sourceNames,
  sourceType,
  collectionId: initialCollectionId,
  onBack,
  onClose,
  onCreated,
}: {
  sourceIds?: string[];
  sourceNames?: string[];
  sourceType?: string;
  collectionId?: number;
  onBack?: () => void;
  onClose: () => void;
  onCreated: (lib: Library) => void;
}) {
  const sources = useAsync((signal) => librariesApi.listCollectionSources(signal), []);
  const [name, setName] = useState("");
  const [collectionId, setCollectionId] = useState<number | null>(initialCollectionId ?? null);
  const [saving, setSaving] = useState(false);

  const collections: CollectionSource[] = sources.data ?? [];
  const selected = collections.find((c) => c.id === collectionId) ?? null;
  // A typed library shows one media type; a mixed collection is offered as
  // movies (the catalogue browse keeps only movie members) but the owner
  // may switch to shows.
  const [type, setType] = useState<"movie" | "show">(
    sourceType === "show" || selected?.mediaType === "show" ? "show" : "movie",
  );
  const chooseCollection = (c: CollectionSource) => {
    setCollectionId(c.id);
    setType(sourceType === "show" || (!sourceType && c.mediaType === "show") ? "show" : "movie");
    if (!name.trim()) setName(c.name);
  };

  const save = async () => {
    if (collectionId === null) return;
    setSaving(true);
    try {
      const lib = await librariesApi.createCollection(
        name.trim(),
        collectionId,
        type,
        undefined,
        sourceIds,
      );
      toast.success(t("Library created"));
      onCreated(lib);
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Drawer
      open
      onClose={onClose}
      title={t("New library from a collection")}
      size="lg"
      footer={
        <div className="flex items-center justify-between gap-3">
          {onBack ? (
            <Button variant="ghost" size="sm" onClick={onBack}>
              <ArrowLeft size={14} /> {t("Back")}
            </Button>
          ) : (
            <span className="text-xs text-fg-muted">
              {t("Starts console-only. Turn on “Show in apps” in its Settings when ready.")}
            </span>
          )}
          <span className="flex gap-2">
            <Button variant="ghost" onClick={onClose}>
              {t("Cancel")}
            </Button>
            <Button
              disabled={saving || collectionId === null || !name.trim()}
              onClick={() => void save()}
            >
              {t("Create library")}
            </Button>
          </span>
        </div>
      }
    >
      <div className="flex flex-col gap-4">
        {sources.loading && !sources.data ? <LoadingRows rows={3} /> : null}
        {sources.error ? (
          <ErrorPanel error={sources.error} onRetry={() => void sources.reload()} />
        ) : null}
        {!sources.loading && collections.length === 0 ? (
          <p className="text-sm text-fg-muted">
            <Link
              to="/collections"
              onClick={onClose}
              className="font-medium text-primary underline"
            >
              {t("Create a collection")}
            </Link>
          </p>
        ) : null}
        {collections.length > 0 ? (
          <div className="library-field">
            <span>{t("Collection")}</span>
            <div className="flex flex-col gap-1">
              {collections.map((c) => (
                <div
                  key={c.id}
                  role="button"
                  tabIndex={0}
                  onClick={() => chooseCollection(c)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" || e.key === " ") {
                      e.preventDefault();
                      chooseCollection(c);
                    }
                  }}
                  aria-current={c.id === collectionId ? "true" : undefined}
                  className={`flex cursor-pointer items-center gap-3 rounded-lg border p-3 text-left transition-colors ${
                    c.id === collectionId
                      ? "border-primary bg-surface-hover"
                      : "border-border hover:border-border-strong"
                  }`}
                >
                  <Layers size={16} className="shrink-0 text-fg-muted" aria-hidden />
                  <span className="min-w-0 flex-1">
                    <span className="block font-medium text-fg">{c.name}</span>
                    <span className="block text-sm text-fg-muted">
                      {tn(c.itemCount, "{count} title", "{count} titles")}
                    </span>
                  </span>
                  {c.id === collectionId ? (
                    <Check size={16} className="shrink-0 text-primary" aria-hidden />
                  ) : null}
                </div>
              ))}
            </div>
          </div>
        ) : null}
        <label className="library-field">
          <span>{t("Library name")}</span>
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder={t("MDBList picks")}
            aria-label={t("Library name")}
          />
        </label>
        {sourceNames?.length ? (
          <p className="text-sm text-fg-muted">
            {t("Titles from: {sources}", { sources: sourceNames.join(", ") })}
          </p>
        ) : (
          <>
            {" "}
            <div className="library-field">
              <span>{t("What goes in it")}</span>
              <Segmented
                ariaLabel={t("What goes in it")}
                value={type}
                onChange={sourceType ? () => {} : setType}
                options={[
                  { value: "movie", label: t("Movies") },
                  { value: "show", label: t("TV shows") },
                ]}
              />
            </div>
          </>
        )}
        <p className="text-sm text-fg-muted">
          {t(
            "The library shows exactly this collection's titles and follows it as it changes. If the collection is deleted, the library simply shows nothing.",
          )}
        </p>
      </div>
    </Drawer>
  );
}
