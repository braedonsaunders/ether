import {
  Alert,
  AlertDescription,
  AlertTitle,
  Badge,
  Button,
  EmptyState,
  Input,
  Spinner,
  SubtabNav,
  confirmDialog,
  toast,
  cn,
} from "@braedonsaunders/appkit-ui";
import {
  Copy,
  Eye,
  Globe2,
  History,
  PencilRuler,
  Redo2,
  RefreshCw,
  Save,
  SearchX,
  Sparkles,
  Trash2,
  Undo2,
  EyeOff,
} from "lucide-react";
import { useCallback, useEffect, useMemo, useState } from "react";
import { useLocation, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { ActionMenu } from "../../components/ActionMenu";
import { Artwork } from "../../components/Artwork";
import { ErrorPanel, PosterSkeletons } from "../../components/feedback";
import { Segmented } from "../../components/fields";
import { PosterGrid } from "../../components/Poster";
import { api, ApiError, errorMessage, isAbort } from "../../lib/api";
import { t, tn } from "../../i18n";
import type { CollectionPreview, CollectionSpec } from "../../lib/contract";
import { formatNumber, relativeTime } from "../../lib/format";
import {
  clearDraft,
  draftKey,
  loadDraft,
  saveDraft,
  useHistory,
  type StoredDraft,
} from "../../lib/history";
import { useAsync, useDebouncedValue, useMediaQuery } from "../../lib/hooks";
import {
  blankSpec,
  describeTargets,
  normalizeSpec,
  previewableSpec,
  specProblems,
} from "../../lib/rules";
import { BuildPanel } from "./BuildPanel";
import { DescribeDialog } from "./DescribeDialog";
import { newCollectionHref } from "./links";
import {
  ArtworkPanel,
  ArtworkPreview,
  AudiencePanel,
  SchedulePanel,
  describeSchedule,
} from "./SettingsPanels";

type Tab = "build" | "schedule" | "artwork" | "audience";

const same = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b);

/** "Refresh daily": one phrase per cadence so it reads naturally once translated. */
function refreshBadge(refresh: CollectionSpec["refresh"]): string {
  switch (refresh) {
    case "live":
      return t("Refresh live");
    case "hourly":
      return t("Refresh hourly");
    case "daily":
      return t("Refresh daily");
    case "weekly":
      return t("Refresh weekly");
    default:
      return t("Refresh");
  }
}

function isTextTarget(target: EventTarget | null): boolean {
  const el = target as HTMLElement | null;
  if (!el) return false;
  return (
    el.isContentEditable ||
    el.tagName === "TEXTAREA" ||
    (el.tagName === "INPUT" &&
      !["checkbox", "radio", "range", "color", "button"].includes((el as HTMLInputElement).type))
  );
}

