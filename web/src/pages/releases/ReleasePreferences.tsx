import {
  Badge,
  Button,
  Input,
  SearchSelect,
  Select,
  Switch,
  Textarea,
} from "@braedonsaunders/appkit-ui";
import { ArrowDown, ArrowUp, ChevronDown, Plus, Trash2, Upload, Download } from "lucide-react";
import { useEffect, useId, useRef, useState } from "react";
import { DragHandle, type DragHandleProps } from "../../components/Sortable";
import { t, getLocale, localized } from "../../i18n";
import { useLocale } from "../../i18n/react";
import { searchSelectLabels } from "../../i18n/appkit-labels";
import { apiFetch, errorMessage, isAbort } from "../../lib/api";
import type { QualityProfile, ReleaseCondition, ReleaseRule } from "../../lib/types";
import type { ConsoleRelease } from "../../lib/releases";
import { TitlePicker } from "./Search";

export const DEFAULT_BUILTIN_RANKING = [
  "language",
  "connection",
  "resolution",
  "releaseType",
  "hdr",
  "verified",
  "videoCodec",
  "audioFormat",
  "size",
] as const;

const BUILTIN_SET = new Set<string>(DEFAULT_BUILTIN_RANKING);

/**
 * Mirrors internal/core.NormalizeProfileRanking: Prefer rules sit at the top
 * by default in their rule order, followed by the built-in steps.
 */
export function normalizeRanking(
  profile: Pick<QualityProfile, "ranking" | "releaseRules">,
): string[] {
  const rules = profile.releaseRules ?? [];
  const preferSteps: string[] = [];
  const validPrefer = new Set<string>();
  for (const r of rules) {
    if (r.action === "prefer" && r.id) {
      const key = `rule:${r.id}`;
      if (!validPrefer.has(key)) {
        validPrefer.add(key);
        preferSteps.push(key);
      }
    }
  }
  const raw = profile.ranking ?? [];
  if (raw.length === 0) {
    return [...preferSteps, ...DEFAULT_BUILTIN_RANKING];
  }
  const seen = new Set<string>();
  const kept: string[] = [];
  for (const step of raw) {
    if (seen.has(step)) continue;
    if (step.startsWith("rule:")) {
      if (validPrefer.has(step)) {
        seen.add(step);
        kept.push(step);
      }
      continue;
    }
    if (BUILTIN_SET.has(step)) {
      seen.add(step);
      kept.push(step);
    }
  }
  const missingPrefer = preferSteps.filter((step) => !seen.has(step));
  const out = [...missingPrefer, ...kept];
  for (const step of DEFAULT_BUILTIN_RANKING) {
    if (!seen.has(step)) {
      out.push(step);
    }
  }
  return out;
}

/** Keeps the relative order of Prefer rules in releaseRules aligned with ranking. */
export function syncRulesWithRanking(rules: ReleaseRule[], ranking: string[]): ReleaseRule[] {
  const byId = new Map<string, ReleaseRule>();
  for (const r of rules) {
    if (r.action === "prefer" && r.id) {
      byId.set(r.id, r);
    }
  }
  const orderedPrefer: ReleaseRule[] = [];
  const used = new Set<string>();
  for (const step of ranking) {
    if (!step.startsWith("rule:")) continue;
    const id = step.slice(5);
    const r = byId.get(id);
    if (r && !used.has(id)) {
      used.add(id);
      orderedPrefer.push(r);
    }
  }
  for (const r of rules) {
    if (r.action === "prefer" && !used.has(r.id)) {
      orderedPrefer.push(r);
    }
  }
  let preferIdx = 0;
  return rules.map((r) => (r.action === "prefer" ? orderedPrefer[preferIdx++]! : r));
}

/** Inserts a new Prefer rule at the end of the leading Prefer-rule block in ranking. */
export function insertPreferRuleInRanking(ranking: string[], ruleId: string): string[] {
  const key = `rule:${ruleId}`;
  const without = ranking.filter((s) => s !== key);
  let insertAt = 0;
  while (insertAt < without.length && without[insertAt]!.startsWith("rule:")) {
    insertAt++;
  }
  return [...without.slice(0, insertAt), key, ...without.slice(insertAt)];
}

