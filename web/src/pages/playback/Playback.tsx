import { Link } from "react-router-dom";
import { ConfigSection } from "../../components/ConfigSection";
import { secretConfigured } from "../../components/IntegrationAccount";
import { DomainPage, useRouteTab } from "../../components/layout";
import { localized, t, tx } from "../../i18n";
import * as S from "../settings/schema";
import { DeviceCapabilities } from "./Devices";
import { StartSpeed } from "./StartSpeed";
import { HardwareStatus } from "./Hardware";
import { MarkerCoverage } from "./Markers";
import { SubtitleTiming } from "./SubtitleTiming";

const TABS = localized(
  () =>
    [
      { key: "transcoding", label: t("Transcoding") },
      { key: "devices", label: t("App compatibility") },
      { key: "markers", label: t("Skip buttons") },
      { key: "subtitles", label: t("Subtitles") },
    ] as const,
);

/** Skip-marker settings for this page. The services account form keeps S.MARKERS. */
const SKIP_MARKERS = localized(() => [
  {
    id: "skip",
    title: t("Skip intros and credits"),
    description: t(
      "Lets an app offer a button to skip the opening, a recap, or the credits. The button still depends on the app.",
    ),
    fields: [
      {
        key: "theIntroDb",
        label: t("Look up intro and credits markers"),
        kind: "bool" as const,
        help: t("Uses a public lookup and remembers each episode it has already checked."),
      },
    ],
  },
]);

function IntroDbStatus({ apiKey }: { apiKey: unknown }) {
  if (secretConfigured(apiKey)) {
    return (
      <p className="text-sm">
        <Link to="/services/introdb" className="text-primary underline">
          {t("TheIntroDB connected")}
        </Link>
      </p>
    );
  }
  return (
    <p className="text-sm text-fg-muted">
      {tx("Add a TheIntroDB key in {link} for better coverage", {
        link: (
          <Link to="/services/introdb" className="text-primary underline">
            {t("Integrations")}
          </Link>
        ),
      })}
    </p>
  );
}

/** How a chosen release reaches the screen: transcoding, devices, skip markers, subtitles. */
export default function Playback() {
  const tab = useRouteTab(TABS);
  return (
    <DomainPage
      title={t("Playback")}
      description={t(
        "How a video is adjusted so the app on the other end can play it. The defaults work for a household.",
      )}
      base="/playback"
      tabs={TABS}
      active={tab}
    >
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
      <ConfigSection
        hidden={tab !== "markers"}
        configKey="markers"
        endpoint="markers"
        groups={SKIP_MARKERS}
        label={t("Skip buttons")}
        omit={["apiKey"]}
        before={(draft) => <IntroDbStatus apiKey={draft.apiKey} />}
        after={tab === "markers" ? () => <MarkerCoverage /> : undefined}
      />
      {tab === "subtitles" ? (
        <>
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
