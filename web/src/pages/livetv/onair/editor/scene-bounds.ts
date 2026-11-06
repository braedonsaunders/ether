import { layerGeometry, evaluate, type Scene, type SceneBindings } from "../scene-renderer";
import { findLayer } from "./scene-edit";
type Point = { x: number; y: number };
export function pathBounds(path: string) {
  const tokens = path.match(/[a-zA-Z]|[-+]?(?:\d*\.\d+|\d+\.?\d*)(?:e[-+]?\d+)?/g) ?? [];
  let i = 0,
    cmd = "",
    p: Point = { x: 0, y: 0 },
    start = p,
    control = p,
    previous = "";
  const points: Point[] = [];
  const push = (q: Point) => {
    points.push(q);
    return q;
  };
  const number = () => Number(tokens[i++]);
  const curve = (a: Point, b: Point, c: Point, d: Point) => {
    push(a);
    push(d);
    const at = (t: number) => ({
      x: (1 - t) ** 3 * a.x + 3 * (1 - t) ** 2 * t * b.x + 3 * (1 - t) * t * t * c.x + t ** 3 * d.x,
      y: (1 - t) ** 3 * a.y + 3 * (1 - t) ** 2 * t * b.y + 3 * (1 - t) * t * t * c.y + t ** 3 * d.y,
    });
    for (const key of ["x", "y"] as const) {
      const A = -a[key] + 3 * b[key] - 3 * c[key] + d[key],
        B = 2 * (a[key] - 2 * b[key] + c[key]),
        C = b[key] - a[key];
      const roots =
        Math.abs(A) < 1e-10
          ? [-C / B]
          : [
              (-B + Math.sqrt(B * B - 4 * A * C)) / (2 * A),
              (-B - Math.sqrt(B * B - 4 * A * C)) / (2 * A),
            ];
      roots.filter((t) => t > 0 && t < 1).forEach((t) => push(at(t)));
    }
  };
  while (i < tokens.length) {
    if (/[a-z]/i.test(tokens[i]!) && !/^e$/i.test(tokens[i]!)) cmd = tokens[i++]!;
    const upper = cmd.toUpperCase(),
      relative = cmd !== upper;
    const point = () => ({
      x: number() + (relative ? p.x : 0),
      y: number() + (relative ? p.y : 0),
    });
    const old = p;
    if (upper === "Z") {
      push((p = start));
      cmd = "";
      previous = upper;
      continue;
    }
    if (!cmd) break;
    if (upper === "M" || upper === "L") {
      p = push(point());
      if (upper === "M") {
        start = p;
        cmd = relative ? "l" : "L";
      }
    } else if (upper === "H") p = push({ x: number() + (relative ? p.x : 0), y: p.y });
    else if (upper === "V") p = push({ x: p.x, y: number() + (relative ? p.y : 0) });
    else if (upper === "C" || upper === "S") {
      const a =
        upper === "C"
          ? point()
          : previous === "C" || previous === "S"
            ? { x: 2 * p.x - control.x, y: 2 * p.y - control.y }
            : p;
      const b = point(),
        end = point();
      curve(p, a, b, end);
      control = b;
      p = end;
    } else if (upper === "Q" || upper === "T") {
      const a =
        upper === "Q"
          ? point()
          : previous === "Q" || previous === "T"
            ? { x: 2 * p.x - control.x, y: 2 * p.y - control.y }
            : p;
      const end = point();
      curve(
        p,
        { x: p.x + ((a.x - p.x) * 2) / 3, y: p.y + ((a.y - p.y) * 2) / 3 },
        { x: end.x + ((a.x - end.x) * 2) / 3, y: end.y + ((a.y - end.y) * 2) / 3 },
        end,
      );
      control = a;
      p = end;
    } else if (upper === "A") {
      let rx = Math.abs(number()),
        ry = Math.abs(number());
      const phi = (number() * Math.PI) / 180,
        large = number(),
        sweep = number(),
        end = point(),
        cos = Math.cos(phi),
        sin = Math.sin(phi),
        dx = (p.x - end.x) / 2,
        dy = (p.y - end.y) / 2,
        x = cos * dx + sin * dy,
        y = -sin * dx + cos * dy;
      push(p);
      push(end);
      if (rx && ry && (p.x !== end.x || p.y !== end.y)) {
        const ratio = (x * x) / (rx * rx) + (y * y) / (ry * ry);
        if (ratio > 1) {
          rx *= Math.sqrt(ratio);
          ry *= Math.sqrt(ratio);
        }
        const sign = large === sweep ? -1 : 1,
          k =
            sign *
            Math.sqrt(
              Math.max(
                0,
                (rx * rx * ry * ry - rx * rx * y * y - ry * ry * x * x) /
                  (rx * rx * y * y + ry * ry * x * x),
              ),
            ),
          cx = (k * rx * y) / ry,
          cy = (-k * ry * x) / rx,
          center = {
            x: cos * cx - sin * cy + (p.x + end.x) / 2,
            y: sin * cx + cos * cy + (p.y + end.y) / 2,
          };
        const a = Math.atan2((y - cy) / ry, (x - cx) / rx);
        let delta = Math.atan2((-y - cy) / ry, (-x - cx) / rx) - a;
        if (sweep && delta < 0) delta += Math.PI * 2;
        if (!sweep && delta > 0) delta -= Math.PI * 2;
        const norm = (n: number) => ((n % (Math.PI * 2)) + Math.PI * 2) % (Math.PI * 2);
        const extrema = [Math.atan2(-ry * sin, rx * cos), Math.atan2(ry * cos, rx * sin)];
        for (const angle of extrema.flatMap((v) => [v, v + Math.PI]))
          if (sweep ? norm(angle - a) <= delta + 1e-10 : norm(a - angle) <= -delta + 1e-10)
            push({
              x: center.x + rx * cos * Math.cos(angle) - ry * sin * Math.sin(angle),
              y: center.y + rx * sin * Math.cos(angle) + ry * cos * Math.sin(angle),
            });
      }
      p = end;
    } else break;
    previous = upper;
    if (p === old && i >= tokens.length) break;
  }
  if (!points.length) return { x: 0, y: 0, w: 0, h: 0 };
  const xs = points.map((p) => p.x),
    ys = points.map((p) => p.y);
  return {
    x: Math.min(...xs),
    y: Math.min(...ys),
    w: Math.max(...xs) - Math.min(...xs),
    h: Math.max(...ys) - Math.min(...ys),
  };
}
export function layerBounds(doc: Scene, b: SceneBindings, t: number) {
  const geometry = layerGeometry(doc, b, t).map((g) => {
    const l = findLayer(doc, g.id)!;
    let local = { x: g.text?.x ?? 0, y: g.text?.y ?? 0, w: g.w, h: g.h };
    if (l.type === "path") local = pathBounds(l.path ?? "");
    const stroke = l.stroke ? Number(evaluate(l.strokeWidth, b, t) ?? 1) / 2 : 0;
    local = {
      x: local.x - stroke,
      y: local.y - stroke,
      w: local.w + stroke * 2,
      h: local.h + stroke * 2,
    };
    const [a = 1, c = 0, d = 0, e = 1, x = 0, y = 0] = g.matrix;
    const transform = (q: Point) => ({ x: a * q.x + d * q.y + x, y: c * q.x + e * q.y + y });
    const corners = [
      { x: local.x, y: local.y },
      { x: local.x + local.w, y: local.y },
      { x: local.x, y: local.y + local.h },
      { x: local.x + local.w, y: local.y + local.h },
    ].map(transform);
    const xs = corners.map((p) => p.x),
      ys = corners.map((p) => p.y);
    return {
      ...g,
      x: corners[0]!.x,
      y: corners[0]!.y,
      w: local.w,
      h: local.h,
      local,
      world: {
        x: Math.min(...xs),
        y: Math.min(...ys),
        w: Math.max(...xs) - Math.min(...xs),
        h: Math.max(...ys) - Math.min(...ys),
      },
    };
  });
  for (const g of [...geometry].reverse()) {
    const layer = findLayer(doc, g.id);
    if (layer?.type !== "group") continue;
    const children = geometry.filter((c) => layer.layers?.some((l) => l.id === c.id));
    if (!children.length) continue;
    const x = Math.min(...children.map((c) => c.world.x)),
      y = Math.min(...children.map((c) => c.world.y)),
      right = Math.max(...children.map((c) => c.world.x + c.world.w)),
      bottom = Math.max(...children.map((c) => c.world.y + c.world.h));
    g.world = { x, y, w: right - x, h: bottom - y };
    const matrix = new DOMMatrix(g.matrix),
      inverse = matrix.inverse();
    const corners = [
      { x, y },
      { x: right, y },
      { x, y: bottom },
      { x: right, y: bottom },
    ].map((p) => inverse.transformPoint(p));
    const lx = Math.min(...corners.map((p) => p.x)),
      ly = Math.min(...corners.map((p) => p.y));
    g.local = {
      x: lx,
      y: ly,
      w: Math.max(...corners.map((p) => p.x)) - lx,
      h: Math.max(...corners.map((p) => p.y)) - ly,
    };
    const origin = matrix.transformPoint({ x: lx, y: ly });
    g.x = origin.x;
    g.y = origin.y;
    g.w = g.local.w;
    g.h = g.local.h;
  }
  return geometry;
}
