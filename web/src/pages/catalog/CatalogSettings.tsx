import { SettingsSection } from "@braedonsaunders/appkit-ui";
import { Database, Globe2, Sparkles } from "lucide-react";
import { useState } from "react";
import { useApp } from "../../app-context";
import { ConfigSection } from "../../components/ConfigSection";
import { TestButton, type TestState } from "../../components/fields";
import { SettingsRow } from "../../components/SettingsRow";
import { LevelBadge } from "../../components/status";
import { t } from "../../i18n";
import { api } from "../../lib/api";
import { HomeReleaseSettings, HomeReleaseStatus } from "./HomeReleaseSettings";
import { TrendingStatus } from "./TrendingStatus";
import { CATALOG } from "../settings/schema";

export function CatalogSettings() {
  const { health } = useApp();
  const item = health.find((h) => h.id === "catalog");
  const [test, setTest] = useState<TestState>({ status: "idle" });
  const runTest = (key: string) => {
    setTest({ status: "running" });
    api.testTMDB(key).then(
      (r) =>
        setTest(
          r.ok
            ? { status: "ok", durationMs: r.durationMs }
            : { status: "fail", message: r.error ?? t("Test failed") },
        ),
      (e) => setTest({ status: "fail", message: (e as Error).message }),
    );
  };
  return (
    <div className="catalog-scroll app-scroll">
      <div className="catalog-settings">
        <div className="space-y-5">
          <div>
            <p className="catalog-eyebrow">{t("CATALOG SETTINGS")}</p>
            <h2 className="text-2xl font-semibold tracking-tight">{t("Shape your library")}</h2>
            <p className="mt-2 text-sm text-fg-muted">
              {t("Connect TMDB and choose the titles and metadata your viewers discover.")}
            </p>
          </div>
          <ConfigSection
            configKey="catalog"
            endpoint="catalog"
            groups={[...CATALOG, HomeReleaseSettings]}
            label={t("Catalog")}
            test={(draft) => (
              <TestButton
                label={t("Test TMDB key")}
                state={test}
                onTest={() => runTest(String(draft.tmdbKey ?? ""))}
              />
            )}
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
