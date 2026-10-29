import {
  Alert,
  AlertDescription,
  Badge,
  Button,
  EmptyState,
  Spinner,
} from "@braedonsaunders/appkit-ui";
import { CalendarClock, Filter, Layers, RefreshCw, SearchX, Users } from "lucide-react";
import type { ReactNode } from "react";
import { Artwork } from "../../components/Artwork";
import { ErrorPanel, PosterSkeletons } from "../../components/feedback";
import { PosterGrid } from "../../components/Poster";
import { api } from "../../lib/api";
import type { CollectionSort, CollectionSpec, CollectionTemplate } from "../../lib/contract";
import { localized, t, tn } from "../../i18n";
import { relativeTime } from "../../lib/format";
import { useAsync } from "../../lib/hooks";
import {
  REFRESH_OPTIONS,
  describeRule,
  describeSource,
  describeTargets,
  previewableSpec,
} from "../../lib/rules";
import { ArtworkPreview, describeSchedule } from "./SettingsPanels";

/** Each sort as it reads mid-sentence ("Sorted by release date"). */
const SORT_PHRASE = localized<Record<CollectionSort, string>>(() => ({
  popularity: t("popularity"),
  rating: t("rating"),
  released: t("release date"),
  latestEpisode: t("latest aired episode"),
  latestSeasonPremiere: t("latest season premiere"),
  added: t("date added"),
  title: t("title"),
  personal: t("personal fit"),
  random: t("shuffle"),
  source: t("source order"),
}));

