import { Badge, EmptyState, Input, PagedTable, Select } from "@braedonsaunders/appkit-ui";
import { History as HistoryIcon } from "lucide-react";
import { useId } from "react";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { Field } from "../../components/fields";
import { Section } from "../../components/layout";
import { fixed } from "../../lib/format";
import { getLocale, t, tn } from "../../i18n";
import { pagedTableLabels } from "../../i18n/appkit-labels";
import type { EtherUser } from "../../lib/types";
import { viewerLabel, type Play, type ViewingFilter } from "./types";

/** Period, viewer and year-in-review filters above History and Stats. */
export function ViewingFilters({
  value,
  onChange,
  viewers,
}: {
  value: ViewingFilter;
  onChange: (next: ViewingFilter) => void;
  viewers: EtherUser[];
}) {
  const id = useId();
  return (
    <div className="flex flex-wrap items-end gap-3" role="group" aria-label={t("Filters")}>
      <Field label={t("Period")} htmlFor={`${id}-days`} className="w-44">
        <Select
          id={`${id}-days`}
          aria-label={t("Period")}
          value={value.days}
          onChange={(e) => onChange({ ...value, days: e.target.value, year: "" })}
        >
          {[7, 30, 90, 365].map((n) => (
            <option key={n} value={String(n)}>
              {tn(n, "Last {count} day", "Last {count} days")}
            </option>
          ))}
        </Select>
      </Field>
      <Field label={t("Viewer")} htmlFor={`${id}-user`} className="w-52">
        <Select
          id={`${id}-user`}
          aria-label={t("Viewer")}
          value={value.user}
          onChange={(e) => onChange({ ...value, user: e.target.value })}
        >
          <option value="">{t("Everyone")}</option>
          {viewers.map((v) => (
            <option key={v.id} value={String(v.id)}>
              {viewerLabel(v)}
            </option>
          ))}
        </Select>
      </Field>
      <Field label={t("Year in review")} htmlFor={`${id}-year`} className="w-32">
        <Input
          id={`${id}-year`}
          type="number"
          min={2000}
          max={new Date().getFullYear()}
          placeholder={t("Year")}
          value={value.year}
          onChange={(e) => onChange({ ...value, year: e.target.value })}
        />
      </Field>
    </div>
  );
}

function statusCell(p: Play) {
  return (
    <div className="space-y-1">
      <div className="flex flex-wrap gap-1">
        <Badge variant={p.completed ? "success" : "secondary"}>
          {p.completed ? t("Completed") : p.ended ? t("Ended") : t("Last observed")}
        </Badge>
        {p.failovers ? (
          <Badge variant="warning">
            {tn(p.failovers, "{count} failover", "{count} failovers")}
          </Badge>
        ) : null}
      </div>
      {p.error ? <p className="text-xs text-danger">{p.error}</p> : null}
    </div>
  );
}

/** Recorded plays for the selected period and viewer. */
export function PlayHistory({
  history,
  error,
  onRetry,
}: {
  history: Play[] | undefined;
  error: unknown;
  onRetry: () => void;
}) {
  if (error && !history) return <ErrorPanel error={error} onRetry={onRetry} />;
  return (
    <Section
      title={t("Play history")}
      description={t("The latest 500 plays in this period, in your device's time zone.")}
    >
      {!history ? (
        <LoadingRows rows={4} />
      ) : history.length ? (
        <PagedTable
          rows={history}
          rowKey={(p) => p.id}
          pageSize={20}
          empty={t("No recorded plays in this period.")}
          defaultSort={{ key: "started", dir: "desc" }}
          labels={pagedTableLabels({
            searchLabel: t("Search play history"),
            searchPlaceholder: t("Find a title, viewer or device…"),
          })}
          columns={[
            {
              key: "title",
              header: t("Title / viewer"),
              search: (p) => p.title + " " + p.username,
              sortValue: (p) => p.title,
              cell: (p) => (
                <>
                  <p className="font-medium text-fg">{p.title}</p>
                  <p className="text-xs text-fg-muted">{p.username || t("Unknown viewer")}</p>
                </>
              ),
            },
            {
              key: "started",
              header: t("Started"),
              sortValue: (p) => p.started,
              cell: (p) => new Date(p.started * 1000).toLocaleString(getLocale()),
            },
            {
              key: "device",
              header: t("Device"),
              search: (p) => (p.device || p.platform) + " " + p.decision,
              cell: (p) => (
                <>
                  {p.device || p.platform || t("Unknown")}
                  <span className="block text-xs text-fg-muted">{p.decision}</span>
                </>
              ),
            },
            {
              key: "duration",
              header: t("Viewing time"),
              align: "right",
              sortValue: (p) => p.watchedMs,
              cell: (p) => t("{minutes} min", { minutes: fixed(p.watchedMs / 60000, 0) }),
            },
            { key: "status", header: t("Status"), cell: statusCell },
          ]}
        />
      ) : (
        <EmptyState
          icon={<HistoryIcon />}
          title={t("No plays yet")}
          description={t(
            "History starts when Ether observes playback. There are no recorded plays in this period.",
          )}
        />
      )}
    </Section>
  );
}
