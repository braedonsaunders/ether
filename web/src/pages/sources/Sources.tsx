import { localized, t } from "../../i18n";
import { Button } from "@braedonsaunders/appkit-ui";
import { Plus } from "lucide-react";
import { useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { Drawer } from "../../components/Drawer";
import { DomainPage } from "../../components/layout";
import { ProwlarrImport } from "../../components/ProwlarrImport";
import { DebridSources } from "./Debrid";
import { HealthStrip } from "./form";
import { IndexerList } from "./Indexers";
import { UsenetProviders } from "./Usenet";
import { SourceUsage } from "./Usage";
import { AddSourceGallery, type GalleryChoice } from "./cards";
import type { DebridKind } from "../../lib/types";

const TABS = localized(
  () =>
    [
      { key: "debrid", label: t("Debrid") },
      { key: "usenet", label: t("Usenet") },
      { key: "indexers", label: t("Indexers") },
      { key: "stats", label: t("Stats") },
    ] as const,
);

type AddDebrid = { nonce: number; kind: DebridKind };
type AddIndexer = { nonce: number; type: "site" | "newznab" | "stremio" };

/**
 * Where playback comes from: one tab per source type, plus Stats.
 * An unknown address opens Debrid.
 * Adding one starts from a gallery, then a drawer.
 */
export default function Sources() {
  const { tab: raw } = useParams();
  const navigate = useNavigate();
  const top = TABS.find((item) => item.key === raw)?.key ?? "debrid";
  const [importing, setImporting] = useState(false);
  const [gallery, setGallery] = useState(false);
  const [version, setVersion] = useState(0);
  const [debridAdd, setDebridAdd] = useState<AddDebrid | null>(null);
  const [usenetAdd, setUsenetAdd] = useState<number | null>(null);
  const [indexerAdd, setIndexerAdd] = useState<AddIndexer | null>(null);

  function pick(choice: GalleryChoice) {
    const nonce = Date.now();
    setGallery(false);
    if (choice.type === "debrid") {
      navigate("/sources/debrid");
      setDebridAdd({ nonce, kind: choice.kind });
    } else if (choice.type === "usenet") {
      navigate("/sources/usenet");
      setUsenetAdd(nonce);
    } else if (choice.type === "prowlarr") {
      navigate("/sources/indexers");
      setImporting(true);
    } else {
      navigate("/sources/indexers");
      setIndexerAdd({ nonce, type: choice.type });
    }
  }

  return (
    <DomainPage
      title={t("Sources")}
      base="/sources"
      tabs={TABS}
      active={top}
      status={<HealthStrip ids={["usenet", "indexers"]} />}
      actions={
        top !== "stats" ? (
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
      {top !== "stats" ? (
        <p className="text-sm text-fg-muted">
          {t("Drag a card to choose which account Ether tries first.")}
        </p>
      ) : null}
      {top === "debrid" ? <DebridSources addKind={debridAdd} /> : null}
      {top === "usenet" ? <UsenetProviders addNonce={usenetAdd} /> : null}
      {top === "indexers" ? <IndexerList version={version} addRequest={indexerAdd} /> : null}
      {top === "stats" ? <SourceUsage /> : null}
      <AddSourceGallery open={gallery} onClose={() => setGallery(false)} onPick={pick} />
      <Drawer
        open={importing}
        onClose={() => setImporting(false)}
        title={t("Import from Prowlarr")}
        description={t("Sync indexers from Prowlarr.")}
        size="lg"
      >
        <ProwlarrImport onImported={() => setVersion((v) => v + 1)} />
      </Drawer>
    </DomainPage>
  );
}
