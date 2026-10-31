import { Button, Input, Select, SettingsSection, Switch, toast } from "@braedonsaunders/appkit-ui";
import { useState } from "react";
import { Link } from "react-router-dom";
import { ChipGroup, NumberInput } from "../../components/fields";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { SettingsRow } from "../../components/SettingsRow";
import {
  CONTENT_RATINGS,
  UserLimitRows,
  userLimits,
  type UserLimits,
} from "../../components/UserLimits";
import { viewerName } from "../../components/pickers";
import { t } from "../../i18n";
import { api, apiFetch, errorMessage } from "../../lib/api";
import type { Viewer } from "../../lib/contract";
import { useAsync } from "../../lib/hooks";
import { PlexWatchSync } from "./sections/PlexWatchSync";
import { ViewerLiveTVAccess } from "./sections/ViewerLiveTV";
import { KID_PRESETS, ListText, normalizePolicy, type SafetyPolicy } from "./shared";

const HOURS = Array.from({ length: 24 }, (_, h) => ({
  value: h,
  label: `${String(h).padStart(2, "0")}:00`,
}));

/**
 * Everything that decides what one viewer may watch and when: their stream
 * limit and rating cap, parental controls, allowed times and Live TV access.
 * One in-place form; the save bar writes whichever parts changed.
 */
