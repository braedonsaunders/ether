import { useRouteTab } from "../../components/layout";
import { ArtworkSettings } from "./Artwork";
import { EpisodeOrders } from "./EpisodeOrder";
import { RatingSources } from "./Ratings";
const TABS = [{ key: "artwork" }, { key: "ratings" }, { key: "episodes" }] as const;
export default function Metadata() {
  const tab = useRouteTab(TABS);
  if (tab === "ratings") return <RatingSources accountOnIntegrations />;
  if (tab === "episodes") return <EpisodeOrders />;
  return <ArtworkSettings />;
}
