import { Badge, Button, Input, Label, cn } from "@braedonsaunders/appkit-ui";
import { CheckCircle2, Eye, EyeOff, PlugZap, XCircle } from "lucide-react";
import { forwardRef, useId, useState, type ComponentProps, type ReactNode } from "react";
import { t } from "../i18n";
import { REDACTED } from "../lib/types";

export function Field({
  label,
  hint,
  error,
  children,
  className,
  htmlFor,
}: {
  label: ReactNode;
  hint?: ReactNode;
  error?: string;
  children: ReactNode;
  className?: string;
  htmlFor?: string;
}) {
  return (
    <div className={cn("space-y-1.5", className)}>
      <Label htmlFor={htmlFor} className="block text-sm font-medium text-fg">
        {label}
      </Label>
      {children}
      {error ? (
        <p className="text-xs text-danger" role="alert">
          {error}
        </p>
      ) : hint ? (
        <p className="text-xs text-fg-muted">{hint}</p>
      ) : null}
    </div>
  );
}

/** Numeric input that never emits NaN. */
export function NumberInput({
  value,
  onChange,
  min,
  max,
  step,
  className,
  ...rest
}: {
  value: number;
  onChange: (n: number) => void;
  min?: number;
  max?: number;
  step?: number | "any";
  className?: string;
} & Omit<
  React.InputHTMLAttributes<HTMLInputElement>,
  "value" | "onChange" | "type" | "min" | "max" | "step"
>) {
  return (
    <Input
      {...rest}
      type="number"
      inputMode="decimal"
      className={cn("tabular-nums", className)}
      value={Number.isFinite(value) ? value : 0}
      min={min}
      max={max}
      step={step}
      onChange={(e) => {
        const n = e.currentTarget.valueAsNumber;
        onChange(Number.isFinite(n) ? n : 0);
      }}
    />
  );
}

/** Password field with the show/hide control inside the right edge. */
export const PasswordInput = forwardRef<
  HTMLInputElement,
  Omit<ComponentProps<typeof Input>, "type">
>(function PasswordInput({ className, disabled, ...rest }, ref) {
  const [shown, setShown] = useState(false);
  return (
    <div className={cn("relative", className)}>
      <Input
        ref={ref}
        {...rest}
        disabled={disabled}
        type={shown ? "text" : "password"}
        className="pr-10"
      />
      <button
        type="button"
        disabled={disabled}
        className="absolute inset-y-0 right-0 flex w-10 items-center justify-center rounded-r-md text-fg-muted hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/40 disabled:pointer-events-none disabled:opacity-50"
        onMouseDown={(e) => e.preventDefault()}
        onClick={() => setShown((s) => !s)}
        aria-label={shown ? t("Hide password") : t("Show password")}
        aria-pressed={shown}
      >
        {shown ? <EyeOff size={16} aria-hidden /> : <Eye size={16} aria-hidden />}
      </button>
    </div>
  );
});

/**
 * Password input that understands the server's redaction placeholder: the
 * stored secret arrives as "••••" and sending it back unchanged keeps it.
 */
export function SecretInput({
  value,
  onChange,
  placeholder,
  id,
  ariaLabel,
  invalid,
}: {
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
  id?: string;
  ariaLabel?: string;
  invalid?: boolean;
}) {
  const isStored = value === REDACTED;
  return (
    <div className="space-y-1">
      {isStored ? (
        <Input
          id={id}
          aria-label={ariaLabel}
          aria-invalid={invalid || undefined}
          type="text"
          value={value}
          placeholder={placeholder}
          autoComplete="new-password"
          spellCheck={false}
          onFocus={(e) => e.currentTarget.select()}
          onChange={(e) => onChange(e.currentTarget.value)}
        />
      ) : (
        <PasswordInput
          id={id}
          aria-label={ariaLabel}
          aria-invalid={invalid || undefined}
          value={value}
          placeholder={placeholder}
          autoComplete="new-password"
          spellCheck={false}
          onChange={(e) => onChange(e.currentTarget.value)}
        />
      )}
      {isStored ? (
        <p className="text-xs text-fg-muted">
          {t("Saved. Leave as is to keep it, or type a new value to replace it.")}
        </p>
      ) : null}
    </div>
  );
}

export interface TestState {
  status: "idle" | "running" | "ok" | "fail";
  message?: string;
  durationMs?: number;
}

