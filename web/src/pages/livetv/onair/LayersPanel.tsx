import { useEffect, useRef, useState } from "react";
import { Button, Input, Popover } from "@braedonsaunders/appkit-ui";
import {
  Type,
  Square,
  Image,
  Folder,
  Grid2X2,
  Sparkles,
  GripVertical,
  Eye,
  EyeOff,
  Lock,
  Unlock,
  ChevronDown,
  ChevronRight,
  MoreHorizontal,
  Copy,
  Trash2,
  Pencil,
  Film,
} from "lucide-react";
import { t } from "../../../i18n";
import type { Scene, Layer } from "./scene-renderer";
import {
  updateLayer,
  moveLayer,
  duplicateLayers,
  removeLayers,
  layerPath,
} from "./editor/scene-edit";
export function LayersPanel(p: {
  document: Scene;
  selected: string[];
  onSelect(ids: string[]): void;
  onChange(doc: Scene): void;
}) {
  const [expanded, setExpanded] = useState<string[]>(() =>
      p.document.layers.filter((l) => l.type === "group").map((l) => l.id),
    ),
    [rename, setRename] = useState(""),
    [menu, setMenu] = useState("");
  const rowRefs = useRef(new Map<string, HTMLDivElement>());
  useEffect(() => {
    const parents = p.selected.flatMap((id) =>
      layerPath(p.document, id)
        .slice(0, -1)
        .map((l) => l.id),
    );
    setExpanded((current) => [...new Set([...current, ...parents])]);
    const frame = requestAnimationFrame(() => {
      rowRefs.current
        .get(p.selected[0] ?? "")
        ?.scrollIntoView({ block: "nearest", inline: "nearest" });
    });
    return () => cancelAnimationFrame(frame);
  }, [p.selected]);
  const names: Record<string, string> = {
    text: t("Text"),
    path: t("Shape"),
    rect: t("Rectangle"),
    ellipse: t("Ellipse"),
    group: t("Group"),
    image: t("Image"),
    art: t("Image"),
    logo: t("Logo"),
    grid: t("Perspective grid"),
    particles: t("Particles"),
    noise: t("Noise"),
    gradient: t("Gradient"),
    video: t("Video"),
  };
  const icons: Record<string, typeof Type> = {
    text: Type,
    path: Square,
    rect: Square,
    ellipse: Square,
    group: Folder,
    image: Image,
    art: Image,
    logo: Image,
    grid: Grid2X2,
    particles: Sparkles,
    noise: Sparkles,
    gradient: Square,
    video: Film,
  };
  const rows = (layers: Layer[], parent?: string, depth = 0): React.ReactNode =>
    [...layers].reverse().map((l) => (
      <div
        key={l.id}
        role="treeitem"
        aria-level={depth + 1}
        aria-selected={p.selected.includes(l.id)}
        aria-expanded={l.type === "group" ? expanded.includes(l.id) : undefined}
      >
        <div
          draggable
          onDragStart={(e) => e.dataTransfer.setData("text/plain", l.id)}
          onDragOver={(e) => e.preventDefault()}
          onDrop={(e) => {
            e.preventDefault();
            const id = e.dataTransfer.getData("text/plain");
            p.onChange(
              moveLayer(
                p.document,
                id,
                e.shiftKey && l.type === "group" ? l.id : parent,
                e.shiftKey ? (l.layers?.length ?? 0) : layers.indexOf(l),
              ),
            );
          }}
          onContextMenu={(e) => {
            e.preventDefault();
            setMenu(l.id);
          }}
          className={`group flex h-8 items-center gap-1 rounded-md pr-1 text-fg-muted transition-colors ${p.selected.includes(l.id) ? "bg-primary-subtle text-primary" : "hover:bg-surface-hover"}`}
          style={{
            paddingLeft: depth * 12 + 2,
            borderLeft: depth ? "1px solid var(--color-border)" : undefined,
          }}
        >
          <GripVertical
            size={10}
            className="shrink-0 cursor-grab opacity-0 group-hover:opacity-50"
          />
          {l.type === "group" ? (
            <button
              className="shrink-0 rounded p-0.5 hover:bg-surface-hover"
              aria-label={expanded.includes(l.id) ? t("Collapse group") : t("Expand group")}
              onClick={() =>
                setExpanded((c) =>
                  c.includes(l.id) ? c.filter((id) => id !== l.id) : [...c, l.id],
                )
              }
            >
              {expanded.includes(l.id) ? <ChevronDown size={12} /> : <ChevronRight size={12} />}
            </button>
          ) : (
            <span className="w-4 shrink-0" />
          )}
          {(() => {
            const Icon = icons[l.type] ?? Square;
            return <Icon size={13} className="mr-1 shrink-0 opacity-70" />;
          })()}
          {rename === l.id ? (
            <Input
              autoFocus
              aria-label={t("Layer name")}
              value={l.name ?? names[l.type] ?? t("Layer")}
              onChange={(e) => p.onChange(updateLayer(p.document, l.id, { name: e.target.value }))}
              onBlur={() => setRename("")}
              onKeyDown={(e) => {
                if (e.key === "Enter") setRename("");
              }}
            />
          ) : (
            <button
              className="min-w-0 flex-1 truncate text-left text-xs text-fg"
              onDoubleClick={() => setRename(l.id)}
              onClick={(e) => p.onSelect(e.shiftKey ? [...new Set([...p.selected, l.id])] : [l.id])}
            >
              {l.name || names[l.type] || t("Layer")}
            </button>
          )}
          <button
            className={`rounded p-1 hover:bg-surface-hover ${l.hidden ? "text-primary" : "opacity-0 group-hover:opacity-100 group-focus-within:opacity-100"}`}
            aria-label={l.hidden ? t("Show layer") : t("Hide layer")}
            onClick={() => p.onChange(updateLayer(p.document, l.id, { hidden: !l.hidden }))}
          >
            {l.hidden ? <EyeOff size={12} /> : <Eye size={12} />}
          </button>
          <button
            className={`rounded p-1 hover:bg-surface-hover ${l.locked ? "text-primary" : "opacity-0 group-hover:opacity-100 group-focus-within:opacity-100"}`}
            aria-label={l.locked ? t("Unlock layer") : t("Lock layer")}
            onClick={() => p.onChange(updateLayer(p.document, l.id, { locked: !l.locked }))}
          >
            {l.locked ? <Lock size={12} /> : <Unlock size={12} />}
          </button>
          <Popover
            open={menu === l.id}
            onOpenChange={(open) => setMenu(open ? l.id : "")}
            className="w-44 p-1"
            trigger={
              <button
                className="rounded p-1 opacity-0 hover:bg-surface-hover group-hover:opacity-100 group-focus-within:opacity-100"
                aria-label={t("Layer actions")}
                onClick={() => setMenu(menu === l.id ? "" : l.id)}
              >
                <MoreHorizontal size={12} />
              </button>
            }
          >
            <Button
              size="sm"
              variant="ghost"
              className="w-full justify-start gap-2"
              onClick={() => {
                setRename(l.id);
                setMenu("");
              }}
            >
              <Pencil size={13} />
              {t("Rename")}
            </Button>
            <Button
              size="sm"
              variant="ghost"
              className="w-full justify-start gap-2"
              onClick={() => {
                p.onChange(duplicateLayers(p.document, [l.id]));
                setMenu("");
              }}
            >
              <Copy size={13} />
              {t("Duplicate")}
            </Button>
            <Button
              size="sm"
              variant="ghost"
              className="w-full justify-start gap-2 text-danger"
              onClick={() => {
                p.onChange(removeLayers(p.document, [l.id]));
                setMenu("");
              }}
            >
              <Trash2 size={13} />
              {t("Delete")}
            </Button>
          </Popover>
        </div>
        {l.layers && expanded.includes(l.id) && (
          <div role="group">{rows(l.layers, l.id, depth + 1)}</div>
        )}
      </div>
    ));
  return (
    <section aria-label={t("Layers")} className="space-y-3">
      <h3 className="text-[11px] font-semibold uppercase tracking-wider text-fg-muted">
        {t("Layers")}
      </h3>
      <div role="tree" aria-label={t("Scene layers")}>
        {rows(p.document.layers)}
      </div>
    </section>
  );
}
