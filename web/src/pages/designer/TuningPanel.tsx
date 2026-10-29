import { Badge, Button, Switch, cn } from "@braedonsaunders/appkit-ui";
import { Brain, Compass, Copy, Layers3, RotateCcw, Sparkles } from "lucide-react";
import { useEffect, useState } from "react";
import { DivergingBars } from "../../components/charts";
import { api } from "../../lib/api";
import type {
  HomeLayout,
  LayoutSection,
  RankerInsight,
  TuningDial,
  Viewer,
} from "../../lib/contract";
import { getLocale, localized, t } from "../../i18n";
import { formatNumber, relativeTime } from "../../lib/format";
import {
  DIAL_DEFAULT,
  SECTIONS,
  effectiveDedupe,
  effectiveTuning,
  setDedupe,
  setTuning,
  tuningIn,
  type LayerKey,
} from "../../lib/layout";

const DIALS = localized<
  {
    dial: TuningDial;
    name: string;
    icon: React.ReactNode;
    low: string;
    high: string;
    words: [string, string, string, string, string];
    hint: string;
  }[]
>(() => [
  {
    dial: "discovery",
    name: t("Discovery"),
    icon: <Compass size={16} />,
    low: t("Proven favourites"),
    high: t("New territory"),
    words: [t("Safe bets"), t("Mostly familiar"), t("Balanced"), t("Adventurous"), t("Explorer")],
    hint: t("How far rows reach beyond what the viewer already loves."),
  },
  {
    dial: "freshness",
    name: t("Freshness"),
    icon: <Sparkles size={16} />,
    low: t("Timeless"),
    high: t("New & trending"),
    words: [
      t("Evergreen"),
      t("Classic-leaning"),
      t("Balanced"),
      t("Of the moment"),
      t("Hot off the press"),
    ],
    hint: t("Whether picks lean on all-time greats or what's new and buzzing."),
  },
  {
    dial: "variety",
    name: t("Variety"),
    icon: <Layers3 size={16} />,
    low: t("Focused"),
    high: t("Varied"),
    words: [
      t("Laser-focused"),
      t("Focused"),
      t("Balanced"),
      t("Varied"),
      t("Something for every mood"),
    ],
    hint: t(
      "How tightly rows stick to the strongest tastes versus spreading across genres, eras and moods.",
    ),
  },
]);

const word = (words: string[], v: number) =>
  words[Math.min(words.length - 1, Math.floor(v * words.length))] ?? "";

/**
 * The page's dials, Home dedup and the learned model, edited in place. Dials
 * resolve viewer → Everyone → engine default, like every other setting here.
 */
