import { SettingsRow } from "./SettingsRow";
import { SearchSelect } from "@braedonsaunders/appkit-ui";
import type { ReactNode } from "react";
import { localized, t } from "../i18n";
import { RATING_LEVELS, canonicalRating } from "../lib/contentRatings";
import type { EtherUser } from "../lib/types";
import { NumberInput } from "./fields";

/**
 * Rating-limit choices: one per age level, so a "PG" limit also admits
 * TV-PG and other countries' equivalents. Values are the level keys.
 */
export const CONTENT_RATINGS = localized(() => [
  { value: "", label: t("Unrestricted") },
  ...RATING_LEVELS.map((l) => ({
    value: l.value,
    label: t("{labels} ({name})", { labels: l.labels, name: l.name }),
  })),
]);

/** The user-record fields a viewer's limits edit. */
export type UserLimits = Pick<
  EtherUser,
  "streamLimit" | "maxContentRating" | "subtitleLanguages" | "audioLanguages"
>;

export function userLimits(u: EtherUser): UserLimits {
  return {
    streamLimit: u.streamLimit,
    maxContentRating: u.maxContentRating,
    subtitleLanguages: u.subtitleLanguages,
    audioLanguages: u.audioLanguages,
  };
}

/** Controlled stream-limit and rating-cap rows, for a form saved elsewhere. */
export function UserLimitRows({
  value,
  onChange,
  isOwner,
  defaultStreamLimit,
  streamLimitHint,
}: {
  value: UserLimits;
  onChange: (next: UserLimits) => void;
  isOwner?: boolean;
  defaultStreamLimit?: number;
  /** Extra copy after the stream-limit description (e.g. a settings link). */
  streamLimitHint?: ReactNode;
}) {
  return (
    <>
      <SettingsRow
        title={t("Stream limit")}
        description={
          <>
            {defaultStreamLimit !== undefined
              ? t("Simultaneous streams. 0 uses the server default ({limit}).", {
                  limit: defaultStreamLimit,
                })
              : t("Simultaneous streams. 0 uses the server default.")}
            {streamLimitHint ? <> {streamLimitHint}</> : null}
          </>
        }
        control={
          <NumberInput
            aria-label={t("Stream limit")}
            className="w-24"
            value={value.streamLimit}
            min={0}
            onChange={(n) => onChange({ ...value, streamLimit: Math.max(0, Math.round(n)) })}
          />
        }
      />
      <SettingsRow
        title={t("Content rating cap")}
        description={
          isOwner
            ? t("The owner is never limited by content rating.")
            : t(
                "Titles above this level, and unrated titles, are hidden from this viewer. TV and other countries' ratings count at their equivalent level.",
              )
        }
        control={
          <div className="w-56">
            <SearchSelect
              value={canonicalRating(value.maxContentRating)}
              onChange={(v) => onChange({ ...value, maxContentRating: v })}
              options={CONTENT_RATINGS}
              ariaLabel={t("Content rating cap")}
              searchable={false}
              placeholder={t("Select…")}
              selectLabel={t("Select")}
              closeLabel={t("Close")}
            />
          </div>
        }
      />
    </>
  );
}
