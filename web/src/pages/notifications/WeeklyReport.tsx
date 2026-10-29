import { getLocale, t } from "../../i18n";
import {
  Alert,
  AlertDescription,
  AlertTitle,
  Button,
  Select,
  SettingsSection,
  Switch,
  toast,
} from "@braedonsaunders/appkit-ui";
import { Download, Eye, RefreshCw } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { Section } from "../../components/layout";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { SettingsRow } from "../../components/SettingsRow";
import { API_BASE, api, apiFetch, errorMessage, getToken } from "../../lib/api";
import { WEEKDAYS_LONG } from "../../lib/format";
import { useAsync } from "../../lib/hooks";
import type { EtherUser } from "../../lib/types";
import { viewerLabel } from "../activity/types";
import type { NewsletterSettings, NotificationsData } from "./types";

const HOURS = Array.from({ length: 24 }, (_, h) => h);

function scheduleSummary(s: NewsletterSettings): string {
  const next =
    s.enabled && s.nextDue
      ? t("Next report {time} UTC.", {
          time: new Date(s.nextDue * 1000).toLocaleString(getLocale(), {
            timeZone: "UTC",
            dateStyle: "full",
            timeStyle: "short",
          }),
        }) + " "
      : "";
  const last = s.lastAttempt
    ? t("Last sent {time} · {status}.", {
        time: new Date(s.lastAttempt * 1000).toLocaleString(getLocale()),
        status: s.lastStatus,
      })
    : t("No weekly reports sent yet.");
  return next + last;
}

/** The report preview, rendered from the server's HTML in a sandboxed frame. */
function Preview({ userId }: { userId: number }) {
  const [html, setHtml] = useState("");
  const [loading, setLoading] = useState(false);

  const load = async () => {
    setLoading(true);
    try {
      const res = await fetch(`${API_BASE}/stats/newsletter?user=${userId}`, {
        headers: { "X-Plex-Token": getToken() },
      });
      if (!res.ok) throw new Error(t("The weekly report isn't available right now."));
      setHtml(await res.text());
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      setLoading(false);
    }
  };

  const download = () => {
    const url = URL.createObjectURL(new Blob([html], { type: "text/html" }));
    const a = document.createElement("a");
    a.href = url;
    a.download = "ether-week.html";
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  };

  return (
    <Section
      title={t("Preview")}
      description={t("The last seven days as the report would look today.")}
      actions={
        <>
          {html ? (
            <Button variant="ghost" size="sm" onClick={download}>
              <Download size={14} /> {t("Download HTML")}
            </Button>
          ) : null}
          <Button variant="outline" size="sm" disabled={loading} onClick={() => void load()}>
            {html ? <RefreshCw size={14} /> : <Eye size={14} />}{" "}
            {html ? t("Refresh preview") : t("Preview weekly report")}
          </Button>
        </>
      }
      bodyClassName={html ? "bg-bg-subtle p-3" : undefined}
    >
      {html ? (
        <iframe
          title={t("Weekly newsletter preview")}
          className="h-[32rem] w-full rounded-lg border border-border bg-white"
          sandbox=""
          srcDoc={html}
        />
      ) : (
        <p className="text-sm text-fg-muted">
          {loading
            ? t("Building the preview…")
            : t("Preview the report for the viewer chosen above.")}
        </p>
      )}
    </Section>
  );
}

/** Weekly report schedule, edited in place, with an inline preview. */
export function WeeklyReport({ notifications }: { notifications: NotificationsData | undefined }) {
  const settings = useAsync(() => apiFetch<NewsletterSettings>("/stats/newsletter/settings"), []);
  const viewers = useAsync<EtherUser[]>(() => api.users(), []);
  const { draft, setDraft, dirty, reset } = useDraft(settings.data);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);

  if (settings.error && !settings.data)
    return <ErrorPanel error={settings.error} onRetry={() => void settings.reload()} />;
  if (!draft) return <LoadingRows rows={3} />;

  const edit = (p: Partial<NewsletterSettings>) => setDraft({ ...draft, ...p });
  const save = async () => {
    setSaving(true);
    setError(null);
    try {
      await apiFetch("/stats/newsletter/settings", {
        method: "PUT",
        body: JSON.stringify(draft),
      });
      toast.success(t("Weekly schedule saved"));
      await settings.reload();
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  };

  const people = viewers.data ?? [];
  const hasEmailChannel = notifications?.channels.some(
    (c) =>
      c.enabled &&
      c.kind === "smtp" &&
      (c.events.includes("newsletter.weekly") || c.events.includes("*")),
  );
  return (
    <>
      {draft.enabled && notifications && !hasEmailChannel ? (
        <Alert variant="warning">
          <AlertTitle>{t("No email channel for the weekly report")}</AlertTitle>
          <AlertDescription>
            {t(
              "Reports are sent through an Email (SMTP) channel with the Weekly newsletter event turned on.",
            )}{" "}
            <Link to="/notifications/channels" className="font-medium underline">
              {t("Set up a channel")}
            </Link>
          </AlertDescription>
        </Alert>
      ) : null}
      <SettingsSection
        title={t("Weekly email")}
        description={t("A summary of what everyone watched over the last seven days.")}
        footer={<p className="text-xs text-fg-muted">{scheduleSummary(settings.data!)}</p>}
      >
        <SettingsRow
          title={t("Email the weekly report")}
          description={t("Queued reports appear in Delivery history.")}
          control={
            <Switch
              aria-label={t("Email the weekly report")}
              checked={draft.enabled}
              onChange={(e) => edit({ enabled: e.target.checked })}
            />
          }
        />
        <SettingsRow
          title={t("Day")}
          description={t("In UTC.")}
          control={
            <div className="w-44">
              <Select
                aria-label={t("Day")}
                value={String(draft.weekday)}
                onChange={(e) => edit({ weekday: Number(e.target.value) })}
              >
                {WEEKDAYS_LONG.map((day, i) => (
                  <option key={day} value={String(i)}>
                    {day}
                  </option>
                ))}
              </Select>
            </div>
          }
        />
        <SettingsRow
          title={t("Time")}
          description={t("In UTC.")}
          control={
            <div className="w-44">
              <Select
                aria-label={t("Time")}
                value={String(draft.hour)}
                onChange={(e) => edit({ hour: Number(e.target.value) })}
              >
                {HOURS.map((h) => (
                  <option key={h} value={String(h)}>
                    {`${String(h).padStart(2, "0")}:00 UTC`}
                  </option>
                ))}
              </Select>
            </div>
          }
        />
        <SettingsRow
          title={t("Report viewer")}
          description={t("Whose viewing the report covers.")}
          control={
            <div className="w-56">
              <Select
                aria-label={t("Report viewer")}
                value={String(draft.userId)}
                onChange={(e) => edit({ userId: Number(e.target.value) })}
              >
                <option value="0">{t("Everyone")}</option>
                {people.map((v) => (
                  <option key={v.id} value={String(v.id)}>
                    {viewerLabel(v)}
                  </option>
                ))}
                {draft.userId > 0 && viewers.data && !people.some((v) => v.id === draft.userId) ? (
                  <option value={String(draft.userId)}>
                    {t("Previously selected viewer (unavailable)")}
                  </option>
                ) : null}
              </Select>
            </div>
          }
        />
      </SettingsSection>
      <Preview userId={draft.userId} />
      <SaveBar
        dirty={dirty}
        saving={saving}
        error={error}
        onSave={save}
        onDiscard={() => {
          reset();
          setError(null);
        }}
      />
    </>
  );
}
