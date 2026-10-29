import { localized, t } from "../../i18n";
import { Button, Input, Select, Switch } from "@braedonsaunders/appkit-ui";
import { useEffect, useId, useState } from "react";
import { Drawer } from "../../components/Drawer";
import { Field, NumberInput, SecretInput } from "../../components/fields";
import { MutationError } from "../../components/feedback";
import {
  TRANSPORTS,
  TRANSPORT_HELP,
  CRITICAL_EVENTS,
  eventLabel,
  transportLabel,
  type Channel,
} from "./types";

const SMTP_FIELDS = localized(
  () =>
    [
      { key: "username", label: t("Username") },
      { key: "password", label: t("Password") },
      { key: "from", label: t("From address") },
      { key: "to", label: t("To addresses") },
    ] as const,
);

/** Create or edit one notification channel. */
export function ChannelDrawer({
  open,
  channel: initial,
  isNew,
  events,
  saving,
  error,
  catalog,
  onClose,
  onSave,
}: {
  open: boolean;
  channel: Channel;
  isNew: boolean;
  /** Event kinds the server can publish. */
  events: string[];
  saving: boolean;
  error: unknown;
  catalog?: { kind: string; description: string; automatic: boolean }[];
  onClose: () => void;
  onSave: (channel: Channel) => void;
}) {
  const id = useId();
  const [channel, setChannel] = useState(initial);
  useEffect(() => setChannel(initial), [initial]);
  const patch = (p: Partial<Channel>) => setChannel((v) => ({ ...v, ...p }));

  return (
    <Drawer
      open={open}
      onClose={onClose}
      title={isNew ? t("New notification channel") : t("Edit notification channel")}
      description={t(
        "Saved endpoints and credentials show as [configured]; leave them as they are to keep them.",
      )}
      size="lg"
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={onClose} disabled={saving}>
            {t("Cancel")}
          </Button>
          <Button onClick={() => onSave(channel)} disabled={saving}>
            {saving ? t("Saving…") : t("Save channel")}
          </Button>
        </div>
      }
    >
      <div className="space-y-6">
        {error ? <MutationError error={error} /> : null}
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label={t("Name")} htmlFor={`${id}-name`}>
            <Input
              id={`${id}-name`}
              value={channel.name}
              placeholder={transportLabel(channel.kind)}
              onChange={(e) => patch({ name: e.target.value })}
            />
          </Field>
          <Field label={t("Transport")} htmlFor={`${id}-kind`}>
            <Select
              id={`${id}-kind`}
              aria-label={t("Transport")}
              value={channel.kind}
              disabled={channel.kind === "webpush" || !isNew}
              onChange={(e) =>
                patch({
                  kind: e.target.value,
                  events: channel.events.filter(
                    (k) => k !== "newsletter.weekly" || e.target.value === "smtp",
                  ),
                })
              }
            >
              {channel.kind === "webpush" ? (
                <option value="webpush">{t("Browser push")}</option>
              ) : null}
              {TRANSPORTS.map((v) => (
                <option key={v} value={v}>
                  {transportLabel(v)}
                </option>
              ))}
            </Select>
          </Field>
          <p className="text-sm text-fg-muted sm:col-span-2">{TRANSPORT_HELP[channel.kind]}</p>
          {channel.kind !== "telegram" && channel.kind !== "webpush" ? (
            <Field
              label={channel.kind === "smtp" ? t("SMTP host:port") : t("Endpoint URL")}
              hint={channel.kind === "smtp" ? t("The server must support TLS.") : undefined}
              htmlFor={`${id}-endpoint`}
              className="sm:col-span-2"
            >
              <SecretInput
                id={`${id}-endpoint`}
                value={channel.endpoint}
                onChange={(v) => patch({ endpoint: v })}
              />
            </Field>
          ) : null}
          {channel.kind !== "smtp" && channel.kind !== "webpush" ? (
            <Field
              label={channel.kind === "telegram" ? t("Bot token") : t("Access token")}
              hint={
                channel.kind === "telegram"
                  ? undefined
                  : t("Optional. A bearer token, or the app key for Gotify.")
              }
              htmlFor={`${id}-token`}
            >
              <SecretInput
                id={`${id}-token`}
                value={channel.token}
                onChange={(v) => patch({ token: v })}
              />
            </Field>
          ) : null}
          {channel.kind === "telegram" ? (
            <Field label={t("Chat ID")} htmlFor={`${id}-target`}>
              <Input
                id={`${id}-target`}
                value={channel.target}
                onChange={(e) => patch({ target: e.target.value })}
              />
            </Field>
          ) : null}
          {channel.kind === "smtp" ? (
            <Field label={t("Connection security")} htmlFor={`${id}-security`}>
              <Select
                id={`${id}-security`}
                aria-label={t("Connection security")}
                value={channel.smtpMode || (channel.endpoint.endsWith(":465") ? "tls" : "starttls")}
                onChange={(e) => patch({ smtpMode: e.target.value })}
              >
                <option value="starttls">{t("STARTTLS (usually 587)")}</option>
                <option value="tls">{t("Implicit TLS (usually 465)")}</option>
              </Select>
            </Field>
          ) : null}
          {channel.kind === "smtp"
            ? SMTP_FIELDS.map((f) => (
                <Field key={f.key} label={f.label} htmlFor={`${id}-${f.key}`}>
                  {f.key === "password" ? (
                    <SecretInput
                      id={`${id}-${f.key}`}
                      value={channel.password}
                      onChange={(v) => patch({ password: v })}
                    />
                  ) : (
                    <Input
                      id={`${id}-${f.key}`}
                      value={channel[f.key]}
                      onChange={(e) => patch({ [f.key]: e.target.value })}
                    />
                  )}
                </Field>
              ))
            : null}
          <Field label={t("Most deliveries per minute")} htmlFor={`${id}-rate`}>
            <NumberInput
              id={`${id}-rate`}
              min={1}
              max={60}
              value={channel.perMinute}
              onChange={(n) => patch({ perMinute: n })}
            />
          </Field>
          <Field
            label={t("Digest delay (minutes)")}
            hint={t("Collect alerts and send them together. 0 sends each one right away.")}
            htmlFor={`${id}-digest`}
          >
            <NumberInput
              id={`${id}-digest`}
              min={0}
              max={10080}
              value={channel.digestMinutes}
              onChange={(n) => patch({ digestMinutes: n })}
            />
          </Field>
        </div>

        <label className="flex items-center justify-between gap-3 rounded-lg border border-border px-4 py-3">
          <span>
            <span className="block text-sm font-medium text-fg">{t("Enabled")}</span>
            <span className="block text-xs text-fg-muted">
              {t("Turn off to stop sending without deleting the channel.")}
            </span>
          </span>
          <Switch
            checked={channel.enabled}
            onChange={(e) => patch({ enabled: e.target.checked })}
          />
        </label>

        <fieldset className="space-y-2">
          <legend className="text-sm font-medium text-fg">{t("Send these events")}</legend>
          <p className="text-xs text-fg-muted">
            {t(
              "Choose exactly what reaches this destination. Server lifecycle alerts bypass digest delays. Weekly reports are delivered by email.",
            )}
          </p>
          <div className="flex flex-wrap gap-2 py-2">
            <Button
              size="sm"
              variant="outline"
              onClick={() => patch({ events: CRITICAL_EVENTS.filter((k) => events.includes(k)) })}
            >
              {t("Critical alerts")}
            </Button>
            <Button
              size="sm"
              variant="outline"
              onClick={() =>
                patch({
                  events: events.filter(
                    (k) =>
                      k !== "test" &&
                      (k !== "newsletter.weekly" || channel.kind === "smtp") &&
                      (catalog?.find((e) => e.kind === k)?.automatic ??
                        !["cert.renewed", "update.available", "user.joined"].includes(k)),
                  ),
                })
              }
            >
              {t("All automatic events")}
            </Button>
            <Button size="sm" variant="ghost" onClick={() => patch({ events: [] })}>
              {t("Clear")}
            </Button>
          </div>
          <div className="grid gap-x-6 gap-y-2 pt-1 sm:grid-cols-2">
            {events
              .filter((k) => k !== "test")
              .map((k) => (
                <label key={k} className="flex items-center justify-between gap-3 py-1 text-sm">
                  <span className="text-fg">
                    {eventLabel(k)}
                    <span className="block text-xs text-fg-muted">
                      {catalog?.find((e) => e.kind === k)?.description ??
                        (["cert.renewed", "update.available", "user.joined"].includes(k)
                          ? t("Owner API integration only; not produced automatically.")
                          : "")}
                      {k === "newsletter.weekly" && channel.kind !== "smtp"
                        ? " " + t("Email channels only.")
                        : ""}
                    </span>
                  </span>
                  <Switch
                    aria-label={eventLabel(k)}
                    disabled={k === "newsletter.weekly" && channel.kind !== "smtp"}
                    checked={
                      (k !== "newsletter.weekly" || channel.kind === "smtp") &&
                      (channel.events.includes(k) || channel.events.includes("*"))
                    }
                    onChange={(e) =>
                      patch({
                        events: e.target.checked
                          ? [...channel.events.filter((v) => v !== "*"), k]
                          : channel.events.includes("*")
                            ? events.filter(
                                (v) =>
                                  v !== "test" &&
                                  v !== k &&
                                  (v !== "newsletter.weekly" || channel.kind === "smtp"),
                              )
                            : channel.events.filter((v) => v !== k),
                      })
                    }
                  />
                </label>
              ))}
          </div>
        </fieldset>
      </div>
    </Drawer>
  );
}