export default function CollectionEditor() {
  const { id } = useParams();
  const [search, setSearch] = useSearchParams();
  const location = useLocation();
  const navigate = useNavigate();
  const templateId = search.get("template");
  const incoming = (location.state as { spec?: CollectionSpec } | null)?.spec;
  const [newDraftId] = useState(() => search.get("draft") ?? `new-${Date.now().toString(36)}`);

  // Keep the draft id in the URL so a reload of a new collection restores it.
  useEffect(() => {
    if (!id && !search.get("draft")) {
      const next = new URLSearchParams(search);
      next.set("draft", newDraftId);
      setSearch(next, { replace: true, state: location.state });
    }
  }, [id, search, setSearch, newDraftId, location.state]);

  const source = useAsync(async () => {
    if (id) return normalizeSpec(await api.collection(id));
    if (incoming)
      return normalizeSpec({ ...incoming, id: undefined, enabled: incoming.enabled ?? false });
    if (templateId) {
      const tpl = (await api.templates()).find((x) => x.id === templateId);
      if (!tpl) throw new ApiError(404, "not_found", t("That template no longer exists."));
      return normalizeSpec({ ...tpl.spec, id: undefined, enabled: false, templateId: tpl.id });
    }
    return blankSpec();
  }, [id, templateId]);

  const config = useAsync(() => api.getConfig(), []);
  const region = config.data?.catalog.region || "US";

  const history = useHistory<CollectionSpec>(blankSpec());
  const spec = history.value;
  const [baseline, setBaseline] = useState<CollectionSpec | null>(null);
  const [restored, setRestored] = useState<StoredDraft<CollectionSpec> | null>(null);
  const [savedLocallyAt, setSavedLocallyAt] = useState<string | null>(null);
  const dKey = draftKey("collection", id ?? newDraftId);

  // Initialise from the server copy (or template), restoring a newer local draft.
  useEffect(() => {
    if (!source.data) return;
    const base = source.data;
    const stored = loadDraft<CollectionSpec>(dKey);
    setBaseline(base);
    if (stored && !same(stored.value, base)) {
      history.reset(normalizeSpec(stored.value));
      setRestored(stored);
    } else {
      history.reset(base);
      setRestored(null);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [source.data, dKey]);

  const ready = baseline !== null;
  const dirty = ready && !same(spec, baseline);
  const isNew = !id;
  const readOnly = Boolean(baseline?.builtIn);

  // Autosave the working copy locally.
  const debouncedSpec = useDebouncedValue(spec, 600);
  useEffect(() => {
    if (!ready) return;
    if (!same(debouncedSpec, baseline)) {
      saveDraft(dKey, debouncedSpec, baseline?.updatedAt);
      setSavedLocallyAt(new Date().toISOString());
    } else {
      clearDraft(dKey);
      setSavedLocallyAt(null);
    }
  }, [debouncedSpec, baseline, dKey, ready, readOnly]);

  useEffect(() => {
    if (!dirty) return;
    const warn = (e: BeforeUnloadEvent) => {
      e.preventDefault();
    };
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [dirty]);

  const update = useCallback(
    (patch: Partial<CollectionSpec>, key?: string) => {
      history.set({ ...history.value, ...patch }, key);
    },
    [history],
  );

  // ---- Live preview ---------------------------------------------------------
  // i18n: memo holds no copy (the server preview request key)
  const previewKey = useMemo(() => JSON.stringify(previewableSpec(spec)), [spec]);
  const debouncedPreview = useDebouncedValue(previewKey, 450);
  const [preview, setPreview] = useState<CollectionPreview | null>(null);
  const [previewError, setPreviewError] = useState<unknown>(null);
  const [previewLoading, setPreviewLoading] = useState(false);
  const [previewNonce, setPreviewNonce] = useState(0);
  useEffect(() => {
    if (!ready) return;
    const ctl = new AbortController();
    setPreviewLoading(true);
    api
      .previewCollection(JSON.parse(debouncedPreview) as CollectionSpec, ctl.signal)
      .then((p) => {
        setPreview(p);
        setPreviewError(null);
      })
      .catch((e) => {
        if (!isAbort(e) && !ctl.signal.aborted) setPreviewError(e);
      })
      .finally(() => {
        if (!ctl.signal.aborted) setPreviewLoading(false);
      });
    return () => ctl.abort();
  }, [debouncedPreview, ready, previewNonce]);

  // ---- Persistence ------------------------------------------------------------
  const [saving, setSaving] = useState(false);
  const [problems, setProblems] = useState<string[]>([]);
  const [rebuilding, setRebuilding] = useState(false);
  const [describeOpen, setDescribeOpen] = useState(false);
  const [tab, setTab] = useState<Tab>(() => {
    const section = search.get("section");
    return section === "artwork" || section === "audience" ? section : "build";
  });
  const wide = useMediaQuery("(min-width: 1024px)");
  const [pane, setPane] = useState<"edit" | "preview">("edit");

  const save = useCallback(
    async (publish?: boolean) => {
      const next = publish === undefined ? spec : { ...spec, enabled: publish };
      const found = specProblems(next);
      setProblems(found);
      if (found.length) {
        toast.error(t("Not saved yet"), { description: found[0] });
        return;
      }
      setSaving(true);
      try {
        const saved = normalizeSpec(
          id ? await api.updateCollection(id, next) : await api.createCollection(next),
        );
        clearDraft(dKey);
        setRestored(null);
        setBaseline(saved);
        history.reset(saved);
        toast.success(
          publish === true ? t("Published") : publish === false ? t("Unpublished") : t("Saved"),
          {
            description: saved.enabled
              ? t("{name} is live.", { name: saved.name })
              : t("{name} is saved as a draft. Publish it when you're ready.", {
                  name: saved.name,
                }),
          },
        );
        if (!id && saved.id)
          navigate(`/collections/${encodeURIComponent(saved.id)}`, { replace: true });
      } catch (e) {
        toast.error(t("Couldn't save"), { description: errorMessage(e) });
      } finally {
        setSaving(false);
      }
    },
    [readOnly, spec, id, dKey, history, navigate],
  );

  const rebuild = async () => {
    if (!id) return;
    setRebuilding(true);
    try {
      const r = await api.rebuildCollection(id);
      setBaseline((b) => (b ? { ...b, itemCount: r.itemCount, lastBuiltAt: r.lastBuiltAt } : b));
      history.reset({ ...spec, itemCount: r.itemCount, lastBuiltAt: r.lastBuiltAt });
      toast.success(t("Rebuilt"), {
        description: tn(r.itemCount, "{count} title.", "{count} titles."),
      });
    } catch (e) {
      toast.error(t("Rebuild failed"), { description: errorMessage(e) });
    } finally {
      setRebuilding(false);
    }
  };

  const remove = async () => {
    if (!id) return;
    const ok = await confirmDialog({
      title: t("Delete collection?"),
      message: t("“{name}” disappears from Plex for everyone. This can't be undone.", {
        name: spec.name,
      }),
      confirmLabel: t("Delete"),
      tone: "danger",
    });
    if (!ok) return;
    try {
      await api.deleteCollection(id);
      clearDraft(dKey);
      toast.success(t("Collection deleted"));
      navigate("/collections");
    } catch (e) {
      toast.error(t("Couldn't delete"), { description: errorMessage(e) });
    }
  };

  const duplicate = () =>
    navigate(newCollectionHref(), {
      state: {
        spec: {
          ...spec,
          id: undefined,
          builtIn: undefined,
          name: t("{name} (copy)", { name: spec.name }),
          enabled: false,
        },
      },
    });

  const discardLocal = () => {
    if (!baseline) return;
    clearDraft(dKey);
    setRestored(null);
    history.reset(baseline);
  };

  // Keyboard: undo/redo outside text fields, save anywhere.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const mod = e.metaKey || e.ctrlKey;
      if (!mod) return;
      const k = e.key.toLowerCase();
      if (k === "s") {
        e.preventDefault();
        void save();
        return;
      }
      if (isTextTarget(e.target)) return;
      if (k === "z" && !e.shiftKey) {
        e.preventDefault();
        history.undo();
      } else if ((k === "z" && e.shiftKey) || k === "y") {
        e.preventDefault();
        history.redo();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [history, save]);

  if (source.error) {
    return (
      <div className="app-scroll min-h-0 flex-1 overflow-y-auto p-6">
        <ErrorPanel error={source.error} onRetry={() => void source.reload()} />
      </div>
    );
  }
  if (!ready)
    return (
      <div className="grid flex-1 place-items-center">
        <Spinner size={22} label={t("Loading collection")} />
      </div>
    );

  const showEdit = wide || pane === "edit";
  const showPreview = wide || pane === "preview";
  const targets = describeTargets(spec);

  return (
    <section aria-label={t("Collection Studio")} className="flex min-h-0 flex-1 flex-col">
      <header className="shrink-0 border-b border-border bg-surface px-3 py-3 sm:px-6">
        <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
          <span className="flex flex-wrap gap-1.5">
            {readOnly ? (
              <Badge variant="secondary">{t("Built-in")}</Badge>
            ) : spec.enabled ? (
              <Badge variant="success">
                <Globe2 size={11} /> {t("Published")}
              </Badge>
            ) : (
              <Badge variant="secondary">{t("Draft")}</Badge>
            )}
            {isNew ? <Badge variant="info">{t("New")}</Badge> : null}
            {dirty ? <Badge variant="warning">{t("Unsaved changes")}</Badge> : null}
          </span>
          <span className="ml-auto text-xs text-fg-subtle" aria-live="polite">
            {savedLocallyAt && dirty ? (
              <>
                <History size={11} className="mr-1 inline" />
                {t("Draft kept on this device · {when}", { when: relativeTime(savedLocallyAt) })}
              </>
            ) : null}
          </span>
        </div>
        <div className="mt-2 flex flex-wrap items-center gap-2">
          <Input
            value={spec.name}
            onChange={(e) => update({ name: e.currentTarget.value }, "name")}
            placeholder={t("Name your collection")}
            aria-label={t("Collection name")}
            disabled={readOnly}
            className="h-11 min-w-0 flex-1 basis-full border-transparent sm:min-w-64 sm:basis-0 bg-transparent px-1 text-xl font-semibold shadow-none hover:border-border focus-visible:border-primary sm:text-xl"
          />
          <div className="flex flex-wrap items-center gap-1.5">
            <Button
              variant="ghost"
              size="icon"
              className="size-9"
              onClick={history.undo}
              disabled={!history.canUndo}
              aria-label={t("Undo")}
              title={t("Undo (⌘Z)")}
            >
              <Undo2 size={16} />
            </Button>
            <Button
              variant="ghost"
              size="icon"
              className="size-9"
              onClick={history.redo}
              disabled={!history.canRedo}
              aria-label={t("Redo")}
              title={t("Redo (⇧⌘Z)")}
            >
              <Redo2 size={16} />
            </Button>
            {!readOnly ? (
              <Button variant="ghost" size="sm" onClick={() => setDescribeOpen(true)}>
                <Sparkles size={15} /> {t("Describe it")}
              </Button>
            ) : null}
            <ActionMenu
              label={t("Collection actions")}
              variant="outline"
              size="icon"
              className="size-9"
              items={[
                { key: "dup", label: t("Duplicate"), icon: Copy, onSelect: duplicate },
                {
                  key: "rebuild",
                  label: t("Rebuild now"),
                  icon: RefreshCw,
                  onSelect: () => void rebuild(),
                  disabled: isNew || dirty || readOnly,
                },
                {
                  key: "unpublish",
                  label: t("Unpublish"),
                  icon: EyeOff,
                  onSelect: () => void save(false),
                  disabled: isNew || !baseline?.enabled,
                },
                {
                  key: "discard",
                  label: t("Discard unsaved changes"),
                  icon: Undo2,
                  onSelect: discardLocal,
                  disabled: !dirty,
                },
                { key: "sep", separator: true },
                {
                  key: "delete",
                  label: t("Delete"),
                  icon: Trash2,
                  onSelect: () => void remove(),
                  danger: true,
                  disabled: isNew || readOnly,
                },
              ]}
            />
            <>
              <Button
                variant="outline"
                size="sm"
                className="h-9"
                onClick={() => void save()}
                disabled={saving || (!dirty && !isNew)}
              >
                <Save size={15} /> {saving ? t("Saving…") : t("Save")}
              </Button>
              {!spec.enabled || dirty || isNew ? (
                <Button size="sm" className="h-9" onClick={() => void save(true)} disabled={saving}>
                  <Globe2 size={15} /> {spec.enabled && !isNew ? t("Save & publish") : t("Publish")}
                </Button>
              ) : null}
            </>
          </div>
        </div>
      </header>

      {restored || problems.length || readOnly ? (
        <div className="shrink-0 space-y-2 border-b border-border bg-surface px-3 py-2 sm:px-6">
          {readOnly ? (
            <Alert variant="info">
              <AlertDescription>
                {t(
                  "Ether manages this collection’s name and rules. Customise its artwork and audience here, or duplicate it to change the title selection.",
                )}
              </AlertDescription>
            </Alert>
          ) : null}
          {restored ? (
            <Alert variant="info">
              <AlertDescription className="flex flex-wrap items-center justify-between gap-2">
                <span>
                  {t("Restored unsaved changes from {when}.", {
                    when: relativeTime(restored.savedAt),
                  })}
                </span>
                <Button variant="ghost" size="sm" onClick={discardLocal}>
                  {t("Discard them")}
                </Button>
              </AlertDescription>
            </Alert>
          ) : null}
          {problems.length ? (
            <Alert variant="warning">
              <AlertTitle>{t("Before you save")}</AlertTitle>
              <AlertDescription>
                <ul className="list-disc pl-5">
                  {problems.map((p) => (
                    <li key={p}>{p}</li>
                  ))}
                </ul>
              </AlertDescription>
            </Alert>
          ) : null}
        </div>
      ) : null}

      {!wide ? (
        <div className="shrink-0 border-b border-border bg-surface px-3 py-2">
          <Segmented
            ariaLabel={t("Show")}
            value={pane}
            onChange={setPane}
            className="w-full [&>button]:flex-1"
            options={[
              { value: "edit", label: t("Edit"), icon: <PencilRuler size={14} /> },
              {
                value: "preview",
                label: preview ? t("Preview · {count}", { count: preview.total }) : t("Preview"),
                icon: <Eye size={14} />,
              },
            ]}
          />
        </div>
      ) : null}

      <div className="flex min-h-0 flex-1">
        {showEdit ? (
          <aside
            className={cn(
              "app-scroll min-h-0 overflow-y-auto bg-surface",
              wide ? "w-1/3 shrink-0 border-r border-border" : "w-full",
            )}
            aria-label={t("Collection editor")}
          >
            <div className="sticky top-0 z-10 border-b border-border bg-surface px-3 sm:px-5">
              <SubtabNav
                ariaLabel={t("Editor sections")}
                active={tab}
                onSelect={(k) => setTab(k as Tab)}
                tabs={[
                  { key: "build", label: t("Build") },
                  { key: "schedule", label: t("Schedule") },
                  { key: "artwork", label: t("Artwork") },
                  { key: "audience", label: t("Audience") },
                ]}
              />
            </div>
            <fieldset
              disabled={readOnly && (tab === "build" || tab === "schedule")}
              className="min-w-0 p-3 sm:p-5"
            >
              {tab === "build" ? <BuildPanel spec={spec} update={update} region={region} /> : null}
              {tab === "schedule" ? (
                <SchedulePanel
                  spec={spec}
                  update={update}
                  onRebuild={!isNew && !dirty ? () => void rebuild() : undefined}
                  rebuilding={rebuilding}
                />
              ) : null}
              {tab === "artwork" ? (
                <ArtworkPanel spec={spec} update={update} preview={preview?.items ?? []} />
              ) : null}
              {tab === "audience" ? <AudiencePanel spec={spec} update={update} /> : null}
            </fieldset>
          </aside>
        ) : null}

        {showPreview ? (
          <section
            className="app-scroll min-h-0 min-w-0 flex-1 overflow-y-auto bg-bg-subtle"
            aria-label={t("Live preview")}
          >
            <div className="mx-auto max-w-6xl space-y-5 p-4 sm:p-6">
              <div className="flex flex-wrap items-start gap-4 rounded-xl border border-border bg-surface p-4">
                {!dirty && spec.id ? (
                  <Artwork
                    src={api.artworkUrl(
                      spec.id,
                      300,
                      [spec.updatedAt, spec.lastBuiltAt].filter(Boolean).join("-"),
                    )}
                    alt={t("{name} collection poster", { name: spec.name })}
                    title={spec.name}
                    className="aspect-[2/3] w-20 shrink-0 rounded-md sm:w-24"
                  />
                ) : (
                  <ArtworkPreview
                    spec={spec}
                    items={preview?.items ?? []}
                    size="sm"
                    className="w-20 shrink-0 sm:w-24"
                  />
                )}
                <div className="min-w-0 flex-1">
                  <p className="text-xs font-medium uppercase tracking-wider text-fg-subtle">
                    {t("How it appears")}
                  </p>
                  <p className="mt-1 truncate text-lg font-semibold text-fg">
                    {spec.name.trim() || t("Untitled collection")}
                  </p>
                  {spec.description ? (
                    <p className="line-clamp-2 text-sm text-fg-muted">{spec.description}</p>
                  ) : null}
                  <div className="mt-2 flex flex-wrap gap-1.5">
                    {targets.length ? (
                      targets.map((target) => (
                        <Badge key={target} variant="default">
                          {target}
                        </Badge>
                      ))
                    ) : (
                      <Badge variant="warning">{t("Not shown anywhere")}</Badge>
                    )}
                    <Badge variant="secondary">
                      {spec.targets.users === "all"
                        ? t("Everyone")
                        : tn(spec.targets.users.length, "{count} viewer", "{count} viewers")}
                    </Badge>
                    {spec.targets.schedule ? (
                      <Badge variant="secondary">{describeSchedule(spec)}</Badge>
                    ) : null}
                    <Badge variant="secondary">{refreshBadge(spec.refresh)}</Badge>
                    {spec.rotate ? (
                      <Badge variant="info">
                        {spec.rotate.every === "weekly"
                          ? t("Rotates {size} weekly", { size: spec.rotate.size })
                          : t("Rotates {size} daily", { size: spec.rotate.size })}
                      </Badge>
                    ) : null}
                  </div>
                </div>
              </div>

              <div className="flex flex-wrap items-center justify-between gap-2">
                <h2 className="flex items-center gap-2 text-sm font-semibold text-fg">
                  {t("Preview")}
                  <span className="font-normal text-fg-muted" aria-live="polite">
                    {preview ? tn(preview.total, "{count} title", "{count} titles") : ""}
                  </span>
                  {previewLoading ? <Spinner size={14} label={t("Updating preview")} /> : null}
                </h2>
                {preview ? (
                  <span className="text-xs text-fg-subtle">
                    {preview.items.length < preview.total
                      ? t("Showing the first {count} · {ms} ms", {
                          count: formatNumber(preview.items.length),
                          ms: preview.tookMs,
                        })
                      : `${preview.tookMs} ms`}
                  </span>
                ) : null}
              </div>
              {preview?.warnings?.map((w) => (
                <Alert key={w} variant="warning">
                  <AlertDescription>{w}</AlertDescription>
                </Alert>
              ))}
              {previewError ? (
                <ErrorPanel error={previewError} onRetry={() => setPreviewNonce((n) => n + 1)} />
              ) : !preview ? (
                <PosterSkeletons count={18} />
              ) : preview.items.length === 0 ? (
                <EmptyState
                  icon={<SearchX />}
                  title={t("No titles match")}
                  description={t("Loosen a rule, switch “all” to “any”, or add another source.")}
                />
              ) : (
                <div className={cn("transition-opacity", previewLoading && "opacity-60")}>
                  <PosterGrid items={preview.items} />
                </div>
              )}
            </div>
          </section>
        ) : null}
      </div>

      <DescribeDialog
        open={describeOpen}
        onClose={() => setDescribeOpen(false)}
        applyLabel={t("Replace rules")}
        onApply={(g) =>
          update({
            sources: g.sources,
            rules: g.rules,
            exclude: g.exclude,
            match: g.match,
            sort: g.sort,
            sortDesc: g.sortDesc,
            mediaType: g.mediaType,
            name: spec.name.trim() ? spec.name : g.name,
            description: spec.description || g.description,
          })
        }
        secondary={{
          label: t("Open as new collection"),
          onApply: (g) => navigate(newCollectionHref(), { state: { spec: g } }),
        }}
      />
    </section>
  );
}
