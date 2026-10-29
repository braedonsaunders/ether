import { Link } from "react-router-dom";
import { ConfigSection } from "../../components/ConfigSection";
import { DomainPage, useRouteTab } from "../../components/layout";
import { localized, t, tx } from "../../i18n";
import * as S from "../settings/schema";
import { DeviceCapabilities } from "./Devices";
import { HardwareStatus } from "./Hardware";
import { MarkerCoverage } from "./Markers";
import { SubtitleTiming } from "./SubtitleTiming";

const TABS = localized(
  () =>
    [
      { key: "transcoding", label: t("Transcoding") },
      { key: "devices", label: t("Device capabilities") },
      { key: "markers", label: t("Markers") },
      { key: "subtitles", label: t("Subtitles") },
    ] as const,
);

/** How a chosen release reaches the screen: transcoding, caching, markers, subtitles. */
export default function Playback() {
  const tab = useRouteTab(TABS);
  return (
    <DomainPage
      title={t("Playback")}
      description={t("How Ether adapts each stream to the app playing it.")}
      base="/playback"
      tabs={TABS}
      active={tab}
    >
      {tab === "transcoding" ? (
        <ConfigSection
          configKey="transcode"
          endpoint="transcode"
          groups={S.TRANSCODE}
          label={t("Transcoding")}
          before={() => <HardwareStatus />}
        />
      ) : null}
      {tab === "devices" ? <DeviceCapabilities /> : null}
      {tab === "markers" ? (
        <ConfigSection
          configKey="markers"
          endpoint="markers"
          groups={S.MARKERS}
          label={t("Markers")}
          after={() => <MarkerCoverage />}
        />
      ) : null}
      {tab === "subtitles" ? (
        <>
          <p className="max-w-3xl text-sm text-fg-muted">
            {tx(
              "Missing subtitles are fetched from OpenSubtitles. The account is set up in {link}.",
              {
                link: (
                  <Link to="/services" className="text-primary underline">
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
