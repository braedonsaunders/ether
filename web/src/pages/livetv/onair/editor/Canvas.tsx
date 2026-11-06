import { useEffect, useMemo, useRef, useState } from "react";
import { Input } from "@braedonsaunders/appkit-ui";
import { t } from "../../../../i18n";
import {
  renderDocument,
  releaseRenderer,
  evaluate,
  type Scene,
  type SceneBindings,
  type Layer,
  type Value,
} from "../scene-renderer";
import { Handles, pathNodes } from "./Handles";
import { isAnimated, setKeyframe } from "./keyframes";
import { layerBounds } from "./scene-bounds";
import {
  findLayer,
  updateLayer,
  removeLayers,
  duplicateLayers,
  groupLayers,
  ungroup,
  insertLayer,
  newLayerId,
  layerPath,
  selectionPath,
  selectionTarget,
  sharedTextLayers,
} from "./scene-edit";
export type CanvasProps = {
  document: Scene;
  bindings: SceneBindings;
  time: number;
  selected: string[];
  onSelect(ids: string[]): void;
  onChange(doc: Scene): void;
  safe?: boolean;
  fit?: boolean;
  zoom?: number;
  pen?: boolean;
  onPenDone?(): void;
  onGestureStart?(): void;
  onGestureEnd?(): void;
  video?: React.ReactNode;
};
// Preserve animation tracks when an ordinary editor gesture changes an animated value.
export function editValue(
  layer: Layer,
  key: keyof Layer,
  value: Value,
  time: number,
): Partial<Layer> {
  return isAnimated(layer[key])
    ? { [key]: setKeyframe(layer, key, time, value)[key] }
    : { [key]: value };
}
type Geometry = ReturnType<typeof layerBounds>;
type SelectionBox = { x: number; y: number; w: number; h: number; rotate: number };
function boxFor(ids: string[], geometry: Geometry): SelectionBox | undefined {
  const chosen = geometry.filter((b) => ids.includes(b.id));
  const first = chosen[0];
  if (!first) return;
  if (chosen.length === 1)
    return {
      x: first.x,
      y: first.y,
      w: Math.abs(first.w * first.scaleX),
      h: Math.abs(first.h * first.scaleY),
      rotate: first.rotate,
    };
  const x = Math.min(...chosen.map((b) => b.world.x)),
    y = Math.min(...chosen.map((b) => b.world.y));
  return {
    x,
    y,
    w: Math.max(...chosen.map((b) => b.world.x + b.world.w)) - x,
    h: Math.max(...chosen.map((b) => b.world.y + b.world.h)) - y,
    rotate: 0,
  };
}
export function Canvas(p: CanvasProps) {
  const viewport = useRef<HTMLDivElement>(null);
  const [fitWidth, setFitWidth] = useState(960);
  useEffect(() => {
    if (!p.fit || !viewport.current) return;
    const observer = new ResizeObserver(([entry]) => {
      if (entry)
        setFitWidth(
          Math.max(120, Math.min(entry.contentRect.width, (entry.contentRect.height * 16) / 9)),
        );
    });
    observer.observe(viewport.current);
    return () => observer.disconnect();
  }, [p.fit]);
  const canvas = useRef<HTMLCanvasElement>(null),
    host = useRef<HTMLDivElement>(null);
  const [scope, setScope] = useState<string>();
  useEffect(() => {
    if (
      scope &&
      (!p.selected.length ||
        p.selected.some(
          (id) => id === scope || !layerPath(p.document, id).some((l) => l.id === scope),
        ))
    )
      setScope(undefined);
  }, [p.selected, scope]);
  const [hover, setHover] = useState<string>(),
    [editing, setEditing] = useState<string>(),
    [marquee, setMarquee] = useState<{ x: number; y: number; w: number; h: number }>(),
    [guides, setGuides] = useState<{ x?: number; y?: number }>({});
  const geometry = useMemo(
    () => layerBounds(p.document, p.bindings, p.time),
    [p.document, p.bindings, p.selected.length ? p.time : 0],
  );
  const [penPreview, setPenPreview] = useState("");
  const penPoints = useRef<{ x: number; y: number; cx?: number; cy?: number }[]>([]);
  useEffect(() => {
    if (!p.pen) {
      penPoints.current = [];
      setPenPreview("");
    }
  }, [p.pen]);
  const gesture = useRef<
    | {
        x: number;
        y: number;
        doc: Scene;
        ids: string[];
        mode: string;
        bounds?: Geometry;
        box?: SelectionBox;
      }
    | undefined
  >(undefined);
  useEffect(() => {
    const ctx = canvas.current?.getContext("2d");
    if (ctx) renderDocument(ctx, p.document, p.bindings, p.time);
  }, [p.document, p.bindings, p.time]);
  useEffect(
    () => () => {
      const ctx = canvas.current?.getContext("2d");
      if (ctx) releaseRenderer(ctx);
    },
    [],
  );
  const selectedRoots = p.selected.filter(
    (id) =>
      !layerPath(p.document, id)
        .slice(0, -1)
        .some((l) => p.selected.includes(l.id)),
  );
  const selectionBox = boxFor(selectedRoots, geometry);
  const frameWidth = p.fit ? fitWidth * (p.zoom ?? 1) : (host.current?.clientWidth ?? 960);
  const screenUnit = 1920 / Math.max(120, frameWidth),
    touch = frameWidth < 600;
  const parentMatrix = (doc: Scene, id: string, bounds: Geometry) => {
    const path = layerPath(doc, id);
    const parent = path.at(-2);
    const matrix = parent ? bounds.find((b) => b.id === parent.id)?.matrix : undefined;
    return matrix ? new DOMMatrix(matrix) : new DOMMatrix();
  };
  const point = (e: { clientX: number; clientY: number }) => {
    const r = host.current!.getBoundingClientRect();
    return {
      x: ((e.clientX - r.left) * 1920) / r.width,
      y: ((e.clientY - r.top) * 1080) / r.height,
    };
  };
  const hit = (q: { x: number; y: number }) =>
    [...geometry].reverse().find((g) => {
      const l = findLayer(p.document, g.id);
      const locked = (layers: Layer[], ancestor = false): boolean => {
        for (const layer of layers) {
          const blocked = ancestor || !!layer.locked;
          if (layer.id === g.id) return blocked;
          if (layer.layers && findLayer({ ...p.document, layers: layer.layers }, g.id))
            return locked(layer.layers, blocked);
        }
        return false;
      };
      if (locked(p.document.layers) || l?.type === "group") return false;
      const m = new DOMMatrix(g.matrix).inverse();
      const pt = m.transformPoint(q);
      return (
        pt.x >= g.local.x && pt.y >= g.local.y && pt.x <= g.local.x + g.w && pt.y <= g.local.y + g.h
      );
    });
  const move = (e: React.PointerEvent) => {
    const q = point(e),
      g = gesture.current;
    if (p.pen && g) {
      const last = penPoints.current.at(-1);
      if (last) {
        last.cx = q.x;
        last.cy = q.y;
        setPenPreview(penPoints.current.map((p, i) => `${i ? "L" : "M"}${p.x} ${p.y}`).join(" "));
      }
      return;
    }
    if (!g) {
      const id = hit(q)?.id;
      setHover(id ? selectionTarget(p.document, id, scope, e.metaKey || e.ctrlKey) : undefined);
      return;
    }
    let dx = q.x - g.x,
      dy = q.y - g.y;
    if (e.shiftKey && g.mode === "move") {
      if (Math.abs(dx) > Math.abs(dy)) dy = 0;
      else dx = 0;
    }
    if (g.mode === "marquee") {
      setMarquee({
        x: Math.min(g.x, q.x),
        y: Math.min(g.y, q.y),
        w: Math.abs(dx),
        h: Math.abs(dy),
      });
      return;
    }
    let doc = g.doc;
    const nextGuides: { x?: number; y?: number } = {};
    for (const id of g.ids) {
      const l = findLayer(g.doc, id);
      if (!l || l.locked) continue;
      const n = (key: keyof Layer, f = 0) =>
        Number(evaluate(l[key] as Value, p.bindings, p.time) ?? f);
      let patch: Partial<Layer> = {};
      if (g.mode.startsWith("node:")) {
        const [, xi, yi, ox, oy] = g.mode.split(":").map(Number);
        const bounds = geometry.find((b) => b.id === id);
        if (bounds) {
          const at = new DOMMatrix(bounds.matrix).inverse().transformPoint(q);
          const nodes = pathNodes(l.path ?? "");
          nodes.tokens[xi!] = String(at.x - ox!);
          nodes.tokens[yi!] = String(at.y - oy!);
          doc = updateLayer(doc, id, { path: nodes.tokens.join(" ") });
        }
        continue;
      }
      const bounds = g.bounds ?? geometry,
        box = g.box;
      const original = bounds.find((b) => b.id === id);
      const parent = parentMatrix(g.doc, id, bounds),
        inverseParent = parent.inverse();
      if ((g.mode === "rotate" || g.mode.startsWith("resize:")) && box && original) {
        const cos = Math.cos(box.rotate),
          sin = Math.sin(box.rotate);
        let anchor = { x: original.anchor.x, y: original.anchor.y };
        if (g.mode === "rotate") {
          const center = {
            x: box.x + (cos * box.w) / 2 - (sin * box.h) / 2,
            y: box.y + (sin * box.w) / 2 + (cos * box.h) / 2,
          };
          let angle =
            Math.atan2(q.y - center.y, q.x - center.x) - Math.atan2(g.y - center.y, g.x - center.x);
          if (e.shiftKey) angle = Math.round(angle / (Math.PI / 12)) * (Math.PI / 12);
          const ax = anchor.x - center.x,
            ay = anchor.y - center.y;
          anchor = {
            x: center.x + ax * Math.cos(angle) - ay * Math.sin(angle),
            y: center.y + ax * Math.sin(angle) + ay * Math.cos(angle),
          };
          patch = editValue(l, "rotate", n("rotate") + angle, p.time);
        } else {
          const side = g.mode.slice(7),
            localDx = dx * cos + dy * sin,
            localDy = -dx * sin + dy * cos;
          let sx = side.includes("e")
            ? 1 + localDx / Math.max(1, box.w)
            : side.includes("w")
              ? 1 - localDx / Math.max(1, box.w)
              : 1;
          let sy = side.includes("s")
            ? 1 + localDy / Math.max(1, box.h)
            : side.includes("n")
              ? 1 - localDy / Math.max(1, box.h)
              : 1;
          if (e.shiftKey) {
            const ratio = Math.abs(sx - 1) > Math.abs(sy - 1) ? sx : sy;
            sx = ratio;
            sy = ratio;
          }
          sx = Math.max(0.01, sx);
          sy = Math.max(0.01, sy);
          const ax = side.includes("w") ? box.w : side.includes("e") ? 0 : box.w / 2,
            ay = side.includes("n") ? box.h : side.includes("s") ? 0 : box.h / 2;
          const lx = (anchor.x - box.x) * cos + (anchor.y - box.y) * sin,
            ly = -(anchor.x - box.x) * sin + (anchor.y - box.y) * cos;
          const nx = ax + (lx - ax) * sx,
            ny = ay + (ly - ay) * sy;
          anchor = { x: box.x + nx * cos - ny * sin, y: box.y + nx * sin + ny * cos };
          patch = {
            ...editValue(l, "scaleX", n("scaleX", 1) * sx, p.time),
            ...editValue(l, "scaleY", n("scaleY", 1) * sy, p.time),
          };
        }
        const local = inverseParent.transformPoint(anchor);
        patch = {
          ...patch,
          ...editValue(l, "x", local.x, p.time),
          ...editValue(l, "y", local.y, p.time),
        };
      } else {
        let x = n("x") + dx,
          y = n("y") + dy;
        const xs = [
            0,
            960,
            1920,
            ...geometry
              .filter((b) => !g.ids.includes(b.id))
              .flatMap((b) => [b.x, b.x + b.w / 2, b.x + b.w]),
          ],
          ys = [
            0,
            540,
            1080,
            ...geometry
              .filter((b) => !g.ids.includes(b.id))
              .flatMap((b) => [b.y, b.y + b.h / 2, b.y + b.h]),
          ];
        const snapX = xs.find((v) => Math.abs(v - x) < 8),
          snapY = ys.find((v) => Math.abs(v - y) < 8);
        if (snapX !== undefined) {
          x = snapX;
          nextGuides.x = x;
        }
        if (snapY !== undefined) {
          y = snapY;
          nextGuides.y = y;
        }
        const delta = inverseParent.transformPoint({ x: x - n("x"), y: y - n("y") }),
          zero = inverseParent.transformPoint({ x: 0, y: 0 });
        patch = {
          ...editValue(l, "x", n("x") + delta.x - zero.x, p.time),
          ...editValue(l, "y", n("y") + delta.y - zero.y, p.time),
        };
      }
      doc = updateLayer(doc, id, patch);
    }
    setGuides(nextGuides);
    p.onChange(doc);
  };
  return (
    <div
      ref={viewport}
      className={
        p.fit
          ? "relative flex h-full min-h-0 items-center justify-center overflow-auto p-6"
          : "overflow-auto p-4"
      }
      style={{
        backgroundImage:
          "radial-gradient(color-mix(in srgb, var(--color-border, #ffffff12) 35%, transparent) 1px, transparent 1px)",
        backgroundSize: "12px 12px",
      }}
    >
      {p.pen && (
        <p className="mb-2 text-sm text-fg-muted">
          {t("Click to add corners, drag to curve, and double-click to close the path.")}
        </p>
      )}
      <div
        ref={host}
        className="relative mx-auto aspect-video shrink-0 touch-none bg-black shadow-[0_8px_40px_#0005] ring-1 ring-white/10 outline-none focus-visible:ring-2 focus-visible:ring-primary"
        style={{ width: p.fit ? fitWidth * (p.zoom ?? 1) : `${(p.zoom ?? 1) * 100}%` }}
        tabIndex={0}
        role="application"
        aria-label={t("Scene canvas")}
        onPointerMove={move}
        onPointerLeave={() => setHover(undefined)}
        onPointerCancel={() => {
          gesture.current = undefined;
          setMarquee(undefined);
          setGuides({});
          p.onGestureEnd?.();
        }}
        onPointerDown={(e) => {
          if ((e.target as HTMLElement).closest("input")) return;
          p.onGestureStart?.();
          const q = point(e);
          if (p.pen) {
            penPoints.current.push(q);
            gesture.current = { ...q, doc: p.document, ids: [], mode: "pen" };
            return;
          }
          const target = (e.target as HTMLElement).dataset.mode;
          const leaf = hit(q)?.id;
          if (scope && leaf && !selectionPath(p.document, leaf).some((l) => l.id === scope))
            setScope(undefined);
          const id = leaf
            ? selectionTarget(p.document, leaf, scope, e.metaKey || e.ctrlKey)
            : undefined;
          const ids = target
            ? p.selected
            : id
              ? e.shiftKey
                ? p.selected.includes(id)
                  ? p.selected.filter((s) => s !== id)
                  : [...p.selected, id]
                : p.selected.includes(id)
                  ? p.selected
                  : [id]
              : [];
          p.onSelect(ids);
          gesture.current = {
            ...q,
            doc: p.document,
            ids: ids.filter(
              (id) =>
                !layerPath(p.document, id)
                  .slice(0, -1)
                  .some((l) => ids.includes(l.id)),
            ),
            mode: target ?? (id ? "move" : "marquee"),
            bounds: geometry,
            box: boxFor(ids, geometry),
          };
          e.currentTarget.setPointerCapture(e.pointerId);
        }}
        onPointerUp={() => {
          p.onGestureEnd?.();
          if (p.pen) {
            gesture.current = undefined;
            return;
          }
          if (marquee) {
            const ids = [
              ...new Set(
                geometry
                  .filter(
                    (b) =>
                      findLayer(p.document, b.id)?.type !== "group" &&
                      !layerPath(p.document, b.id).some((l) => l.locked) &&
                      b.world.x >= marquee.x &&
                      b.world.y >= marquee.y &&
                      b.world.x + b.world.w <= marquee.x + marquee.w &&
                      b.world.y + b.world.h <= marquee.y + marquee.h,
                  )
                  .map((b) => selectionTarget(p.document, b.id, scope)),
              ),
            ];
            p.onSelect(
              ids.filter(
                (id) =>
                  !layerPath(p.document, id)
                    .slice(0, -1)
                    .some((l) => ids.includes(l.id)),
              ),
            );
          }
          gesture.current = undefined;
          setMarquee(undefined);
          setGuides({});
        }}
        onDoubleClick={(e) => {
          if (p.pen) {
            const points = penPoints.current;
            const first = points[0];
            if (first) {
              const path =
                points
                  .map((q, i) =>
                    i
                      ? q.cx !== undefined
                        ? `Q${q.cx} ${q.cy} ${q.x} ${q.y}`
                        : `L${q.x} ${q.y}`
                      : `M${q.x} ${q.y}`,
                  )
                  .join(" ") + " Z";
              const id = newLayerId(p.document, "path");
              p.onChange(
                insertLayer(p.document, {
                  id,
                  type: "path",
                  name: t("Pen path"),
                  path,
                  fill: "{accent}",
                  stroke: "{text}",
                  strokeWidth: 2,
                }),
              );
              p.onSelect([id]);
            }
            penPoints.current = [];
            setPenPreview("");
            p.onPenDone?.();
            return;
          }
          const leaf = hit(point(e))?.id;
          if (!leaf) return;
          const path = selectionPath(p.document, leaf),
            current = p.selected[0];
          const index = path.findIndex((l) => l.id === current);
          const selected = findLayer(p.document, current ?? "");
          if (current === leaf && selected?.type === "text") {
            setEditing(leaf);
            return;
          }
          const next = path[Math.min(path.length - 1, index + 1)];
          if (next) {
            setScope(index >= 0 ? path[index]!.id : undefined);
            p.onSelect([next.id]);
          }
        }}
        onKeyDown={(e) => {
          if ((e.target as HTMLElement).matches("input,textarea")) return;
          if (e.key === "Escape") {
            e.preventDefault();
            e.stopPropagation();
            const path = selectionPath(p.document, p.selected[0] ?? "");
            const parent = path.at(-2);
            setScope(parent ? selectionPath(p.document, parent.id).at(-2)?.id : undefined);
            p.onSelect(parent ? [parent.id] : []);
            return;
          }
          let doc = p.document;
          const cmd = e.metaKey || e.ctrlKey;
          if (e.key === "Delete" || e.key === "Backspace") doc = removeLayers(doc, p.selected);
          else if (cmd && e.key.toLowerCase() === "d") doc = duplicateLayers(doc, p.selected);
          else if (cmd && e.key.toLowerCase() === "g")
            doc = e.shiftKey
              ? p.selected.reduce((d, id) => ungroup(d, id), doc)
              : groupLayers(doc, p.selected);
          else if (e.key.startsWith("Arrow")) {
            for (const id of p.selected) {
              const l = findLayer(doc, id);
              if (!l || l.locked) continue;
              const key = e.key === "ArrowLeft" || e.key === "ArrowRight" ? "x" : "y";
              const delta =
                (e.shiftKey ? 10 : 1) * (e.key === "ArrowLeft" || e.key === "ArrowUp" ? -1 : 1);
              doc = updateLayer(
                doc,
                id,
                editValue(
                  l,
                  key,
                  Number(evaluate(l[key], p.bindings, p.time) ?? 0) + delta,
                  p.time,
                ),
              );
            }
          } else return;
          e.preventDefault();
          p.onChange(doc);
        }}
      >
        {!p.document.layers.length && (
          <div className="pointer-events-none absolute inset-0 z-10 flex flex-col items-center justify-center gap-2 bg-bg-subtle text-fg-muted">
            <span className="text-sm font-medium">{t("Your scene starts here")}</span>
            <span className="max-w-64 px-4 text-center text-xs">
              {t("Press T to add text, or pick a template scene")}
            </span>
          </div>
        )}
        {p.video}
        <canvas ref={canvas} width={960} height={540} className="h-full w-full" />
        <svg
          viewBox="0 0 1920 1080"
          className="pointer-events-none absolute inset-0 h-full w-full overflow-visible"
          aria-hidden="true"
        >
          {penPreview && <path d={penPreview} fill="none" stroke="#76a9ff" strokeWidth="3" />}
          {p.safe && (
            <g fill="none" stroke="#ffffff88" strokeWidth="2" strokeDasharray="12 8">
              <rect x="96" y="54" width="1728" height="972" />
              <rect x="192" y="108" width="1536" height="864" />
            </g>
          )}
          {geometry
            .filter((b) => b.id === hover && !p.selected.includes(b.id))
            .map((b) => (
              <g
                key={b.id}
                transform={`translate(${b.x} ${b.y}) rotate(${(b.rotate * 180) / Math.PI})`}
                fill="none"
                stroke="#76a9ff"
                strokeWidth={screenUnit}
              >
                <rect width={b.w * b.scaleX} height={b.h * b.scaleY} />
              </g>
            ))}
          {selectionBox && (
            <g
              transform={`translate(${selectionBox.x} ${selectionBox.y}) rotate(${(selectionBox.rotate * 180) / Math.PI})`}
              fill="none"
              stroke="#76a9ff"
              strokeWidth={screenUnit}
            >
              <rect width={selectionBox.w} height={selectionBox.h} />
              {(
                [
                  ["nw", 0, 0],
                  ["n", 0.5, 0],
                  ["ne", 1, 0],
                  ["e", 1, 0.5],
                  ["se", 1, 1],
                  ["s", 0.5, 1],
                  ["sw", 0, 1],
                  ["w", 0, 0.5],
                ] as const
              ).map(([side, x, y]) => {
                const cx = x * selectionBox.w,
                  cy = y * selectionBox.h,
                  size = (touch ? 11 : 7) * screenUnit,
                  target = (touch ? 30 : 18) * screenUnit;
                return (
                  <g key={side}>
                    <rect
                      data-mode={`resize:${side}`}
                      className={`pointer-events-auto ${side === "n" || side === "s" ? "cursor-ns-resize" : side === "e" || side === "w" ? "cursor-ew-resize" : side === "nw" || side === "se" ? "cursor-nwse-resize" : "cursor-nesw-resize"}`}
                      x={cx - target / 2}
                      y={cy - target / 2}
                      width={target}
                      height={target}
                      fill="transparent"
                      stroke="none"
                    />
                    <rect
                      className="pointer-events-none"
                      x={cx - size / 2}
                      y={cy - size / 2}
                      width={size}
                      height={size}
                      fill="white"
                    />
                  </g>
                );
              })}
              <line x1={selectionBox.w / 2} x2={selectionBox.w / 2} y1="0" y2={-30 * screenUnit} />
              <circle
                data-mode="rotate"
                className="pointer-events-auto cursor-grab"
                cx={selectionBox.w / 2}
                cy={-30 * screenUnit}
                r={(touch ? 16 : 10) * screenUnit}
                fill="transparent"
                stroke="none"
              />
              <circle
                className="pointer-events-none"
                cx={selectionBox.w / 2}
                cy={-30 * screenUnit}
                r={(touch ? 6 : 4) * screenUnit}
                fill="white"
              />
            </g>
          )}
          {geometry
            .filter((b) => p.selected.includes(b.id))
            .map((b) => (
              <Handles
                key={`nodes-${b.id}`}
                layer={findLayer(p.document, b.id)!}
                matrix={b.matrix}
              />
            ))}
          {marquee && <rect {...marquee} fill="#76a9ff33" stroke="#76a9ff" />}
          {guides.x !== undefined && (
            <line x1={guides.x} x2={guides.x} y1="0" y2="1080" stroke="#ff77d4" />
          )}
          {guides.y !== undefined && (
            <line y1={guides.y} y2={guides.y} x1="0" x2="1920" stroke="#ff77d4" />
          )}
        </svg>
        {editing && (
          <Input
            autoFocus
            className="absolute left-1/4 top-1/2 w-1/2"
            aria-label={t("Edit text")}
            value={String(
              typeof findLayer(p.document, editing)?.text === "string"
                ? findLayer(p.document, editing)?.text
                : (evaluate(findLayer(p.document, editing)?.text, p.bindings, p.time) ?? ""),
            )}
            onChange={(e) => {
              const stack = [...layerPath(p.document, editing)]
                .reverse()
                .find((l) => l.type === "group" && sharedTextLayers(l).length > 1);
              const targets = stack ? sharedTextLayers(stack) : [findLayer(p.document, editing)!];
              p.onChange(
                targets.reduce(
                  (document, layer) =>
                    updateLayer(
                      document,
                      layer.id,
                      editValue(layer, "text", e.target.value, p.time),
                    ),
                  p.document,
                ),
              );
            }}
            onBlur={() => setEditing(undefined)}
            onKeyDown={(e) => {
              if (e.key === "Enter" || e.key === "Escape") setEditing(undefined);
            }}
          />
        )}
      </div>
    </div>
  );
}
