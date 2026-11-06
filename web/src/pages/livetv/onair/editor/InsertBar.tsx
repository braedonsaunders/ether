import { useEffect, useState, type ReactNode } from "react";
import { Button, Popover, Tooltip } from "@braedonsaunders/appkit-ui";
import {
  MousePointer2,
  Type,
  PenTool,
  Square,
  Circle,
  Minus,
  Star,
  Hexagon,
  Image,
  Film,
  Database,
  Sparkles,
  ChevronDown,
  Tv,
  Clock,
  User,
  Text,
} from "lucide-react";
import { t } from "../../../../i18n";
import { LibraryPicker } from "../../library/LibraryPicker";
import type { StyleScene, AssetKind } from "../studio-model";
import type { Layer } from "../scene-renderer";
import { insertLayer, newLayerId } from "./scene-edit";
export function ToolButton(p: {
  label: string;
  shortcut?: string;
  active?: boolean;
  onClick?(): void;
  children: ReactNode;
  disabled?: boolean;
}) {
  return (
    <Tooltip
      content={
        <span className="flex items-center gap-4">
          {p.label}
          {p.shortcut && <kbd className="text-[10px] opacity-60">{p.shortcut}</kbd>}
        </span>
      }
    >
      <Button
        size="sm"
        variant={p.active ? "secondary" : "ghost"}
        className="h-8 min-w-8 px-2"
        aria-label={p.label}
        aria-pressed={p.active}
        disabled={p.disabled}
        onClick={p.onClick}
      >
        {p.children}
      </Button>
    </Tooltip>
  );
}
export function InsertBar(p: {
  scene: StyleScene;
  onChange(scene: StyleScene): void;
  onSelect(ids: string[]): void;
  onPen?(): void;
  onSelectTool?(): void;
  pen?: boolean;
  children?: ReactNode;
  bindings?: Record<string, string>;
}) {
  const [pick, setPick] = useState<AssetKind>();
  const names: Record<string, string> = {
    text: t("Text"),
    path: t("Path"),
    rect: t("Rectangle"),
    ellipse: t("Ellipse"),
    line: t("Line"),
    star: t("Star"),
    polygon: t("Polygon"),
    gradient: t("Gradient"),
    grid: t("Perspective grid"),
    particles: t("Particles"),
    noise: t("Noise"),
    glow: t("Glow"),
  };
  const add = (type: string, props: Partial<Layer> = {}) => {
    const id = newLayerId(p.scene.document, type);
    p.onChange({
      ...p.scene,
      document: insertLayer(p.scene.document, {
        id,
        type,
        name: names[type] ?? type,
        x: 760,
        y: 440,
        w: 400,
        h: 200,
        fill: "{accent}",
        ...props,
      }),
    });
    p.onSelect([id]);
  };
  const shape = (kind: string) => {
    const path =
      kind === "ellipse"
        ? "M0 100 A200 100 0 1 0 400 100 A200 100 0 1 0 0 100 Z"
        : kind === "line"
          ? "M0 0 L400 200"
          : kind === "star"
            ? Array.from({ length: 10 }, (_, i) => {
                const a = (i * Math.PI) / 5 - Math.PI / 2,
                  r = i % 2 ? 80 : 180;
                return `${i ? "L" : "M"}${200 + Math.cos(a) * r} ${200 + Math.sin(a) * r}`;
              }).join(" ") + " Z"
            : kind === "polygon"
              ? "M200 0 L400 100 L300 200 L100 200 L0 100 Z"
              : "M0 0 H400 V200 H0 Z";
    add("path", {
      name: names[kind] ?? kind,
      path,
      stroke: "{text}",
      strokeWidth: kind === "line" ? 4 : 0,
    });
  };
  const [panel, setPanel] = useState("");
  const text = () =>
    add("text", {
      text: t("Your title"),
      font: "{type.display}",
      size: 96,
      fill: "{text}",
      w: undefined,
      h: undefined,
    });
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (
        e.metaKey ||
        e.ctrlKey ||
        e.altKey ||
        (e.target as HTMLElement).closest('input,textarea,select,[contenteditable="true"]')
      )
        return;
      if (e.key.toLowerCase() === "t") {
        e.preventDefault();
        text();
      } else if (e.key.toLowerCase() === "p") {
        e.preventDefault();
        p.onPen?.();
      } else if (e.key.toLowerCase() === "v") {
        e.preventDefault();
        p.onSelectTool?.();
      }
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [p.scene]);
  const dropdown = (key: string, label: string, icon: ReactNode, content: ReactNode) => (
    <Popover
      align="start"
      open={panel === key}
      onOpenChange={(open) => setPanel(open ? key : "")}
      className="w-64 p-3"
      trigger={
        <span>
          <ToolButton label={label} onClick={() => setPanel(panel === key ? "" : key)}>
            {icon}
            <ChevronDown size={10} />
          </ToolButton>
        </span>
      }
    >
      {content}
    </Popover>
  );
  const effect = (kind: string) => {
    add(kind === "gradient" ? "rect" : kind === "glow" ? "ellipse" : kind, {
      name: names[kind],
      glow: kind === "glow" ? 32 : undefined,
      w: 1920,
      h: kind === "grid" ? 440 : 1080,
      x: 0,
      y: kind === "grid" ? 640 : 0,
      fill:
        kind === "gradient"
          ? {
              linear: [0, 0, 1920, 1080],
              stops: [
                [0, "{bg}"],
                [1, "{accent}"],
              ],
            }
          : "{accent}",
      stroke: kind === "grid" ? "{accent}" : undefined,
      strokeWidth: kind === "grid" ? 2 : undefined,
      count: kind === "particles" ? 80 : undefined,
      opacity: kind === "noise" ? 0.1 : 1,
    });
    setPanel("");
  };
  const slots = [
    ["channel.name", t("Channel name"), Tv],
    ["next.title", t("Next programme"), Text],
    ["next.time", t("Next start time"), Clock],
    ["next.poster", t("Next poster"), Image],
    ["next.backdrop", t("Next backdrop"), Image],
    ["now.title", t("Now playing"), Film],
    ["viewer.name", t("Viewer name"), User],
    ["clock", t("Clock"), Clock],
  ] as const;
  return (
    <div
      role="toolbar"
      aria-label={t("Scene tools")}
      className="flex h-11 min-w-max shrink-0 items-center gap-0.5 border-b border-border bg-surface px-2"
    >
      <ToolButton label={t("Select")} shortcut="V" active={!p.pen} onClick={p.onSelectTool}>
        <MousePointer2 size={16} />
      </ToolButton>
      <ToolButton label={t("Text")} shortcut="T" onClick={text}>
        <Type size={17} />
      </ToolButton>
      <ToolButton label={t("Pen")} shortcut="P" active={p.pen} onClick={p.onPen}>
        <PenTool size={16} />
      </ToolButton>
      {dropdown(
        "shape",
        t("Shape"),
        <Square size={16} />,
        <>
          <p className="mb-3 text-xs font-medium text-fg-muted">{t("Insert shape")}</p>
          <div className="grid grid-cols-3 gap-2">
            {[
              ["rect", Square],
              ["ellipse", Circle],
              ["line", Minus],
              ["star", Star],
              ["polygon", Hexagon],
            ].map(([kind, Icon]) => {
              const Component = Icon as typeof Square;
              return (
                <button
                  key={String(kind)}
                  className="flex flex-col items-center gap-2 rounded-lg border border-border p-3 text-xs hover:border-primary hover:bg-primary-subtle"
                  onClick={() => {
                    shape(String(kind));
                    setPanel("");
                  }}
                >
                  <Component size={24} />
                  {names[String(kind)]}
                </button>
              );
            })}
          </div>
        </>,
      )}
      <ToolButton label={t("Image")} onClick={() => setPick("image")}>
        <Image size={16} />
      </ToolButton>
      <ToolButton label={t("Video")} onClick={() => setPick("video")}>
        <Film size={16} />
      </ToolButton>
      {dropdown(
        "data",
        t("Live data"),
        <Database size={16} />,
        <>
          <p className="mb-2 text-xs font-medium text-fg-muted">{t("Make it live")}</p>
          {slots.map(([slot, label, Icon]) => (
            <button
              key={slot}
              className="flex w-full items-center gap-3 rounded-md px-2 py-2 text-left hover:bg-surface-hover"
              onClick={() => {
                add(slot.includes("poster") || slot.includes("backdrop") ? "image" : "text", {
                  name: label,
                  text: `{${slot}}`,
                  src: `{${slot}}`,
                  font: "{type.body}",
                  size: 60,
                  fill: "{text}",
                });
                setPanel("");
              }}
            >
              <Icon size={16} className="shrink-0 text-fg-muted" />
              <span className="min-w-0">
                <span className="block text-xs font-medium">{label}</span>
                <span className="block truncate text-[10px] text-fg-muted">
                  {p.bindings?.[slot] || t("Updates during playback")}
                </span>
              </span>
            </button>
          ))}
        </>,
      )}
      {dropdown(
        "effects",
        t("Effects"),
        <Sparkles size={16} />,
        <>
          <p className="mb-3 text-xs font-medium text-fg-muted">{t("Set the atmosphere")}</p>
          <div className="grid grid-cols-2 gap-2">
            {["gradient", "grid", "particles", "noise", "glow"].map((kind) => (
              <button
                key={kind}
                className="overflow-hidden rounded-lg border border-border text-left hover:border-primary"
                onClick={() => effect(kind)}
              >
                <span
                  className="block h-12"
                  style={{
                    background:
                      kind === "gradient"
                        ? "linear-gradient(120deg,#24104b,#ec4899,#22d3ee)"
                        : kind === "glow"
                          ? "radial-gradient(ellipse,#a78bfa,#24104b 65%)"
                          : kind === "grid"
                            ? "repeating-linear-gradient(0deg,transparent 0 8px,#22d3ee55 9px 10px),repeating-linear-gradient(90deg,#15182e 0 15px,#22d3ee66 16px 17px)"
                            : kind === "noise"
                              ? "repeating-conic-gradient(#a1a1aa 0% 25%,#27272a 0% 50%) 0/3px 3px"
                              : "radial-gradient(circle at 20% 30%,white 1px,transparent 2px),radial-gradient(circle at 70% 60%,#22d3ee 2px,#15182e 3px)",
                  }}
                />
                <span className="block p-2 text-xs">{names[kind]}</span>
              </button>
            ))}
          </div>
        </>,
      )}
      {p.children}
      <LibraryPicker
        open={!!pick}
        kinds={pick ? [pick] : []}
        onClose={() => setPick(undefined)}
        onPick={(asset) => {
          if (asset.kind === "video")
            p.onChange({
              ...p.scene,
              video: { assetDigest: asset.digest, mode: "background", fit: "cover", start: 0 },
            });
          else add("image", { src: asset.digest, name: asset.name });
          setPick(undefined);
        }}
      />
    </div>
  );
}
