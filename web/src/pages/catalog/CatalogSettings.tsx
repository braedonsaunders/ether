import { SettingsSection } from "@braedonsaunders/appkit-ui";
import { Database, Globe2, Sparkles } from "lucide-react";
import { Link } from "react-router-dom";
import { useApp } from "../../app-context";
import { ConfigSection } from "../../components/ConfigSection";
import { secretConfigured } from "../../components/IntegrationAccount";
import { SettingsRow } from "../../components/SettingsRow";
import { LevelBadge } from "../../components/status";
import { t, tx } from "../../i18n";
import {
  HomeReleaseSettings,
  HomeReleaseStatus,
  UpcomingEpisodeSettings,
} from "./HomeReleaseSettings";
import { TrendingStatus } from "./TrendingStatus";
import { CATALOG } from "../settings/schema";

function TmdbAccount({ apiKey }: { apiKey: unknown }) {
  if (secretConfigured(apiKey)) {
    return (
      <p className="text-sm">
        <Link to="/services/tmdb" className="text-primary underline">
          {t("TMDB connected")}
        </Link>
      </p>
    );
  }
  return (
    <p className="text-sm text-fg-muted">
      {tx("Connect TMDB in {link}.", {
        link: (
          <Link to="/services/tmdb" className="text-primary underline">
            {t("Integrations")}
          </Link>
        ),
      })}
    </p>
  );
}

export function CatalogSettings() {
  const { health } = useApp();
  const item = health.find((h) => h.id === "catalog");
  return (
    <div className="catalog-scroll app-scroll">
      <div className="catalog-settings">
        <div className="space-y-5">
          <div>
            <p className="catalog-eyebrow">{t("CATALOG SETTINGS")}</p>
            <h2 className="text-2xl font-semibold tracking-tight">{t("Shape your library")}</h2>
            <p className="mt-2 text-sm text-fg-muted">
              {t("Choose the titles and metadata your viewers discover.")}
            </p>
          </div>
          <ConfigSection
            configKey="catalog"
            endpoint="catalog"
            groups={[...CATALOG, HomeReleaseSettings, UpcomingEpisodeSettings]}
            label={t("Catalog")}
            omit={["tmdbKey"]}
            before={(draft) => <TmdbAccount apiKey={draft.tmdbKey} />}
          />
        </div>
        <aside className="space-y-5" aria-label={t("Catalog status")}>
          <HomeReleaseStatus />
          <TrendingStatus />
          {item ? (
            <SettingsSection title={t("Catalog health")}>
              <SettingsRow
                title={t("TMDB catalog")}
                description={item.details.join(" · ")}
                control={<LevelBadge level={item.level} label={item.summary} />}
              />
            </SettingsSection>
          ) : null}
          <div className="catalog-settings-note">
            <Globe2 size={21} />
            <h3>{t("Metadata, your way")}</h3>
            <p>
              {t(
                "Language and region tailor title names, descriptions, release dates and availability.",
              )}
            </p>
          </div>
          <div className="catalog-settings-note">
            <Sparkles size={21} />
            <h3>{t("A focused library")}</h3>
            <p>
              {t(
                "Popularity and vote thresholds shape browsing. Other released titles remain discoverable through Plex search.",
              )}
            </p>
          </div>
          <div className="catalog-settings-note">
            <Database size={21} />
            <h3>{t("Always up to date")}</h3>
            <p>
              {t(
                "Settings apply to new requests without a restart. Unknown and future release dates stay out of browsing.",
              )}
            </p>
          </div>
        </aside>
      </div>
    </div>
  );
}
