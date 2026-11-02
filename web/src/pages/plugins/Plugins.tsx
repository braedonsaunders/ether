/**
 * Server › Plugins: Installed, Catalogue and Develop (docs/extend/SPEC.md
 * WS10a, WS11). Owned by WS10a.
 */
import { DomainPage } from "../../components/layout";
import { t } from "../../i18n";

export default function Plugins() {
  return (
    <DomainPage title={t("Plugins")} base="/plugins">
      {null}
    </DomainPage>
  );
}