const FIELDS = localized((): [string, string][] => [
  ["language", t("Declared audio language")],
  ["subtitles", t("Declared subtitle language")],
  ["audio_matches_original", t("Original-language audio")],
  ["name", t("Release name")],
  ["group", t("Release group")],
  ["source", t("Release type")],
  ["video", t("Video codec")],
  ["audio", t("Audio codec")],
  ["height", t("Resolution")],
  ["size_gib", t("Size (GiB)")],
  ["hdr", t("HDR")],
  ["edition", t("Edition")],
  ["indexer", t("Indexer")],
  ["protocol", t("Protocol")],
]);

const SINGLE_VALUE = new Set([
  "group",
  "source",
  "video",
  "audio",
  "indexer",
  "protocol",
  "edition",
]);

const LANGUAGE_CODES =
  "en es es-419 es-ES pt pt-BR pt-PT fr de it nl sv da no nb nn fi is ja ko zh pl cs hu ro tr ru uk ar he hi th vi id".split(
    " ",
  );

const CHOICES: Record<string, string[]> = {
  source: ["webdl", "webrip", "bluray", "remux", "hdtv", "dvd", "cam"],
  video: ["h264", "hevc", "av1", "mpeg2", "xvid"],
  audio: ["aac", "ac3", "eac3", "truehd", "dts", "dtshd", "opus"],
  hdr: ["hdr10", "dv"],
  protocol: ["usenet", "torrent", "http"],
};

function fieldLabel(field: string) {
  return FIELDS.find(([value]) => value === field)?.[1] ?? field;
}

function mentionsOriginal(rule: ReleaseRule) {
  return [rule.conditions, rule.unless ?? [], rule.fallback ?? []].some((group) =>
    group.some((c) => c.field === "audio_matches_original"),
  );
}

function languageSuggestions() {
  const names = new Intl.DisplayNames([getLocale()], { type: "language" });
  return LANGUAGE_CODES.map((value) => ({ value, label: names.of(value) ?? value })).sort((a, b) =>
    a.label.localeCompare(b.label, getLocale()),
  );
}

function suggestionsFor(field: string) {
  if (field === "language" || field === "subtitles") return languageSuggestions();
  return (CHOICES[field] ?? []).map((value) => ({ value, label: value }));
}

const condition = (field = "name"): ReleaseCondition => ({
  field,
  operator:
    field === "name" ? "contains" : ["height", "size_gib"].includes(field) ? "range" : "equals",
  value: "",
});

function retarget(current: ReleaseCondition, field: string): ReleaseCondition {
  const next = condition(field);
  if (next.operator !== "range" && current.value) next.value = current.value;
  return next;
}

function createRule(
  name: string,
  c: ReleaseCondition,
  action: ReleaseRule["action"] = "prefer",
): ReleaseRule {
  return { id: crypto.randomUUID(), name, action, match: "all", conditions: [c] };
}

/** Same conflict the server rejects on save. Exceptions and fallbacks stay allowed. */
export function releaseRuleConflict(rules: ReleaseRule[]): string {
  const needs: { name: string; field: string; value: string }[] = [];
  for (const r of rules) {
    if (
      r.disabled ||
      r.action !== "require" ||
      r.match === "any" ||
      r.unless?.length ||
      r.fallback?.length
    )
      continue;
    const seen = new Map<string, string>();
    for (const c of r.conditions) {
      if (!SINGLE_VALUE.has(c.field) || c.operator !== "equals") continue;
      const prev = seen.get(c.field);
      if (prev != null && prev.toLowerCase() !== (c.value ?? "").toLowerCase()) {
        return t(
          "Required rule {name} cannot match: {field} would have to equal both {left} and {right}.",
          { name: r.name, field: fieldLabel(c.field), left: prev, right: c.value ?? "" },
        );
      }
      seen.set(c.field, c.value ?? "");
    }
    for (const [field, value] of seen) needs.push({ name: r.name, field, value });
  }
  for (let i = 0; i < needs.length; i++) {
    for (let j = i + 1; j < needs.length; j++) {
      const a = needs[i]!;
      const b = needs[j]!;
      if (a.field === b.field && a.value.toLowerCase() !== b.value.toLowerCase()) {
        return t(
          "Required rules {a} and {b} cannot match the same release: {field} would have to equal both {left} and {right}.",
          { a: a.name, b: b.name, field: fieldLabel(a.field), left: a.value, right: b.value },
        );
      }
    }
  }
  return "";
}

