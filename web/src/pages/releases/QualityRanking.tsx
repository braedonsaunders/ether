import {
  Badge,
  Button,
  SearchSelect,
  SettingsSection,
  Switch,
  Textarea,
} from "@braedonsaunders/appkit-ui";
import { ChevronDown, Merge, Plus, RotateCcw, Split, Trash2 } from "lucide-react";
import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { MoreOptions } from "../../components/ConfigSection";
import { ChipGroup, NumberInput } from "../../components/fields";
import { SettingsRow } from "../../components/SettingsRow";
import { t, tx } from "../../i18n";
import { useLocale } from "../../i18n/react";
import { searchSelectLabels } from "../../i18n/appkit-labels";
import { isPluginDescriptor, textOf } from "../../lib/extend";
import { qualityGateLabel } from "../../lib/releases";
import {
  BUILTIN_FORMATS,
  RULE_PREFIX,
  allQualities,
  builtinFormat,
  definitionKey,
  formatApplies,
  isPluginFormat,
  isRuleFormat,
  itemName,
  qualityName,
  resolutionGroups,
  withRuleScores,
} from "../../lib/qualityRanking";
import type { QualityItem, QualityProfile, ReleaseRule } from "../../lib/types";
import { LANGUAGE_TAGS } from "../libraries/browser/CatalogPickers";
import { REGIONAL_TRACK_TAGS, baseLanguage } from "../setup/language";
import { OrderButtons, swap } from "../sources/form";
import {
  PluginBadge,
  RuleEditorCard,
  RulePreview,
  createRule,
  descriptorLabel,
  descriptorOwnerName,
  newCondition,
  useRankingRegistry,
} from "./ReleasePreferences";
import { QualityHelp } from "./QualityHelp";
import type { ResolverPolicy } from "./resolverDraft";

type Patch = (p: Partial<QualityProfile>) => void;

