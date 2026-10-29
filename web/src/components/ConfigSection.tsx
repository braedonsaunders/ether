import { SettingsSection, Spinner, toast } from "@braedonsaunders/appkit-ui";
import { useCallback, useState, type ReactNode } from "react";
import { useApp } from "../app-context";
import { t } from "../i18n";
import { api, ApiError } from "../lib/api";
import { useAsync } from "../lib/hooks";
import type { EtherConfig } from "../lib/types";
import { asObj, GenericFields, SchemaField, type FieldGroup } from "../pages/settings/SchemaForm";
import { ErrorPanel } from "./feedback";
import { SaveBar, useDraft } from "./SaveBar";

type Obj = Record<string, unknown>;

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
  omit,
  hidden = false,
  show,
  showAdvanced = true,
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
  /** Keys edited elsewhere: kept in the saved value, never shown under "Advanced". */
  omit?: string[];
  /** Keep the form (and its unsaved edits) mounted without rendering it, e.g. on another tab. */
  hidden?: boolean;
  /** Which groups to render; all of them when omitted. Hidden groups keep their edits. */
  show?: (group: FieldGroup, index: number) => boolean;
  /** Whether to render the "Advanced" block of options this UI has no schema for. */
  showAdvanced?: boolean;
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

  const known = new Set([...groups.flatMap((g) => g.fields.map((f) => f.key)), ...(omit ?? [])]);
  const extra = Object.keys(draft).filter((k) => !known.has(k));
  const change = (next: Obj) => setDraft(next);
  return (
    <>
      {before?.(draft, cfg)}
      {groups.map((g, i) =>
        show && !show(g, i) ? null : (
          <SettingsSection
            key={g.title ?? `group-${i}`}
            title={g.title ?? (i === 0 ? label : undefined)}
            description={g.description}
          >
            {g.fields.map((f) => (
              <SchemaField
                key={f.key}
                def={f}
                value={draft[f.key]}
                path={`${configKey}.${f.key}`}
                error={errors[`${configKey}.${f.key}`]}
                errors={errors}
                onChange={(v) => change({ ...draft, [f.key]: v })}
              />
            ))}
          </SettingsSection>
        ),
      )}
      {extra.length && showAdvanced ? (
        <SettingsSection
          title={t("Advanced")}
          description={t("Additional options this server supports.")}
        >
          <GenericFields
            value={draft}
            keys={extra}
            onChange={change}
            errors={errors}
            prefix={configKey}
          />
        </SettingsSection>
      ) : null}
      {test ? <div className="flex justify-end">{test(draft)}</div> : null}
      {after?.(draft, cfg)}
      {saveBar}
    </>
  );
}
