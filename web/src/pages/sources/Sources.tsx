import { localized, t } from "../../i18n";
import { Button, SubtabNav } from "@braedonsaunders/appkit-ui";
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
import { Availability } from "./Availability";
import { SourceUsage } from "./Usage";
import { AddSourceGallery, type GalleryChoice } from "./cards";
import type { DebridKind } from "../../lib/types";

const TABS = localized(
  () =>
    [
      { key: "yours", label: t("Your sources") },
      { key: "availability", label: t("Will it play") },
      { key: "stats", label: t("Stats") },
    ] as const,
);

const SOURCE_SUBS = localized(
  () =>
    [
      { key: "debrid", label: t("Debrid") },
      { key: "usenet", label: t("Usenet") },
      { key: "indexers", label: t("Indexers") },
    ] as const,
);

type SourceSub = (typeof SOURCE_SUBS)[number]["key"];

type AddDebrid = { nonce: number; kind: DebridKind };
type AddIndexer = { nonce: number; type: "site" | "newznab" | "stremio" };

function isSourceSub(value: string | undefined): value is SourceSub {
  return value === "debrid" || value === "usenet" || value === "indexers";
}

/**
 * Where playback comes from. Each source type has its own sub-tab under
 * /sources/yours. An unknown address opens Your sources, not a renamed page.
 * Adding one starts from a gallery, then a drawer.
 */
export default function Sources() {
  const { tab: raw, sub: rawSub } = useParams();
  const navigate = useNavigate();
  const top = TABS.find((item) => item.key === raw)?.key ?? "yours";
  const sourceSub: SourceSub = top === "yours" && isSourceSub(rawSub) ? rawSub : "debrid";
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
      navigate("/sources/yours/debrid");
      setDebridAdd({ nonce, kind: choice.kind });
    } else if (choice.type === "usenet") {
      navigate("/sources/yours/usenet");
      setUsenetAdd(nonce);
    } else if (choice.type === "prowlarr") {
      navigate("/sources/yours/indexers");
      setImporting(true);
    } else {
      navigate("/sources/yours/indexers");
      setIndexerAdd({ nonce, type: choice.type });
    }
  }

  return (
    <DomainPage
      title={t("Sources")}
      description={t(
        "Where video comes from, whether those accounts are working, and how they are doing.",
      )}
      base="/sources"
      tabs={TABS}
      active={top}
      status={<HealthStrip ids={["usenet", "indexers"]} />}
      actions={
        top === "yours" ? (
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
      {top === "yours" ? (
        <>
          <SubtabNav
            ariaLabel={t("Source types")}
            active={sourceSub}
            onSelect={(key) => navigate(`/sources/yours/${key}`)}
            tabs={SOURCE_SUBS.map((item) => ({ key: item.key, label: item.label }))}
          />
          <p className="text-sm text-fg-muted">
            {t(
              "Each card is an account or a search site. Drag a card, or use the arrows, to change what Ether tries first. A problem on a card says what to fix.",
            )}
          </p>
          {sourceSub === "debrid" ? <DebridSources addKind={debridAdd} /> : null}
          {sourceSub === "usenet" ? <UsenetProviders addNonce={usenetAdd} /> : null}
          {sourceSub === "indexers" ? (
            <IndexerList version={version} addRequest={indexerAdd} />
          ) : null}
        </>
      ) : null}
      {top === "availability" ? <Availability /> : null}
      {top === "stats" ? <SourceUsage /> : null}
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