function ValueField({
  value,
  suggestions,
  mono,
  onChange,
}: {
  value: string;
  suggestions: { value: string; label: string }[];
  mono?: boolean;
  onChange: (value: string) => void;
}) {
  const listId = useId();
  return (
    <>
      <Input
        aria-label={t("Value")}
        className={mono ? "font-mono text-xs" : ""}
        value={value}
        list={suggestions.length ? listId : undefined}
        onChange={(e) => onChange(e.target.value)}
      />
      {suggestions.length > 0 && (
        <datalist id={listId}>
          {suggestions.map((option) => (
            <option key={option.value} value={option.value}>
              {option.label}
            </option>
          ))}
        </datalist>
      )}
    </>
  );
}

function ConditionEditor({
  value: c,
  onChange,
}: {
  value: ReleaseCondition;
  onChange: (c: ReleaseCondition) => void;
}) {
  const numeric = ["height", "size_gib"].includes(c.field);
  const language = ["language", "subtitles"].includes(c.field);
  const original = c.field === "audio_matches_original";
  return (
    <div className="flex flex-col gap-2 sm:flex-row sm:flex-wrap sm:items-center">
      <div className="min-w-0 sm:min-w-40 sm:flex-1">
        <SearchSelect
          {...searchSelectLabels()}
          ariaLabel={t("What it describes")}
          value={c.field}
          options={FIELDS.map(([value, label]) => ({ value, label }))}
          onChange={(v) => onChange(retarget(c, v))}
        />
      </div>
      {original ? (
        <p className="text-xs text-fg-muted sm:flex-1">
          {t(
            "Declared audio must match this title's original language. Subtitles do not count, and a filename alone cannot make the comparison.",
          )}
        </p>
      ) : numeric ? (
        <div className="flex items-center gap-2">
          <Input
            className="w-24"
            type="number"
            min="0"
            step="any"
            aria-label={t("Minimum")}
            value={c.min ?? 0}
            onChange={(e) =>
              onChange({ ...c, min: e.target.value === "" ? 0 : Number(e.target.value) })
            }
          />
          <span className="text-xs text-fg-muted">{t("to")}</span>
          <Input
            className="w-24"
            type="number"
            min="0"
            step="any"
            aria-label={t("Maximum (0 means unlimited)")}
            value={c.max ?? 0}
            onChange={(e) =>
              onChange({ ...c, max: e.target.value === "" ? 0 : Number(e.target.value) })
            }
          />
        </div>
      ) : (
        <>
          {!language && (
            <Select
              className="w-full sm:w-32"
              aria-label={t("Comparison")}
              value={c.operator}
              onChange={(e) =>
                onChange({ ...c, operator: e.target.value as ReleaseCondition["operator"] })
              }
            >
              <option value="equals">{t("Equals")}</option>
              <option value="contains">{t("Contains")}</option>
              <option value="regex">{t("Regex")}</option>
            </Select>
          )}
          <div className="min-w-0 sm:min-w-40 sm:flex-1">
            <ValueField
              value={c.value ?? ""}
              mono={c.operator === "regex"}
              suggestions={c.operator === "equals" ? suggestionsFor(c.field) : []}
              onChange={(value) => onChange({ ...c, value })}
            />
          </div>
        </>
      )}
    </div>
  );
}