/** The profile's quality order, Radarr/Sonarr style: best first, groups rank equally. */
export function QualityOrderSection({ draft, patch }: { draft: QualityProfile; patch: Patch }) {
  const locale = useLocale();
  const order = draft.qualityOrder ?? [];
  const listed = new Set(order.flatMap((i) => i.qualities));
  const unlisted = useMemo(
    () =>
      allQualities(draft.allowCam)
        .filter((q) => !listed.has(q))
        .map((q) => ({ value: q, label: qualityName(q) })),
    [draft.allowCam, order, locale],
  );
  const [toAdd, setToAdd] = useState("");
  const chosen = unlisted.some((o) => o.value === toAdd) ? toAdd : (unlisted[0]?.value ?? "");
  const gates = draft.allowedQualities ?? [];
  const set = (next: QualityItem[]) => patch({ qualityOrder: next });

  const merge = (i: number) => {
    const above = order[i - 1]!;
    const item = order[i]!;
    const merged = {
      name: above.name || itemName(above),
      qualities: [...above.qualities, ...item.qualities],
    };
    set([...order.slice(0, i - 1), merged, ...order.slice(i + 1)]);
  };
  const split = (i: number) => {
    const singles = order[i]!.qualities.map((q) => ({ qualities: [q] }));
    set([...order.slice(0, i), ...singles, ...order.slice(i + 1)]);
  };
  const dropQuality = (i: number, q: string) => {
    const item = order[i]!;
    const rest = item.qualities.filter((x) => x !== q);
    set(
      rest.length
        ? order.map((x, j) => (j === i ? { ...x, qualities: rest } : x))
        : order.filter((_, j) => j !== i),
    );
  };

  return (
    <SettingsSection
      title={t("Qualities")}
      description={t(
        "Higher in the list plays first, as in Radarr and Sonarr. Qualities in one row rank the same.",
      )}
    >
      <div className="space-y-3 px-5 py-4">
        {order.length === 0 ? (
          <p className="text-sm text-fg-muted">
            {t("Every quality ranks the same. Custom formats decide.")}
          </p>
        ) : (
          <ol className="divide-y divide-border overflow-hidden rounded-lg border border-border">
            {order.map((item, i) => {
              const name = itemName(item);
              return (
                <li
                  key={`${i}-${item.qualities.join(",")}`}
                  data-quality-item={i}
                  className="flex items-start gap-3 px-3 py-2.5"
                >
                  <span className="mt-1 w-5 text-xs tabular-nums text-fg-muted">{i + 1}</span>
                  <div className="min-w-0 flex-1 space-y-1.5">
                    {name && item.qualities.length > 1 ? (
                      <p className="text-sm font-medium text-fg">{name}</p>
                    ) : null}
                    <span className="flex flex-wrap gap-1.5">
                      {item.qualities.map((q) => (
                        <Badge key={q} variant="outline" className="gap-1">
                          {qualityName(q)}
                          {item.qualities.length > 1 || order.length > 1 ? (
                            <button
                              type="button"
                              className="text-fg-muted hover:text-fg"
                              aria-label={t("Remove {name}", { name: qualityName(q) })}
                              onClick={() => dropQuality(i, q)}
                            >
                              ×
                            </button>
                          ) : null}
                        </Badge>
                      ))}
                    </span>
                  </div>
                  <span className="flex shrink-0 items-center gap-1">
                    {i > 0 ? (
                      <Button
                        variant="ghost"
                        size="icon"
                        aria-label={t("Rank {name} the same as the row above", {
                          name: name || qualityName(item.qualities[0]!),
                        })}
                        onClick={() => merge(i)}
                      >
                        <Merge size={14} />
                      </Button>
                    ) : null}
                    {item.qualities.length > 1 ? (
                      <Button
                        variant="ghost"
                        size="icon"
                        aria-label={t("Split {name} into one row per quality", {
                          name: name || qualityName(item.qualities[0]!),
                        })}
                        onClick={() => split(i)}
                      >
                        <Split size={14} />
                      </Button>
                    ) : null}
                    <OrderButtons
                      label={name || qualityName(item.qualities[0]!)}
                      i={i}
                      n={order.length}
                      onMove={(d) => set(swap(order, i, i + d))}
                    />
                  </span>
                </li>
              );
            })}
          </ol>
        )}
        {order.length > 0 ? (
          <div>
            <Button
              variant="ghost"
              size="sm"
              onClick={() => set(resolutionGroups(draft.maxHeight, draft.minHeight))}
            >
              <RotateCcw size={14} />
              {t("Reset resolution groups")}
            </Button>
          </div>
        ) : null}
        {unlisted.length > 0 ? (
          <MoreOptions label={t("More options: Add a quality")}>
            <div className="flex flex-col gap-2 py-3 sm:flex-row sm:items-center">
              <div className="min-w-0 sm:flex-1">
                <SearchSelect
                  {...searchSelectLabels()}
                  ariaLabel={t("Quality to add")}
                  value={chosen}
                  options={unlisted}
                  onChange={setToAdd}
                />
              </div>
              <Button
                size="sm"
                disabled={!chosen}
                onClick={() => set([...order, { qualities: [chosen] }])}
              >
                <Plus size={14} />
                {t("Add quality")}
              </Button>
            </div>
          </MoreOptions>
        ) : null}
      </div>
      {gates.length ? (
        <SettingsRow
          stacked
          title={t("Allowed qualities")}
          description={t(
            "Prefer these qualities. When none match, Ether uses the best available release.",
          )}
        >
          <span className="flex flex-wrap items-center gap-1.5">
            {gates.map((q) => (
              <Badge key={q} variant="outline">
                {qualityGateLabel(q)}
              </Badge>
            ))}
            <Button variant="ghost" size="sm" onClick={() => patch({ allowedQualities: null })}>
              {t("Allow all")}
            </Button>
          </span>
        </SettingsRow>
      ) : null}
    </SettingsSection>
  );
}

/**
 * The profile's audio language, as in Radarr: Original plays each title's
 * own language, Any plays every language, and a language plays only
 * releases with that audio.
 */
