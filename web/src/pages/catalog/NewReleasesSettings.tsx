import { Input, SettingsSection, toast } from "@braedonsaunders/appkit-ui";
import { Link } from "react-router-dom";
import { useState } from "react";
import { MoreOptions } from "../../components/ConfigSection";
import { MutationError } from "../../components/feedback";
import { SettingsRow } from "../../components/SettingsRow";
import { t, tn, tx } from "../../i18n";
import { api, apiFetch } from "../../lib/api";
import { formatNumber, relativeTime } from "../../lib/format";
import { useAsync, useInterval } from "../../lib/hooks";
import {
  applyReleaseMode,
  customRequestCeiling,
  releaseMode,
  type ReleaseMode,
} from "../../lib/release-mode";
import type { AvailabilityConfig } from "../../lib/types";

interface Summary {
  ready: number;
  waiting: number;
  lastChecked?: number;
}

const MODES: {
  value: ReleaseMode;
  recommended?: boolean;
  title: () => string;
  body: () => string;
}[] = [
  {
    value: "playable",
    recommended: true,
    title: () => t("When they can play"),
    body: () =>
      t(
        "A new movie appears once Ether finds a release you can play with your quality settings. Ether checks for you automatically.",
      ),
  },
  {
    value: "digital",
    title: () => t("On digital release"),
    body: () =>
      t(
        "Appears on its streaming or digital release date. No checking, but some may not play yet.",
      ),
  },
  {
    value: "immediate",
    title: () => t("Right away"),
    body: () => t("Every new movie shows immediately, even if it's only in theatres."),
  },
];

export function NewReleasesSettings() {
  const config = useAsync(() => api.getConfig(), []);
  const summary = useAsync((signal) => apiFetch<Summary>("/availability/summary", { signal }), []);
  useInterval(() => void summary.reload(), 30000);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const cfg = config.data?.availability;
  if (!cfg) return null;
  const mode = releaseMode(cfg);
  const ceiling = customRequestCeiling(cfg);

  const save = async (next: AvailabilityConfig) => {
    setSaving(true);
    setError(null);
    try {
      await api.putSection("availability", next);
      await config.reload();
      await summary.reload();
      toast.success(t("Settings saved"));
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  };

  return (
    <SettingsSection title={t("New releases")}>
      {error ? <MutationError error={error} /> : null}
      <div className="new-release-modes" role="radiogroup" aria-label={t("New releases")}>
        {MODES.map((item) => {
          const selected = mode === item.value;
          return (
            <div key={item.value} className="new-release-mode">
              <button
                type="button"
                role="radio"
                aria-checked={selected}
                disabled={saving}
                className={`new-release-card${selected ? " is-selected" : ""}`}
                onClick={() => {
                  if (!selected) void save(applyReleaseMode(cfg, item.value));
                }}
              >
                <span className="new-release-card-head">
                  <span className="new-release-card-title">{item.title()}</span>
                  {item.recommended ? (
                    <span className="new-release-recommended">{t("Recommended")}</span>
                  ) : null}
                </span>
                <span className="new-release-card-body">{item.body()}</span>
              </button>
              {selected && item.value === "playable" && summary.data ? (
                <p className="new-release-status">
                  {tx("{ready} ready · {waiting} · checked {when}", {
                    ready: formatNumber(summary.data.ready),
                    waiting: (
                      <Link to="/catalog?waiting=1&type=movie" className="text-primary underline">
                        {tn(
                          summary.data.waiting,
                          "{count} waiting for a release",
                          "{count} waiting for a release",
                        )}
                      </Link>
                    ),
                    when: summary.data.lastChecked
                      ? relativeTime(new Date(summary.data.lastChecked * 1000))
                      : t("not yet"),
                  })}
                </p>
              ) : null}
            </div>
          );
        })}
      </div>
      <MoreOptions label={t("More options: Advanced")} text={t("Advanced")}>
        <SettingsRow
          title={t("Daily request ceiling")}
          description={t("Leave empty to let Ether choose a daily limit from your indexers.")}
          control={
            <Input
              aria-label={t("Daily request ceiling")}
              type="number"
              inputMode="numeric"
              min={0}
              max={100000}
              className="w-28 tabular-nums"
              defaultValue={ceiling ?? ""}
              key={`${cfg.effort}-${cfg.feedRequests}`}
              onBlur={(e) => {
                const raw = e.currentTarget.value.trim();
                const value = raw === "" ? 0 : Number(raw);
                if (!Number.isFinite(value) || value <= 0) {
                  if (ceiling !== null) void save({ ...cfg, effort: "normal", feedRequests: 400 });
                  return;
                }
                if (value !== ceiling)
                  void save({ ...cfg, effort: "custom", feedRequests: Math.round(value) });
              }}
            />
          }
        />
      </MoreOptions>
    </SettingsSection>
  );
}