export function ViewerAccess({ viewer }: { viewer: Viewer }) {
  const id = viewer.id;
  const name = viewerName(viewer);
  const users = useAsync(() => api.users(), []);
  const config = useAsync(() => api.getConfig(), []);
  const policy = useAsync(
    async (signal) =>
      normalizePolicy(await apiFetch<SafetyPolicy>(`/safety/policies/${id}`, { signal })),
    [id],
  );
  const user = users.data?.find((u) => u.id === id);
  const limits = useDraft<UserLimits>(user ? userLimits(user) : null);
  const rules = useDraft<SafetyPolicy>(policy.data);
  const [saving, setSaving] = useState(false);
  const [presetBusy, setPresetBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);

  const patchLimits = (p: Partial<UserLimits>) => limits.setDraft((d) => (d ? { ...d, ...p } : d));
  const patchPolicy = (p: Partial<SafetyPolicy>) => rules.setDraft((d) => (d ? { ...d, ...p } : d));

  const applyPreset = async (age: string) => {
    setPresetBusy(true);
    try {
      const preset = await apiFetch<SafetyPolicy>(`/safety/policies/presets/kids?age=${age}`);
      rules.setDraft(normalizePolicy(preset));
    } catch (e) {
      toast.error(t("Couldn't load the preset"), { description: errorMessage(e) });
    } finally {
      setPresetBusy(false);
    }
  };

  const save = async () => {
    setSaving(true);
    setError(null);
    try {
      if (limits.dirty && limits.draft && user) {
        const saved = await api.updateUser(id, limits.draft);
        const next = saved ?? { ...user, ...limits.draft };
        users.setData((list) => (list ?? []).map((u) => (u.id === id ? next : u)));
      }
      if (rules.dirty && rules.draft) {
        await apiFetch(`/safety/policies/${id}`, {
          method: "PUT",
          body: JSON.stringify(rules.draft),
        });
        policy.setData(rules.draft);
      }
      toast.success(t("Preferences saved"), {
        description: t("{name}'s Ether preferences are up to date.", { name }),
      });
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  };

  const l = limits.draft;
  const p = rules.draft;
  return (
    <div className="max-w-3xl space-y-6">
      {users.error ? (
        <ErrorPanel error={users.error} onRetry={() => void users.reload()} />
      ) : !users.data ? (
        <LoadingRows rows={2} />
      ) : !user || !l ? (
        <SettingsSection title={t("Streams and ratings")}>
          <p className="py-3 text-sm text-fg-muted">
            {t("This viewer has no user record on the server yet, so there are no limits to set.")}
          </p>
        </SettingsSection>
      ) : (
        <>
          <SettingsSection title={t("Streams and ratings")}>
            <UserLimitRows
              value={l}
              onChange={(next) => patchLimits(next)}
              isOwner={user.isOwner}
              defaultStreamLimit={config.data?.server.defaultStreamLimit}
              streamLimitHint={
                <Link to="/server" className="font-medium text-primary hover:underline">
                  {t("Change the server default")}
                </Link>
              }
            />
          </SettingsSection>
          <SettingsSection
            title={t("Account languages")}
            description={t(
              "Preferred languages reported to Plex apps for this account. Track selection rules live on the Audio & subtitles tab.",
            )}
          >
            <SettingsRow
              title={t("Subtitle languages")}
              description={t("Language codes in preference order, e.g. en, fr.")}
              control={
                <div className="w-56">
                  <ListText
                    ariaLabel={t("Account subtitle languages")}
                    placeholder="en, fr"
                    value={l.subtitleLanguages}
                    onChange={(v) => patchLimits({ subtitleLanguages: v })}
                  />
                </div>
              }
            />
            <SettingsRow
              title={t("Audio languages")}
              description={t("Preferred audio tracks in order.")}
              control={
                <div className="w-56">
                  <ListText
                    ariaLabel={t("Account audio languages")}
                    placeholder="en"
                    value={l.audioLanguages}
                    onChange={(v) => patchLimits({ audioLanguages: v })}
                  />
                </div>
              }
            />
          </SettingsSection>
        </>
      )}

      {policy.error ? (
        <ErrorPanel error={policy.error} onRetry={() => void policy.reload()} />
      ) : !p ? (
        <LoadingRows rows={3} />
      ) : (
        <>
          <SettingsSection
            title={t("Parental controls")}
            description={t(
              "Adult titles are always excluded. Unrated titles are blocked when a rating limit applies. Where this and the rating cap above differ, the stricter one wins.",
            )}
          >
            <SettingsRow
              title={t("Start from a preset")}
              description={t(
                "Replaces the parental controls, playback restrictions and allowed times below.",
              )}
              control={
                <div className="flex flex-wrap gap-2">
                  {KID_PRESETS.map((preset) => (
                    <Button
                      key={preset.value}
                      variant="outline"
                      size="sm"
                      disabled={presetBusy}
                      onClick={() => void applyPreset(preset.value)}
                    >
                      {preset.label}
                    </Button>
                  ))}
                </div>
              }
            />
            <SettingsRow
              title={t("Maximum content rating")}
              control={
                <div className="w-44">
                  <Select
                    aria-label={t("Maximum content rating")}
                    value={p.maxContentRating}
                    onChange={(e) => patchPolicy({ maxContentRating: e.target.value })}
                  >
                    {CONTENT_RATINGS.map((r) => (
                      <option key={r.value} value={r.value}>
                        {r.label}
                      </option>
                    ))}
                  </Select>
                </div>
              }
            />
            <SettingsRow
              title={t("Blocked genres")}
              description={t("Comma-separated, e.g. Horror, Thriller.")}
              control={
                <div className="w-64">
                  <ListText
                    ariaLabel={t("Blocked genres")}
                    value={p.blockedGenres}
                    onChange={(v) => patchPolicy({ blockedGenres: v })}
                  />
                </div>
              }
            />
            <SettingsRow
              title={t("Blocked keywords")}
              description={t("Comma-separated TMDB keywords.")}
              control={
                <div className="w-64">
                  <ListText
                    ariaLabel={t("Blocked keywords")}
                    value={p.blockedKeywords}
                    onChange={(v) => patchPolicy({ blockedKeywords: v })}
                  />
                </div>
              }
            />
          </SettingsSection>

          <SettingsSection title={t("Playback restrictions")}>
            <SettingsRow
              title={t("Local network only")}
              description={t("Block playback from outside your home network.")}
              control={
                <Switch
                  aria-label={t("Local network only")}
                  checked={p.remoteDisabled}
                  onChange={(e) => patchPolicy({ remoteDisabled: e.target.checked })}
                />
              }
            />
            <SettingsRow
              title={t("Maximum bitrate")}
              description={t("In kbps. 0 uses the server default.")}
              control={
                <NumberInput
                  aria-label={t("Maximum bitrate")}
                  className="w-28"
                  min={0}
                  value={p.maxBitrateKbps}
                  onChange={(n) => patchPolicy({ maxBitrateKbps: Math.max(0, Math.round(n)) })}
                />
              }
            />
            <SettingsRow
              title={t("Concurrent streams")}
              description={t(
                "0 adds no extra limit. Where this and the stream limit above differ, the lower one wins.",
              )}
              control={
                <NumberInput
                  aria-label={t("Concurrent streams")}
                  className="w-24"
                  min={0}
                  value={p.streamLimit}
                  onChange={(n) => patchPolicy({ streamLimit: Math.max(0, Math.round(n)) })}
                />
              }
            />
          </SettingsSection>

          <SettingsSection
            title={t("Allowed times")}
            description={t(
              "Hours when this viewer can start playback. Leave every hour clear to allow any time.",
            )}
          >
            <SettingsRow
              title={t("Timezone")}
              control={
                <Input
                  aria-label={t("Playback timezone")}
                  className="w-56"
                  value={p.timezone}
                  onChange={(e) => patchPolicy({ timezone: e.target.value })}
                />
              }
            />
            <SettingsRow stacked title={t("Allowed hours")}>
              <ChipGroup
                ariaLabel={t("Allowed hours")}
                size="sm"
                options={HOURS}
                value={p.allowedHours}
                onChange={(v) => patchPolicy({ allowedHours: [...v].sort((a, b) => a - b) })}
              />
            </SettingsRow>
          </SettingsSection>
        </>
      )}

      <ViewerLiveTVAccess viewerId={id} />

      <PlexWatchSync viewerId={id} plexId={viewer.plexId} />

      <SaveBar
        dirty={limits.dirty || rules.dirty}
        saving={saving}
        error={error}
        onSave={save}
        onDiscard={() => {
          limits.reset();
          rules.reset();
          setError(null);
        }}
      />
    </div>
  );
}