export function LanguageSection({ draft, patch }: { draft: QualityProfile; patch: Patch }) {
  const { locale } = useLocale();
  const stored = draft.language?.trim() || "original";
  const { options, current } = useMemo(() => {
    const display = (code: string) => {
      try {
        return new Intl.DisplayNames([locale], { type: "language" }).of(code) ?? code;
      } catch {
        return code;
      }
    };
    const seen = new Set<string>();
    const languages: { value: string; label: string }[] = [];
    for (const tag of LANGUAGE_TAGS) {
      const code = baseLanguage(tag);
      if (!code || seen.has(code)) continue;
      seen.add(code);
      languages.push({ value: code, label: display(code) });
    }
    for (const tag of REGIONAL_TRACK_TAGS) {
      if (seen.has(tag)) continue;
      seen.add(tag);
      languages.push({ value: tag, label: display(tag) });
    }
    languages.sort((a, b) => a.label.localeCompare(b.label, locale));
    const all = [
      { value: "original", label: t("Original") },
      { value: "any", label: t("Any") },
      ...languages,
    ];
    // An imported profile can carry a language name ("french") rather
    // than a code; keep it selectable as-is.
    const match = all.find((o) => o.value.toLowerCase() === stored.toLowerCase());
    if (!match) {
      all.splice(2, 0, {
        value: stored,
        label: stored.charAt(0).toUpperCase() + stored.slice(1),
      });
    }
    return { options: all, current: match ? match.value : stored };
  }, [locale, stored]);
  return (
    <SettingsSection title={t("Language")}>
      <SettingsRow
        title={t("Audio language")}
        description={t(
          "As in Radarr: Original is each title's own language. A language plays only releases with that audio; releases with several audio tracks always play.",
        )}
        control={
          <SearchSelect
            {...searchSelectLabels()}
            ariaLabel={t("Audio language")}
            value={current}
            options={options}
            onChange={(v) => patch({ language: v })}
          />
        }
      />
    </SettingsSection>
  );
}

/** One scored format row. */
function ScoreRow({
  label,
  badge,
  help,
  score,
  onScore,
  onRemove,
}: {
  label: string;
  badge?: React.ReactNode;
  help?: string;
  score: number;
  onScore: (n: number) => void;
  onRemove?: () => void;
}) {
  return (
    <div className="flex items-center gap-2 px-3 py-2">
      <span className="flex min-w-0 flex-1 flex-wrap items-center gap-2 text-sm text-fg">
        {help ? <QualityHelp summary={label} detail={help} /> : label}
        {badge}
      </span>
      <NumberInput
        aria-label={t("Score for {name}", { name: label })}
        value={score}
        className="w-28"
        onChange={onScore}
      />
      {onRemove ? (
        <Button
          variant="ghost"
          size="icon"
          aria-label={t("Remove {name}", { name: label })}
          onClick={onRemove}
        >
          <Trash2 size={14} />
        </Button>
      ) : null}
    </div>
  );
}

/** A group of score rows under a small heading. */
function ScoreGroup({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="space-y-1.5">
      <h4 className="text-xs font-semibold uppercase tracking-[0.12em] text-fg-muted">{title}</h4>
      <div className="divide-y divide-border overflow-hidden rounded-lg border border-border">
        {children}
      </div>
    </div>
  );
}

/**
 * The profile's custom formats with their scores, Radarr/Sonarr style: Ether's
 * built-in release and streaming formats, the owner's and imported formats,
 * Prefer rules and plugin steps. Releases of one quality rank by the total.
 */
