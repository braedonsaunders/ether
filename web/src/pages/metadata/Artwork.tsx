import { Badge, Button, Input, Spinner, Switch, cn, toast } from "@braedonsaunders/appkit-ui";
import { Check, ImageIcon, Pin, RefreshCw, RotateCcw, SlidersHorizontal } from "lucide-react";
import { useEffect, useState } from "react";
import { Artwork } from "../../components/Artwork";
import { Drawer } from "../../components/Drawer";
import { ErrorPanel, MutationError } from "../../components/feedback";
import { ListInput } from "../../components/fields";
import { Section } from "../../components/layout";
import { TitleSearch } from "../../components/pickers";
import { titleSubtitle } from "../../components/Poster";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { localized, t } from "../../i18n";
import { api, apiFetch, errorMessage } from "../../lib/api";
import type { TitleCard } from "../../lib/contract";
import { useAsync, useDebouncedValue } from "../../lib/hooks";
import { blankSpec } from "../../lib/rules";
import { nativeSelect } from "./shared";
import { MetadataWorkspace } from "./MetadataWorkspace";
import {
  normalizeOverlays,
  PRESETS,
  explanationURL,
  type ArtworkExplanation,
  type Overlays,
} from "./model";
import { PosterProviders } from "./PosterProviders";
import { PosterCanvas } from "./PosterCanvas";

type Variant = { path: string; language?: string; tags?: string[] };
type Art = {
  posters: Variant[] | null;
  backdrops: Variant[] | null;
  pinnedPoster?: string;
  pinnedBackdrop?: string;
};
const OPTIONS = localized(
  () =>
    [
      {
        key: "status",
        label: t("Release status badge"),
        hint: t("Recent releases, unwatched new episodes and seasons."),
      },
      {
        key: "quality",
        label: t("Quality badge"),
        hint: t("Last verified playback: resolution, HDR, Dolby Vision and Atmos."),
      },
      {
        key: "ratings",
        label: t("Rating badges"),
        hint: t("TMDB by default; cached provider scores when configured."),
      },
      {
        key: "details",
        label: t("Title details"),
        hint: t("Year, runtime and content rating from the catalogue."),
      },
      {
        key: "personalize",
        label: t("Match posters to taste"),
        hint: t("Choose tagged artwork for each viewer."),
      },
      {
        key: "localize",
        label: t("Localized artwork"),
        hint: t("Follow the language requested by Plex."),
      },
    ] as const,
);

