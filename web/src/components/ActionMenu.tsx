import {
  Button,
  ContextMenu,
  useContextMenu,
  type ContextMenuEntry,
} from "@braedonsaunders/appkit-ui";
import { MoreHorizontal } from "lucide-react";
import type { ReactNode, MouseEvent } from "react";
import { t } from "../i18n";

/** A button that opens an AppKit context menu below itself (kebab by default). */
export function ActionMenu({
  items,
  label = t("More actions"),
  children,
  variant = "ghost",
  size = "icon",
  className,
}: {
  items: ContextMenuEntry[];
  label?: string;
  children?: ReactNode;
  variant?: "ghost" | "outline" | "default" | "subtle" | "secondary";
  size?: "sm" | "icon" | "md";
  className?: string;
}) {
  const menu = useContextMenu();
  return (
    <>
      <Button
        type="button"
        variant={variant}
        size={size}
        aria-label={children ? undefined : label}
        aria-haspopup="menu"
        aria-expanded={menu.open}
        className={className ?? (size === "icon" ? "size-8" : undefined)}
        onClick={(e: MouseEvent<HTMLButtonElement>) => {
          e.stopPropagation();
          menu.openBelow(e.currentTarget);
        }}
      >
        {children ?? <MoreHorizontal size={16} />}
      </Button>
      <ContextMenu open={menu.open} position={menu.position} items={items} onClose={menu.close} />
    </>
  );
}