export function TuningPanel({
  draft,
  onChange,
  layer,
  section,
  viewers,
}: {
  draft: HomeLayout;
  onChange: (d: HomeLayout) => void;
  layer: LayerKey;
  section: LayoutSection;
  viewers: Viewer[];
}) {
  const page = SECTIONS.find((s) => s.value === section)?.label ?? section;
  const who =
    layer === "global"
      ? t("everyone")
      : (viewers.find((v) => v.id === layer)?.title ??
        viewers.find((v) => v.id === layer)?.username ??
        t("this viewer"));
  const dedupe = effectiveDedupe(draft, layer, section);
  const otherSections = SECTIONS.filter((s) => s.value !== section);
  return (
    <div className="space-y-5">
      <section aria-label={t("How {page} picks", { page })} className="space-y-3">
        <div className="flex items-end justify-between gap-2">
          <div>
            <h2 className="text-xs font-semibold uppercase tracking-wider text-fg-subtle">
              {t("How {page} picks", { page })}
            </h2>
            <p className="mt-0.5 text-xs text-fg-muted">
              {layer === "global"
                ? t("Dials for {page} for everyone. Viewers can be tuned individually.", { page })
                : t("Dials for {viewer}'s {page}; unset dials follow Everyone.", {
                    viewer: who,
                    page,
                  })}
            </p>
          </div>
          {DIALS.some((d) => tuningIn(draft, layer, section, d.dial) !== undefined) ? (
            <Button
              variant="ghost"
              size="sm"
              className="h-7 px-2 text-xs"
              onClick={() => {
                let next = draft;
                for (const d of DIALS) next = setTuning(next, layer, section, d.dial, undefined);
                onChange(next);
              }}
            >
              <RotateCcw size={12} /> {t("Reset dials")}
            </Button>
          ) : null}
        </div>
        {DIALS.map((d) => (
          <Dial
            key={d.dial}
            spec={d}
            value={effectiveTuning(draft, layer, section, d.dial)}
            layer={layer}
            onChange={(v) => onChange(setTuning(draft, layer, section, d.dial, v))}
          />
        ))}
        {otherSections.length ? (
          <div className="flex flex-wrap items-center gap-1.5 text-xs text-fg-muted">
            <Copy size={12} /> {t("Copy these dials to")}
            {otherSections.map((s) => (
              <button
                key={s.value}
                type="button"
                className="rounded-full border border-border px-2 py-0.5 font-medium text-fg hover:bg-surface-hover"
                onClick={() => {
                  let next = draft;
                  for (const d of DIALS)
                    next = setTuning(
                      next,
                      layer,
                      s.value,
                      d.dial,
                      tuningIn(draft, layer, section, d.dial),
                    );
                  onChange(next);
                }}
              >
                {s.label}
              </button>
            ))}
          </div>
        ) : null}
      </section>

      {section !== "home" ? (
        <section
          aria-label={t("Home dedup")}
          className={cn(
            "flex items-start gap-3 rounded-xl border bg-surface p-3",
            dedupe.from === "override" ? "border-info/40" : "border-border",
          )}
        >
          <Switch
            aria-label={t("Don't repeat what Home shows")}
            checked={dedupe.on}
            onChange={(e) => onChange(setDedupe(draft, layer, section, e.currentTarget.checked))}
          />
          <div className="min-w-0 flex-1">
            <p className="text-sm font-medium text-fg">{t("Don't repeat what Home shows")}</p>
            <p className="text-xs text-fg-muted">
              {t(
                "Titles the viewer's Home leads with stay off {page}, so switching tabs always shows something new.",
                { page },
              )}
            </p>
          </div>
          {dedupe.from !== "default" ? (
            <button
              type="button"
              aria-label={t("Reset Home dedup")}
              title={t("Reset")}
              onClick={() => onChange(setDedupe(draft, layer, section, undefined))}
              className="text-fg-subtle hover:text-fg"
            >
              <RotateCcw size={13} />
            </button>
          ) : null}
        </section>
      ) : null}

      <LearningCard />
    </div>
  );
}

function Dial({
  spec,
  value,
  layer,
  onChange,
}: {
  spec: (typeof DIALS)[number];
  value: { v: number; from: "override" | "global" | "default" };
  layer: LayerKey;
  onChange: (v: number | undefined) => void;
}) {
  const pct = Math.round(value.v * 100);
  const own = value.from === (layer === "global" ? "global" : "override");
  return (
    <div
      className={cn(
        "rounded-xl border bg-surface p-3",
        value.from === "override" ? "border-info/40" : "border-border",
      )}
    >
      <div className="flex items-center gap-2">
        <span className="grid size-7 place-items-center rounded-lg bg-primary-subtle text-primary">
          {spec.icon}
        </span>
        <div className="min-w-0 flex-1">
          <p className="text-sm font-semibold text-fg">{spec.name}</p>
          <p className="truncate text-xs text-fg-muted" title={spec.hint}>
            {word(spec.words, value.v)}
          </p>
        </div>
        {value.from === "default" ? (
          <span className="text-[11px] text-fg-subtle">{t("Default")}</span>
        ) : value.from === "global" && layer !== "global" ? (
          <Badge variant="secondary">{t("From Everyone")}</Badge>
        ) : value.from === "override" ? (
          <Badge variant="info">{t("Override")}</Badge>
        ) : null}
        {own ? (
          <button
            type="button"
            aria-label={t("Reset {name}", { name: spec.name })}
            title={t("Reset")}
            onClick={() => onChange(undefined)}
            className="text-fg-subtle hover:text-fg"
          >
            <RotateCcw size={13} />
          </button>
        ) : null}
      </div>
      <div className="relative mt-3">
        <div className="pointer-events-none absolute inset-x-0 top-1/2 h-1.5 -translate-y-1/2 rounded-full bg-gradient-to-r from-primary/15 via-primary/40 to-primary" />
        <div
          className="pointer-events-none absolute top-1/2 h-3 w-px -translate-y-1/2 bg-fg-subtle/60"
          style={{ left: `${DIAL_DEFAULT * 100}%` }}
          aria-hidden
        />
        <input
          type="range"
          min={0}
          max={100}
          step={5}
          value={pct}
          aria-label={spec.name}
          aria-valuetext={word(spec.words, value.v)}
          onChange={(e) => onChange(Number(e.currentTarget.value) / 100)}
          className="relative z-10 h-5 w-full cursor-pointer appearance-none bg-transparent [&::-moz-range-thumb]:size-4 [&::-moz-range-thumb]:rounded-full [&::-moz-range-thumb]:border-2 [&::-moz-range-thumb]:border-primary [&::-moz-range-thumb]:bg-surface [&::-webkit-slider-thumb]:size-4 [&::-webkit-slider-thumb]:appearance-none [&::-webkit-slider-thumb]:rounded-full [&::-webkit-slider-thumb]:border-2 [&::-webkit-slider-thumb]:border-primary [&::-webkit-slider-thumb]:bg-surface [&::-webkit-slider-thumb]:shadow"
        />
      </div>
      <div className="mt-1 flex justify-between text-[11px] text-fg-subtle">
        <span>{spec.low}</span>
        <span>{spec.high}</span>
      </div>
    </div>
  );
}