export function CustomFormatsSection({
  draft,
  patch,
  resolver,
  formatsText,
  onFormatsText,
  revealed,
}: {
  draft: QualityProfile;
  patch: Patch;
  resolver: ResolverPolicy | null;
  /** The shared custom format definitions, as JSON. */
  formatsText: string;
  onFormatsText: (text: string) => void;
  revealed: boolean;
}) {
  const locale = useLocale();
  const { steps, fields } = useRankingRegistry();
  const scores = draft.customFormatScores ?? {};
  const rules = draft.releaseRules ?? [];
  const preferRules = rules.filter((r) => r.action === "prefer");
  const definitions = resolver?.customFormats ?? [];
  const pluginSteps = steps.filter((d) => isPluginDescriptor(d));
  const plugins = draft.pluginScores ?? {};

  const setScore = (id: string, n: number) => patch({ customFormatScores: { ...scores, [id]: n } });
  const removeScore = (id: string) => {
    const next = { ...scores };
    delete next[id];
    patch({ customFormatScores: next });
  };
  const commitRules = (nextRules: ReleaseRule[]) =>
    patch({ releaseRules: nextRules, customFormatScores: withRuleScores(nextRules, scores) });

  const defName = (id: string) => {
    const d = definitions.find((f) => definitionKey(f) === id);
    return (d?.name as string | undefined) || id;
  };
  const builtins = BUILTIN_FORMATS.filter((f) => formatApplies(f, draft.mediaType));
  const listedBuiltins = builtins.filter((f) => scores[f.id] != null);
  const listedCustom = Object.keys(scores).filter(
    (id) => !id.startsWith("ether:") && !isRuleFormat(id) && !isPluginFormat(id),
  );
  const addable = useMemo(
    () => [
      ...builtins
        .filter((f) => scores[f.id] == null)
        .map((f) => ({
          value: f.id,
          label: f.kind === "streaming" ? t("{name} (streaming)", { name: f.label }) : f.label,
        })),
      ...definitions
        .map((f) => definitionKey(f))
        .filter((id) => id && scores[id] == null)
        .map((id) => ({ value: id, label: defName(id) })),
    ],
    [scores, definitions, locale, draft.mediaType],
  );
  const [toAdd, setToAdd] = useState("");
  const chosen = addable.some((o) => o.value === toAdd) ? toAdd : (addable[0]?.value ?? "");
  const [pluginToAdd, setPluginToAdd] = useState("");
  const addablePlugins = pluginSteps.filter((d) => plugins[d.id] == null);
  const chosenPlugin = addablePlugins.some((d) => d.id === pluginToAdd)
    ? pluginToAdd
    : (addablePlugins[0]?.id ?? "");

  const builtinRows = (kind: "release" | "streaming") =>
    listedBuiltins
      .filter((f) => f.kind === kind)
      .map((f) => (
        <ScoreRow
          key={f.id}
          label={f.label}
          help={f.help}
          score={scores[f.id] ?? 0}
          onScore={(n) => setScore(f.id, n)}
          onRemove={() => removeScore(f.id)}
        />
      ));
  const releaseRows = builtinRows("release");
  const streamingRows = builtinRows("streaming");

  return (
    <SettingsSection
      title={t("Custom formats")}
      description={t("Releases of the same quality rank by the total of their format scores.")}
    >
      <SettingsRow
        title={t("Minimum score")}
        description={t(
          "Prefer releases that reach this score. When none do, Ether uses the best available release. Scores of -10000 or lower on a custom format block matching releases.",
        )}
        control={
          <span className="flex items-center gap-2">
            <Switch
              aria-label={t("Set a minimum score")}
              checked={draft.minCustomFormatScore != null}
              onChange={(e) => patch({ minCustomFormatScore: e.currentTarget.checked ? 0 : null })}
            />
            {draft.minCustomFormatScore != null ? (
              <NumberInput
                aria-label={t("Minimum score")}
                value={draft.minCustomFormatScore}
                className="w-24"
                onChange={(n) => patch({ minCustomFormatScore: n })}
              />
            ) : null}
          </span>
        }
      />
      <div className="space-y-5 px-5 py-4">
        {listedCustom.length ? (
          <ScoreGroup title={t("Imported and your own")}>
            {listedCustom.map((id) => (
              <ScoreRow
                key={id}
                label={defName(id)}
                badge={
                  definitions.some((f) => definitionKey(f) === id) ? null : (
                    <Badge variant="warning">{t("Not found")}</Badge>
                  )
                }
                score={scores[id] ?? 0}
                onScore={(n) => setScore(id, n)}
                onRemove={() => removeScore(id)}
              />
            ))}
          </ScoreGroup>
        ) : null}
        {releaseRows.length ? <ScoreGroup title={t("Release")}>{releaseRows}</ScoreGroup> : null}
        {streamingRows.length ? (
          <ScoreGroup title={t("How it plays here")}>{streamingRows}</ScoreGroup>
        ) : null}
        {addable.length ? (
          <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
            <div className="min-w-0 sm:flex-1">
              <SearchSelect
                {...searchSelectLabels()}
                ariaLabel={t("Format to add")}
                value={chosen}
                options={addable}
                onChange={setToAdd}
              />
            </div>
            <Button
              size="sm"
              disabled={!chosen}
              onClick={() => setScore(chosen, builtinFormat(chosen) ? 10 : 0)}
            >
              <Plus size={14} />
              {t("Add format")}
            </Button>
          </div>
        ) : null}

        <ScoreGroup title={t("Prefer rules")}>
          {preferRules.map((r) => (
            <div key={r.id} data-prefer-rule={r.id}>
              <ScoreRow
                label={r.name || t("Prefer rule")}
                score={scores[RULE_PREFIX + r.id] ?? 0}
                onScore={(n) => setScore(RULE_PREFIX + r.id, n)}
              />
              <RuleEditorCard
                rule={r}
                fields={fields}
                onChange={(rulePatch) =>
                  commitRules(rules.map((x) => (x.id === r.id ? { ...x, ...rulePatch } : x)))
                }
                onRemove={() => commitRules(rules.filter((x) => x.id !== r.id))}
              />
            </div>
          ))}
          <div className="px-3 py-2">
            <Button
              variant="ghost"
              size="sm"
              disabled={rules.length >= 32}
              onClick={() =>
                commitRules([
                  ...rules,
                  createRule(t("Prefer rule"), newCondition(fields), "prefer"),
                ])
              }
            >
              <Plus size={14} />
              {t("Add a Prefer rule")}
            </Button>
          </div>
        </ScoreGroup>

        {pluginSteps.length || Object.keys(plugins).length ? (
          <ScoreGroup title={t("Plugins")}>
            {Object.keys(plugins).map((id) => (
              <PluginScoreRow key={id} id={id} draft={draft} patch={patch} />
            ))}
            {addablePlugins.length ? (
              <div className="flex flex-col gap-2 px-3 py-2 sm:flex-row sm:items-center">
                <div className="min-w-0 sm:flex-1">
                  <SearchSelect
                    {...searchSelectLabels()}
                    ariaLabel={t("Plugin to add")}
                    value={chosenPlugin}
                    options={addablePlugins.map((d) => ({
                      value: d.id,
                      label: descriptorLabel(d),
                    }))}
                    onChange={setPluginToAdd}
                  />
                </div>
                <Button
                  size="sm"
                  disabled={!chosenPlugin}
                  onClick={() => patch({ pluginScores: { ...plugins, [chosenPlugin]: {} } })}
                >
                  <Plus size={14} />
                  {t("Add plugin")}
                </Button>
              </div>
            ) : null}
          </ScoreGroup>
        ) : null}
      </div>
      <MoreOptions label={t("More options: Custom format definitions")} reveal={revealed}>
        <div className="space-y-4 py-3">
          <label className="block space-y-2 px-4 text-sm font-medium text-fg">
            {t("Custom format definitions")}
            <Textarea
              className="min-h-40 w-full font-mono text-xs"
              spellCheck={false}
              value={formatsText}
              onChange={(e) => onFormatsText(e.target.value)}
            />
          </label>
          <p className="px-4 text-xs text-fg-muted">
            {tx("Radarr and Sonarr custom format JSON, for example from {source}.", {
              source: (
                <a
                  href="https://trash-guides.info"
                  target="_blank"
                  rel="noreferrer"
                  className="text-primary underline"
                >
                  TRaSH Guides
                </a>
              ),
            })}
          </p>
          <p className="px-4 pb-2 text-sm">
            <Link to="/quality/history" className="text-primary underline">
              {t("Inspect release scores and rejection reasons")}
            </Link>
          </p>
        </div>
      </MoreOptions>
      <RulePreview profile={draft} />
    </SettingsSection>
  );
}

