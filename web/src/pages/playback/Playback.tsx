import { Link } from "react-router-dom";
import { ConfigSection } from "../../components/ConfigSection";
import { DomainPage, useRouteTab } from "../../components/layout";
import { localized, t, tx } from "../../i18n";
import * as S from "../settings/schema";
import { DeviceCapabilities } from "./Devices";
import { StartSpeed } from "./StartSpeed";
import { HardwareStatus } from "./Hardware";
import { MarkerCoverage } from "./Markers";
import { SkipButtons } from "./SkipButtons";
import { HouseholdAudio } from "./HouseholdAudio";
import { SubtitleTiming } from "./SubtitleTiming";

const TABS = localized(
  () =>
    [
      { key: "subtitles", label: t("Audio & subtitles") },
      { key: "transcoding", label: t("Transcoding") },
      { key: "devices", label: t("App compatibility") },
      { key: "markers", label: t("Skip buttons") },
    ] as const,
);

/** How a chosen release reaches the screen: transcoding, devices, skip markers, subtitles. */
export default function Playback() {
  const tab = useRouteTab(TABS);
  return (
    <DomainPage title={t("Playback")} base="/playback" tabs={TABS} active={tab}>
      <ConfigSection
        hidden={tab !== "transcoding"}
        configKey="transcode"
        endpoint="transcode"
        groups={S.TRANSCODE}
        label={t("Transcoding")}
        before={() => <HardwareStatus />}
      />
      {tab === "transcoding" ? <StartSpeed /> : null}
      {tab === "devices" ? <DeviceCapabilities /> : null}
      <SkipButtons hidden={tab !== "markers"} />
      {tab === "markers" ? <MarkerCoverage /> : null}
      {tab === "subtitles" ? (
        <>
          <HouseholdAudio />
          <p className="max-w-3xl text-sm text-fg-muted">
            {tx(
              "When someone turns subtitles on and the file has none, Ether downloads them from OpenSubtitles. The account is set up in {link}.",
              {
                link: (
                  <Link to="/services/opensubtitles" className="text-primary underline">
                    {t("Integrations")}
                  </Link>
                ),
              },
            )}
          </p>
          <SubtitleTiming />
        </>
      ) : null}
    </DomainPage>
  );
}
