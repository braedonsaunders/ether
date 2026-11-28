import { SettingsSection, Spinner, toast } from "@braedonsaunders/appkit-ui";
import { Fragment, useCallback, useEffect, useState, type ReactNode } from "react";
import { useApp } from "../app-context";
import { t } from "../i18n";
import { api, ApiError } from "../lib/api";
import { useAsync } from "../lib/hooks";
import type { EtherConfig } from "../lib/types";
import {
  asObj,
  hasFieldError,
  SchemaField,
  useSettingsSchemas,
  type FieldDef,
  type FieldGroup,
} from "../pages/settings/SchemaForm";
import { ErrorPanel } from "./feedback";
import { SaveBar, useDraft } from "./SaveBar";

type Obj = Record<string, unknown>;

/**
 * Power-user controls for one group. Collapsed until the owner asks.
 * `label` is the accessible name (`More options: {title}`). `text` is the
 * visible label when a group needs a more specific name.
 */
export function MoreOptions({
  label,
  text,
  hasError = false,
  reveal = false,
  setting,
  children,
}: {
  label: string;
  /** Stable id a ⌘K setting jump uses to open these options. */
  setting?: string;
  text?: string;
  hasError?: boolean;
  /** Opens the options when it turns true, such as a link to them elsewhere. */
  reveal?: boolean;
  children: ReactNode;
}) {
  const [open, setOpen] = useState(false);
  useEffect(() => {
    if (hasError || reveal) setOpen(true);
  }, [hasError, reveal]);
  return (
    <div className="border-t border-border">
      <button
        type="button"
        className="w-full px-4 py-3 text-left text-sm font-medium text-primary"
        aria-expanded={open}
        aria-label={label}
        data-more={setting}
        onClick={() => setOpen((v) => !v)}
      >
        {open && !text ? t("Fewer options") : (text ?? t("More options"))}
      </button>
      {open ? children : null}
    </div>
  );
}

/** The draft's top-level keys rendered by `groups` (fields and More options). */
function pickShown(draft: Obj, groups: FieldGroup[]): Obj {
  const keys = new Set(groups.flatMap((g) => [...g.fields, ...(g.more ?? [])]).map((f) => f.key));
  return Object.fromEntries(Object.entries(draft).filter(([key]) => keys.has(key)));
}

/**
 * One owner config section (`config.json` key) edited in place: its field
 * groups, server-validated errors beside each field, and the shared save bar
 * while there are unsaved changes. Pages with tabs keep one mounted instance
 * and pass `show`, so a draft survives switching tabs.
 */
