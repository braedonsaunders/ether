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
      help: t("Movies stay hidden until a digital or physical release date."),
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
      help: t("List upcoming episodes in seasons that are already airing."),
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
    </section>
  );
}
