import { localized, t } from "../../i18n";
import { ConfigSection } from "../../components/ConfigSection";
import { DomainPage, useRouteTab } from "../../components/layout";
import { PLAY_VERSIONS, RELEASE_SEARCH } from "../settings/schema";
import { BlocklistTab } from "./Blocklist";
import { ClientRulesTab } from "./ClientRules";
import { HistoryTab } from "./History";
import { PinnedTab } from "./Pinned";
import { QualityTab } from "./Quality";

const TABS = localized(
  () =>
    [
      { key: "profiles", label: t("Profiles") },
      { key: "devices", label: t("Device rules") },
      { key: "searching", label: t("Searching") },
      { key: "blocklist", label: t("Blocklist") },
      { key: "pinned", label: t("Pinned") },
      { key: "history", label: t("History") },
    ] as const,
);

/** Which release Ether picks for each play, and why. */
export default function Releases() {
  const tab = useRouteTab(TABS);
  return (
    <DomainPage title={t("Releases")} base="/releases" tabs={TABS} active={tab}>
      {tab === "profiles" ? <QualityTab /> : null}
      {tab === "devices" ? <ClientRulesTab /> : null}
      {tab === "searching" ? (
        <ConfigSection
          configKey="resolver"
          endpoint="resolver"
          groups={[...RELEASE_SEARCH, ...PLAY_VERSIONS]}
          label={t("Searching")}
        />
      ) : null}
      {tab === "blocklist" ? <BlocklistTab /> : null}
      {tab === "pinned" ? <PinnedTab /> : null}
      {tab === "history" ? <HistoryTab /> : null}
    </DomainPage>
  );
}
