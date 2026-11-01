import { SettingsSection, Spinner, toast } from "@braedonsaunders/appkit-ui";
import { useCallback, useEffect, useState, type ReactNode } from "react";
import { useApp } from "../app-context";
import { t } from "../i18n";
import { api, ApiError } from "../lib/api";
import { useAsync } from "../lib/hooks";
import type { EtherConfig } from "../lib/types";
import { asObj, SchemaField, type FieldDef, type FieldGroup } from "../pages/settings/SchemaForm";
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
  children,
}: {
  label: string;
  text?: string;
  hasError?: boolean;
  children: ReactNode;
}) {
  const [open, setOpen] = useState(false);
  useEffect(() => {
    if (hasError) setOpen(true);
  }, [hasError]);
  return (
    <div className="border-t border-border">
      <button
        type="button"
        className="w-full px-4 py-3 text-left text-sm font-medium text-primary"
        aria-expanded={open}
        aria-label={label}
        onClick={() => setOpen((v) => !v)}
      >
        {text ?? t("More options")}
      </button>
      {open ? children : null}
    </div>
  );
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
  groups,
  label,
  before,
  after,
  test,
  onSaved,
  hidden = false,
  show,
  onFieldErrors,
}: {
  /** Key in config.json, e.g. "cache" or "openSubtitles". */
  configKey: string;
  /**
   * `/config/<endpoint>` for sections the server saves on their own; omit to
   * save the whole config (sections this UI has no dedicated endpoint for).
   */
  endpoint?: string;
  groups: FieldGroup[];
  /** Used in the "Settings saved" toast. */
  label: string;
  /** Status or context rendered above the fields, given the live draft. */
  before?: (draft: Obj, cfg: EtherConfig) => ReactNode;
  after?: (draft: Obj, cfg: EtherConfig) => ReactNode;
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
  /** Field keys (without the section prefix) the server rejected, to reveal their tab. */
  onFieldErrors?: (keys: string[]) => void;
}) {
  const { refreshDashboard } = useApp();
  const config = useAsync(() => api.getConfig(), []);
  const cfg = config.data;
  const stored = cfg ? asObj((cfg as unknown as Obj)[configKey]) : null;
  const { draft, setDraft, dirty, reset } = useDraft<Obj>(stored);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [error, setError] = useState<unknown>(null);
  const [saving, setSaving] = useState(false);

  const save = useCallback(async () => {
    if (!cfg || !draft) return;
    setSaving(true);
    setError(null);
    setErrors({});
    try {
      if (endpoint) await api.putSection(endpoint, draft);
      else {
        // Merge into the live config: another form on the page may have just saved.
        const live = await api.getConfig();
        await api.putConfig({ ...live, [configKey]: draft } as EtherConfig);
      }
      toast.success(t("Settings saved"), { description: label });
      await config.reload();
      void refreshDashboard();
      onSaved?.(draft);
    } catch (e) {
      setError(e);
      if (e instanceof ApiError && e.fields) {
        setErrors(Object.fromEntries(e.fields.map((f) => [f.field, f.message])));
        const prefix = `${configKey}.`;
        onFieldErrors?.(
          e.fields
            .filter((f) => f.field.startsWith(prefix))
            .map((f) => f.field.slice(prefix.length)),
        );
      }
    } finally {
      setSaving(false);
    }
  }, [cfg, config, configKey, draft, endpoint, label, onFieldErrors, onSaved, refreshDashboard]);

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
  if (!cfg || !draft)
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
      onChange={(v) => change({ ...draft, [f.key]: v })}
    />
  );
  return (
    <>
      {before?.(draft, cfg)}
      {groups.map((g, i) =>
        show && !show(g, i) ? null : (
          <SettingsSection
            key={g.id ?? g.title ?? `group-${i}`}
            title={g.title ?? (i === 0 ? label : undefined)}
            description={g.description}
          >
            {g.fields.map((f, j) => renderField(f, `${f.key}-${j}`))}
            {g.more?.length ? (
              <MoreOptions
                label={g.title ? t("More options: {title}", { title: g.title }) : t("More options")}
                hasError={g.more.some((f) => Boolean(errors[`${configKey}.${f.key}`]))}
              >
                {g.more.map((f, j) => renderField(f, `more-${f.key}-${j}`))}
              </MoreOptions>
            ) : null}
          </SettingsSection>
        ),
      )}
      {test ? <div className="flex justify-end">{test(draft)}</div> : null}
      {after?.(draft, cfg)}
      {saveBar}
    </>
  );
}
