import { Badge, Button, Input, Switch, toast } from "@braedonsaunders/appkit-ui";
import { SlidersHorizontal } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";
import { Drawer } from "../../components/Drawer";
import { Field } from "../../components/fields";
import { secretConfigured } from "../../components/IntegrationAccount";
import { MutationError } from "../../components/feedback";
import { localized, t, tx } from "../../i18n";
import { api, apiFetch } from "../../lib/api";
import { useAsync } from "../../lib/hooks";
import { nativeSelect } from "./shared";

import type { PosterProviderConfig as Posters } from "../../lib/types";
const empty: Posters = {
  enabled: false,
  source: "postersplus",
  baseUrl: "",
  apiKey: "",
  urlTemplate: "",
  profile: "",
};
const sources = localized(() => [
  { value: "postersplus", label: "PostersPlus" },
  { value: "xrdb", label: "XRDB" },
  { value: "betterposters", label: "BetterPosters" },
  { value: "rpdb", label: "Rating Poster Database" },
  { value: "custom", label: t("Custom URL template") },
]);
const normalize = (value?: Posters): Posters => ({
  ...empty,
  ...value,
  source: value?.source || "postersplus",
});

export function PosterProviders({ onSaved }: { onSaved: () => void }) {
  const config = useAsync(async () => normalize((await api.getConfig()).posters), []);
  const [draft, setDraft] = useState<Posters | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [saving, setSaving] = useState(false);
  const save = async () => {
    if (!draft || saving) return;
    setSaving(true);
    setError(null);
    try {
      const result = await apiFetch<{ value: Posters }>("/config/posters", {
        method: "PUT",
        body: JSON.stringify(draft),
      });
      config.setData(normalize(result.value));
      setDraft(null);
      onSaved();
      toast.success(t("Poster provider saved"));
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  };
  return (
    <>
      <section className="metadata-control-section" aria-label={t("Poster provider")}>
        <div className="metadata-section-title">
          <h3>{t("Poster provider")}</h3>
          <Button
            size="sm"
            variant="ghost"
            aria-label={t("Configure poster provider")}
            disabled={!config.data}
            onClick={() => {
              setError(null);
              setDraft({ ...config.data! });
            }}
          >
            <SlidersHorizontal size={13} /> {t("Configure")}
          </Button>
        </div>
        {config.error ? (
          <div className="space-y-2">
            <MutationError error={config.error} />
            <Button size="sm" variant="outline" onClick={() => void config.reload()}>
              {t("Retry")}
            </Button>
          </div>
        ) : (
          <>
            <div className="flex items-center justify-between gap-2 text-xs">
              <span>
                {config.loading
                  ? t("Loading provider…")
                  : config.data?.enabled
                    ? sources.find((s) => s.value === config.data?.source)?.label ||
                      t("Custom provider")
                    : t("TMDB artwork")}
              </span>
              {config.data?.enabled && <Badge variant="secondary">{t("Enabled")}</Badge>}
            </div>
            <p className="mt-1 text-[11px] text-fg-muted">
              {config.data?.enabled
                ? t("Movie and show posters, with TMDB as a fallback.")
                : t("Add a provider for movie and show posters.")}
            </p>
            <PosterAccount posters={config.data} />
          </>
        )}
      </section>
      <Drawer
        open={draft !== null}
        onClose={() => {
          if (!saving) setDraft(null);
        }}
        title={t("Poster provider")}
        description={t("Choose the artwork underneath your Ether poster design.")}
        size="lg"
        footer={
          <div className="flex w-full justify-end gap-2">
            <Button variant="ghost" size="sm" disabled={saving} onClick={() => setDraft(null)}>
              {t("Cancel")}
            </Button>
            <Button size="sm" disabled={saving} onClick={() => void save()}>
              {saving ? t("Saving…") : t("Save poster provider")}
            </Button>
          </div>
        }
      >
        {draft && (
          <div className="space-y-6">
            <MutationError error={error} />
            <fieldset className="space-y-5" disabled={saving}>
              <div className="flex items-center justify-between gap-4 rounded-lg border border-border p-4">
                <div>
                  <label htmlFor="poster-provider-enabled" className="text-sm font-medium">
                    {t("Use poster provider")}
                  </label>
                  <p className="mt-1 text-xs text-fg-muted">
                    {t("When off, posters come from TMDB.")}
                  </p>
                </div>
                <Switch
                  id="poster-provider-enabled"
                  aria-label={t("Enable poster provider")}
                  checked={draft.enabled}
                  onChange={(e) => setDraft({ ...draft, enabled: e.currentTarget.checked })}
                />
              </div>
              <Field label={t("Source")} htmlFor="poster-provider-source">
                <select
                  id="poster-provider-source"
                  className={nativeSelect}
                  value={draft.source}
                  onChange={(e) => setDraft({ ...draft, source: e.target.value })}
                >
                  {sources.map((s) => (
                    <option key={s.value} value={s.value}>
                      {s.label}
                    </option>
                  ))}
                </select>
              </Field>
              {(draft.source === "postersplus" || draft.source === "xrdb") && (
                <Field
                  label={t("Server URL")}
                  htmlFor="poster-provider-url"
                  hint={t("Your server may be on your LAN.")}
                >
                  <Input
                    id="poster-provider-url"
                    placeholder={
                      draft.source === "xrdb"
                        ? "https://extendedratings.com"
                        : "http://postersplus:8000"
                    }
                    value={draft.baseUrl}
                    onChange={(e) => setDraft({ ...draft, baseUrl: e.target.value })}
                  />
                </Field>
              )}
              {draft.source === "xrdb" && (
                <Field label="Profile ID or alias (optional)" htmlFor="poster-provider-profile">
                  <Input
                    id="poster-provider-profile"
                    value={draft.profile || ""}
                    onChange={(e) => setDraft({ ...draft, profile: e.target.value })}
                  />
                </Field>
              )}
              <PosterAccount posters={draft} />
            </fieldset>
            <div className="space-y-2 rounded-lg bg-bg-subtle p-4 text-xs leading-relaxed text-fg-muted">
              <p>
                {t("Missing posters fall back to TMDB. Seasons and episodes keep TMDB artwork.")}
              </p>
              <p>
                {t(
                  "Ether badges appear on top. Turn off any badges that are already included by your provider.",
                )}
              </p>
              <p>
                {t(
                  "Saving updates this preview. Plex refreshes its own cached posters separately.",
                )}
              </p>
            </div>
          </div>
        )}
      </Drawer>
    </>
  );
}

function PosterAccount({ posters }: { posters?: Posters | null }) {
  const connected = secretConfigured(posters?.apiKey) || secretConfigured(posters?.urlTemplate);
  if (connected) {
    return (
      <p className="text-sm">
        <Link to="/services" className="text-primary underline">
          {t("Poster service connected")}
        </Link>
      </p>
    );
  }
  return (
    <p className="text-sm text-fg-muted">
      {tx("Connect your poster service in {link}.", {
        link: (
          <Link to="/services" className="text-primary underline">
            {t("Integrations")}
          </Link>
        ),
      })}
    </p>
  );
}
