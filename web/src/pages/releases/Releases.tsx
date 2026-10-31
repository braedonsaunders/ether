import { localized, t } from "../../i18n";
import { ConfigSection } from "../../components/ConfigSection";
import { DomainPage, useRouteTab } from "../../components/layout";
import { PLAY_VERSIONS } from "../settings/schema";
import { BlocklistTab } from "./Blocklist";
import { ClientRulesTab } from "./ClientRules";
import { HistoryTab } from "./History";
import { PinnedTab } from "./Pinned";
import { QualityTab } from "./Quality";

const TABS = localized(
  () =>
    [
      { key: "quality", label: t("Quality profiles") },
      { key: "devices", label: t("Device rules") },
      { key: "blocklist", label: t("Blocklist") },
      { key: "pinned", label: t("Pinned") },
      { key: "history", label: t("History") },
    ] as const,
);

/** Which release Ether picks for each play, and why. */
export default function Releases() {
  const tab = useRouteTab(TABS);
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
      {tab === "quality" ? (
        <>
          <QualityTab />
          <ConfigSection
            configKey="resolver"
            endpoint="resolver"
            groups={PLAY_VERSIONS}
            label={t("Versions in the app")}
          />
        </>
      ) : null}
      {tab === "devices" ? <ClientRulesTab /> : null}
      {tab === "pinned" ? <PinnedTab /> : null}
      {tab === "blocklist" ? <BlocklistTab /> : null}
      {tab === "history" ? <HistoryTab /> : null}
    </DomainPage>
  );
}
