import { t } from "../../i18n";
import { Badge, Button, EmptyState, confirmDialog, toast } from "@braedonsaunders/appkit-ui";
import { BellRing, Pencil, Plus, Send, Smartphone, Trash2 } from "lucide-react";
import { useState } from "react";
import { Section } from "../../components/layout";
import { apiFetch, errorMessage } from "../../lib/api";
import { BrowserPush } from "./BrowserPush";
import { ChannelDrawer } from "./ChannelDrawer";
import {
  blankChannel,
  channelName,
  eventLabel,
  transportLabel,
  type Channel,
  type NotificationsData,
} from "./types";

/** Where alerts go: one row per channel, each edited in a drawer. */
export function Channels({
  data,
  onChanged,
}: {
  data: NotificationsData;
  onChanged: () => Promise<void>;
}) {
  const [editing, setEditing] = useState<{ channel: Channel; isNew: boolean } | null>(null);
  const [open, setOpen] = useState(false);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<unknown>(null);
  const [busy, setBusy] = useState<string | null>(null);

  const edit = (c: Channel, isNew = false) => {
    setEditing({ channel: c, isNew });
    setSaveError(null);
    setOpen(true);
  };

  const save = async (channel: Channel) => {
    setSaving(true);
    setSaveError(null);
    try {
      await apiFetch(`/notifications/${channel.id}`, {
        method: "PUT",
        body: JSON.stringify(channel),
      });
      toast.success(t("Channel saved"));
      setOpen(false);
      await onChanged();
    } catch (e) {
      setSaveError(e);
    } finally {
      setSaving(false);
    }
  };

  const run = async (id: string, fn: () => Promise<void>) => {
    setBusy(id);
    try {
      await fn();
      await onChanged();
    } catch (e) {
      toast.error(errorMessage(e));
      await onChanged();
    } finally {
      setBusy(null);
    }
  };

  const test = (c: Channel) =>
    run(c.id, async () => {
      await apiFetch(`/notifications/${c.id}/test`, { method: "POST", body: "{}" });
      toast.success(t("Test accepted by destination"), {
        description: t("Check your inbox or device. The result is saved in Delivery history."),
      });
    });

  const remove = async (c: Channel) => {
    const ok = await confirmDialog({
      title: t("Delete channel?"),
      message: t("{channel} stops receiving alerts. Its delivery history is kept.", {
        channel: channelName(c),
      }),
      confirmLabel: t("Delete"),
      tone: "danger",
    });
    if (ok)
      await run(c.id, async () => {
        await apiFetch(`/notifications/${c.id}`, { method: "DELETE" });
        toast.success(t("Channel deleted"));
      });
  };

  return (
    <>
      <Section
        title={t("Channels")}
        description={t("Alerts for playback, sources and the server.")}
        actions={
          <Button size="sm" onClick={() => edit(blankChannel(), true)}>
            <Plus size={14} /> {t("New channel")}
          </Button>
        }
        bodyClassName={data.channels.length ? "divide-y divide-border" : undefined}
      >
        {data.channels.length ? (
          data.channels.map((c) => (
            <div key={c.id} className="flex flex-wrap items-center justify-between gap-3 px-5 py-4">
              <div className="min-w-0 space-y-1">
                <div className="flex flex-wrap items-center gap-2">
                  <p className="font-medium text-fg">{channelName(c)}</p>
                  <Badge variant="secondary">{transportLabel(c.kind)}</Badge>
                  <Badge variant={c.enabled ? "success" : "outline"}>
                    {c.enabled ? t("On") : t("Off")}
                  </Badge>
                </div>
                <p className="text-xs text-fg-muted">
                  {c.events.length ? c.events.map(eventLabel).join(", ") : t("No events selected")}
                  {c.digestMinutes
                    ? ` · ${t("digest every {minutes} min", { minutes: c.digestMinutes })}`
                    : ""}
                </p>
              </div>
              <div className="flex flex-wrap gap-2">
                <Button
                  size="sm"
                  variant="ghost"
                  disabled={busy === c.id}
                  onClick={() =>
                    void run(c.id, async () => {
                      await apiFetch(`/notifications/${c.id}`, {
                        method: "PUT",
                        body: JSON.stringify({ ...c, enabled: !c.enabled }),
                      });
                    })
                  }
                >
                  {c.enabled ? t("Pause") : t("Resume")}
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={busy === c.id}
                  onClick={() => void test(c)}
                >
                  <Send size={14} /> {t("Send test")}
                </Button>
                <Button variant="outline" size="sm" onClick={() => edit(c)}>
                  <Pencil size={14} /> {t("Edit")}
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  disabled={busy === c.id}
                  onClick={() => void remove(c)}
                  aria-label={t("Delete {channel}", { channel: channelName(c) })}
                >
                  <Trash2 size={14} />
                </Button>
              </div>
            </div>
          ))
        ) : (
          <EmptyState
            icon={<BellRing />}
            title={t("No channels yet")}
            description={t(
              "Add a webhook, Discord, Slack, Telegram, email, ntfy or Gotify channel to get alerts.",
            )}
          />
        )}
      </Section>

      <BrowserPush data={data} onChanged={onChanged} />
      <Section
        title={t("Alerts on your phone")}
        description={t("Choose where you want to receive background alerts.")}
      >
        <div className="flex items-start gap-3 text-sm text-fg-muted">
          <Smartphone size={18} className="mt-0.5 shrink-0" />
          <p>{t("Use browser push, or an ntfy, Gotify, Telegram or email channel.")}</p>
        </div>
      </Section>

      {editing ? (
        <ChannelDrawer
          open={open}
          channel={editing.channel}
          isNew={editing.isNew}
          events={data.events}
          catalog={data.eventCatalog}
          saving={saving}
          error={saveError}
          onClose={() => setOpen(false)}
          onSave={(c) => void save(c)}
        />
      ) : null}
    </>
  );
}