/** What the learned ranker has picked up from real viewing. */
function LearningCard() {
  const [r, setR] = useState<RankerInsight | null | "none">(null);
  useEffect(() => {
    let live = true;
    api
      .rankerInsight()
      .then((x) => live && setR(x))
      .catch(() => live && setR("none"));
    return () => {
      live = false;
    };
  }, []);
  if (r === "none") return null;
  const lift =
    r && r.auc > 0 && r.baselineAuc > 0.5 ? (r.auc - 0.5) / (r.baselineAuc - 0.5) - 1 : null;
  const features = (r && r.features) ?? [];
  const maxW = Math.max(0.0001, ...features.map((f) => Math.abs(f.weight)));
  return (
    <section
      aria-label={t("How it learns")}
      className="relative overflow-hidden rounded-xl border border-border bg-surface p-3"
    >
      <div
        className="pointer-events-none absolute -right-10 -top-12 size-36 rounded-full bg-primary/10 blur-2xl"
        aria-hidden
      />
      <div className="relative flex items-center gap-2">
        <span className="grid size-7 place-items-center rounded-lg bg-primary-subtle text-primary">
          <Brain size={16} />
        </span>
        <div className="min-w-0 flex-1">
          <p className="text-sm font-semibold text-fg">{t("How it learns")}</p>
          <p className="text-xs text-fg-muted">
            {r === null
              ? t("Checking the model…")
              : r.active
                ? r.trainedAt
                  ? t("Ranking from what viewers actually play · trained {when}", {
                      when: relativeTime(r.trainedAt),
                    })
                  : t("Ranking from what viewers actually play")
                : t(
                    "Watching what viewers play; it switches on once it beats the hand-tuned ranking.",
                  )}
          </p>
        </div>
        {r && r !== null ? (
          <Badge variant={r.active ? "success" : "secondary"}>
            {r.active ? t("Active") : t("Learning")}
          </Badge>
        ) : null}
      </div>
      {r ? (
        <div className="relative mt-3 space-y-3">
          <div className="grid grid-cols-3 gap-2 text-center">
            <Stat label={t("Seen")} value={formatNumber(r.examples)} />
            <Stat label={t("Played")} value={formatNumber(r.positives)} />
            <Stat
              label={t("vs hand-tuned")}
              value={
                lift === null
                  ? "—"
                  : new Intl.NumberFormat(getLocale(), {
                      style: "percent",
                      signDisplay: "always",
                      maximumFractionDigits: 0,
                    }).format(Math.round(lift * 100) / 100 + 0)
              }
              tone={lift === null ? undefined : lift >= 0 ? "good" : "bad"}
            />
          </div>
          {features.length ? (
            <div className="space-y-1.5">
              <p className="text-[11px] font-semibold uppercase tracking-wider text-fg-subtle">
                {t("What drives plays")}
              </p>
              <DivergingBars
                ariaLabel={t("What drives plays")}
                data={features.slice(0, 6).map((f) => ({
                  key: f.name,
                  label: f.label || f.name,
                  weight: f.weight / maxW,
                }))}
              />
            </div>
          ) : null}
        </div>
      ) : null}
    </section>
  );
}

function Stat({ label, value, tone }: { label: string; value: string; tone?: "good" | "bad" }) {
  return (
    <div className="rounded-lg bg-bg-subtle px-2 py-1.5">
      <p
        className={cn(
          "text-base font-semibold tabular-nums",
          tone === "good" ? "text-success" : tone === "bad" ? "text-danger" : "text-fg",
        )}
      >
        {value}
      </p>
      <p className="text-[10.5px] uppercase tracking-wider text-fg-subtle">{label}</p>
    </div>
  );
}
