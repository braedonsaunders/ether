import { Badge } from "@braedonsaunders/appkit-ui";
import { TrendingUp } from "lucide-react";
import { t, tn } from "../../i18n";
import { apiFetch } from "../../lib/api";
import { useAsync, useInterval } from "../../lib/hooks";
import { relativeTime } from "../../lib/format";

type Chart = {
  mediaType: string;
  window: string;
  titles: number;
  fetchedAt?: string;
  state: string;
};
export function TrendingStatus() {
  const data = useAsync(() => apiFetch<{ value: { trending?: Chart[] } }>("/status/catalog"), []);
  useInterval(() => void data.reload(), 60000);
  return (
    <section
      className="rounded-xl border border-border bg-surface p-5"
      aria-label={t("Global trending status")}
    >
      <h3 className="flex items-center gap-2 text-sm font-semibold">
        <TrendingUp size={18} />
        {t("Global trending")}
      </h3>
      {data.error ? (
        <p className="mt-3 text-sm text-danger">{t("Chart status unavailable")}</p>
      ) : null}
      <div className="mt-4 divide-y divide-border">
        {(data.data?.value.trending ?? []).map((c) => (
          <div className="py-3" key={c.mediaType + c.window}>
            <div className="flex items-center justify-between gap-2 text-sm">
              <span>
                {c.mediaType === "movie" ? t("Movies") : t("TV shows")} ·{" "}
                {c.window === "day" ? t("Today") : t("This week")}
              </span>
              <Badge variant="secondary">{c.state}</Badge>
            </div>
            <p className="mt-1 text-xs text-fg-muted">
              {tn(c.titles, "{count} title · updated {when}", "{count} titles · updated {when}", {
                when: relativeTime(c.fetchedAt),
              })}
            </p>
          </div>
        ))}
      </div>
    </section>
  );
}