function ConditionGroup({
  values,
  onChange,
}: {
  values: ReleaseCondition[];
  onChange: (v: ReleaseCondition[]) => void;
}) {
  return (
    <div className="space-y-2">
      {values.map((c, i) => (
        <div key={i} className="flex items-start gap-1">
          <div className="min-w-0 flex-1">
            <ConditionEditor
              value={c}
              onChange={(v) => onChange(values.map((x, j) => (i === j ? v : x)))}
            />
          </div>
          <Button
            variant="ghost"
            size="icon"
            aria-label={t("Remove condition")}
            onClick={() => onChange(values.filter((_, j) => i !== j))}
          >
            <Trash2 size={14} />
          </Button>
        </div>
      ))}
      <Button
        size="sm"
        variant="ghost"
        disabled={values.length >= 16}
        onClick={() => onChange([...values, condition()])}
      >
        <Plus size={14} />
        {t("Add condition")}
      </Button>
    </div>
  );
}

export function RuleEditorCard({
  rule: r,
  handle,
  stepNumber,
  canMoveUp,
  canMoveDown,
  onMove,
  onChange,
  onRemove,
}: {
  rule: ReleaseRule;
  handle?: DragHandleProps;
  stepNumber?: number;
  canMoveUp?: boolean;
  canMoveDown?: boolean;
  onMove?: (dir: -1 | 1) => void;
  onChange: (patch: Partial<ReleaseRule>) => void;
  onRemove: () => void;
}) {
  const [advanced, setAdvanced] = useState(false);
  return (
    <article className={`space-y-3 px-3 py-3 ${r.disabled ? "opacity-60" : ""}`}>
      <div className="flex items-center gap-2">
        {handle ? <DragHandle handle={handle} label={t("Move rule")} /> : null}
        {stepNumber != null ? (
          <span className="w-5 text-xs tabular-nums text-fg-muted">{stepNumber}</span>
        ) : null}
        <Input
          aria-label={t("Rule name")}
          value={r.name}
          className="min-w-0 flex-1 font-medium"
          onChange={(e) => onChange({ name: e.target.value })}
        />
        <Switch
          aria-label={t("Enabled")}
          checked={!r.disabled}
          onChange={(e) => onChange({ disabled: !e.currentTarget.checked })}
        />
        <Button variant="ghost" size="icon" aria-label={t("Remove rule")} onClick={onRemove}>
          <Trash2 size={14} />
        </Button>
      </div>
      <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
        <Select
          aria-label={t("Action")}
          className="w-full font-medium sm:w-36"
          value={r.action}
          onChange={(e) =>
            onChange({
              action: e.target.value as ReleaseRule["action"],
              fallback: e.target.value === "require" ? r.fallback : [],
            })
          }
        >
          <option value="require">{t("Only play")}</option>
          <option value="prefer">{t("Prefer")}</option>
          <option value="avoid">{t("Never play")}</option>
        </Select>
        <span className="text-xs text-fg-muted">
          {r.action === "require"
            ? t("Must match to play.")
            : r.action === "avoid"
              ? t("Matching releases are skipped.")
              : t("Ranks ahead of lower steps.")}
        </span>
      </div>
      {r.conditions[0] && (
        <ConditionEditor
          value={r.conditions[0]}
          onChange={(v) => onChange({ conditions: [v, ...r.conditions.slice(1)] })}
        />
      )}
      <div className="flex items-center gap-1">
        <Button
          size="sm"
          variant="ghost"
          aria-expanded={advanced}
          onClick={() => setAdvanced((v) => !v)}
        >
          {t("Advanced")}
          <ChevronDown size={13} />
        </Button>
        <span className="flex-1" />
        {onMove ? (
          <>
            <Button
              size="icon"
              variant="ghost"
              disabled={!canMoveUp}
              aria-label={t("Move up")}
              onClick={() => onMove(-1)}
            >
              <ArrowUp size={13} />
            </Button>
            <Button
              size="icon"
              variant="ghost"
              disabled={!canMoveDown}
              aria-label={t("Move down")}
              onClick={() => onMove(1)}
            >
              <ArrowDown size={13} />
            </Button>
          </>
        ) : null}
      </div>
      {advanced && (
        <div className="space-y-4 border-t border-border pt-3">
          <Select
            aria-label={t("Match conditions")}
            value={r.match ?? "all"}
            onChange={(e) => onChange({ match: e.target.value as "all" | "any" })}
          >
            <option value="all">{t("Match all conditions")}</option>
            <option value="any">{t("Match any condition")}</option>
          </Select>
          <div className="space-y-2">
            <p className="text-sm font-medium">{t("Other conditions")}</p>
            <ConditionGroup
              values={r.conditions.slice(1)}
              onChange={(rest) =>
                onChange({ conditions: [r.conditions[0]!, ...rest].filter(Boolean) })
              }
            />
          </div>
          <div className="space-y-2">
            <p className="text-sm font-medium">{t("Except when any matches")}</p>
            <ConditionGroup values={r.unless ?? []} onChange={(unless) => onChange({ unless })} />
          </div>
          {r.action === "require" && (
            <div className="space-y-2">
              <p className="text-sm font-medium">{t("Fallbacks")}</p>
              <p className="text-xs text-fg-muted">
                {t(
                  "Empty means no fallback. Matching alternatives are tried after primary matches.",
                )}
              </p>
              <ConditionGroup
                values={r.fallback ?? []}
                onChange={(fallback) => onChange({ fallback })}
              />
            </div>
          )}
          <p className="text-xs text-fg-muted">
            {t("Expressions ignore case. Lookarounds and backreferences are not supported.")}
          </p>
        </div>
      )}
    </article>
  );
}