export function ConfigSection({
  configKey,
  endpoint,
  groups: givenGroups,
  schemaId,
  label,
  before,
  afterGroup,
  test,
  onSaved,
  hidden = false,
  show,
  fieldDisabled,
}: {
  /** Key in config.json, e.g. "cache" or "openSubtitles". */
  configKey: string;
  /**
   * `/config/<endpoint>` for sections the server saves on their own; omit to
   * save the whole config (sections this UI has no dedicated endpoint for).
   */
  endpoint?: string;
  /** Field groups built by the caller. Prefer schemaId for served schemas. */
  groups?: FieldGroup[];
  /**
   * Served settingsSchema descriptor id (or ids, concatenated in order)
   * whose groups render this section. The form waits for the registry
   * before showing fields; the draft loads independently.
   */
  schemaId?: string | readonly string[];
  /** Used in the "Settings saved" toast. */
  label: string;
  /** Status or context rendered above the fields, given the live draft. */
  before?: (draft: Obj, cfg: EtherConfig) => ReactNode;
  /** Content rendered right after a group's section (e.g. a related panel). */
  afterGroup?: (group: FieldGroup, index: number) => ReactNode;
  /** A connection test rendered under the fields, given the live draft. */
  test?: (draft: Obj) => ReactNode;
  onSaved?: (value: Obj) => void;
  /**
   * Keys edited on another page. The form still saves the whole draft, so these
   * values are kept. They are not rendered.
   */
  omit?: string[];
  /** Keep the form (and its unsaved edits) mounted without rendering it, e.g. on another tab. */
  hidden?: boolean;
  /** Which groups to render; all of them when omitted. Hidden groups keep their edits. */
  show?: (group: FieldGroup, index: number) => boolean;
  /** Disables fields that do not apply for the current draft while preserving their values. */
  fieldDisabled?: (field: FieldDef, draft: Obj) => boolean;
}) {
  const { refreshDashboard } = useApp();
  const schemaIds =
    schemaId === undefined ? [] : typeof schemaId === "string" ? [schemaId] : schemaId;
  const served = useSettingsSchemas(schemaIds);
  const groups = givenGroups ?? served.groups;
  const config = useAsync(() => api.getConfig(), []);
  const cfg = config.data;
  const stored = cfg ? asObj((cfg as unknown as Obj)[configKey]) : null;
  const { draft, setDraft, dirty, reset } = useDraft<Obj>(stored);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [error, setError] = useState<unknown>(null);
  const [saving, setSaving] = useState(false);

  const save = useCallback(async () => {
    if (!cfg || !draft || !groups) return;
    setSaving(true);
    setError(null);
    setErrors({});
    try {
      // Send only the keys this form shows: the server merges a section patch onto
      // the stored value, so another form editing the same section is never undone.
      if (endpoint) await api.putSection(endpoint, pickShown(draft, groups));
      else {
        // Merge only this form's keys into the live section: another form on
        // the page may have just saved the same section. The live `cache` is
        // the effective (possibly shared) policy, so it is only sent back by
        // the cache form itself; the server keeps the stored one otherwise.
        const live = (await api.getConfig()) as unknown as Obj;
        const { cache: _effectiveCache, ...rest } = live;
        await api.putConfig({
          ...rest,
          [configKey]: { ...asObj(live[configKey]), ...pickShown(draft, groups) },
        } as unknown as EtherConfig);
      }
      toast.success(t("Settings saved"), { description: label });
      await config.reload();
      void refreshDashboard();
      onSaved?.(draft);
    } catch (e) {
      setError(e);
      if (e instanceof ApiError && e.fields) {
        setErrors(Object.fromEntries(e.fields.map((f) => [f.field, f.message])));
      }
    } finally {
      setSaving(false);
    }
  }, [cfg, config, configKey, draft, endpoint, groups, label, onSaved, refreshDashboard]);

  const saveBar = (
    <SaveBar
      dirty={dirty}
      saving={saving}
      error={error}
      onSave={save}
      onDiscard={() => {
        reset();
        setErrors({});
        setError(null);
      }}
    />
  );
  if (hidden) return saveBar;
  if (config.error) return <ErrorPanel error={config.error} onRetry={() => void config.reload()} />;
  if (!givenGroups && served.error && !groups)
    return <ErrorPanel error={served.error} onRetry={() => void config.reload()} />;
  if (!cfg || !draft || !groups)
    return (
      <div className="py-10 text-center">
        <Spinner size={20} label={t("Loading settings")} />
      </div>
    );

  const change = (next: Obj) => setDraft(next);
  const renderField = (f: FieldDef, reactKey: string) => (
    <SchemaField
      key={reactKey}
      def={f}
      value={draft[f.key]}
      path={`${configKey}.${f.key}`}
      error={errors[`${configKey}.${f.key}`]}
      errors={errors}
      disabled={fieldDisabled?.(f, draft)}
      onChange={(v) => change({ ...draft, [f.key]: v })}
    />
  );
  return (
    <>
      {before?.(draft, cfg)}
      {groups.map((g, i) =>
        show && !show(g, i) ? null : (
          <Fragment key={g.id ?? g.title ?? `group-${i}`}>
            <SettingsSection
              title={g.title ?? (i === 0 ? label : undefined)}
              description={g.description}
            >
              {g.fields.map((f, j) => renderField(f, `${f.key}-${j}`))}
              {g.more?.length ? (
                <MoreOptions
                  label={
                    g.title ? t("More options: {title}", { title: g.title }) : t("More options")
                  }
                  hasError={g.more.some((f) => hasFieldError(f, errors, configKey))}
                >
                  {g.more.map((f, j) => renderField(f, `more-${f.key}-${j}`))}
                </MoreOptions>
              ) : null}
            </SettingsSection>
            {afterGroup?.(g, i)}
          </Fragment>
        ),
      )}
      {test ? <div className="flex justify-end">{test(draft)}</div> : null}
      {saveBar}
    </>
  );
}
