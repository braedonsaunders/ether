import { Badge, Button, Input, Select, toast } from "@braedonsaunders/appkit-ui";
import { RotateCcw } from "lucide-react";
import { useEffect, useId, useState } from "react";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { Field } from "../../components/fields";
import { t, tn } from "../../i18n";
import { formatBytes } from "../../lib/format";
import { useAsync } from "../../lib/hooks";
import {
  MEDIA_TV,
  PROFILE_DOWNLOAD,
  PROFILE_NONE,
  qualityLabel,
  releaseConsole,
  type ConsoleSeasonEpisode,
  type ConsoleSeasonSearch,
} from "../../lib/releases";
import { RecordDrawer } from "../sources/form";
import { PinModeField } from "./pinMode";

export interface SeasonPinTarget {
  key?: string;
  show?: string;
  season?: number;
}

const pad = (n: number | undefined) => String(n ?? 0).padStart(2, "0");

function bestChoices(data: ConsoleSeasonSearch | null): Record<string, string> {
  const next: Record<string, string> = {};
  for (const ep of data?.episodes ?? []) {
    next[ep.key] = ep.winner || ep.releases?.[0]?.release.id || "";
  }
  return next;
}

/**
 * Bulk-pin a release for every aired episode in a season: defaults to each
 * episode's highest-ranked candidate, lets the owner switch to a season pack's
 * per-episode files or override individual episodes, and saves in one action.
 */