export function ArtworkSettings() {
  const doc = useAsync(
    async () => normalizeOverlays(await apiFetch<Overlays>("/discovery/overlays/settings")),
    [],
  );
  const users = useAsync(() => api.users(), []);
  const { draft, setDraft, dirty, reset } = useDraft(doc.data);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);
  // Use the same catalogue and image pipeline as the Collection Studio.
  // A bounded pool keeps refresh instant and avoids refetching the catalogue.
  const samples = useAsync(async (signal) => {
    const result = await api.previewCollection(
      { ...blankSpec("mixed"), name: "Artwork preview", limit: 60 },
      signal,
    );
    const titles = [
      ...new Map(
        (result.items ?? [])
          .filter((card) => card.poster && (card.type === "movie" || card.type === "show"))
          .map((card) => [card.key, card]),
      ).values(),
    ];
    for (let i = titles.length - 1; i > 0; i--) {
      const j = Math.floor(Math.random() * (i + 1));
      [titles[i], titles[j]] = [titles[j]!, titles[i]!];
    }
    return titles;
  }, []);
  const [pickedTitle, setTitle] = useState<TitleCard | null>(null);
  const title = pickedTitle ?? samples.data?.[0] ?? null;
  const alternatives = (samples.data ?? []).filter((card) => card.key !== title?.key);
  const refreshTitle = () => {
    if (alternatives.length)
      setTitle(alternatives[Math.floor(Math.random() * alternatives.length)]!);
  };
  const [lang, setLang] = useState("en");
  const [viewer, setViewer] = useState<number | null>(null);
  const [nonce, setNonce] = useState(0);
  const [pinning, setPinning] = useState<TitleCard | null>(null);
  const preview = useDebouncedValue(draft, 180);
  const previewUser = viewer ?? (users.data?.find((u) => u.isOwner) ?? users.data?.[0])?.id ?? null;
  const [mode, setMode] = useState<"poster" | "library" | "compare">("poster");
  const explanationRequest =
    title && preview && previewUser != null
      ? explanationURL(preview, title.key, previewUser, lang, nonce)
      : null;
  const explanation = useAsync(
    async (signal) => {
      if (!explanationRequest) return null;
      const report = await apiFetch<ArtworkExplanation>(explanationRequest, { signal });
      return { request: explanationRequest, report };
    },
    [explanationRequest],
  );
  // useAsync keeps old data during reloads. Never attribute a previous title's
  // or viewer's evidence to the newly selected preview, even before effects run.
  const explanationData =
    !explanation.loading && !explanation.error && explanation.data?.request === explanationRequest
      ? explanation.data?.report
      : null;
  if (doc.error)
    return (
      <MetadataWorkspace
        aside={<ErrorPanel error={doc.error} onRetry={() => void doc.reload()} />}
        main={null}
      />
    );
  if (!draft || !preview)
    return (
      <MetadataWorkspace
        aside={<Spinner size={20} label={t("Loading artwork settings")} />}
        main={null}
      />
    );
  const set = (patch: Partial<Overlays>) => setDraft({ ...draft, ...patch });
  const save = async () => {
    setSaving(true);
    setError(null);
    try {
      const next = await apiFetch<Overlays>("/discovery/overlays/settings", {
        method: "PUT",
        body: JSON.stringify(draft),
      });
      doc.setData(normalizeOverlays(next));
      setNonce((n) => n + 1);
      toast.success(t("Artwork settings saved"), {
        description: t("Plex picks up the new artwork when it refreshes."),
      });
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  };

  return (
    <>
      <MetadataWorkspace
        fitPreview
        actions={
          <Badge variant={dirty ? "warning" : "secondary"}>
            {dirty ? t("Unsaved design") : t("Saved design")}
          </Badge>
        }
        aside={
          <>
            <div className="metadata-enable">
              <div>
                <label htmlFor="overlay-enabled">{t("Ether posters")}</label>
                <p>{t("Serve customized posters")}</p>
              </div>
              <Switch
                id="overlay-enabled"
                aria-label={t("Ether posters")}
                checked={draft.enabled}
                disabled={saving}
                onChange={(e) => set({ enabled: e.currentTarget.checked })}
              />
            </div>
            <PlexMetadataToggle />
            <PosterProviders onSaved={() => setNonce((n) => n + 1)} />
            <section className="metadata-control-section" aria-label={t("Design presets")}>
              <div className="metadata-section-title">
                <h3>{t("Preset")}</h3>
              </div>
              <div className="metadata-presets">
                {PRESETS.map((p) => {
                  const selected = Object.entries(p.settings).every(
                    ([k, v]) => draft[k as keyof Overlays] === v,
                  );
                  return (
                    <button
                      key={p.key}
                      type="button"
                      className={cn("metadata-preset", selected && "selected")}
                      aria-pressed={selected}
                      disabled={saving}
                      onClick={() => set(p.settings)}
                    >
                      <span className={`metadata-preset-art preset-${p.key}`}>
                        <span style={{ background: p.settings.accent }} />
                        {selected && <Check size={13} />}
                      </span>
                      <strong>{p.name}</strong>
                      <small>{p.description}</small>
                    </button>
                  );
                })}
              </div>
            </section>
            <fieldset disabled={!draft.enabled || saving} className="metadata-control-section">
              <div className="metadata-section-title">
                <h3>{t("Badge appearance")}</h3>
              </div>
              <label className="metadata-field">
                {t("Badge finish")}
                <select
                  className={nativeSelect}
                  value={draft.style}
                  onChange={(e) => set({ style: e.currentTarget.value as Overlays["style"] })}
                >
                  <option value="glass">{t("Glass")}</option>
                  <option value="solid">{t("Solid")}</option>
                  <option value="minimal">{t("Minimal")}</option>
                </select>
              </label>
              <div className="metadata-field-row">
                <label className="metadata-field">
                  {t("Placement")}
                  <select
                    className={nativeSelect}
                    value={draft.position}
                    onChange={(e) =>
                      set({ position: e.currentTarget.value as Overlays["position"] })
                    }
                  >
                    <option value="bottom">{t("Bottom")}</option>
                    <option value="top">{t("Top")}</option>
                  </select>
                </label>
                <label className="metadata-field">
                  {t("Badge size")}
                  <select
                    className={nativeSelect}
                    value={draft.size}
                    onChange={(e) => set({ size: e.currentTarget.value as Overlays["size"] })}
                  >
                    <option value="small">{t("Small")}</option>
                    <option value="medium">{t("Medium")}</option>
                    <option value="large">{t("Large")}</option>
                  </select>
                </label>
              </div>
              <div className="metadata-field">
                <span>{t("Accent colour")}</span>
                <div className="metadata-swatches">
                  {[
                    ["#d4a574", t("Sand")],
                    ["#a7c4b5", t("Sage")],
                    ["#9ab6d4", t("Glacier")],
                    ["#c0a6d8", t("Lavender")],
                    ["#e8e4dc", t("Pearl")],
                  ].map(([color, name]) => (
                    <button
                      type="button"
                      key={color}
                      aria-label={name}
                      aria-pressed={draft.accent === color}
                      onClick={() => set({ accent: color })}
                      style={{ background: color }}
                    >
                      {draft.accent === color && <Check size={16} />}
                    </button>
                  ))}
                  <label className="metadata-custom-color" title={t("Custom accent")}>
                    <input
                      type="color"
                      aria-label={t("Custom accent")}
                      value={draft.accent}
                      onChange={(e) => set({ accent: e.currentTarget.value })}
                    />
                    <SlidersHorizontal size={15} />
                  </label>
                  <code>{draft.accent}</code>
                </div>
              </div>
            </fieldset>
            <section className="metadata-control-section" aria-label={t("Poster layers")}>
              <div className="metadata-section-title">
                <h3>{t("Poster layers")}</h3>
                <span className="text-[10px] text-fg-muted">
                  {explanation.loading || draft !== preview
                    ? t("Checking title…")
                    : t("Live title data")}
                </span>
              </div>
              {explanation.error && !explanation.loading ? (
                <p className="text-xs text-fg-muted">
                  {t("Preview data unavailable.")}{" "}
                  <button className="underline" onClick={() => void explanation.reload()}>
                    {t("Retry title data")}
                  </button>
                </p>
              ) : null}
              {OPTIONS.map((o) => (
                <div className="metadata-toggle" key={o.key}>
                  <div>
                    <label htmlFor={`overlay-${o.key}`}>{o.label}</label>
                    <p>{o.hint}</p>
                    {explanationData?.layers.find((l) => l.key === o.key && l.state !== "off") &&
                      (() => {
                        const layer = explanationData!.layers.find((l) => l.key === o.key)!;
                        return (
                          <p
                            className={`metadata-layer-state is-${layer.state}`}
                            title={layer.reason}
                          >
                            {layer.state === "ready" ? layer.labels.join(" · ") : layer.reason}
                          </p>
                        );
                      })()}
                  </div>
                  <Switch
                    id={`overlay-${o.key}`}
                    aria-label={o.label}
                    checked={draft[o.key]}
                    disabled={!draft.enabled || saving}
                    onChange={(e) => set({ [o.key]: e.currentTarget.checked })}
                  />
                </div>
              ))}
            </section>
            <Section
              title={t("Preview title")}
              description={t("Try your design on artwork from your catalogue.")}
            >
              <TitleSearch placeholder={t("Search movies and shows…")} onPick={setTitle} />
              {title && (
                <Button size="sm" variant="ghost" onClick={() => setPinning(title)}>
                  <Pin size={13} /> {t("Edit this title’s artwork")}
                </Button>
              )}
              <div className="metadata-field-row">
                <label className="metadata-field">
                  {t("Preview as")}
                  <select
                    className={nativeSelect}
                    value={previewUser ?? ""}
                    onChange={(e) => setViewer(Number(e.currentTarget.value))}
                  >
                    <option value="" disabled>
                      {t("Select a viewer")}
                    </option>
                    {users.data?.map((u) => (
                      <option key={u.id} value={u.id}>
                        {u.title || u.username}
                      </option>
                    ))}
                  </select>
                </label>
                <label className="metadata-field">
                  {t("Preview language")}
                  <Input
                    value={lang}
                    onChange={(e) => setLang(e.currentTarget.value)}
                    placeholder="en, fr-CA, es"
                  />
                </label>
              </div>
              {users.error ? (
                <ErrorPanel error={users.error} onRetry={() => void users.reload()} />
              ) : null}
            </Section>
            <Section
              title={t("Title artwork")}
              description={t("Give an individual title its own poster or backdrop.")}
            >
              <TitleSearch placeholder={t("Find a title to customise…")} onPick={setPinning} />
            </Section>
          </>
        }
        main={
          <>
            <div className="metadata-preview-toolbar">
              <div className="metadata-preview-title" aria-live="polite">
                <h2 title={title?.title}>{title?.title ?? t("Artwork preview")}</h2>
                <p>{title ? titleSubtitle(title) : t("Catalogue artwork")}</p>
              </div>
              <Button
                size="sm"
                variant="ghost"
                aria-label={t("Try another artwork")}
                title={t("Try another artwork")}
                disabled={!alternatives.length || samples.loading}
                onClick={refreshTitle}
              >
                <RefreshCw size={15} />
              </Button>
              <div className="metadata-view-switch" aria-label={t("Preview view")}>
                {(
                  [
                    ["poster", t("Poster")],
                    ["library", t("Library")],
                    ["compare", t("Compare")],
                  ] as const
                ).map(([key, label]) => (
                  <button
                    type="button"
                    aria-pressed={mode === key}
                    key={key}
                    onClick={() => setMode(key)}
                  >
                    {label}
                  </button>
                ))}
              </div>
            </div>
            {title ? (
              <PosterCanvas
                title={title}
                neighbours={alternatives.slice(0, 2)}
                mode={mode}
                settings={preview}
                user={previewUser}
                lang={lang}
                nonce={nonce}
              />
            ) : (
              <div className="metadata-stage metadata-artwork-empty">
                {samples.loading ? (
                  <Spinner size={22} label={t("Loading catalogue artwork")} />
                ) : samples.error ? (
                  <ErrorPanel error={samples.error} onRetry={() => void samples.reload()} />
                ) : (
                  <>
                    <ImageIcon size={28} />
                    <h2>{t("No catalogue artwork yet")}</h2>
                    <p>
                      {t(
                        "Search for a movie or show in Preview title, or refresh after your catalogue has synced.",
                      )}
                    </p>
                    <Button size="sm" variant="outline" onClick={() => void samples.reload()}>
                      {t("Reload catalogue artwork")}
                    </Button>
                  </>
                )}
              </div>
            )}
            <div className="metadata-preview-caption">
              <span className="metadata-live-dot" />
              <span>
                {mode === "library"
                  ? t("Library preview · Plex layouts vary")
                  : !explanation.loading &&
                      explanationData &&
                      draft.enabled &&
                      explanationData.labels.length === 0
                    ? t("No selected badges available · see Poster layers")
                    : t("Originals preserved · available badges only")}
              </span>
              <Badge variant="outline">
                {draft.enabled ? t("Ether design") : t("Original artwork")}
              </Badge>
            </div>
          </>
        }
      />
      <TitleArtworkDrawer
        title={pinning}
        lang={lang}
        onClose={() => setPinning(null)}
        onSaved={() => setNonce((n) => n + 1)}
      />
      <SaveBar
        dirty={dirty}
        saving={saving}
        error={error}
        onSave={save}
        onDiscard={() => {
          reset();
          setError(null);
        }}
      />
    </>
  );
}

