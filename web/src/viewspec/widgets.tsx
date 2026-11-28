/**
 * The Ether widget and frame registry, rendered. Every widget validates its
 * resolved props before rendering and fails soft: a bad widget shows one
 * plain sentence in its place, never a broken page. Widgets are Ether
 * components fed by plugin data, so a plugin page looks like the console
 * because it is the console.
 */
import { Badge, Button, Skeleton, SubtabNav } from "@braedonsaunders/appkit-ui";
import { Tv } from "lucide-react";
import { useEffect, useState, type ReactNode } from "react";
import { t } from "../i18n";
import { api } from "../lib/api";
import type { TitleCard } from "../lib/contract";
import { Artwork } from "../components/Artwork";
import { Drawer } from "../components/Drawer";
import { LiveChart, type LiveSeries } from "../components/LiveChart";
import { Section } from "../components/layout";
import { PosterCard, PosterGrid, Shelf } from "../components/Poster";
import { StatusDot } from "../components/status";
import { TitleSearch } from "../components/pickers";
import type { HealthLevel } from "../lib/health";
import { SchemaGroups, type FieldDef, type FieldGroup } from "../pages/settings/SchemaForm";
import { usePluginOptions } from "../pages/plugins/options";
import { usePluginView } from "./context";
import { validateFrameProps, validateWidgetProps } from "./registry";

function WidgetError({ errors }: { errors: string[] }) {
  if (errors.length) console.warn("Ether plugin widget:", errors.join("; "));
  return (
    <p role="alert" className="text-sm text-danger">
      {t("This part of the plugin page could not be shown.")}
    </p>
  );
}

function isObj(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function str(value: unknown): string {
  return typeof value === "string" ? value : value == null ? "" : String(value);
}

/**
 * Coerce a plugin-supplied title object to the console's TitleCard. The
 * plugin's loader computed these fields; Ether only checks the shape it
 * needs to draw a poster tile.
 */
function toTitleCard(raw: unknown): TitleCard | null {
  if (!isObj(raw)) return null;
  const key = str(raw.key || raw.ratingKey);
  const title = str(raw.title);
  if (!key || !title) return null;
  const type =
    raw.type === "show" || raw.type === "episode" || raw.type === "movie" ? raw.type : "movie";
  return {
    key,
    type,
    title,
    year: typeof raw.year === "number" ? raw.year : undefined,
    summary: typeof raw.summary === "string" ? raw.summary : undefined,
    poster: typeof raw.poster === "string" ? raw.poster : undefined,
    backdrop: typeof raw.backdrop === "string" ? raw.backdrop : undefined,
    rating: typeof raw.rating === "number" ? raw.rating : undefined,
    contentRating: typeof raw.contentRating === "string" ? raw.contentRating : undefined,
    runtimeMinutes: typeof raw.runtimeMinutes === "number" ? raw.runtimeMinutes : undefined,
    genres: Array.isArray(raw.genres)
      ? raw.genres.filter((g): g is string => typeof g === "string")
      : undefined,
    showTitle: typeof raw.showTitle === "string" ? raw.showTitle : undefined,
    season: typeof raw.season === "number" ? raw.season : undefined,
    episode: typeof raw.episode === "number" ? raw.episode : undefined,
  };
}

function toTitleCards(raw: unknown): TitleCard[] {
  if (!Array.isArray(raw)) return [];
  return raw.map(toTitleCard).filter((card): card is TitleCard => card !== null);
}

/**
 * Rating keys are kind * 10^15 + TMDB id (internal/core/ratingkey.go), kept
 * below 2^53 so JavaScript handles them exactly. ether.titleCard resolves a
 * TMDB id through the same catalogue endpoint as a rating key.
 */
function ratingKeyForTmdb(tmdbId: number, mediaType?: string): string {
  const kind = mediaType === "show" || mediaType === "tv" ? 2 : 1;
  return String(kind * 1_000_000_000_000_000 + Math.trunc(tmdbId));
}

function tmdbFromRatingKey(key: string): { tmdbId: number; mediaType: string } | null {
  const n = Number(key);
  if (!Number.isFinite(n) || n < 1_000_000_000_000_000) return null;
  const kind = Math.floor(n / 1_000_000_000_000_000);
  if (kind !== 1 && kind !== 2) return null;
  return {
    tmdbId: n - kind * 1_000_000_000_000_000,
    mediaType: kind === 2 ? "show" : "movie",
  };
}

function TitleCardWidget({ props }: { props: Record<string, unknown> }) {
  const inline = toTitleCard(props.item);
  const key =
    props.ratingKey != null
      ? str(props.ratingKey)
      : typeof props.tmdbId === "number"
        ? ratingKeyForTmdb(props.tmdbId, str(props.mediaType) || undefined)
        : "";
  const [fetched, setFetched] = useState<TitleCard | null>(null);
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    if (inline || !key) return;
    let live = true;
    setFailed(false);
    api
      .title(key)
      .then((detail) => {
        if (live) setFetched(detail);
      })
      .catch(() => {
        if (live) setFailed(true);
      });
    return () => {
      live = false;
    };
  }, [inline, key]);
  const card = inline ?? fetched;
  if (card) {
    return (
      <div className="w-32 max-w-full">
        <PosterCard item={card} reason={str(props.reason) || undefined} />
      </div>
    );
  }
  if (failed || !key) {
    return <p className="text-sm text-fg-muted">{t("Title not found")}</p>;
  }
  return (
    <div role="status" aria-label={t("Loading titles")} className="w-32 max-w-full">
      <Skeleton className="aspect-[2/3] w-full rounded-md" />
    </div>
  );
}