export function SeasonPinDrawer({
  open,
  target,
  label,
  onClose,
  onPinned,
}: {
  open: boolean;
  target: SeasonPinTarget | null;
  label?: string;
  onClose: () => void;
  onPinned?: () => void;
}) {
  const id = useId();
  const [profile, setProfile] = useState("");
  const [freshTick, setFreshTick] = useState(0);
  const [sourceMode, setSourceMode] = useState("best");
  const [choices, setChoices] = useState<Record<string, string>>({});
  const [note, setNote] = useState("");
  const [fallback, setFallback] = useState(false);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<unknown>(null);

  const targetKey = target ? `${target.key ?? ""}|${target.show ?? ""}|${target.season ?? ""}` : "";

  const profiles = useAsync(
    () => (open ? releaseConsole.profiles(MEDIA_TV) : Promise.resolve(null)),
    [open],
  );

  const result = useAsync<ConsoleSeasonSearch | null>(
    (signal) =>
      open && target
        ? releaseConsole.season(target, { profile, fresh: freshTick > 0 }, signal)
        : Promise.resolve(null),
    [open, targetKey, profile, freshTick],
  );

  useEffect(() => {
    if (!open) {
      setFreshTick(0);
      setSourceMode("best");
      setChoices({});
      setNote("");
      setFallback(false);
      setSaveError(null);
    }
  }, [open, targetKey]);

  useEffect(() => {
    if (result.data) {
      setSourceMode("best");
      setChoices(bestChoices(result.data));
      setSaveError(null);
    }
  }, [result.data]);

  const data = result.data;
  const episodes = data?.episodes ?? [];
  const packs = data?.packs ?? [];
  const listedProfiles = profiles.data?.profiles ?? [];

  const activePack = sourceMode.startsWith("pack:")
    ? (packs.find((p) => `pack:${p.id}` === sourceMode) ?? null)
    : null;

  const applySourceMode = (mode: string) => {
    setSourceMode(mode);
    if (!data) return;
    if (mode === "best") {
      setChoices(bestChoices(data));
      return;
    }
    const pack = packs.find((p) => `pack:${p.id}` === mode);
    if (!pack) return;
    const next: Record<string, string> = {};
    for (const ep of episodes) {
      next[ep.key] = pack.episodeReleases[ep.key] ?? "";
    }
    setChoices(next);
  };

  const selectedPins = episodes
    .map((ep) => ({ key: ep.key, releaseId: choices[ep.key] ?? "" }))
    .filter((item) => item.releaseId !== "");

  const save = async () => {
    if (selectedPins.length === 0) return;
    setSaving(true);
    setSaveError(null);
    try {
      const saved = await releaseConsole.pinBatch(selectedPins, note, fallback);
      toast.success(tn(saved.length, "Pinned {count} episode", "Pinned {count} episodes"));
      onPinned?.();
      onClose();
    } catch (e) {
      setSaveError(e);
    } finally {
      setSaving(false);
    }
  };

  return (
    <RecordDrawer
      open={open}
      title={t("Pin a release for every episode")}
      description={data?.titleName || label}
      onClose={onClose}
      onSave={() => void save()}
      saving={saving}
      saveDisabled={result.loading || selectedPins.length === 0}
      saveLabel={
        selectedPins.length > 0
          ? tn(selectedPins.length, "Pin {count} episode", "Pin {count} episodes")
          : t("Pin releases")
      }
      error={saveError}
    >
      <div className="space-y-4">
        <div className="flex flex-wrap items-end gap-3">
          <Field label={t("Quality profile")} htmlFor={`${id}-profile`} className="min-w-48 flex-1">
            <Select
              id={`${id}-profile`}
              aria-label={t("Quality profile")}
              value={profile}
              onChange={(e) => setProfile(e.target.value)}
            >
              <option value="">
                {profiles.data?.default
                  ? t("Default ({profile})", { profile: profiles.data.default })
                  : t("Default")}
              </option>
              {listedProfiles.map((p) => (
                <option key={p} value={p}>
                  {p}
                </option>
              ))}
              <option value={PROFILE_NONE}>{t("No profile limits")}</option>
              <option value={PROFILE_DOWNLOAD}>{t("Offline download")}</option>
            </Select>
          </Field>
          <Button
            variant="outline"
            size="sm"
            disabled={result.loading}
            onClick={() => setFreshTick((n) => n + 1)}
          >
            <RotateCcw size={14} /> {t("Search again")}
          </Button>
        </div>

        {packs.length > 0 ? (
          <div className="space-y-2">
            <Field label={t("Release source")} htmlFor={`${id}-source`}>
              <Select
                id={`${id}-source`}
                aria-label={t("Release source")}
                value={sourceMode}
                onChange={(e) => applySourceMode(e.target.value)}
              >
                <option value="best">{t("Best release per episode")}</option>
                {packs.map((pack) => (
                  <option key={pack.id} value={`pack:${pack.id}`}>
                    {t("Season pack · {name} ({quality})", {
                      name: pack.name,
                      quality: qualityLabel(pack.info),
                    })}
                  </option>
                ))}
              </Select>
            </Field>
            {activePack ? (
              <div className="rounded-lg border border-border bg-bg-subtle px-3 py-2 text-xs text-fg-muted">
                <p className="break-all font-mono text-fg">{activePack.name}</p>
                <p className="mt-0.5">
                  {qualityLabel(activePack.info)} · {formatBytes(activePack.size)} ·{" "}
                  {activePack.indexer} ·{" "}
                  {tn(
                    Object.keys(activePack.episodeReleases).length,
                    "Covers {count} episode",
                    "Covers {count} episodes",
                  )}
                </p>
              </div>
            ) : null}
          </div>
        ) : null}

        {result.error ? (
          <ErrorPanel error={result.error} onRetry={() => void result.reload()} />
        ) : result.loading || !data ? (
          <div className="space-y-2">
            <p className="text-xs text-fg-muted">{t("Searching releases for every episode…")}</p>
            <LoadingRows rows={4} />
          </div>
        ) : episodes.length === 0 ? (
          <p className="text-sm text-fg-muted">{t("No aired episodes in this season yet.")}</p>
        ) : (
          <ul className="divide-y divide-border rounded-lg border border-border">
            {episodes.map((ep: ConsoleSeasonEpisode) => {
              const epReleases = ep.releases ?? [];
              const chosenId = choices[ep.key] ?? "";
              const chosen = epReleases.find((r) => r.release.id === chosenId)?.release;
              const code = `E${pad(ep.episode)}${ep.title ? ` · ${ep.title}` : ""}`;
              return (
                <li key={ep.key} className="space-y-2 px-3 py-3">
                  <div className="flex flex-wrap items-center justify-between gap-2">
                    <span className="text-sm font-medium text-fg">{code}</span>
                    {ep.pin ? <Badge variant="secondary">{t("Pinned")}</Badge> : null}
                  </div>
                  <Select
                    aria-label={t("Release for {episode}", { episode: code })}
                    value={chosenId}
                    disabled={epReleases.length === 0}
                    onChange={(e) => setChoices((prev) => ({ ...prev, [ep.key]: e.target.value }))}
                  >
                    <option value="">
                      {epReleases.length ? t("Skip this episode") : t("No matching release")}
                    </option>
                    {epReleases.map((cr) => (
                      <option key={cr.release.id} value={cr.release.id}>
                        {`${cr.release.name} (${qualityLabel(cr.release.info)} · ${formatBytes(cr.release.size)} · ${cr.release.indexer})`}
                      </option>
                    ))}
                  </Select>
                  {chosen ? (
                    <p className="text-xs text-fg-muted">
                      <span className="break-all font-mono text-fg">{chosen.name}</span>
                      {" · "}
                      {qualityLabel(chosen.info)} · {formatBytes(chosen.size)} · {chosen.indexer}
                    </p>
                  ) : epReleases.length === 0 ? (
                    <p className="text-xs text-fg-subtle">
                      {t("No matching release found for this profile.")}
                    </p>
                  ) : null}
                </li>
              );
            })}
          </ul>
        )}

        <PinModeField id={`${id}-mode`} fallback={fallback} onChange={setFallback} />

        <Field
          label={t("Note")}
          htmlFor={`${id}-note`}
          hint={t("Optional: why you pinned these releases.")}
        >
          <Input
            id={`${id}-note`}
            value={note}
            maxLength={500}
            onChange={(e) => setNote(e.currentTarget.value)}
            placeholder={t("e.g. Blu-ray remux pack with original audio")}
          />
        </Field>
      </div>
    </RecordDrawer>
  );
}