/** One title's artwork choices: a record, so it's edited in a drawer. */
function TitleArtworkDrawer({
  title,
  lang,
  onClose,
  onSaved,
}: {
  title: TitleCard | null;
  lang: string;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [art, setArt] = useState<Art>({ posters: [], backdrops: [] });
  const [choices, setChoices] = useState<Art | null>(null);
  const [loadError, setLoadError] = useState<unknown>(null);
  const [saveError, setSaveError] = useState<unknown>(null);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    if (!title) return;
    const ctl = new AbortController();
    setChoices(null);
    setLoadError(null);
    setSaveError(null);
    void Promise.all([
      apiFetch<Art>(`/discovery/overlays/artwork/${title.key}`, { signal: ctl.signal }),
      apiFetch<Art>(`/discovery/overlays/variants/${title.key}?lang=${encodeURIComponent(lang)}`, {
        signal: ctl.signal,
      }),
    ])
      .then(([current, loaded]) => {
        if (ctl.signal.aborted) return;
        setChoices(loaded);
        setArt({
          ...current,
          posters: (loaded.posters ?? []).map((v) => ({
            ...v,
            tags: current.posters?.find((p) => p.path === v.path)?.tags ?? [],
          })),
          backdrops: loaded.backdrops,
        });
      })
      .catch((e) => {
        if (!ctl.signal.aborted) setLoadError(e);
      });
    return () => ctl.abort();
    // Reload only for a different title; the language is read when it opens.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [title?.key]);

  const save = async () => {
    if (!title) return;
    setSaving(true);
    setSaveError(null);
    try {
      await apiFetch(`/discovery/overlays/artwork/${title.key}`, {
        method: "PUT",
        body: JSON.stringify(art),
      });
      toast.success(t("Artwork saved"), { description: title.title });
      onSaved();
      onClose();
    } catch (e) {
      setSaveError(e);
    } finally {
      setSaving(false);
    }
  };

  return (
    <Drawer
      open={title !== null}
      onClose={onClose}
      title={title?.title ?? t("Title artwork")}
      description={t("Pick the poster and backdrop for this title.")}
      size="xl"
      footer={
        <div className="flex w-full flex-wrap items-center justify-between gap-2">
          <Button
            variant="ghost"
            disabled={!choices}
            onClick={() => setArt({ ...art, pinnedPoster: "", pinnedBackdrop: "" })}
          >
            <RotateCcw size={14} /> {t("Use automatic choices")}
          </Button>
          <div className="flex gap-2">
            <Button variant="outline" onClick={onClose}>
              {t("Cancel")}
            </Button>
            <Button disabled={!choices || saving} onClick={() => void save()}>
              {saving ? t("Saving…") : t("Save title artwork")}
            </Button>
          </div>
        </div>
      }
    >
      {loadError ? (
        <ErrorPanel error={loadError} />
      ) : !choices ? (
        <div className="py-10 text-center">
          <Spinner size={20} label={t("Loading artwork choices")} />
        </div>
      ) : (
        <div className="space-y-6">
          {saveError ? <MutationError error={saveError} /> : null}
          <section className="space-y-3" aria-label={t("Posters")}>
            <div>
              <h3 className="text-sm font-semibold text-fg">{t("Poster")}</h3>
              <p className="text-xs text-fg-muted">
                {t(
                  "Click a poster to pin it for everyone. Tag posters with the genres or people they feature so taste matching can pick one per viewer.",
                )}
              </p>
            </div>
            {choices.posters?.length ? (
              <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
                {choices.posters.map((variant) => {
                  const pinned = art.pinnedPoster === variant.path;
                  return (
                    <div key={variant.path} className="space-y-2">
                      <button
                        type="button"
                        aria-pressed={pinned}
                        aria-label={
                          variant.language
                            ? t("Pin poster ({language})", { language: variant.language })
                            : t("Pin poster (textless)")
                        }
                        className={cn(
                          "relative block w-full overflow-hidden rounded-lg border-2",
                          pinned ? "border-primary" : "border-transparent",
                        )}
                        onClick={() => setArt({ ...art, pinnedPoster: pinned ? "" : variant.path })}
                      >
                        <Artwork
                          src={`https://image.tmdb.org/t/p/w185${variant.path}`}
                          alt={
                            variant.language
                              ? t("Poster ({language})", { language: variant.language })
                              : t("Poster (textless)")
                          }
                          className="aspect-[2/3]"
                        />
                        {pinned ? (
                          <span className="absolute left-1.5 top-1.5 inline-flex items-center gap-1 rounded bg-primary px-1.5 py-0.5 text-[11px] font-medium text-primary-fg">
                            <Pin size={11} /> {t("Pinned")}
                          </span>
                        ) : null}
                      </button>
                      <ListInput
                        ariaLabel={t("Focus tags for {path}", { path: variant.path })}
                        placeholder={t("Drama, actor name")}
                        value={art.posters?.find((p) => p.path === variant.path)?.tags ?? []}
                        onChange={(tags) =>
                          setArt((current) => ({
                            ...current,
                            posters:
                              current.posters?.map((p) =>
                                p.path === variant.path ? { ...p, tags: tags.map(String) } : p,
                              ) ?? [],
                          }))
                        }
                      />
                    </div>
                  );
                })}
              </div>
            ) : (
              <p className="flex items-center gap-2 text-sm text-fg-muted">
                <ImageIcon size={14} /> {t("TMDB has no alternate posters for this title.")}
              </p>
            )}
          </section>
          <section className="space-y-2">
            <label htmlFor="pinned-backdrop" className="block text-sm font-semibold text-fg">
              {t("Backdrop")}
            </label>
            <div className="metadata-backdrops">
              {choices.backdrops?.map((v, index) => (
                <button
                  type="button"
                  key={v.path}
                  aria-label={
                    v.language
                      ? t("Pin backdrop {number} ({language})", {
                          number: index + 1,
                          language: v.language,
                        })
                      : t("Pin backdrop {number} (textless)", { number: index + 1 })
                  }
                  aria-pressed={art.pinnedBackdrop === v.path}
                  onClick={() =>
                    setArt({ ...art, pinnedBackdrop: art.pinnedBackdrop === v.path ? "" : v.path })
                  }
                  className={cn(
                    "overflow-hidden rounded-lg border-2",
                    art.pinnedBackdrop === v.path ? "border-primary" : "border-transparent",
                  )}
                >
                  <Artwork
                    src={`https://image.tmdb.org/t/p/w300${v.path}`}
                    alt={t("Backdrop {number}", { number: index + 1 })}
                    className="aspect-video"
                  />
                </button>
              ))}
            </div>
            <select
              id="pinned-backdrop"
              className={nativeSelect}
              value={art.pinnedBackdrop || ""}
              onChange={(e) => setArt({ ...art, pinnedBackdrop: e.currentTarget.value })}
            >
              <option value="">{t("Automatic textless backdrop")}</option>
              {choices.backdrops?.map((v) => (
                <option key={v.path} value={v.path}>
                  {t("{language} · {path}", {
                    language: v.language || t("Textless"),
                    path: v.path,
                  })}
                </option>
              ))}
            </select>
          </section>
        </div>
      )}
    </Drawer>
  );
}

type plexSettings = { claimed?: boolean; usePlexMetadata?: boolean };

function PlexMetadataToggle() {
  const doc = useAsync(async () => (await api.getConfig()).plex as plexSettings | undefined, []);
  const [saving, setSaving] = useState(false);
  const claimed = Boolean(doc.data?.claimed);
  const on = Boolean(doc.data?.usePlexMetadata);
  return (
    <div className="metadata-enable">
      <div>
        <label htmlFor="plex-metadata">{t("Use Plex's metadata for Plex apps")}</label>
        <p>
          {claimed
            ? t("Plex apps use Plex's details when a title is matched. Other apps stay on TMDB.")
            : t("Claim this server to turn this on.")}
        </p>
      </div>
      <Switch
        id="plex-metadata"
        aria-label={t("Use Plex's metadata for Plex apps")}
        checked={on}
        disabled={!claimed || saving || !doc.data}
        onChange={(e) => {
          const next = e.currentTarget.checked;
          setSaving(true);
          void api
            .putSection("plex", { usePlexMetadata: next })
            .then(() => doc.setData({ ...(doc.data ?? {}), usePlexMetadata: next }))
            .catch((err) => toast.error(t("Couldn't save"), { description: errorMessage(err) }))
            .finally(() => setSaving(false));
        }}
      />
    </div>
  );
}
