import { useAsync, useInterval } from "../../lib/hooks";
import { localized, t } from "../../i18n";
import { apiFetch } from "../../lib/api";
import { formatNumber } from "../../lib/format";
import type { FieldGroup } from "../settings/SchemaForm";

export const HomeReleaseSettings: FieldGroup = localized(() => ({
  title: t("Movie release window"),
  fields: [
    {
      key: "gateMoviesByHomeRelease",
      label: t("Wait for digital or physical release"),
      kind: "bool",
      help: t(
        "Movies appear after the earliest digital or physical release anywhere in the world. If TMDB has no such date, they appear 90 days after theatrical release and remain visible if dates change. Movies awaiting metadata stay hidden while the catalog checks them in the background. This does not verify release quality.",
      ),
    },
  ],
}));

export const UpcomingEpisodeSettings: FieldGroup = localized(() => ({
  title: t("Upcoming episodes"),
  fields: [
    {
      key: "showUpcomingEpisodes",
      label: t("Show scheduled episodes"),
      kind: "bool",
      help: t(
        "Show unaired episodes with known air dates inside already airing seasons. They are marked as unaired and cannot be played. Alternate episode orders keep their existing lists.",
      ),
    },
  ],
}));

export function HomeReleaseStatus() {
  const data = useAsync(
    () =>
      apiFetch<{ value: { homeReleaseChecked: number; homeReleasePending: number } }>(
        "/status/catalog",
      ),
    [],
  );
  useInterval(() => void data.reload(), 60000);
  const status = data.data?.value;
  if (!status) return null;
  return (
    <section
      className="rounded-xl border border-border bg-surface p-5"
      aria-label={t("Movie release metadata")}
    >
      <h3 className="text-sm font-semibold">{t("Movie release metadata")}</h3>
      <p className="mt-2 text-xs text-fg-muted">
        {t("{checked} checked · {pending} awaiting details", {
          checked: formatNumber(status.homeReleaseChecked ?? 0),
          pending: formatNumber(status.homeReleasePending ?? 0),
        })}
      </p>
      <p className="mt-2 text-xs text-fg-muted">
        {t(
          "When enabled, checks run in bounded batches in the background. Missing and future dates are rechecked.",
        )}
      </p>
    </section>
  );
}
