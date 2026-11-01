import { SettingsSection } from "@braedonsaunders/appkit-ui";
import { SettingsRow } from "../../components/SettingsRow";
import { t } from "../../i18n";
import { apiFetch } from "../../lib/api";
import { useAsync } from "../../lib/hooks";

export type PlexViewerDefaults = {
  audioLanguage?: string;
  autoSelectAudio?: boolean;
  subtitleLanguage?: string;
  subtitleMode?: string;
  hasProfile?: boolean;
  filterMovies?: string;
  filterTelevision?: string;
  allowSync?: boolean | null;
  hasSettings?: boolean;
  labelMap?: string;
};

function modeLabel(mode: string | undefined): string {
  switch (mode) {
    case "foreign-only":
      return t("Only for foreign audio");
    case "always":
      return t("Always on");
    case "forced-only":
      return t("Forced tracks only");
    case "off":
      return t("Off unless I turn them on");
    default:
      return mode || t("None");
  }
}

function yesNo(v: boolean | null | undefined): string {
  if (v == null) return t("Not set");
  return v ? t("Yes") : t("No");
}

/**
 * Read-only plex.tv account and share values. Ether owner rules still win.
 */
export function FromPlexDefaults({
  viewerId,
  show,
}: {
  viewerId: number;
  show: "tracks" | "share";
}) {
  const data = useAsync(
    (signal) =>
      apiFetch<PlexViewerDefaults>(`/viewers/plex-defaults/${viewerId}`, { signal }).catch(
        () => null,
      ),
    [viewerId],
  );
  const d = data.data;
  if (!d) return null;
  if (show === "tracks" && !d.hasProfile && !d.audioLanguage && !d.subtitleLanguage) return null;
  if (show === "share" && !d.hasSettings && !d.filterMovies && !d.filterTelevision && d.allowSync == null)
    return null;
  return (
    <SettingsSection
      title={t("From Plex")}
      description={t("Read-only defaults from this viewer's plex.tv account. Ether rules win.")}
    >
      {show === "tracks" ? (
        <>
          <SettingsRow title={t("Audio language")} control={<span>{d.audioLanguage || t("None")}</span>} />
          <SettingsRow title={t("Auto-select audio")} control={<span>{yesNo(d.autoSelectAudio)}</span>} />
          <SettingsRow
            title={t("Subtitle language")}
            control={<span>{d.subtitleLanguage || t("None")}</span>}
          />
          <SettingsRow title={t("Subtitle mode")} control={<span>{modeLabel(d.subtitleMode)}</span>} />
        </>
      ) : (
        <>
          <SettingsRow title={t("Movie filter")} control={<span>{d.filterMovies || t("None")}</span>} />
          <SettingsRow title={t("TV filter")} control={<span>{d.filterTelevision || t("None")}</span>} />
          <SettingsRow title={t("Downloads")} control={<span>{yesNo(d.allowSync)}</span>} />
          <p className="px-5 py-2 text-xs text-fg-muted">
            {t("Label filters match the TMDB collection name only.")}
          </p>
        </>
      )}
    </SettingsSection>
  );
}