const CHART_PALETTE = [
  { stroke: "stroke-primary", fill: "fill-primary", swatch: "bg-primary" },
  { stroke: "stroke-series-up", fill: "fill-series-up", swatch: "bg-series-up" },
  {
    stroke: "stroke-series-down",
    fill: "fill-series-down",
    swatch: "bg-series-down",
  },
];

function LiveChartWidget({ props }: { props: Record<string, unknown> }) {
  const rawSeries = Array.isArray(props.series) ? props.series : [];
  const series: LiveSeries[] = rawSeries
    .filter(isObj)
    .map((s, i) => ({
      key: str(s.key) || `series-${i}`,
      label: str(s.label),
      values: (Array.isArray(s.values) ? s.values : [])
        .map((v) => (typeof v === "number" ? v : Number(v)))
        .filter((v) => Number.isFinite(v)),
      ...CHART_PALETTE[i % CHART_PALETTE.length]!,
    }))
    .filter((s) => s.values.length > 0);
  const times = (Array.isArray(props.times) ? props.times : []).map(str);
  const unit = str(props.unit);
  if (series.length === 0) {
    return <p className="text-sm text-fg-muted">{t("No data yet")}</p>;
  }
  return (
    <div>
      <div className="mb-1 flex flex-wrap gap-x-4 gap-y-1">
        {series.map((s) => (
          <span key={s.key} className="flex items-center gap-1.5 text-xs text-fg-muted">
            <span className={`size-2 rounded-full ${s.swatch}`} aria-hidden />
            {s.label}
          </span>
        ))}
      </div>
      <LiveChart
        series={series}
        times={times}
        max={typeof props.max === "number" ? props.max : undefined}
        area={props.area === true}
        ariaLabel={str(props.label) || t("Plugin chart")}
        format={(v) => (unit ? `${v} ${unit}` : String(v))}
        className="h-40"
      />
    </div>
  );
}

const FIELD_KINDS = new Set([
  "text",
  "secret",
  "duration",
  "readonly",
  "number",
  "bool",
  "select",
  "multiSelect",
  "list",
  "bytes",
  "hidden",
]);

