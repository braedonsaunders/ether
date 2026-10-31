import { Badge, Button, SettingsSection, Spinner, Switch, toast } from "@braedonsaunders/appkit-ui";
import { RefreshCw, X } from "lucide-react";
import { useCallback, useMemo, useRef, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { Artwork } from "../../components/Artwork";
import { secretConfigured } from "../../components/IntegrationAccount";
import { ErrorPanel } from "../../components/feedback";
import { NumberInput, PasswordInput } from "../../components/fields";
import { Section } from "../../components/layout";
import { TitleSearch } from "../../components/pickers";
import { titleSubtitle } from "../../components/Poster";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { SettingsRow } from "../../components/SettingsRow";
import { localized, t, tn, tx } from "../../i18n";
import { apiFetch, errorMessage } from "../../lib/api";
import type { TitleCard } from "../../lib/contract";
import { useAsync } from "../../lib/hooks";
import { nativeSelect } from "./shared";
import { MetadataWorkspace } from "./MetadataWorkspace";

export type RatingSettings = {
  enabled: boolean;
  provider: string;
  apiKey: string;
  critic: string;
  audience: string;
  cacheHours: number;
};
type RatingBadge = { source: string; value: number; scale: number };

/** The server keeps the key and sends this placeholder back. */
const STORED_KEY = "[configured]";

const SCORE_CHOICES = localized(() => [
  { value: "", label: t("Keep catalogue default") },
  { value: "imdb", label: "IMDb" },
  { value: "tomatoes", label: t("Rotten Tomatoes critics") },
  { value: "popcorn", label: t("Rotten Tomatoes audience (MDBList)") },
]);

/**
 * Field-level draft. A saved document adopts only fields the user has not
 * changed, so a card toggle can update `enabled` without wiping an unsaved
 * cache interval. `adopt` takes a server response wholesale after our own save.
 */
function useFieldDraft<T extends Record<string, unknown>>(saved: T | null) {
  const savedKey = useMemo(() => JSON.stringify(saved ?? null), [saved]);
  const [adopted, setAdopted] = useState(savedKey);
  const [base, setBase] = useState(savedKey);
  const [draft, setDraft] = useState<T | null>(() => (saved ? structuredClone(saved) : null));
  if (adopted !== savedKey) {
    let next: T | null;
    if (!saved) next = null;
    else if (!draft || base === "null" || JSON.stringify(draft) === base)
      next = structuredClone(saved);
    else {
      const previous = JSON.parse(base) as T;
      next = { ...draft };
      for (const key of Object.keys(saved) as (keyof T)[]) {
        if (JSON.stringify(draft[key]) === JSON.stringify(previous[key])) next[key] = saved[key];
      }
    }
    setAdopted(savedKey);
    setBase(savedKey);
    setDraft(next);
  }
  const dirty = draft !== null && JSON.stringify(draft) !== savedKey;
  const reset = useCallback(() => {
    setDraft(saved ? structuredClone(saved) : null);
    setBase(savedKey);
    // savedKey stands in for the saved snapshot.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [savedKey]);
  const adopt = useCallback((next: T) => {
    const key = JSON.stringify(next);
    setAdopted(key);
    setBase(key);
    setDraft(structuredClone(next));
  }, []);
  return { draft, setDraft, dirty, reset, adopt };
}

/** OMDb / MDBList rating sources, edited in place, with a live badge check beside them. */
export function RatingSources({
  saved,
  onPersisted,
  persist,
  disabled,
  accountOnIntegrations,
}: {
  /** Parent-owned saved document. Omit on the metadata page, which loads its own. */
  saved?: RatingSettings | null;
  /** Called with the server's saved document after this form's own successful save. */
  onPersisted?: (next: RatingSettings) => void;
  /** Serialize this save with other integration writes. */
  persist?: (task: () => Promise<void>) => Promise<unknown>;
  /** Disable the shared save bar while another integration write is in flight. */
  disabled?: boolean;
  /** Metadata keeps the score choices. The API key is edited on Integrations. */
  accountOnIntegrations?: boolean;
} = {}) {
  const controlled = saved !== undefined;
  const doc = useAsync(() => apiFetch<RatingSettings>("/discovery/ratings/settings"), []);
  const external = useFieldDraft(controlled ? (saved ?? null) : null);
  const internal = useDraft(controlled ? undefined : doc.data);
  const draft = controlled ? external.draft : internal.draft;
  const setDraft = controlled ? external.setDraft : internal.setDraft;
  const dirty = controlled ? external.dirty : internal.dirty;
  const reset = controlled ? external.reset : internal.reset;
  const draftRef = useRef(draft);
  draftRef.current = draft;
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);

  // The metadata page shares its workspace with Artwork and Episode order.
  // A bare spinner here unmounts that chrome and the tab switch flickers.
  const hold = (node: ReactNode) =>
    accountOnIntegrations ? <MetadataWorkspace aside={node} main={null} /> : node;
  if (!controlled && doc.error)
    return hold(<ErrorPanel error={doc.error} onRetry={() => void doc.reload()} />);
  if (controlled && saved == null && doc.error)
    return hold(<ErrorPanel error={doc.error} onRetry={() => void doc.reload()} />);
  if (!draft) return hold(<Spinner size={20} label={t("Loading rating settings")} />);

  const set = (patch: Partial<RatingSettings>) => setDraft({ ...draft, ...patch });
  const save = async () => {
    const write = async () => {
      const body = draftRef.current;
      if (!body) return;
      setSaving(true);
      setError(null);
      try {
        const next = await apiFetch<RatingSettings>("/discovery/ratings/settings", {
          method: "PUT",
          body: JSON.stringify(body),
        });
        if (controlled) {
          external.adopt(next);
          onPersisted?.(next);
        } else {
          doc.setData(next);
        }
        toast.success(t("Rating sources saved"));
      } catch (e) {
        setError(e);
      } finally {
        setSaving(false);
      }
    };
    if (persist) await persist(write);
    else await write();
  };

  return (
    <>
      <MetadataWorkspace
        aside={
          <>
            <SettingsSection
              title={t("Rating sources")}
              description={t(
                "Show IMDb and Rotten Tomatoes scores in Plex using your own OMDb or MDBList key. Scores a source doesn't have stay blank. The source's US content rating (IMDb's, such as TV-G) also rates titles TMDB leaves unrated.",
              )}
            >
              <SettingsRow
                stacked
                title={<label htmlFor="ratings-enabled">{t("External ratings")}</label>}
                description={t("Off: Plex shows the catalogue's own rating.")}
                control={
                  <Switch
                    id="ratings-enabled"
                    aria-label={t("External ratings")}
                    checked={draft.enabled}
                    onChange={(e) => set({ enabled: e.currentTarget.checked })}
                  />
                }
              />
              <SettingsRow
                stacked
                title={<label htmlFor="ratings-provider">{t("Provider")}</label>}
                description={t(
                  "OMDb has IMDb, Rotten Tomatoes critics and Metacritic. MDBList adds audience and Letterboxd scores.",
                )}
                control={
                  <select
                    id="ratings-provider"
                    className={`${nativeSelect} sm:w-48`}
                    value={draft.provider}
                    onChange={(e) => set({ provider: e.currentTarget.value })}
                  >
                    <option value="omdb">OMDb</option>
                    <option value="mdblist">MDBList</option>
                  </select>
                }
              />
              {accountOnIntegrations ? (
                <SettingsRow
                  stacked
                  title={t("Account")}
                  description={
                    secretConfigured(draft.apiKey) ? (
                      <Link to="/services/ratings" className="text-primary underline">
                        {t("OMDb and MDBList connected")}
                      </Link>
                    ) : (
                      tx("Connect OMDb or MDBList in {link}.", {
                        link: (
                          <Link to="/services/ratings" className="text-primary underline">
                            {t("Integrations")}
                          </Link>
                        ),
                      })
                    )
                  }
                />
              ) : (
                <SettingsRow
                  stacked
                  title={<label htmlFor="ratings-key">{t("API key")}</label>}
                  description={
                    draft.apiKey === STORED_KEY
                      ? t("Saved. Leave as is to keep it, or type a new key to replace it.")
                      : t("Your own key from the provider's website.")
                  }
                  control={
                    <PasswordInput
                      id="ratings-key"
                      autoComplete="new-password"
                      spellCheck={false}
                      className="sm:w-64"
                      value={draft.apiKey}
                      onFocus={(e) => {
                        if (draft.apiKey === STORED_KEY) e.currentTarget.select();
                      }}
                      onChange={(e) => set({ apiKey: e.currentTarget.value })}
                    />
                  }
                />
              )}
              {(
                [
                  ["critic", t("Plex rating"), t("The main score on a title's page.")],
                  ["audience", t("Plex audience rating"), t("The second score beside it.")],
                ] as const
              ).map(([key, label, hint]) => (
                <SettingsRow
                  stacked
                  key={key}
                  title={<label htmlFor={`ratings-${key}`}>{label}</label>}
                  description={hint}
                  control={
                    <select
                      id={`ratings-${key}`}
                      className={`${nativeSelect} sm:w-64`}
                      value={draft[key]}
                      onChange={(e) => set({ [key]: e.currentTarget.value })}
                    >
                      {SCORE_CHOICES.map((c) => (
                        <option key={c.value} value={c.value}>
                          {c.label}
                        </option>
                      ))}
                    </select>
                  }
                />
              ))}
              <SettingsRow
                stacked
                title={<label htmlFor="ratings-cache">{t("Keep scores for")}</label>}
                description={t("Hours before a title's scores are looked up again.")}
                control={
                  <span className="flex items-center gap-2">
                    <NumberInput
                      id="ratings-cache"
                      className="w-24"
                      min={1}
                      max={2160}
                      value={draft.cacheHours}
                      onChange={(n) => set({ cacheHours: n })}
                    />
                    <span className="text-sm text-fg-muted">
                      {tn(draft.cacheHours, "hour", "hours")}
                    </span>
                  </span>
                }
              />
            </SettingsSection>
          </>
        }
        main={
          <div className="space-y-4">
            <BadgePreview
              enabled={Boolean((controlled ? saved : doc.data)?.enabled)}
              dirty={dirty}
            />
            <Section
              title={t("Plex rating fields")}
              description={
                dirty
                  ? t("Draft selection. Save to apply to Plex.")
                  : t("Current score mapping for title pages.")
              }
            >
              <dl className="metadata-detail-list">
                {[
                  [t("External ratings"), draft.enabled ? t("Enabled") : t("Disabled")],
                  [t("Provider"), draft.provider === "mdblist" ? "MDBList" : "OMDb"],
                  [
                    t("Plex rating"),
                    SCORE_CHOICES.find((c) => c.value === draft.critic)?.label ||
                      t("Catalogue default"),
                  ],
                  [
                    t("Audience rating"),
                    SCORE_CHOICES.find((c) => c.value === draft.audience)?.label ||
                      t("Catalogue default"),
                  ],
                  [t("Cache lifetime"), tn(draft.cacheHours, "{count} hour", "{count} hours")],
                ].map(([name, value]) => (
                  <div key={name}>
                    <dt>{name}</dt>
                    <dd>{value}</dd>
                  </div>
                ))}
              </dl>
            </Section>
            <Section
              title={t("Available scores")}
              description={t(
                "Availability varies by title and provider. Missing scores stay blank.",
              )}
            >
              <table className="metadata-table">
                <thead>
                  <tr>
                    <th>{t("Score")}</th>
                    <th>OMDb</th>
                    <th>MDBList</th>
                  </tr>
                </thead>
                <tbody>
                  {[
                    ["IMDb", t("Yes"), t("Yes")],
                    [t("Rotten Tomatoes critics"), t("Yes"), t("Yes")],
                    [t("Rotten Tomatoes audience"), "—", t("Yes")],
                    ["Metacritic", t("Yes"), t("Yes")],
                    ["Letterboxd", "—", t("Yes")],
                  ].map((row) => (
                    <tr key={row[0]}>
                      {row.map((cell, i) => (
                        <td key={i}>{cell}</td>
                      ))}
                    </tr>
                  ))}
                </tbody>
              </table>
            </Section>
          </div>
        }
      />

      <SaveBar
        dirty={dirty}
        saving={saving}
        disabled={disabled}
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

/** Look up one title's badges with the saved sources. */
function BadgePreview({ enabled, dirty }: { enabled: boolean; dirty: boolean }) {
  const [title, setTitle] = useState<TitleCard | null>(null);
  const [badges, setBadges] = useState<RatingBadge[] | null>(null);
  const [busy, setBusy] = useState(false);
  const refresh = async (card: TitleCard) => {
    setBusy(true);
    try {
      setBadges(
        await apiFetch<RatingBadge[]>(`/discovery/ratings/refresh/${card.key}`, {
          method: "POST",
          body: "{}",
        }),
      );
    } catch (e) {
      toast.error(t("Couldn't look up ratings"), { description: errorMessage(e) });
    } finally {
      setBusy(false);
    }
  };
  return (
    <Section
      title={t("Title badges")}
      description={
        enabled
          ? t("Check the scores Plex will show for a title.")
          : t("Turn on external ratings and save to check a title's scores.")
      }
    >
      {title ? (
        <div className="space-y-3">
          <div className="flex items-center gap-3">
            <Artwork src={title.poster} alt="" title="" className="h-16 w-11 shrink-0 rounded" />
            <div className="min-w-0 flex-1">
              <p className="truncate text-sm font-medium text-fg">{title.title}</p>
              <p className="truncate text-xs text-fg-muted">{titleSubtitle(title)}</p>
            </div>
            <Button
              variant="ghost"
              size="icon"
              className="size-8"
              aria-label={t("Choose another title")}
              onClick={() => {
                setTitle(null);
                setBadges(null);
              }}
            >
              <X size={14} />
            </Button>
          </div>
          <Button
            variant="outline"
            size="sm"
            disabled={busy || !enabled}
            onClick={() => void refresh(title)}
          >
            <RefreshCw size={14} /> {busy ? t("Looking up…") : t("Refresh ratings")}
          </Button>
          {dirty ? (
            <p className="text-xs text-fg-muted">
              {t("Uses your saved sources, not unsaved changes.")}
            </p>
          ) : null}
          {badges ? (
            badges.length ? (
              <ul className="flex flex-wrap gap-1.5" aria-label={t("Rating badges")}>
                {badges.map((b) => (
                  <li key={b.source}>
                    <Badge variant="outline">
                      {b.source}: {b.value}/{b.scale}
                    </Badge>
                  </li>
                ))}
              </ul>
            ) : (
              <p className="text-sm text-fg-muted">{t("No scores found for this title.")}</p>
            )
          ) : null}
        </div>
      ) : (
        <TitleSearch placeholder={t("Find a movie or show…")} onPick={setTitle} />
      )}
    </Section>
  );
}
