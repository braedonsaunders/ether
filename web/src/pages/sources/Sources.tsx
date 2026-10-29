import { localized, t } from "../../i18n";
import { Button } from "@braedonsaunders/appkit-ui";
import { Download } from "lucide-react";
import { useState } from "react";
import { Drawer } from "../../components/Drawer";
import { DomainPage, useRouteTab } from "../../components/layout";
import { ProwlarrImport } from "../../components/ProwlarrImport";
import { DebridSources } from "./Debrid";
import { HealthStrip } from "./form";
import { IndexerList } from "./Indexers";
import { UsenetProviders } from "./Usenet";
import { Availability } from "./Availability";
import { SourceUsage } from "./Usage";

const TABS = localized(
  () =>
    [
      { key: "usenet", label: t("Usenet providers") },
      { key: "indexers", label: t("Indexers") },
      { key: "debrid", label: t("Debrid") },
      { key: "availability", label: t("Availability") },
      { key: "usage", label: t("Usage & costs") },
    ] as const,
);

/** Where releases come from: Usenet providers, indexers, debrid accounts, and what they cost. */
export default function Sources() {
  const tab = useRouteTab(TABS);
  const [importing, setImporting] = useState(false);
  const [version, setVersion] = useState(0);
  return (
    <DomainPage
      title={t("Sources")}
      description={t("The Usenet providers, indexers and debrid accounts Ether streams from.")}
      base="/sources"
      tabs={TABS}
      active={tab}
      status={<HealthStrip ids={["usenet", "indexers"]} />}
      actions={
        tab === "indexers" ? (
          <Button variant="outline" size="sm" onClick={() => setImporting(true)}>
            <Download size={14} /> {t("Import from Prowlarr")}
          </Button>
        ) : null
      }
    >
      {tab === "usenet" ? <UsenetProviders /> : null}
      {tab === "indexers" ? <IndexerList version={version} /> : null}
      {tab === "debrid" ? <DebridSources /> : null}
      {tab === "availability" ? <Availability /> : null}
      {tab === "usage" ? <SourceUsage /> : null}
      <Drawer
        open={importing}
        onClose={() => setImporting(false)}
        title={t("Import from Prowlarr")}
        description={t(
          "Optional: add the indexers a Prowlarr instance already manages, searched through Prowlarr's Newznab and Torznab endpoints.",
        )}
        size="lg"
      >
        <ProwlarrImport onImported={() => setVersion((v) => v + 1)} />
      </Drawer>
    </DomainPage>
  );
}
