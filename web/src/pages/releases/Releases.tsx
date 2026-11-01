import { localized, t } from "../../i18n";
import { ConfigSection } from "../../components/ConfigSection";
import { DomainPage, useRouteTab } from "../../components/layout";
import { PLAY_VERSIONS, RELEASE_SEARCH } from "../settings/schema";
import { BlocklistTab } from "./Blocklist";
import { ClientRulesTab } from "./ClientRules";
import { HistoryTab } from "./History";
import { PinnedTab } from "./Pinned";
import { QualityTab } from "./Quality";
import { EqualQuality } from "../sources/EqualQuality";

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
    <DomainPage title={t("Releases")} base="/releases" tabs={TABS} active={tab}>
      {tab === "quality" ? (
        <>
          <QualityTab />
          <EqualQuality />
          <ConfigSection
            configKey="resolver"
            endpoint="resolver"
            groups={PLAY_VERSIONS}
            label={t("Versions in the app")}
          />
          <ConfigSection
            configKey="resolver"
            endpoint="resolver"
            groups={RELEASE_SEARCH}
            label={t("How Ether searches")}
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