export function TestButton({
  label = t("Test"),
  state,
  onTest,
  disabled,
}: {
  label?: string;
  state: TestState;
  onTest: () => void;
  disabled?: boolean;
}) {
  return (
    <span className="inline-flex flex-wrap items-center gap-2">
      <Button
        type="button"
        variant="outline"
        size="sm"
        onClick={onTest}
        disabled={disabled || state.status === "running"}
      >
        <PlugZap size={14} /> {state.status === "running" ? t("Testing…") : label}
      </Button>
      {state.status === "ok" ? (
        <Badge variant="success">
          <CheckCircle2 size={12} />{" "}
          {state.durationMs != null ? t("OK · {ms} ms", { ms: state.durationMs }) : t("OK")}
        </Badge>
      ) : null}
      {state.status === "fail" ? (
        <Badge variant="destructive" title={state.message}>
          <XCircle size={12} />{" "}
          {state.message ? t("Failed: {message}", { message: state.message }) : t("Failed")}
        </Badge>
      ) : null}
    </span>
  );
}

/** A row of toggle chips (weekdays, devices…). */
export function ChipGroup<T extends string | number>({
  options,
  value,
  onChange,
  ariaLabel,
  size = "md",
}: {
  options: { value: T; label: string }[];
  value: T[];
  onChange: (v: T[]) => void;
  ariaLabel: string;
  size?: "sm" | "md";
}) {
  return (
    <div role="group" aria-label={ariaLabel} className="flex flex-wrap gap-1.5">
      {options.map((o) => {
        const on = value.includes(o.value);
        return (
          <button
            key={String(o.value)}
            type="button"
            aria-pressed={on}
            onClick={() => onChange(on ? value.filter((v) => v !== o.value) : [...value, o.value])}
            className={cn(
              "rounded-full border font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50",
              size === "sm" ? "px-2.5 py-0.5 text-xs" : "px-3 py-1 text-sm",
              on
                ? "border-primary/40 bg-primary-subtle text-primary"
                : "border-border bg-surface text-fg-muted hover:border-border-strong hover:text-fg",
            )}
          >
            {o.label}
          </button>
        );
      })}
    </div>
  );
}

/** Single-choice segmented control rendered as a radio group (keyboard: arrows). */
export function Segmented<T extends string>({
  options,
  value,
  onChange,
  ariaLabel,
  className,
  size = "md",
}: {
  options: { value: T; label: ReactNode; icon?: ReactNode }[];
  value: T;
  onChange: (v: T) => void;
  ariaLabel: string;
  className?: string;
  size?: "sm" | "md";
}) {
  const name = useId();
  return (
    <div
      role="radiogroup"
      aria-label={ariaLabel}
      className={cn(
        "inline-flex flex-wrap gap-0.5 rounded-lg border border-border bg-bg-subtle p-0.5",
        className,
      )}
    >
      {options.map((o, i) => {
        const on = o.value === value;
        return (
          <button
            key={o.value}
            id={`${name}-${i}`}
            type="button"
            role="radio"
            aria-checked={on}
            tabIndex={on ? 0 : -1}
            onClick={() => onChange(o.value)}
            onKeyDown={(e) => {
              if (e.key !== "ArrowRight" && e.key !== "ArrowLeft") return;
              e.preventDefault();
              const next =
                options[(i + (e.key === "ArrowRight" ? 1 : options.length - 1)) % options.length];
              if (next) {
                onChange(next.value);
                (
                  e.currentTarget.parentElement?.children[options.indexOf(next)] as
                    HTMLElement | undefined
                )?.focus();
              }
            }}
            className={cn(
              "inline-flex items-center justify-center gap-1.5 rounded-md font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50",
              size === "sm" ? "h-7 px-2.5 text-xs" : "h-8 px-3 text-sm",
              on ? "bg-surface text-fg shadow-sm" : "text-fg-muted hover:text-fg",
            )}
          >
            {o.icon}
            {o.label}
          </button>
        );
      })}
    </div>
  );
}

/** Comma-separated list editor that commits on blur (languages, blocklists…). */
export function ListInput({
  value,
  onChange,
  placeholder,
  ariaLabel,
  numeric,
}: {
  value: (string | number)[] | null;
  onChange: (v: (string | number)[]) => void;
  placeholder?: string;
  ariaLabel?: string;
  numeric?: boolean;
}) {
  const text = (value ?? []).join(", ");
  return (
    <Input
      key={text}
      defaultValue={text}
      aria-label={ariaLabel}
      placeholder={placeholder ?? t("Comma-separated")}
      onBlur={(e) => {
        const parts = e.currentTarget.value
          .split(",")
          .map((s) => s.trim())
          .filter(Boolean);
        onChange(numeric ? parts.map(Number).filter((n) => Number.isFinite(n)) : parts);
      }}
    />
  );
}
