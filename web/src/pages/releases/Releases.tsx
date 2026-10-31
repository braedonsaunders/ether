import { Navigate, useLocation, useParams } from "react-router-dom";
import { localized, t } from "../../i18n";
import { ConfigSection } from "../../components/ConfigSection";
import { DomainPage, useRouteTab } from "../../components/layout";
import * as S from "../settings/schema";
import { BlocklistTab } from "./Blocklist";
import { ClientRulesTab } from "./ClientRules";
import { HistoryTab } from "./History";
import { PinnedTab } from "./Pinned";
import { QualityTab } from "./Quality";
import { ScoringTab, StartupTab } from "./Scoring";

const TABS = localized(
  () =>
    [
      { key: "quality", label: t("Quality profiles") },
      { key: "rules", label: t("Client rules") },
      { key: "selection", label: t("Selection rules") },
      { key: "scoring", label: t("Custom scoring") },
      { key: "startup", label: t("Playback start") },
      { key: "pinned", label: t("Pinned") },
      { key: "blocklist", label: t("Blocklist") },
      { key: "history", label: t("History") },
    ] as const,
);

/** Which release Ether picks for each play, and why. */
export default function Releases() {
  const { tab: rawTab } = useParams();
  const location = useLocation();
  const tab = useRouteTab(TABS);
  if (rawTab === "search") {
    return <Navigate to={{ pathname: "/releases/pinned", search: location.search }} replace />;
  }
  return (
    <DomainPage
      title={t("Releases")}
      description={t(
        "Choose the quality you want. Ether ranks releases for the app playing them and fails over when one breaks; find a title to see why, pin a release or block one.",
      )}
      base="/releases"
      tabs={TABS}
      active={tab}
    >
      {tab === "quality" ? <QualityTab /> : null}
      {tab === "rules" ? <ClientRulesTab /> : null}
      {tab === "selection" ? (
        <ConfigSection
          configKey="resolver"
          endpoint="resolver"
          groups={S.RESOLVER}
          label={t("Release selection")}
          omit={[
            "optimisticStart",
            "healthRace",
            "sourcePreference",
            "sourceScoreMargin",
            "customFormats",
            "customFormatScores",
          ]}
        />
      ) : null}
      {tab === "scoring" ? <ScoringTab /> : null}
      {tab === "startup" ? <StartupTab /> : null}
      {tab === "pinned" ? <PinnedTab /> : null}
      {tab === "blocklist" ? <BlocklistTab /> : null}
      {tab === "history" ? <HistoryTab /> : null}
    </DomainPage>
  );
}
