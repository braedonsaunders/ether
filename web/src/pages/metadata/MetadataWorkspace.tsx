import { Button } from "@braedonsaunders/appkit-ui";
import type { ReactNode } from "react";
import { NavLink, useLocation } from "react-router-dom";
import { Images, ListVideo, Star } from "lucide-react";
import { Workspace } from "../../components/workspace";
import { localized, t } from "../../i18n";
import "./metadata.css";

const sections = localized(() => [
  { key: "artwork", label: t("Artwork"), icon: Images },
  { key: "ratings", label: t("Ratings"), icon: Star },
  { key: "episodes", label: t("Episode order"), icon: ListVideo },
]);

export function MetadataWorkspace({
  aside,
  main,
  actions,
  fitPreview = false,
}: {
  aside: ReactNode;
  main: ReactNode;
  actions?: ReactNode;
  fitPreview?: boolean;
}) {
  const location = useLocation();
  return (
    <Workspace
      title={t("Metadata")}
      actions={
        <>
          {actions}
          <Button
            size="sm"
            variant="ghost"
            className="lg:hidden"
            onClick={() =>
              document.querySelector(".metadata-canvas")?.scrollIntoView({ block: "start" })
            }
          >
            {t("Preview ↓")}
          </Button>
        </>
      }
      asideLabel={t("Metadata controls")}
      mainLabel={t("Metadata preview")}
      mainClassName={`metadata-canvas ${fitPreview ? "metadata-canvas-fit" : ""}`}
      aside={
        <>
          <nav className="metadata-nav" aria-label={t("Metadata sections")}>
            {sections.map(({ key, label, icon: Icon }) => (
              <NavLink
                key={key}
                to={`/metadata/${key}`}
                className={({ isActive }) =>
                  `metadata-nav-item ${isActive || (key === "artwork" && location.pathname.endsWith("/metadata")) ? "is-active" : ""}`
                }
              >
                <Icon size={15} />
                <span>{label}</span>
              </NavLink>
            ))}
          </nav>
          <div className="metadata-controls">{aside}</div>
        </>
      }
      main={main}
    />
  );
}