/** Read-only preview of a saved collection or a template, without opening an editor. */
export function CollectionDetail({
  spec,
  template,
  actions,
}: {
  spec: CollectionSpec;
  template?: CollectionTemplate;
  actions: ReactNode;
}) {
  const previewKey = JSON.stringify(previewableSpec(spec));
  const preview = useAsync(
    (signal) => api.previewCollection(previewableSpec(spec), signal),
    [previewKey, spec.lastBuiltAt],
  );
  const items = preview.data?.items ?? [];
  const targets = describeTargets(spec);
  const total = preview.data?.total;
  const refresh = REFRESH_OPTIONS.find((r) => r.value === spec.refresh)?.label ?? spec.refresh;
  const sort = SORT_PHRASE[spec.sort] ?? spec.sort;
  const sorted = spec.sortDesc
    ? t("Sorted by {sort} (descending)", { sort })
    : t("Sorted by {sort} (ascending)", { sort });
  const rotation = !spec.rotate
    ? ""
    : spec.rotate.every === "weekly"
      ? tn(spec.rotate.size, "Rotates {count} title weekly", "Rotates {count} titles weekly")
      : tn(spec.rotate.size, "Rotates {count} title daily", "Rotates {count} titles daily");
  return (
    <div className="min-w-0">
      <section
        className="relative isolate overflow-hidden border-b border-border bg-surface"
        aria-labelledby="collection-title"
      >
        {items[0] ? (
          <>
            <Artwork
              src={items[0].backdrop ?? items[0].poster}
              alt=""
              title=""
              icon={<span />}
              className="pointer-events-none absolute inset-0 -z-10 opacity-15"
              imgClassName="object-cover"
            />
            <div className="pointer-events-none absolute inset-0 -z-10 bg-gradient-to-t from-surface via-surface/80 to-surface/40" />
          </>
        ) : null}
        <div className="flex items-start gap-5 p-5 sm:gap-6 sm:p-6 xl:p-8">
          <div className="hidden w-28 shrink-0 sm:block xl:w-32">
            {!template && spec.id ? (
              <Artwork
                src={api.artworkUrl(
                  spec.id,
                  300,
                  [spec.updatedAt, spec.lastBuiltAt].filter(Boolean).join("-"),
                )}
                alt={t("{name} collection poster", { name: spec.name })}
                title={spec.name}
                className="aspect-[2/3] w-full rounded-md shadow-md ring-1 ring-border"
              />
            ) : (
              <ArtworkPreview spec={spec} items={items} />
            )}
          </div>
          <div className="min-w-0 flex-1 py-1">
            <div className="mb-3 flex flex-wrap items-center gap-2">
              <span className="text-[10px] font-semibold uppercase tracking-[0.14em] text-fg-muted">
                {template
                  ? t("{category} template", { category: template.category })
                  : spec.builtIn
                    ? t("Managed by Ether")
                    : t("Collection preview")}
              </span>
              {!template ? (
                <Badge variant={spec.enabled ? "success" : "secondary"}>
                  {spec.enabled ? t("Published") : t("Draft")}
                </Badge>
              ) : null}
            </div>
            <h2
              id="collection-title"
              className="text-2xl font-semibold leading-tight tracking-tight text-fg xl:text-3xl"
            >
              {spec.name}
            </h2>
            <p className="mt-2 max-w-xl text-sm leading-relaxed text-fg-muted">
              {spec.description ||
                t("A curated selection from your catalogue, ready to discover in Plex.")}
            </p>
            <div className="mt-3 flex flex-wrap items-center gap-2 text-xs text-fg-subtle">
              <span>
                {spec.mediaType === "show"
                  ? t("TV shows")
                  : spec.mediaType === "mixed"
                    ? t("Movies & TV shows")
                    : t("Movies")}
              </span>
              <span aria-hidden>·</span>
              <span>
                {spec.rules.length
                  ? tn(spec.rules.length, "{count} rule", "{count} rules")
                  : t("Whole source")}
              </span>
              {spec.limit > 0 ? (
                <>
                  <span aria-hidden>·</span>
                  <span>{tn(spec.limit, "Up to {count} title", "Up to {count} titles")}</span>
                </>
              ) : null}
            </div>
            <div className="mt-5 flex flex-wrap items-center gap-2">{actions}</div>
          </div>
        </div>
        <dl className="grid grid-cols-3 border-t border-border bg-surface/60 px-5 py-4 sm:px-6 xl:px-8">
          <Readout
            icon={<Layers size={13} />}
            label={t("Appears in")}
            value={targets.length ? targets.join(" · ") : t("Not placed yet")}
            detail={
              template
                ? t("Customise before publishing")
                : spec.enabled
                  ? t("Available in Plex")
                  : t("Publish to make it visible")
            }
          />
          <Readout
            icon={<Users size={13} />}
            label={t("Audience")}
            value={
              spec.targets.users === "all"
                ? t("Everyone")
                : tn(
                    spec.targets.users.length,
                    "{count} selected viewer",
                    "{count} selected viewers",
                  )
            }
            detail={spec.targets.schedule ? describeSchedule(spec) : t("No viewing schedule")}
          />
          <Readout
            icon={<CalendarClock size={13} />}
            label={t("Refresh")}
            value={refresh}
            detail={
              spec.lastBuiltAt
                ? t("Built {when}", { when: relativeTime(spec.lastBuiltAt) })
                : t("Not built yet")
            }
          />
        </dl>
      </section>
      <div className="space-y-6 p-5 sm:p-6 xl:p-8">
        <section aria-labelledby="collection-titles">
          <div className="mb-4 flex flex-wrap items-center justify-between gap-2">
            <div className="flex items-baseline gap-2">
              <h3 id="collection-titles" className="text-sm font-semibold text-fg">
                {template ? t("A taste of this template") : t("In this collection")}
              </h3>
              <span aria-live="polite" className="text-xs tabular-nums text-fg-muted">
                {total === undefined ? "" : tn(total, "{count} title", "{count} titles")}
              </span>
              {preview.loading ? <Spinner size={12} label={t("Loading title preview")} /> : null}
            </div>
            <Button
              variant="ghost"
              size="sm"
              className="h-7 px-2 text-xs"
              disabled={preview.loading}
              onClick={() => void preview.reload()}
            >
              <RefreshCw size={12} /> {t("Refresh preview")}
            </Button>
          </div>
          {preview.data?.warnings?.map((w) => (
            <Alert key={w} variant="warning" className="mb-3">
              <AlertDescription>{w}</AlertDescription>
            </Alert>
          ))}
          {preview.error ? (
            <ErrorPanel error={preview.error} onRetry={() => void preview.reload()} />
          ) : !preview.data ? (
            <PosterSkeletons count={12} />
          ) : items.length ? (
            <>
              <PosterGrid items={items} />
              <p className="mt-4 text-[11px] text-fg-subtle">
                {items.length < (total ?? 0)
                  ? tn(
                      items.length,
                      "Showing the first {count} matching title. Preview of current catalogue matches.",
                      "Showing the first {count} matching titles. Preview of current catalogue matches.",
                    )
                  : t("Preview of current catalogue matches.")}
              </p>
            </>
          ) : (
            <EmptyState
              icon={<SearchX />}
              title={t("No titles match yet")}
              description={
                template
                  ? t("Try customising this template to match more of your catalogue.")
                  : t("Edit the collection to broaden its sources or rules.")
              }
              className="py-10"
            />
          )}
        </section>
        <details className="group rounded-lg border border-border bg-surface">
          <summary className="flex cursor-pointer items-center gap-2 p-3 text-xs font-medium text-fg marker:content-none">
            <Filter size={13} className="text-fg-muted" /> {t("How titles are selected")}{" "}
            <span className="ml-auto text-fg-subtle group-open:hidden">{t("View rules ↓")}</span>
          </summary>
          <div className="space-y-4 border-t border-border p-4 text-xs text-fg-muted">
            <div>
              <h4 className="mb-1.5 font-medium text-fg">{t("Sources")}</h4>
              <ul className="space-y-1">
                {spec.sources.map((s, i) => (
                  <li key={i}>{describeSource(s)}</li>
                ))}
              </ul>
            </div>
            <div>
              <h4 className="mb-1.5 font-medium text-fg">
                {spec.match === "any" ? t("Match any rule") : t("Match every rule")}
              </h4>
              {spec.rules.length ? (
                <ul className="space-y-1">
                  {spec.rules.map((r, i) => (
                    <li key={i}>{describeRule(r)}</li>
                  ))}
                </ul>
              ) : (
                <p>{t("No additional rules.")}</p>
              )}
            </div>
            {spec.exclude.length ? (
              <div>
                <h4 className="mb-1.5 font-medium text-fg">{t("Exclude")}</h4>
                <ul className="space-y-1">
                  {spec.exclude.map((r, i) => (
                    <li key={i}>{describeRule(r)}</li>
                  ))}
                </ul>
              </div>
            ) : null}
            <p>
              {sorted}
              {rotation ? ` · ${rotation}` : ""}.
            </p>
          </div>
        </details>
      </div>
    </div>
  );
}

function Readout({
  icon,
  label,
  value,
  detail,
}: {
  icon: ReactNode;
  label: string;
  value: string;
  detail: string;
}) {
  return (
    <div className="min-w-0 border-l border-border px-3 first:border-l-0 first:pl-0 sm:px-5">
      <dt className="flex items-center gap-1.5 text-[10px] font-medium uppercase tracking-wide text-fg-subtle">
        <span className="hidden sm:inline">{icon}</span>
        {label}
      </dt>
      <dd className="mt-1 break-words text-xs font-medium leading-relaxed text-fg">{value}</dd>
      <dd className="mt-0.5 text-[11px] leading-relaxed text-fg-muted">{detail}</dd>
    </div>
  );
}