/** Convert one JSON field definition to SchemaForm's FieldDef, or report why not. */
function toFieldDef(raw: unknown): { def?: FieldDef; error?: string } {
  if (!isObj(raw)) return { error: "field must be an object" };
  const key = str(raw.key);
  const label = str(raw.label);
  const kind = str(raw.kind);
  if (kind !== "group" && !FIELD_KINDS.has(kind)) return {};
  if (!key || !label) return { error: "field needs a key and a label" };
  const help = typeof raw.help === "string" ? raw.help : undefined;
  if (kind === "group") {
    const fields: FieldDef[] = [];
    for (const child of Array.isArray(raw.fields) ? raw.fields : []) {
      const converted = toFieldDef(child);
      if (converted.def) fields.push(converted.def);
    }
    return { def: { key, label, help, kind: "group", fields } };
  }
  switch (kind) {
    case "text":
    case "secret":
    case "duration":
    case "readonly":
      return {
        def: {
          key,
          label,
          help,
          kind,
          placeholder: typeof raw.placeholder === "string" ? raw.placeholder : undefined,
        },
      };
    case "number":
      return {
        def: {
          key,
          label,
          help,
          kind: "number",
          min: typeof raw.min === "number" ? raw.min : undefined,
          max: typeof raw.max === "number" ? raw.max : undefined,
          step: typeof raw.step === "number" ? raw.step : undefined,
          unit: typeof raw.unit === "string" ? raw.unit : undefined,
        },
      };
    case "bool":
      return { def: { key, label, help, kind: "bool" } };
    case "select":
    case "multiSelect":
      return {
        def: {
          key,
          label,
          help,
          kind,
          search: raw.search === true,
          placeholder: typeof raw.placeholder === "string" ? raw.placeholder : undefined,
          optionsFrom: raw.search === true ? undefined : key,
          options: (Array.isArray(raw.options) ? raw.options : []).filter(isObj).map((o) => ({
            value: str(o.value),
            label: str(o.label),
            description: typeof o.hint === "string" ? o.hint : undefined,
          })),
          numeric: raw.numeric === true,
        },
      };
    case "list":
      return {
        def: {
          key,
          label,
          help,
          kind: "list",
          placeholder: typeof raw.placeholder === "string" ? raw.placeholder : undefined,
          numeric: raw.numeric === true,
        },
      };
    case "bytes":
      return {
        def: { key, label, help, kind: "bytes", unit: raw.unit === "MiB" ? "MiB" : "GiB" },
      };
    default:
      return { def: { key, label, help, kind: "hidden" } };
  }
}

function SettingsFormWidget({ props }: { props: Record<string, unknown> }) {
  const view = usePluginView();
  const options = usePluginOptions(view.plugin);
  const [values, setValues] = useState<Record<string, unknown>>(() =>
    isObj(props.values) ? { ...props.values } : {},
  );
  const [busy, setBusy] = useState(false);
  const groups: FieldGroup[] = [];
  const errors: string[] = [];
  for (const rawGroup of Array.isArray(props.groups) ? props.groups : []) {
    if (!isObj(rawGroup)) {
      errors.push("settings group must be an object");
      continue;
    }
    const fields: FieldDef[] = [];
    for (const rawField of Array.isArray(rawGroup.fields) ? rawGroup.fields : []) {
      const converted = toFieldDef(rawField);
      if (converted.error) errors.push(converted.error);
      else if (converted.def) fields.push(converted.def);
    }
    if (fields.length)
      groups.push({
        id: typeof rawGroup.id === "string" ? rawGroup.id : undefined,
        title: typeof rawGroup.title === "string" ? rawGroup.title : undefined,
        description: typeof rawGroup.description === "string" ? rawGroup.description : undefined,
        fields,
      });
  }
  if (!groups.length && errors.length) return <WidgetError errors={errors} />;
  const submit = async () => {
    setBusy(true);
    try {
      await view.postAction(str(props.action), { values });
    } finally {
      setBusy(false);
    }
  };
  return (
    <SchemaGroups
      groups={groups}
      value={values}
      onChange={setValues}
      errors={{}}
      prefix="pluginSettings"
      showUnknownFields={false}
      loadOptions={options}
      searchOptions={options}
      footer={
        <Button type="button" size="sm" disabled={busy} onClick={() => void submit()}>
          {str(props.submitLabel) || t("Save")}
        </Button>
      }
    />
  );
}