type PreviewBody = { releases?: ConsoleRelease[]; warning?: string; note?: string };

/**
 * Filter rules (Only play / Never play) plus the rule builder and import/export
 * controls inside Section 1 ("Never play"). Adding a Prefer rule appends it to
 * releaseRules and places its step at the top of the Section 2 ranking ladder.
 */
export function FilterRulesEditor({
  profile,
  onChange,
  error,
}: {
  profile: QualityProfile;
  onChange: (patch: Pick<QualityProfile, "releaseRules" | "ranking">) => void;
  error?: string;
}) {
  useLocale();
  const rules = profile.releaseRules ?? [];
  const filterRules = rules.filter((r) => r.action === "require" || r.action === "avoid");
  const [term, setTerm] = useState("");
  const [field, setField] = useState("name");
  const [action, setAction] = useState<ReleaseRule["action"]>("prefer");
  const [importError, setImportError] = useState("");
  const file = useRef<HTMLInputElement>(null);
  const profileRef = useRef(profile);
  const importAbort = useRef<AbortController | null>(null);
  profileRef.current = profile;
  useEffect(() => () => importAbort.current?.abort(), []);

  const conflict = releaseRuleConflict(rules);

  function updateRule(id: string, patch: Partial<ReleaseRule>) {
    const nextRules = rules.map((r) => (r.id === id ? { ...r, ...patch } : r));
    const currentRanking = normalizeRanking(profile);
    let nextRanking = currentRanking;
    if (patch.action === "prefer") {
      nextRanking = insertPreferRuleInRanking(currentRanking, id);
    } else if (patch.action === "require" || patch.action === "avoid") {
      nextRanking = currentRanking.filter((s) => s !== `rule:${id}`);
    }
    onChange({
      releaseRules: syncRulesWithRanking(nextRules, nextRanking),
      ranking: nextRanking,
    });
  }

  function removeRule(id: string) {
    const nextRules = rules.filter((r) => r.id !== id);
    const nextRanking = normalizeRanking(profile).filter((s) => s !== `rule:${id}`);
    onChange({
      releaseRules: nextRules,
      ranking: nextRanking,
    });
  }

  function addTerm() {
    const value = term.trim();
    if (!value || rules.length >= 32) return;
    const c = condition(field);
    if (field === "audio_matches_original") {
      c.value = "";
    } else if (c.operator === "range") {
      const [min, max] = value.split(/[-–]/).map((part) => part.trim());
      c.min = min ? Number(min) : 0;
      c.max = max != null && max !== "" ? Number(max) : c.min;
    } else {
      c.value = value;
    }
    const added = createRule(value, c, action);
    const nextRules = [...rules, added];
    const currentRanking = normalizeRanking(profile);
    const nextRanking =
      action === "prefer" ? insertPreferRuleInRanking(currentRanking, added.id) : currentRanking;
    onChange({
      releaseRules: syncRulesWithRanking(nextRules, nextRanking),
      ranking: nextRanking,
    });
    setTerm("");
  }

  async function importRules(f: File) {
    const startRules = JSON.stringify(profileRef.current.releaseRules ?? []);
    const controller = new AbortController();
    importAbort.current?.abort();
    importAbort.current = controller;
    try {
      if (f.size > 32768) throw Error(t("Rules file is too large."));
      const text = await new Promise<string>((resolve, reject) => {
        const reader = new FileReader();
        reader.onload = () => resolve(String(reader.result ?? ""));
        reader.onerror = () => reject(Error(t("Could not read the rules file.")));
        reader.readAsText(f);
      });
      const data = JSON.parse(text) as ReleaseRule[];
      const currentRules = profileRef.current.releaseRules ?? [];
      if (!Array.isArray(data) || data.length + currentRules.length > 32)
        throw Error(t("Use a rules export with at most 32 rules."));
      const next = data.map((r) => ({ ...r, id: crypto.randomUUID() }));
      const mergedRules = [...currentRules, ...next];
      let nextRanking = normalizeRanking(profileRef.current);
      for (const r of next) {
        if (r.action === "prefer") {
          nextRanking = insertPreferRuleInRanking(nextRanking, r.id);
        }
      }
      const body = await apiFetch<PreviewBody>("/releases/rules/preview", {
        method: "POST",
        signal: controller.signal,
        body: JSON.stringify({
          profile: {
            ...profileRef.current,
            releaseRules: mergedRules,
            ranking: nextRanking,
          },
          names: [],
        }),
      });
      if (controller.signal.aborted) return;
      if (JSON.stringify(profileRef.current.releaseRules ?? []) !== startRules) {
        setImportError(t("The profile changed during import. Nothing was added."));
        return;
      }
      if (body.warning) throw Error(body.warning);
      onChange({
        releaseRules: syncRulesWithRanking(mergedRules, nextRanking),
        ranking: nextRanking,
      });
      setImportError("");
    } catch (e) {
      if (isAbort(e) || controller.signal.aborted) return;
      setImportError(errorMessage(e));
    }
  }

  function exportRules() {
    const url = URL.createObjectURL(
      new Blob([JSON.stringify(rules, null, 2)], { type: "application/json" }),
    );
    const a = document.createElement("a");
    a.href = url;
    a.download = "release-rules.json";
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  }

  return (
    <div className="space-y-4 border-t border-border px-5 py-4">
      <div className="space-y-1">
        <h4 className="text-sm font-medium text-fg">{t("Rules")}</h4>
        <p className="text-xs text-fg-muted">
          {t(
            "Only play and Never play rules filter releases here. Prefer rules become steps in What matters most below.",
          )}
        </p>
      </div>
      <div className="space-y-2">
        <Input
          aria-label={t("Add rule")}
          placeholder={t("Type a language, tag or release group…")}
          value={term}
          onChange={(e) => setTerm(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && term.trim() && rules.length < 32) {
              e.preventDefault();
              addTerm();
            }
          }}
        />
        <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
          <div className="min-w-0 sm:flex-1">
            <SearchSelect
              {...searchSelectLabels()}
              ariaLabel={t("What it describes")}
              value={field}
              options={FIELDS.map(([value, label]) => ({ value, label }))}
              onChange={setField}
            />
          </div>
          <Select
            aria-label={t("Action")}
            className="w-full sm:w-36"
            value={action}
            onChange={(e) => setAction(e.target.value as ReleaseRule["action"])}
          >
            <option value="require">{t("Only play")}</option>
            <option value="prefer">{t("Prefer")}</option>
            <option value="avoid">{t("Never play")}</option>
          </Select>
          <Button size="sm" disabled={rules.length >= 32 || !term.trim()} onClick={addTerm}>
            <Plus size={14} />
            {t("Add rule")}
          </Button>
        </div>
        {field === "audio_matches_original" && (
          <p className="text-xs text-fg-muted">
            {t(
              "Declared audio must match this title's original language. Subtitles do not count, and a filename alone cannot make the comparison.",
            )}
          </p>
        )}
      </div>
      {(error || importError || conflict) && (
        <p role="alert" className="text-sm text-danger">
          {error || importError || conflict}
        </p>
      )}
      {filterRules.length > 0 && (
        <div className="divide-y divide-border overflow-hidden rounded-lg border border-border">
          {filterRules.map((r) => (
            <RuleEditorCard
              key={r.id}
              rule={r}
              onChange={(patch) => updateRule(r.id, patch)}
              onRemove={() => removeRule(r.id)}
            />
          ))}
        </div>
      )}
      <div className="flex flex-wrap gap-2">
        <Button variant="ghost" size="sm" onClick={() => file.current?.click()}>
          <Upload size={14} />
          {t("Import rules")}
        </Button>
        <Button variant="ghost" size="sm" disabled={!rules.length} onClick={exportRules}>
          <Download size={14} />
          {t("Export rules")}
        </Button>
        <input
          ref={file}
          type="file"
          accept="application/json,.json"
          className="hidden"
          aria-label={t("Import rules")}
          onChange={(e) => {
            const f = e.target.files?.[0];
            if (f) void importRules(f);
            e.target.value = "";
          }}
        />
      </div>
    </div>
  );
}

