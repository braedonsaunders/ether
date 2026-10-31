import { Button, Input, SettingsSection, Spinner, Switch, toast } from "@braedonsaunders/appkit-ui";
import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { ErrorPanel } from "../../../components/feedback";
import { SaveBar, useDraft } from "../../../components/SaveBar";
import { SettingsRow } from "../../../components/SettingsRow";
import { t, tn, tx } from "../../../i18n";
import { apiFetch } from "../../../lib/api";
import { useAsync } from "../../../lib/hooks";

type Policy = {
  userId: number;
  disabled: boolean;
  channels: string[];
  favorites: string[];
  maxRating: string;
};
type ChannelSummary = { id: string; number: string; name: string; enabled: boolean };

/** One viewer's Live TV access: channels and extra parental limits (viewer profile › Access). */
export function ViewerLiveTVAccess({ viewerId }: { viewerId: number }) {
  const policy = useAsync(
    (signal) => apiFetch<Policy>(`/livetv/policies/${viewerId}`, { signal }),
    [viewerId],
  );
  const channels = useAsync(
    (signal) => apiFetch<ChannelSummary[]>("/livetv/channels", { signal }),
    [],
  );
  const { draft, setDraft, dirty, reset } = useDraft(policy.data);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const sorted = useMemo(
    () =>
      [...(channels.data ?? [])].sort(
        (a, b) => (parseFloat(a.number) || 0) - (parseFloat(b.number) || 0),
      ),
    [channels.data],
  );

  if (policy.error) return <ErrorPanel error={policy.error} onRetry={() => void policy.reload()} />;
  if (!draft)
    return (
      <div className="py-6 text-center">
        <Spinner size={18} label={t("Loading Live TV access")} />
      </div>
    );

  const allowed = draft.channels ?? [];
  const save = async () => {
    setSaving(true);
    setError(null);
    try {
      await apiFetch(`/livetv/policies/${viewerId}`, {
        method: "PUT",
        body: JSON.stringify(draft),
      });
      policy.setData(draft);
      toast.success(t("Live TV access saved"));
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  };

  return (
    <>
      <SettingsSection
        title={t("Live TV")}
        description={t(
          "This viewer's existing rating and library limits also apply to the guide and every channel.",
        )}
      >
        <SettingsRow
          title={
            <label htmlFor={`livetv-off-${viewerId}`}>{t("Disable Live TV for this viewer")}</label>
          }
          description={t("Hides Live TV and the guide in their apps.")}
          control={
            <Switch
              id={`livetv-off-${viewerId}`}
              aria-label={t("Disable Live TV for this viewer")}
              checked={draft.disabled}
              onChange={(e) => setDraft({ ...draft, disabled: e.currentTarget.checked })}
            />
          }
        />
        <SettingsRow
          title={<label htmlFor={`livetv-rating-${viewerId}`}>{t("Live TV maximum rating")}</label>}
          description={t(
            "An extra limit for Live TV only, such as TV-PG. Leave blank to use their usual limit.",
          )}
          control={
            <Input
              id={`livetv-rating-${viewerId}`}
              className="sm:w-40"
              value={draft.maxRating || ""}
              disabled={draft.disabled}
              placeholder={t("No extra limit")}
              onChange={(e) => setDraft({ ...draft, maxRating: e.currentTarget.value })}
            />
          }
        />
      </SettingsSection>
      <SettingsSection
        title={t("Allowed channels")}
        description={
          allowed.length
            ? tn(
                allowed.length,
                "Only the channel selected below.",
                "Only the {count} channels selected below.",
              )
            : t("All channels. Select channels to allow only those.")
        }
      >
        {channels.error ? (
          <ErrorPanel error={channels.error} onRetry={() => void channels.reload()} />
        ) : !channels.data ? (
          <Spinner size={16} label={t("Loading channels")} />
        ) : sorted.length === 0 ? (
          <p className="py-2 text-sm text-fg-muted">
            {tx("No channels yet. {link}.", {
              link: (
                <Link to="/livetv/channels" className="font-medium text-fg underline">
                  {t("Create one in Live TV")}
                </Link>
              ),
            })}
          </p>
        ) : (
          <div className="space-y-3 py-2">
            <div className="grid gap-2 sm:grid-cols-2">
              {sorted.map((c) => (
                <div key={c.id} className="flex items-center gap-2 text-sm">
                  <Switch
                    id={`livetv-${viewerId}-channel-${c.id}`}
                    aria-label={t("Allow {name}", { name: c.name })}
                    checked={allowed.includes(c.id)}
                    disabled={draft.disabled}
                    onChange={(e) =>
                      setDraft({
                        ...draft,
                        channels: e.currentTarget.checked
                          ? [...allowed, c.id]
                          : allowed.filter((id) => id !== c.id),
                      })
                    }
                  />
                  <label
                    htmlFor={`livetv-${viewerId}-channel-${c.id}`}
                    className="min-w-0 truncate"
                  >
                    <span className="tabular-nums text-fg-muted">{c.number}</span> {c.name}
                  </label>
                </div>
              ))}
            </div>
            {allowed.length ? (
              <Button
                variant="ghost"
                size="sm"
                disabled={draft.disabled}
                onClick={() => setDraft({ ...draft, channels: [] })}
              >
                {t("Allow all channels")}
              </Button>
            ) : null}
          </div>
        )}
      </SettingsSection>
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
