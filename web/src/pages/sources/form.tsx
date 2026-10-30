import { t } from "../../i18n";
import { Button, Input, Switch, toast } from "@braedonsaunders/appkit-ui";
import { ArrowDown, ArrowUp, Pencil, Trash2 } from "lucide-react";
import { useCallback, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { useApp } from "../../app-context";
import { Drawer } from "../../components/Drawer";
import { MutationError } from "../../components/feedback";
import { NumberInput, type TestState } from "../../components/fields";
import { LevelBadge, StatusDot } from "../../components/status";
import { SettingsRow } from "../../components/SettingsRow";
import { ApiError, errorMessage } from "../../lib/api";
import type { HealthItem } from "../../lib/health";

/** Shared building blocks for the Streaming pages (Sources, Releases, Playback). */

export type Errors = Record<string, string>;

export function swap<T>(list: T[], i: number, j: number): T[] {
  if (j < 0 || j >= list.length) return list;
  const next = [...list];
  [next[i], next[j]] = [next[j] as T, next[i] as T];
  return next;
}

/** Server field errors for one record, keyed by the field name after `prefix`. */
export function recordErrors(e: unknown, prefix: string): Errors {
  if (!(e instanceof ApiError) || !e.fields) return {};
  const out: Errors = {};
  for (const f of e.fields) {
    if (f.field.startsWith(prefix)) out[f.field.slice(prefix.length)] = f.message;
    else out[f.field] = f.message;
  }
  return out;
}

/** Report a failed list action (reorder, remove) without losing the page. */
export function toastError(title: string, e: unknown) {
  toast.error(title, { description: errorMessage(e) });
}

function Help({ help, error }: { help?: ReactNode; error?: string }) {
  if (!help && !error) return null;
  return (
    <>
      {help}
      {error ? (
        <span className="block text-xs text-danger" role="alert">
          {error}
        </span>
      ) : null}
    </>
  );
}

export function TextRow({
  label,
  value,
  onChange,
  error,
  placeholder,
  help,
  type,
}: {
  label: string;
  value: string;
  onChange: (v: string) => void;
  error?: string;
  placeholder?: string;
  help?: ReactNode;
  type?: string;
}) {
  return (
    <SettingsRow
      title={label}
      description={<Help help={help} error={error} />}
      control={
        <Input
          aria-label={label}
          aria-invalid={error ? true : undefined}
          type={type}
          value={value}
          placeholder={placeholder}
          onChange={(e) => onChange(e.currentTarget.value)}
          className="sm:w-72"
          spellCheck={false}
        />
      }
    />
  );
}

export function NumRow({
  label,
  value,
  onChange,
  error,
  help,
  min,
  max,
  unit,
}: {
  label: string;
  value: number;
  onChange: (n: number) => void;
  error?: string;
  help?: ReactNode;
  min?: number;
  max?: number;
  unit?: string;
}) {
  return (
    <SettingsRow
      title={label}
      description={<Help help={help} error={error} />}
      control={
        <span className="flex items-center gap-2">
          <NumberInput
            aria-label={label}
            value={value}
            min={min}
            max={max}
            step={1}
            className="w-28"
            onChange={(n) => onChange(Math.round(n))}
          />
          {unit ? <span className="text-sm text-fg-muted">{unit}</span> : null}
        </span>
      }
    />
  );
}

export function BoolRow({
  label,
  value,
  onChange,
  help,
}: {
  label: string;
  value: boolean;
  onChange: (v: boolean) => void;
  help?: ReactNode;
}) {
  return (
    <SettingsRow
      title={label}
      description={help}
      control={
        <Switch
          checked={value}
          onChange={(e) => onChange(e.currentTarget.checked)}
          aria-label={label}
        />
      }
    />
  );
}

export function OrderButtons({
  label,
  i,
  n,
  onMove,
  disabled,
}: {
  label: string;
  i: number;
  n: number;
  onMove: (d: -1 | 1) => void;
  disabled?: boolean;
}) {
  return (
    <span className="flex">
      <Button
        variant="ghost"
        size="icon"
        className="size-8"
        onClick={() => onMove(-1)}
        disabled={disabled || i === 0}
        aria-label={t("Move {name} up", { name: label })}
      >
        <ArrowUp size={14} />
      </Button>
      <Button
        variant="ghost"
        size="icon"
        className="size-8"
        onClick={() => onMove(1)}
        disabled={disabled || i === n - 1}
        aria-label={t("Move {name} down", { name: label })}
      >
        <ArrowDown size={14} />
      </Button>
    </span>
  );
}

/** One record in a list: summary on the left, status and actions on the right. */
export function RecordItem({
  title,
  description,
  meta,
  editLabel,
  onEdit,
  order,
}: {
  title: ReactNode;
  description?: ReactNode;
  meta?: ReactNode;
  /** Accessible name of the edit button, e.g. "Edit Primary news". */
  editLabel: string;
  onEdit: () => void;
  order?: ReactNode;
}) {
  return (
    <li className="flex flex-wrap items-center gap-3 px-5 py-3">
      <div className="min-w-0 flex-1 basis-48">
        <p className="truncate text-sm font-medium text-fg">{title}</p>
        {description ? <p className="text-xs text-fg-muted">{description}</p> : null}
      </div>
      {meta ? <div className="flex flex-wrap items-center gap-2">{meta}</div> : null}
      <div className="flex items-center gap-1">
        {order}
        <Button variant="outline" size="sm" onClick={onEdit} aria-label={editLabel}>
          <Pencil size={13} /> {t("Edit")}
        </Button>
      </div>
    </li>
  );
}

/** Connection test state per record id. */
export function useTests() {
  const [tests, setTests] = useState<Record<string, TestState>>({});
  const runTest = useCallback(
    (key: string, fn: () => Promise<{ ok: boolean; error?: string; durationMs: number }>) => {
      setTests((s) => ({ ...s, [key]: { status: "running" } }));
      fn().then(
        (r) =>
          setTests((s) => ({
            ...s,
            [key]: r.ok
              ? { status: "ok", durationMs: r.durationMs }
              : { status: "fail", message: r.error ?? t("failed") },
          })),
        (e) =>
          setTests((s) => ({ ...s, [key]: { status: "fail", message: (e as Error).message } })),
      );
    },
    [],
  );
  const reset = useCallback((key: string) => {
    setTests((s) => {
      const next = { ...s };
      delete next[key];
      return next;
    });
  }, []);
  return { tests, runTest, reset };
}

/** Live health for the parts of the server a page controls, shown above its tabs. */
export function HealthStrip({ ids }: { ids: HealthItem["id"][] }) {
  const { health, dashboard } = useApp();
  if (!dashboard) return null;
  const items = health.filter((h) => ids.includes(h.id));
  if (!items.length) return null;
  return (
    <ul aria-label={t("Status")} className="grid gap-3 sm:grid-cols-2">
      {items.map((h) => (
        <li key={h.id}>
          <Link
            to={h.href}
            className="flex items-start gap-3 rounded-xl border border-border bg-surface px-4 py-3 transition-colors hover:bg-surface-hover"
          >
            <StatusDot level={h.level} className="mt-1.5" />
            <span className="min-w-0 flex-1">
              <span className="flex items-center justify-between gap-2 text-sm font-medium text-fg">
                {h.label}
                <LevelBadge level={h.level} label={h.statusLabel} />
              </span>
              <span className="block truncate text-xs text-fg-muted">{h.summary}</span>
            </span>
          </Link>
        </li>
      ))}
    </ul>
  );
}

/**
 * Create/edit one record (provider, indexer, profile, rule) in a drawer. The
 * draft lives in the caller; closing without saving discards it.
 */
export function RecordDrawer({
  open,
  title,
  description,
  onClose,
  onSave,
  saving = false,
  saveLabel,
  saveDisabled,
  onRemove,
  removeLabel = t("Remove"),
  removeDisabled,
  tools,
  error,
  children,
}: {
  open: boolean;
  title: ReactNode;
  description?: ReactNode;
  onClose: () => void;
  onSave: () => void;
  saving?: boolean;
  saveLabel: string;
  /** Holds the save back until the record is ready, e.g. a preview has loaded. */
  saveDisabled?: boolean;
  /** Shown for existing records only. */
  onRemove?: () => void;
  removeLabel?: string;
  removeDisabled?: boolean;
  /** Extra footer controls on the left, e.g. a connection test. */
  tools?: ReactNode;
  error?: unknown;
  children: ReactNode;
}) {
  return (
    <Drawer
      open={open}
      onClose={onClose}
      title={title}
      description={description}
      size="lg"
      footer={
        <div className="flex w-full flex-wrap items-center gap-2">
          {tools}
          {onRemove ? (
            <Button
              variant="ghost"
              size="sm"
              className="text-danger"
              onClick={onRemove}
              disabled={saving || removeDisabled}
            >
              <Trash2 size={14} /> {removeLabel}
            </Button>
          ) : null}
          <span className="flex-1" />
          <Button variant="ghost" size="sm" onClick={onClose} disabled={saving}>
            {t("Cancel")}
          </Button>
          <Button size="sm" onClick={onSave} disabled={saving || saveDisabled}>
            {saving ? t("Saving…") : saveLabel}
          </Button>
        </div>
      }
    >
      <div className="space-y-4">
        {error ? <MutationError error={error} /> : null}
        {children}
      </div>
    </Drawer>
  );
}