function ActionButtonWidget({ props }: { props: Record<string, unknown> }) {
  const view = usePluginView();
  const [busy, setBusy] = useState(false);
  const variant =
    props.variant === "outline" ||
    props.variant === "destructive" ||
    props.variant === "ghost" ||
    props.variant === "secondary"
      ? props.variant
      : "default";
  return (
    <Button
      type="button"
      size="sm"
      variant={variant}
      disabled={busy}
      onClick={() => {
        setBusy(true);
        void view
          .postAction(str(props.action), isObj(props.params) ? props.params : {})
          .finally(() => setBusy(false));
      }}
    >
      {str(props.label)}
    </Button>
  );
}

function TitleMatchWidget({ props }: { props: Record<string, unknown> }) {
  const view = usePluginView();
  const label = str(props.label);
  return (
    <div className="space-y-2">
      {label ? <p className="text-sm font-medium text-fg">{label}</p> : null}
      <TitleSearch
        type={str(props.mediaType) || undefined}
        placeholder={str(props.placeholder) || undefined}
        onPick={(item) => {
          const tmdb = tmdbFromRatingKey(item.key);
          void view.postAction(str(props.action), {
            ...(isObj(props.params) ? props.params : {}),
            titleKey: item.key,
            ratingKey: item.key,
            tmdbId: tmdb?.tmdbId,
            mediaType: tmdb?.mediaType ?? item.type,
            title: item.title,
            year: item.year,
          });
        }}
      />
    </div>
  );
}

const LEVELS: HealthLevel[] = ["ok", "warn", "error", "off", "unknown"];

function HealthStripWidget({ props }: { props: Record<string, unknown> }) {
  const items = (Array.isArray(props.items) ? props.items : []).filter(isObj);
  if (items.length === 0) return null;
  return (
    <ul className="flex flex-wrap gap-x-5 gap-y-2" aria-label={t("Health")}>
      {items.map((item, i) => {
        const level = LEVELS.includes(item.level as HealthLevel)
          ? (item.level as HealthLevel)
          : "unknown";
        return (
          <li key={i} className="flex items-center gap-2 text-sm">
            <StatusDot level={level} pulse={level === "warn" || level === "error"} />
            <span className="text-fg">{str(item.label)}</span>
            {item.value != null && item.value !== "" ? (
              <span className="text-fg-muted">{str(item.value)}</span>
            ) : null}
          </li>
        );
      })}
    </ul>
  );
}

/**
 * Screen plugin-supplied image URLs: allow only absolute external http(s) URLs
 * without credentials or control characters. Relative and same-origin paths
 * are fetched with the owner's token, so a plugin screen must not reach them.
 */
export function safeLogoUrl(raw: unknown): string | null {
  if (typeof raw !== "string" || !raw || /[\\\u0000-\u001f\u007f]/.test(raw)) return null;
  if (!raw.startsWith("https://") && !raw.startsWith("http://")) return null;
  try {
    const url = new URL(raw);
    if (url.protocol !== "https:" && url.protocol !== "http:") return null;
    if (url.origin === window.location.origin) return null;
    return url.hostname && !url.username && !url.password ? raw : null;
  } catch {
    return null;
  }
}

function ChannelRowWidget({ props }: { props: Record<string, unknown> }) {
  const level = LEVELS.includes(props.level as HealthLevel) ? (props.level as HealthLevel) : null;
  const logo = safeLogoUrl(props.logo);
  return (
    <div className="flex items-center gap-3 py-1">
      {props.number != null && props.number !== "" ? (
        <span className="w-12 shrink-0 text-right text-sm font-medium tabular-nums text-fg-muted">
          {str(props.number)}
        </span>
      ) : null}
      {logo ? (
        <Artwork
          src={logo}
          alt=""
          title={str(props.name)}
          className="h-9 w-14 shrink-0 rounded"
        />
      ) : (
        <span
          aria-hidden
          className="grid h-9 w-14 shrink-0 place-items-center rounded bg-bg-subtle text-fg-subtle"
        >
          <Tv size={16} />
        </span>
      )}
      <div className="min-w-0 flex-1">
        <p className="truncate text-sm font-medium text-fg">{str(props.name)}</p>
        {props.nowPlaying || props.group ? (
          <p className="truncate text-xs text-fg-muted">
            {[str(props.nowPlaying), str(props.group)].filter(Boolean).join(" · ")}
          </p>
        ) : null}
      </div>
      {level ? <StatusDot level={level} /> : null}
    </div>
  );
}