/** A plugin ranking step the profile uses: its settings and removal. */
function PluginScoreRow({ id, draft, patch }: { id: string; draft: QualityProfile; patch: Patch }) {
  const { steps } = useRankingRegistry();
  const [open, setOpen] = useState(false);
  const plugins = draft.pluginScores ?? {};
  const desc = steps.find((d) => d.id === id);
  const meta = desc?.meta;
  const title = desc ? textOf(desc.label) || id : id;
  const editor = typeof meta?.editor === "object" ? meta.editor : null;
  const editorKind =
    editor?.kind ?? editor?.editor ?? (typeof meta?.editor === "string" ? meta.editor : "none");
  const choices = (editor?.options ?? editor?.choices ?? meta?.options ?? meta?.choices ?? []).map(
    (o) =>
      typeof o === "string"
        ? { value: o, label: o }
        : { value: o.value, label: textOf(o.label) || o.value },
  );
  const config = plugins[id] ?? {};
  const setConfig = (next: Record<string, unknown>) =>
    patch({ pluginScores: { ...plugins, [id]: next } });
  const remove = () => {
    const next = { ...plugins };
    delete next[id];
    patch({ pluginScores: next });
  };
  const expandable = ["ordered_list", "choice", "range", "toggle"].includes(editorKind) && !!desc;
  const num = (v: unknown) => (typeof v === "number" ? v : 0);
  const list = Array.isArray(config.value)
    ? config.value.filter((v): v is string => typeof v === "string")
    : [];
  return (
    <div data-plugin-score={id} className="space-y-3 px-3 py-2.5">
      <div className="flex items-center gap-2">
        <div className="min-w-0 flex-1">
          <p className="flex flex-wrap items-center gap-2 text-sm font-medium text-fg">
            {title}
            {desc ? <PluginBadge owner={descriptorOwnerName(desc)} /> : null}
          </p>
          <p className="truncate text-xs text-fg-muted">
            {desc
              ? t("Adds the points the plugin gives each release.")
              : t("This plugin is off or removed. It adds nothing.")}
          </p>
        </div>
        {expandable ? (
          <Button
            variant="ghost"
            size="sm"
            aria-expanded={open}
            aria-label={t("Configure {name}", { name: title })}
            onClick={() => setOpen(!open)}
          >
            {t("Values")}
            <ChevronDown size={13} />
          </Button>
        ) : null}
        <Button
          variant="ghost"
          size="icon"
          aria-label={t("Remove {name}", { name: title })}
          onClick={remove}
        >
          <Trash2 size={14} />
        </Button>
      </div>
      {open && expandable ? (
        <div className="space-y-3 border-t border-border pt-3">
          {editorKind === "toggle" ? (
            <label className="flex items-center gap-2 text-sm">
              <Switch
                aria-label={title}
                checked={config.value === true}
                onChange={(e) => setConfig({ value: e.currentTarget.checked })}
              />
              {t("Enabled")}
            </label>
          ) : null}
          {editorKind === "choice" ? (
            <SearchSelect
              {...searchSelectLabels()}
              ariaLabel={title}
              value={typeof config.value === "string" ? config.value : ""}
              options={choices}
              onChange={(value) => setConfig({ value })}
            />
          ) : null}
          {editorKind === "ordered_list" ? (
            <div className="space-y-1.5">
              <ChipGroup
                ariaLabel={title}
                size="sm"
                value={list}
                onChange={(next) =>
                  setConfig({
                    value: next
                      .filter((x) => list.includes(x))
                      .concat(next.filter((x) => !list.includes(x))),
                  })
                }
                options={[
                  ...choices.map((c) => c.value),
                  ...list.filter((x) => !choices.some((c) => c.value === x)),
                ].map((x) => ({
                  value: x,
                  label: choices.find((c) => c.value === x)?.label ?? x,
                }))}
              />
              <p className="text-xs text-fg-muted">
                {list.length
                  ? t("Preference: {order}", { order: list.join(" › ") })
                  : t("No preference")}
              </p>
            </div>
          ) : null}
          {editorKind === "range" ? (
            <div className="grid grid-cols-2 gap-3">
              <label className="space-y-1 text-sm">
                {t("Minimum")}
                <NumberInput
                  aria-label={t("Minimum")}
                  value={num(config.min)}
                  onChange={(min) => setConfig({ min, max: Math.max(min, num(config.max)) })}
                />
              </label>
              <label className="space-y-1 text-sm">
                {t("Maximum")}
                <NumberInput
                  aria-label={t("Maximum")}
                  value={num(config.max)}
                  onChange={(max) => setConfig({ min: Math.min(max, num(config.min)), max })}
                />
              </label>
            </div>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}
