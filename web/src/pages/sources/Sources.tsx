import { localized, t } from "../../i18n";
import { Button } from "@braedonsaunders/appkit-ui";
import { Plus } from "lucide-react";
import { useEffect, useState } from "react";
import { useParams } from "react-router-dom";
import { Drawer } from "../../components/Drawer";
import { DomainPage, useRouteTab } from "../../components/layout";
import { ProwlarrImport } from "../../components/ProwlarrImport";
import { DebridSources } from "./Debrid";
import { HealthStrip } from "./form";
import { IndexerList } from "./Indexers";
import { UsenetProviders } from "./Usenet";
import { Availability } from "./Availability";
import { SourceUsage } from "./Usage";
import { AddSourceGallery, type GalleryChoice } from "./cards";
import type { DebridKind } from "../../lib/types";

const TABS = localized(
  () =>
    [
      { key: "overview", label: t("Your sources") },
      { key: "availability", label: t("Will it play") },
      { key: "usage", label: t("Cost") },
    ] as const,
);

type AddDebrid = { nonce: number; kind: DebridKind };
type AddIndexer = { nonce: number; type: "site" | "newznab" | "stremio" };

/**
 * Where playback comes from. Configured accounts are cards in try order.
 * Adding one starts from a gallery, then a drawer for that record.
 * Old /sources/usenet, /debrid and /indexers addresses still open this page.
 */
export default function Sources() {
  const tab = useRouteTab(TABS);
  const { tab: raw } = useParams();
  const [importing, setImporting] = useState(false);
  const [gallery, setGallery] = useState(false);
  const [version, setVersion] = useState(0);
  const [debridAdd, setDebridAdd] = useState<AddDebrid | null>(null);
  const [usenetAdd, setUsenetAdd] = useState<number | null>(null);
  const [indexerAdd, setIndexerAdd] = useState<AddIndexer | null>(null);

  useEffect(() => {
    if (raw !== "usenet" && raw !== "debrid" && raw !== "indexers") return;
    document.getElementById(`sources-${raw}`)?.scrollIntoView({ block: "start" });
  }, [raw, tab]);

  function pick(choice: GalleryChoice) {
    const nonce = Date.now();
    setGallery(false);
    if (choice.type === "debrid") setDebridAdd({ nonce, kind: choice.kind });
    else if (choice.type === "usenet") setUsenetAdd(nonce);
    else if (choice.type === "prowlarr") setImporting(true);
    else setIndexerAdd({ nonce, type: choice.type });
  }

  return (
    <DomainPage
      title={t("Sources")}
      description={t(
        "Where video comes from, whether those accounts are working, and what they cost.",
      )}
      base="/sources"
      tabs={TABS}
      active={tab}
      status={<HealthStrip ids={["usenet", "indexers"]} />}
      actions={
        tab === "overview" ? (
          <>
            <Button variant="outline" size="sm" onClick={() => setImporting(true)}>
              {t("Import from Prowlarr")}
            </Button>
            <Button size="sm" onClick={() => setGallery(true)}>
              <Plus size={14} /> {t("Add a source")}
            </Button>
          </>
        ) : null
      }
    >
      {tab === "overview" ? (
        <>
          <p className="text-sm text-fg-muted">
            {t(
              "Each card is an account or a search site. Drag a card, or use the arrows, to change what Ether tries first. A problem on a card says what to fix.",
            )}
          </p>
          <DebridSources addKind={debridAdd} />
          <UsenetProviders addNonce={usenetAdd} />
          <IndexerList version={version} addRequest={indexerAdd} />
        </>
      ) : null}
      {tab === "availability" ? <Availability /> : null}
      {tab === "usage" ? <SourceUsage /> : null}
      <AddSourceGallery open={gallery} onClose={() => setGallery(false)} onPick={pick} />
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