/** Render one Ether widget by name from resolved props. */
export function EtherWidget({ name, props }: { name: string; props: Record<string, unknown> }) {
  const errors = validateWidgetProps(name, props);
  if (errors.length) return <WidgetError errors={errors} />;
  switch (name) {
    case "ether.posterGrid": {
      const items = toTitleCards(props.items);
      return items.length ? (
        <PosterGrid items={items} />
      ) : (
        <p className="text-sm text-fg-muted">{str(props.empty) || t("No titles")}</p>
      );
    }
    case "ether.posterShelf":
      return (
        <Shelf
          items={toTitleCards(props.items)}
          ariaLabel={str(props.label) || t("Titles")}
          empty={str(props.empty) || undefined}
        />
      );
    case "ether.titleCard":
      return <TitleCardWidget props={props} />;
    case "ether.liveChart":
      return <LiveChartWidget props={props} />;
    case "ether.settingsForm":
      return <SettingsFormWidget props={props} />;
    case "ether.actionButton":
      return <ActionButtonWidget props={props} />;
    case "ether.titleMatch":
      return <TitleMatchWidget props={props} />;
    case "ether.healthStrip":
      return <HealthStripWidget props={props} />;
    case "ether.channelRow":
      return <ChannelRowWidget props={props} />;
    case "ether.badge": {
      const variant = str(props.variant);
      const allowed = ["default", "secondary", "outline", "destructive", "warning", "success"];
      return (
        <Badge
          variant={
            (allowed.includes(variant) ? variant : "secondary") as
              "default" | "secondary" | "outline" | "destructive" | "warning" | "success"
          }
        >
          {str(props.label)}
        </Badge>
      );
    }
    default:
      return <WidgetError errors={[`unknown Ether widget "${name}"`]} />;
  }
}

/** Render one Ether frame around already-rendered children. */
export function EtherFrame({
  name,
  props,
  children,
}: {
  name: string;
  props: Record<string, unknown>;
  children: ReactNode;
}) {
  const view = usePluginView();
  const [drawerOpen, setDrawerOpen] = useState(false);
  const errors = validateFrameProps(name, props);
  if (errors.length) return <WidgetError errors={errors} />;
  switch (name) {
    case "ether.section":
      return (
        <Section
          title={str(props.title) || undefined}
          description={str(props.description) || undefined}
        >
          {children}
        </Section>
      );
    case "ether.tabs": {
      const tabs = (Array.isArray(props.tabs) ? props.tabs : [])
        .filter(isObj)
        .map((tab) => ({ key: str(tab.key), label: str(tab.label) }))
        .filter((tab) => tab.key);
      const param = str(props.param) || "tab";
      const active = view.params[param] ?? tabs[0]?.key ?? "";
      return (
        <div className="space-y-4">
          {tabs.length > 1 ? (
            <div className="border-b border-border">
              <SubtabNav
                ariaLabel={t("Plugin sections")}
                active={active}
                onSelect={(key) => view.setParams({ [param]: key })}
                tabs={tabs}
              />
            </div>
          ) : null}
          {children}
        </div>
      );
    }
    case "ether.drawer": {
      const title = str(props.title);
      return (
        <>
          <Button type="button" size="sm" variant="outline" onClick={() => setDrawerOpen(true)}>
            {str(props.openLabel) || title}
          </Button>
          <Drawer open={drawerOpen} onClose={() => setDrawerOpen(false)} title={title}>
            <div className="space-y-4">{children}</div>
          </Drawer>
        </>
      );
    }
    default:
      return <WidgetError errors={[`unknown Ether frame "${name}"`]} />;
  }
}
