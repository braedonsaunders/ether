import { Button } from "@braedonsaunders/appkit-ui";
import { useState } from "react";
import { ErrorPanel } from "../../components/feedback";
import { Section } from "../../components/layout";
import { TitleSearch } from "../../components/pickers";
import { t } from "../../i18n";
import { apiFetch } from "../../lib/api";

type Coverage = {
  showId: number;
  cachedEpisodes: number;
  introEpisodes: number;
  creditsEpisodes: number;
  recapEpisodes: number;
};

/** How many of a show's episodes have intro, credits and recap markers. */
export function MarkerCoverage() {
  const [show, setShow] = useState("");
  const [showName, setShowName] = useState("");
  const [coverage, setCoverage] = useState<Coverage | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<unknown>(null);

  const check = async () => {
    setCoverage(null);
    setError(null);
    setLoading(true);
    try {
      setCoverage(await apiFetch<Coverage>(`/markers/shows/${encodeURIComponent(show)}`));
    } catch (e) {
      setError(e);
    } finally {
      setLoading(false);
    }
  };

  const stats = coverage
    ? [
        { key: "cached", label: t("Cached episodes"), value: coverage.cachedEpisodes },
        { key: "intros", label: t("Intros"), value: coverage.introEpisodes },
        { key: "credits", label: t("Credits"), value: coverage.creditsEpisodes },
        { key: "recaps", label: t("Recaps"), value: coverage.recapEpisodes },
      ]
    : [];

  return (
    <Section
      title={t("Intro & credits coverage")}
      description={t(
        "Episodes already stored on this server. The times come from IntroDB, chapter titles, and subtitle dialogue.",
      )}
    >
      <div className="flex flex-wrap items-start gap-3">
        <div className="min-w-64 flex-1">
          <TitleSearch
            type="show"
            placeholder={t("Find a show…")}
            onPick={(title) => {
              setShow(String(Number(title.key) % 1000000000000000));
              setShowName(title.title);
              setCoverage(null);
            }}
          />
        </div>
        <Button
          variant="outline"
          disabled={!/^\d+$/.test(show) || loading}
          onClick={() => void check()}
        >
          {loading ? t("Checking…") : t("Check coverage")}
        </Button>
      </div>
      {showName ? <p className="text-sm font-medium text-fg">{showName}</p> : null}
      <ErrorPanel error={error} />
      {coverage ? (
        <dl className="grid grid-cols-2 gap-3 sm:grid-cols-4" aria-label={t("Marker coverage")}>
          {stats.map((s) => (
            <div key={s.key} className="rounded-lg border border-border bg-bg-subtle px-3 py-2">
              <dt className="text-xs text-fg-muted">{s.label}</dt>
              <dd className="text-lg font-semibold tabular-nums text-fg">{s.value}</dd>
            </div>
          ))}
        </dl>
      ) : null}
    </Section>
  );
}