function originalPreviewNote(mode: "names" | "title", rules: ReleaseRule[], serverNote: string) {
  if (!rules.some(mentionsOriginal)) return "";
  if (mode === "names" || serverNote === "original_language_unavailable") {
    return t(
      "Original-language audio cannot be compared from a filename. Search a title to use the catalog language.",
    );
  }
  if (serverNote === "original_language_unknown") {
    return t("This title has no original language, so original-language audio stays unverified.");
  }
  return "";
}

function explanationLabel(outcome: string, state: string, original: boolean) {
  if (outcome === "unknown" && original) return t("Original-language audio was not confirmed.");
  if (outcome === "unknown") return t("Needs verification");
  if (outcome === "exclude") return t("Excluded");
  if (outcome === "fallback") return t("Fallback");
  if (outcome === "prefer") return t("Preferred");
  if (outcome === "skip") return t("Exception matched");
  if (state === "match") return t("Matches");
  return t("Does not match");
}

export function RulePreview({ profile }: { profile: QualityProfile }) {
  const [mode, setMode] = useState<"names" | "title">("names");
  const [names, setNames] = useState("");
  const [key, setKey] = useState("");
  const [title, setTitle] = useState("");
  const [result, setResult] = useState<ConsoleRelease[] | null>(null);
  const [warning, setWarning] = useState("");
  const [note, setNote] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [requested, setRequested] = useState(false);
  const [attempt, setAttempt] = useState(0);
  const rules = profile.releaseRules ?? [];
  const serialized = JSON.stringify(profile);
  const originalNote = originalPreviewNote(mode, rules, note);
  useEffect(() => {
    if (!requested) return;
    const empty = mode === "title" ? !key : !names.trim();
    if (empty) {
      setResult(null);
      setWarning("");
      setNote("");
      setError("");
      setBusy(false);
      return;
    }
    const abort = new AbortController();
    let current = true;
    setError("");
    setBusy(true);
    const timer = setTimeout(() => {
      void apiFetch<PreviewBody>("/releases/rules/preview", {
        method: "POST",
        signal: abort.signal,
        body: JSON.stringify({
          profile: JSON.parse(serialized) as QualityProfile,
          ...(mode === "title" ? { key } : { names: names.split("\n").filter((x) => x.trim()) }),
        }),
      })
        .then((body) => {
          if (!current) return;
          setResult(body.releases ?? []);
          setWarning(body.warning ?? "");
          setNote(body.note ?? "");
        })
        .catch((e) => {
          if (!current || isAbort(e)) return;
          setError(errorMessage(e));
          setResult(null);
        })
        .finally(() => {
          if (current) setBusy(false);
        });
    }, 350);
    return () => {
      current = false;
      clearTimeout(timer);
      abort.abort();
    };
  }, [serialized, names, key, mode, requested, attempt]);
  return (
    <div className="space-y-3 border-t border-border px-5 py-4">
      <h4 className="text-sm font-medium">{t("Try before saving")}</h4>
      <Select
        aria-label={t("Preview source")}
        value={mode}
        onChange={(e) => {
          setRequested(false);
          setResult(null);
          setWarning("");
          setNote("");
          setError("");
          setBusy(false);
          setMode(e.target.value as "names" | "title");
        }}
      >
        <option value="names">{t("Paste release names")}</option>
        <option value="title">{t("Search a title")}</option>
      </Select>
      {mode === "names" ? (
        <Textarea
          aria-label={t("Release names")}
          rows={3}
          value={names}
          onChange={(e) => setNames(e.target.value)}
        />
      ) : (
        <>
          <TitlePicker
            onPick={(k, n) => {
              setKey(k);
              setTitle(n);
            }}
          />
          {title && <p className="text-xs text-fg-muted">{title}</p>}
        </>
      )}
      <p className="text-xs text-fg-muted">
        {mode === "names"
          ? t("Filename preview checks rules only. Search a title to include full profile ranking.")
          : t(
              "Uses your indexers. No settings are saved and no media is downloaded. Device limits apply when playing.",
            )}
      </p>
      <Button
        size="sm"
        variant="outline"
        disabled={busy || (mode === "title" ? !key : !names.trim())}
        onClick={() => {
          setRequested(true);
          setAttempt((n) => n + 1);
        }}
      >
        {busy ? t("Checking…") : requested ? t("Check again") : t("Preview")}
      </Button>
      <div aria-live="polite" className="space-y-2">
        {originalNote && <p className="text-sm text-fg-muted">{originalNote}</p>}
        {warning && (
          <p role="alert" className="text-sm text-danger">
            {warning}
          </p>
        )}
        {error && (
          <p role="alert" className="text-sm text-danger">
            {error}
          </p>
        )}
        {result?.length === 0 && <p className="text-sm text-fg-muted">{t("No releases found")}</p>}
        {result?.map((row, i) => (
          <div key={`${row.release.id}-${i}`} className="rounded-lg border border-border p-3">
            <div className="mb-2 flex items-start gap-2">
              <span className="min-w-0 flex-1 break-all text-xs font-medium">
                {row.release.name}
              </span>
              <Badge
                variant={
                  row.rejected ? "warning" : row.preferences?.fallback ? "secondary" : "success"
                }
              >
                {row.rejected
                  ? row.preferences?.unknown
                    ? t("Needs verification")
                    : t("Excluded")
                  : row.preferences?.fallback
                    ? t("Fallback")
                    : t("Matches")}
              </Badge>
            </div>
            {row.reason && <p className="mb-1 text-xs text-danger">{row.reason}</p>}
            {row.preferences?.explanations.map((e) => (
              <p key={e.id} className="text-xs text-fg-muted">
                <span className="font-medium">{e.name}</span>
                {" · "}
                {explanationLabel(
                  e.outcome,
                  e.state,
                  rules.some((rule) => rule.id === e.id && mentionsOriginal(rule)),
                )}
              </p>
            ))}
          </div>
        ))}
      </div>
    </div>
  );
}
