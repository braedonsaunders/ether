import { crispStatic } from "./crisp-static";
import { getToken, SESSION_HEADER } from "../../../lib/api";
import type { Bindings, Look } from "./model";

let resourceToken = "";
export function setResourceToken(token: string) {
  resourceToken = token;
}
function resourceHeaders(url: string): HeadersInit {
  const target = new URL(url, globalThis.location.href);
  const token = resourceToken || getToken();
  return token &&
    target.origin === globalThis.location.origin &&
    target.pathname.startsWith("/ether/api/")
    ? { [SESSION_HEADER]: token }
    : {};
}
export type Value =
  | number
  | string
  | { keys: [number, Value, string?][] }
  | { linear?: number[]; radial?: number[]; stops: [number, string][] };
export type Params = {
  start?: number;
  step?: number;
  amplitude?: number;
  speed?: number;
  seed?: number;
};
export type Layer = {
  id: string;
  name?: string;
  hidden?: boolean;
  locked?: boolean;
  start?: number;
  end?: number;
  type: string;
  x?: Value;
  y?: Value;
  w?: Value;
  h?: Value;
  scale?: Value;
  scaleX?: Value;
  scaleY?: Value;
  rotate?: Value;
  opacity?: Value;
  fill?: Value;
  stroke?: Value;
  strokeWidth?: Value;
  join?: CanvasLineJoin;
  cap?: CanvasLineCap;
  fillRule?: CanvasFillRule;
  path?: string;
  clip?: string;
  text?: Value;
  font?: Value;
  size?: Value;
  weight?: number;
  track?: Value;
  align?: CanvasTextAlign;
  maxWidth?: number;
  fit?: string;
  maxLines?: number;
  src?: Value;
  tint?: Value;
  tintBlend?: string;
  blend?: string;
  blur?: number;
  glow?: number;
  count?: number;
  seed?: number;
  repeat?: number;
  behave?: Record<string, Params>[];
  reveal?: { out?: boolean; wipe: string; from: number; to: number; ease?: string };
  layers?: Layer[];
};
export type Scene = {
  v: number;
  moment: string;
  duration: number;
  poster: number;
  slots?: string[];
  layers: Layer[];
  post?: { type: string; amount: Value; seed?: number }[];
};
export type SceneBindings = {
  tokens: Record<string, unknown>;
  slots: Record<string, string>;
  assets?: Record<string, CanvasImageSource>;
};
type Context = CanvasRenderingContext2D | OffscreenCanvasRenderingContext2D;
export const normalizeFont = (s: string) => s.toLowerCase().replace(/[ _-]/g, "");
const clamp = (n: number, a = 0, z = 1) => Math.max(a, Math.min(z, n));
export function random(seed: number, i: number) {
  let x = Math.imul((seed >>> 0) ^ Math.imul((i + 1) >>> 0, 0x9e3779b1), 0x85ebca6b);
  x ^= x >>> 13;
  x = Math.imul(x, 0xc2b2ae35);
  x ^= x >>> 16;
  return (x >>> 0) / 4294967296;
}
function lookup(tokens: Record<string, unknown>, key: string): unknown {
  if (key in tokens) return tokens[key];
  const at = key.indexOf(".");
  return at >= 0 && typeof tokens[key.slice(0, at)] === "object" && tokens[key.slice(0, at)]
    ? lookup(tokens[key.slice(0, at)] as Record<string, unknown>, key.slice(at + 1))
    : undefined;
}
export function resolve(s: string, b: SceneBindings): string {
  for (let i = 0; i < 16; i++) {
    const match = /\{([^}]+)\}/.exec(s);
    if (!match) break;
    s =
      s.slice(0, match.index) +
      String(b.slots[match[1]!] ?? lookup(b.tokens, match[1]!) ?? "") +
      s.slice(match.index + match[0].length);
  }
  return s;
}
export function ease(name: string, u: number, energy = 1): number {
  u = clamp(u);
  switch (name) {
    case "in":
      return u ** 3;
    case "out":
      return 1 - (1 - u) ** 3;
    case "inOut":
      return u < 0.5 ? 4 * u ** 3 : 1 - (-2 * u + 2) ** 3 / 2;
    case "back": {
      const c = 1.70158 * energy;
      return 1 + (c + 1) * (u - 1) ** 3 + c * (u - 1) ** 2;
    }
    case "elastic":
    case "spring":
      return u === 0 || u === 1 ? u : 1 - 2 ** (-10 * u) * Math.cos(u * Math.PI * 6) * energy;
    default:
      return u;
  }
}
function rgba(s: string): number[] {
  const [hex = "", alpha = "1"] = s.split("@");
  let h = hex.replace("#", "");
  if (h.length === 3) h = [...h].map((c) => c + c).join("");
  const a = clamp(Number(alpha) * (h.length === 8 ? parseInt(h.slice(6), 16) / 255 : 1));
  const v = parseInt(h.slice(0, 6), 16) || 0;
  return [((v >>> 16) & 255) * a, ((v >>> 8) & 255) * a, (v & 255) * a, 255 * a];
}
function color(s: string): string {
  const c = rgba(s),
    a = c[3]! / 255;
  return `rgba(${a ? c[0]! / a : 0},${a ? c[1]! / a : 0},${a ? c[2]! / a : 0},${a})`;
}
export function evaluate(v: Value | undefined, b: SceneBindings, t: number): unknown {
  if (typeof v === "string") return resolve(v, b);
  if (v && typeof v === "object" && "keys" in v) {
    let prev = v.keys[0]!;
    for (const next of v.keys.slice(1)) {
      if (t < next[0]) {
        const u = ease(
          next[2] ?? "linear",
          (t - prev[0]) / Math.max(0.00001, next[0] - prev[0]),
          Number(lookup(b.tokens, "energy") ?? 1),
        );
        const a = evaluate(prev[1], b, t),
          z = evaluate(next[1], b, t);
        if (typeof a === "string" && typeof z === "string") {
          if (
            /^#[\da-f]{6}([\da-f]{2})?(@[\d.]+)?$/i.test(a) &&
            /^#[\da-f]{6}([\da-f]{2})?(@[\d.]+)?$/i.test(z)
          ) {
            const aa = rgba(a),
              zz = rgba(z),
              out = aa.map((c, i) => clamp(c + (zz[i]! - c) * u, 0, 255));
            const alpha = out[3]! / 255;
            return (
              "#" +
              out
                .map((c, i) =>
                  Math.floor((i < 3 && alpha ? c / alpha : c) + 0.5)
                    .toString(16)
                    .padStart(2, "0"),
                )
                .join("")
            );
          }
          return u >= 1 ? z : a;
        }
        return Number(a ?? 0) + (Number(z ?? 0) - Number(a ?? 0)) * u;
      }
      prev = next;
    }
    return evaluate(prev[1], b, t);
  }
  return v;
}
const number = (v: unknown, fallback: number) =>
  v === undefined || v === null || !Number.isFinite(Number(v)) ? fallback : Number(v);
function surface(w: number, h: number) {
  const c =
    typeof OffscreenCanvas !== "undefined"
      ? new OffscreenCanvas(w, h)
      : Object.assign(document.createElement("canvas"), { width: w, height: h });
  return c;
}
const blend = (s?: string): GlobalCompositeOperation =>
  !s || s === "normal" ? "source-over" : (s as GlobalCompositeOperation);
// Static scene plates (including text) are reused; deterministic post effects
// still run at the requested time. No asset copies or animation approximations.
const staticFrames = new Map<string, ReturnType<typeof surface>>();
/** Shared text layout used by drawing and editor geometry. Measurements use loaded fonts. */
export function measureText(c: Context, l: Layer, bindings: SceneBindings, t: number) {
  const n = (v: Value | undefined, fallback = 0) => number(evaluate(v, bindings, t), fallback);
  const s = (v: Value | undefined) => String(evaluate(v, bindings, t) ?? "");
  let value = s(l.text),
    size = n(l.size, 60);
  let track = n(l.track);
  for (const b of l.behave ?? [])
    if (b.typewriter)
      value = [...value]
        .slice(
          0,
          Math.max(
            0,
            Math.floor((t - (b.typewriter.start ?? 0)) / Math.max(0.001, b.typewriter.step ?? 0)) +
              1,
          ),
        )
        .join("");
  const setFont = () => {
    c.font = `${l.weight ?? 400} ${size}px "onair-${normalizeFont(s(l.font))}", "onair-notosans"`;
  };
  setFont();
  const measure = (line: string) =>
    c.measureText(line).width + Math.max(0, [...line].length - 1) * track;
  if (l.fit === "shrink" && l.maxWidth && measure(value) > l.maxWidth) {
    const ratio = l.maxWidth / measure(value);
    size *= ratio;
    track *= ratio;
    setFont();
  }
  let lines = [value];
  if (l.fit === "wrap" && l.maxWidth) {
    lines = [];
    let line = "";
    for (const word of value.trim().split(/\s+/)) {
      const test = line ? line + " " + word : word;
      if (line && measure(test) > l.maxWidth) {
        lines.push(line);
        line = word;
      } else line = test;
    }
    lines.push(line);
  }
  if (l.maxLines && lines.length > l.maxLines) {
    lines = lines.slice(0, l.maxLines);
    let last = [...lines.at(-1)!];
    while (last.length && measure(last.join("") + "…") > (l.maxWidth ?? Infinity)) last.pop();
    lines[lines.length - 1] = last.join("") + "…";
  }
  const widths = lines.map(measure);
  const w = Math.max(0, ...widths);
  const metrics = c.measureText("Mg");
  const ascent = metrics.actualBoundingBoxAscent || size * 0.8;
  const descent = metrics.actualBoundingBoxDescent || size * 0.2;
  return {
    lines,
    widths,
    size,
    track,
    font: c.font,
    x: l.align === "center" ? -w / 2 : l.align === "right" ? -w : 0,
    y: -ascent,
    w,
    h: ascent + descent + Math.max(0, lines.length - 1) * size * 1.15,
  };
}
export function resolvedTransform(l: Layer, bindings: SceneBindings, t: number) {
  const n = (v: Value | undefined, fallback = 0) => number(evaluate(v, bindings, t), fallback);
  const energy = number(lookup(bindings.tokens, "energy"), 1);
  let x = n(l.x),
    y = n(l.y),
    sx = n(l.scale, 1) * n(l.scaleX, 1),
    sy = n(l.scale, 1) * n(l.scaleY, 1),
    rot = n(l.rotate),
    alpha = n(l.opacity, 1);
  for (const b of l.behave ?? [])
    for (const [kind, p] of Object.entries(b)) {
      const u = Math.max(0, t - (p.start ?? 0)),
        speed = p.speed || 1,
        a = (p.amplitude ?? 0) * energy;
      switch (kind) {
        case "wiggle":
          rot += Math.sin(u * speed * 6.28) * a;
          break;
        case "bounce":
          y -= Math.abs(Math.sin(u * speed * Math.PI)) * a;
          break;
        case "drift":
          x += u * a;
          y += Math.sin(u * speed) * a * 0.2;
          break;
        case "pop":
          sx *= ease("back", u * speed, energy);
          sy *= ease("back", u * speed, energy);
          break;
        case "squash": {
          const k = Math.sin(u * speed * 6.28) * a * 0.01;
          sx *= 1 + k;
          sy /= Math.max(0.1, 1 + k);
          break;
        }
        case "flicker":
          alpha *= 1 - a * random((l.seed ?? 0) + (p.seed ?? 0), Math.trunc(t * 24));
          break;
      }
    }
  return { x, y, scaleX: sx, scaleY: sy, rotate: rot, opacity: alpha };
}
export type LayerGeometry = {
  id: string;
  x: number;
  y: number;
  w: number;
  h: number;
  rotate: number;
  scaleX: number;
  scaleY: number;
  anchor: { x: number; y: number };
  matrix: number[];
  text?: ReturnType<typeof measureText>;
};
/** Geometry in 1920×1080 document coordinates, with inherited group transforms. */
export function layerGeometry(doc: Scene, bindings: SceneBindings, t: number): LayerGeometry[] {
  const out: LayerGeometry[] = [];
  const c = surface(1, 1).getContext("2d") as Context;
  const visit = (ls: Layer[], parent: DOMMatrix) => {
    for (const l of ls) {
      if (l.hidden || (l.start && t < l.start) || (l.end && t >= l.end)) continue;
      const r = resolvedTransform(l, bindings, t);
      const m = parent
        .translate(r.x, r.y)
        .rotate((r.rotate * 180) / Math.PI)
        .scale(r.scaleX, r.scaleY);
      const text = l.type === "text" ? measureText(c, l, bindings, t) : undefined;
      const n = (v: Value | undefined, fallback: number) =>
        number(evaluate(v, bindings, t), fallback);
      const anchor = { x: m.e, y: m.f };
      const origin = m.transformPoint({ x: text?.x ?? 0, y: text?.y ?? 0 });
      out.push({
        id: l.id,
        x: origin.x,
        y: origin.y,
        w: text?.w ?? n(l.w, 1920),
        h: text?.h ?? n(l.h, 1080),
        rotate: Math.atan2(m.b, m.a),
        scaleX: Math.hypot(m.a, m.b),
        scaleY: (m.a * m.d - m.b * m.c) / Math.max(1e-12, Math.hypot(m.a, m.b)),
        anchor,
        matrix: [m.a, m.b, m.c, m.d, m.e, m.f],
        text,
      });
      if (l.layers) visit(l.layers, m);
    }
  };
  visit(doc.layers, new DOMMatrix());
  return out;
}

export function renderScene(ctx: Context, doc: Scene, bindings: SceneBindings, t: number) {
  const width = ctx.canvas.width,
    height = ctx.canvas.height;
  const n = (v: Value | undefined, fallback = 0) => number(evaluate(v, bindings, t), fallback);
  const s = (v: Value | undefined) => String(evaluate(v, bindings, t) ?? "");
  const energy = number(lookup(bindings.tokens, "energy"), 1);
  const paint = (c: Context, v: Value | undefined): string | CanvasGradient => {
    const p = evaluate(v, bindings, t);
    if (p && typeof p === "object" && "stops" in p) {
      const g = p as { linear?: number[]; radial?: number[]; stops: [number, string][] };
      const a = (g.linear ?? g.radial)!;
      const gradient = g.linear
        ? c.createLinearGradient(a[0]!, a[1]!, a[2]!, a[3]!)
        : c.createRadialGradient(
            a[0]!,
            a[1]!,
            Math.max(0, a[2]!),
            a[0]!,
            a[1]!,
            Math.max(0, a[3]!),
          );
      for (let j = 0; j < g.stops.length - 1; j++) {
        const [from, first] = g.stops[j]!,
          [to, last] = g.stops[j + 1]!;
        const a = rgba(resolve(first, bindings)),
          b = rgba(resolve(last, bindings));
        for (let k = 0; k < 32; k++) {
          const u = k / 32,
            c = a.map((v, i) => v + (b[i]! - v) * u),
            alpha = c[3]! / 255;
          gradient.addColorStop(
            from + (to - from) * u,
            `rgba(${alpha ? c[0]! / alpha : 0},${alpha ? c[1]! / alpha : 0},${alpha ? c[2]! / alpha : 0},${alpha})`,
          );
        }
      }
      const final = g.stops.at(-1)!;
      gradient.addColorStop(final[0], color(resolve(final[1], bindings)));
      return gradient;
    }
    return color(String(p ?? "#000000@0"));
  };
  function text(c: Context, l: Layer) {
    const { lines, widths, size, track, font } = measureText(c, l, bindings, t);
    c.font = font;
    const measure = (line: string) => widths[lines.indexOf(line)] ?? 0;
    c.textBaseline = "alphabetic";
    c.textAlign = "left";
    if (l.maxWidth && l.align !== "center" && l.align !== "right") {
      c.beginPath();
      c.rect(0, -size * 2, l.maxWidth, size * (lines.length + 3));
      c.clip();
    }
    lines.forEach((line, i) => {
      let x = l.align === "center" ? -measure(line) / 2 : l.align === "right" ? -measure(line) : 0;
      if (!track) c.fillText(line, x, i * size * 1.15);
      else {
        const chars = [...line];
        chars.forEach((ch, j) => {
          c.fillText(ch, x, i * size * 1.15);
          const next = chars[j + 1];
          x +=
            c.measureText(ch).width +
            track +
            (next
              ? c.measureText(ch + next).width - c.measureText(ch).width - c.measureText(next).width
              : 0);
        });
      }
    });
  }
  function layers(c: Context, ls: Layer[]) {
    for (const l of ls)
      for (let rep = 0; rep < Math.max(1, l.repeat ?? 1); rep++) {
        if (l.hidden || (l.start && t < l.start) || (l.end && t >= l.end)) continue;
        const {
          x,
          y,
          scaleX: sx,
          scaleY: sy,
          rotate: rot,
          opacity: alpha,
        } = resolvedTransform(l, bindings, t);
        if (alpha <= 0 || sx === 0 || sy === 0) continue;
        c.save();
        c.translate(x, y);
        c.rotate(rot);
        c.scale(sx, sy);
        const isolated = !!(
          l.blur ||
          l.glow ||
          (l.blend && l.blend !== "normal") ||
          alpha !== 1 ||
          l.behave?.some((b) => b.chroma || b.glint)
        );
        let draw = c;
        let scratch: ReturnType<typeof surface> | undefined;
        if (isolated) {
          scratch = surface(width, height);
          draw = scratch.getContext("2d", { willReadFrequently: true }) as Context;
          draw.setTransform(c.getTransform());
        }
        draw.save();
        if (l.clip) draw.clip(new Path2D(l.clip));
        const w = n(l.w, 1920),
          h = n(l.h, 1080),
          seed = ((l.seed ?? 0) + rep + number(lookup(bindings.tokens, "seed"), 7)) >>> 0;
        if (l.reveal) {
          const r = l.reveal,
            progress = ease(r.ease ?? "linear", (t - r.from) / (r.to - r.from), energy),
            u = r.out ? 1 - progress : progress,
            extent = Math.max(l.maxWidth ?? 0, w);
          draw.beginPath();
          draw.rect(
            r.wipe === "rtl" ? extent * (1 - u) : -10000,
            -10000,
            r.wipe === "rtl" ? 10000 : 10000 + extent * u,
            20000,
          );
          draw.clip();
        }
        draw.fillStyle = paint(draw, l.fill);
        draw.strokeStyle = paint(draw, l.stroke);
        draw.lineWidth = n(l.strokeWidth, 1);
        draw.lineJoin = l.join ?? "miter";
        draw.lineCap = l.cap ?? "butt";
        if (l.type === "group") layers(draw, l.layers ?? []);
        else if (l.type === "text") text(draw, l);
        else if (["art", "logo", "image"].includes(l.type)) {
          const image = bindings.assets?.[s(l.src)];
          if (image) {
            const im = image as { width: number; height: number };
            const scale = (l.fit === "contain" ? Math.min : Math.max)(w / im.width, h / im.height);
            draw.beginPath();
            draw.rect(0, 0, w, h);
            draw.clip();
            draw.drawImage(
              image,
              (w - im.width * scale) / 2,
              (h - im.height * scale) / 2,
              im.width * scale,
              im.height * scale,
            );
            if (l.tint) {
              draw.globalCompositeOperation =
                l.tintBlend === "silhouette" ? "source-in" : "multiply";
              draw.fillStyle = paint(draw, l.tint);
              draw.fillRect(0, 0, w, h);
            }
          } else if (l.id === "station-logo" || l.id === "bug-shadow") {
            draw.save();
            draw.translate(0, 64);
            text(draw, {
              ...l,
              type: "text",
              text: "{channel.name}",
              font: "{type.display}",
              size: 48,
              weight: 600,
              track: 2,
              fill: l.tint ?? "{accent}",
              fit: "shrink",
              maxWidth: 288,
            });
            draw.restore();
          }
        } else if (l.type === "noise") {
          draw.imageSmoothingEnabled = false;
          draw.drawImage(
            crispStatic(seed, Math.max(0, Math.floor(t * 30)), width, height),
            0,
            0,
            w,
            h,
          );
        } else {
          const path = new Path2D(l.type === "path" ? l.path : undefined);
          if (["rect", "gradient"].includes(l.type)) path.rect(0, 0, w, h);
          if (["ellipse", "blob"].includes(l.type)) {
            for (let j = 0; j < 96; j++) {
              const a = (j * Math.PI * 2) / 96,
                k =
                  l.type === "blob"
                    ? 1 +
                      0.06 * Math.sin(a * 7 + t * 2 + random(seed, 1) * 6.28) +
                      0.03 * Math.sin(a * 11 - t)
                    : 1;
              const xx = w / 2 + ((Math.cos(a) * w) / 2) * k,
                yy = h / 2 + ((Math.sin(a) * h) / 2) * k;
              if (!j) path.moveTo(xx, yy);
              else path.lineTo(xx, yy);
            }
            path.closePath();
          }
          if (l.type === "particles")
            for (let j = 0; j < (l.count ?? 0); j++) {
              const xx = random(seed, j) * w + Math.sin(t + j) * 15,
                yy = (random(seed, j + 2050) * h + t * (8 + random(seed, j + 4100) * 35)) % h,
                r = 1 + random(seed, j + 6200) * 4;
              path.rect(xx - r, yy - r, r * 2, r * 2);
            }
          if (l.type === "grid") {
            for (let j = 0; j < 24; j++) {
              const xx = ((j - 12) * w) / 8;
              path.moveTo(w / 2 + xx * 0.05, 0);
              path.lineTo(w / 2 + xx, h);
            }
            for (let j = 0; j < 16; j++) {
              const yy = ((j / 16 + t * 0.035) % 1) ** 2 * h;
              path.moveTo(0, yy);
              path.lineTo(w, yy);
            }
          }
          if (l.fill !== undefined) draw.fill(path, l.fillRule ?? "nonzero");
          if (l.stroke !== undefined) draw.stroke(path);
        }
        draw.restore();
        if (scratch) {
          pixelBehaviours(draw, l, t, width, height);
          c.save();
          c.setTransform(1, 0, 0, 1, 0, 0);
          c.globalAlpha *= clamp(alpha);
          if (l.glow) {
            c.globalCompositeOperation = "screen";
            c.filter = `blur(${(l.glow * width) / 1920}px)`;
            c.drawImage(scratch, 0, 0);
          }
          c.globalCompositeOperation = blend(l.blend);
          c.filter = l.blur ? `blur(${(l.blur * width) / 1920}px)` : "none";
          c.drawImage(scratch, 0, 0);
          c.restore();
        }
        c.restore();
      }
  }
  ctx.save();
  ctx.resetTransform();
  ctx.clearRect(0, 0, width, height);
  ctx.globalAlpha = 1;
  ctx.globalCompositeOperation = "source-over";
  ctx.filter = "none";
  ctx.scale(width / 1920, height / 1080);
  const layerJSON = JSON.stringify(doc.layers);
  const stationary =
    !/"(?:keys|behave|src)":/.test(layerJSON) &&
    !/"type":"(?:noise|particles|blob|grid)"/.test(layerJSON);
  if (stationary) {
    const key = JSON.stringify([width, height, layerJSON, bindings.tokens, bindings.slots]);
    let cached = staticFrames.get(key);
    if (!cached) {
      cached = surface(width, height);
      const prepared = cached.getContext("2d", { willReadFrequently: true }) as Context;
      prepared.scale(width / 1920, height / 1080);
      layers(prepared, doc.layers);
      if (staticFrames.size >= 8) staticFrames.delete(staticFrames.keys().next().value!);
      staticFrames.set(key, cached);
    }
    ctx.save();
    ctx.resetTransform();
    ctx.drawImage(cached, 0, 0);
    ctx.restore();
  } else layers(ctx, doc.layers);
  ctx.restore();
  if (doc.post?.some((e) => n(e.amount) !== 0)) {
    const pixels = ctx.getImageData(0, 0, width, height),
      data = pixels.data;
    const effects = doc.post
      .map((e) => ({
        type: e.type,
        amount: n(e.amount),
        seed: ((e.seed ?? 0) + number(lookup(bindings.tokens, "seed"), 7)) >>> 0,
      }))
      .filter((e) => e.amount !== 0);
    const frame = Math.trunc(t * 24);
    const x2 = new Float64Array(width);
    for (let x = 0; x < width; x++) {
      const dx = (x / width - 0.5) * 1.4;
      x2[x] = dx * dx;
    }
    const flickers = effects.map((e) => 1 - e.amount * random(e.seed, frame));
    const pattern = effects.map((e) => e.type).join(",");
    const common =
      pattern === "grain,flicker,vignette" && effects.every((e) => e.amount >= 0 && e.amount <= 1);
    const vhs =
      pattern === "grain,scanlines" && effects.every((e) => e.amount >= 0 && e.amount <= 1);
    const quantize = (v: number) => (v < 0 ? 0 : v > 255 ? 255 : v | 0);
    for (let y = 0; y < height; y++) {
      const dy = (y / height - 0.5) * 1.4,
        y2 = dy * dy;
      for (let x = 0; x < width; x++) {
        const i = (y * width + x) * 4,
          a = data[i + 3]! / 255;
        let r = data[i]! * a,
          g = data[i + 1]! * a,
          b = data[i + 2]! * a;
        if (a === 1 && (common || vhs)) {
          const grain = effects[0]!,
            offset =
              (random(grain.seed, x + y * width + frame * 100003) - 0.5) *
              Math.min(grain.amount, 1) *
              24;
          r = quantize(r + offset);
          g = quantize(g + offset);
          b = quantize(b + offset);
          if (common) {
            const flicker = flickers[1]!,
              vignette = 1 - effects[2]!.amount * Math.min(1, x2[x]! + y2);
            r = (((r * flicker) | 0) * vignette) | 0;
            g = (((g * flicker) | 0) * vignette) | 0;
            b = (((b * flicker) | 0) * vignette) | 0;
          } else if (y % 3 === 0) {
            const v = 1 - effects[1]!.amount;
            r = (r * v) | 0;
            g = (g * v) | 0;
            b = (b * v) | 0;
          }
          data[i] = r;
          data[i + 1] = g;
          data[i + 2] = b;
          continue;
        }
        for (let j = 0; j < effects.length; j++) {
          const e = effects[j]!;
          let v = 1,
            offset = 0;
          switch (e.type) {
            case "grain":
              offset =
                (random(e.seed, x + y * width + frame * 100003) - 0.5) * Math.min(e.amount, 1) * 24;
              break;
            case "scanlines":
              if (y % 3 === 0) v = 1 - e.amount;
              break;
            case "vignette":
              v = 1 - e.amount * Math.min(1, x2[x]! + y2);
              break;
            case "flicker":
              v = flickers[j]!;
              break;
          }
          r = Math.floor(Math.max(0, Math.min(a * 255, r * v + offset)));
          g = Math.floor(Math.max(0, Math.min(a * 255, g * v + offset)));
          b = Math.floor(Math.max(0, Math.min(a * 255, b * v + offset)));
        }
        data[i] = a ? r / a : 0;
        data[i + 1] = a ? g / a : 0;
        data[i + 2] = a ? b / a : 0;
      }
    }
    ctx.putImageData(pixels, 0, 0);
  }
}

function pixelBehaviours(ctx: Context, l: Layer, t: number, w: number, h: number) {
  if (!l.behave?.some((b) => b.glint || b.chroma)) return;
  const pixels = ctx.getImageData(0, 0, w, h),
    d = pixels.data;
  for (const b of l.behave) {
    if (b.glint) {
      const p = b.glint,
        center = ((Math.max(0, t - (p.start ?? 0)) * (p.speed || 1) * 0.3) % 1.5) - 0.25;
      for (let y = 0; y < h; y++)
        for (let x = 0; x < w; x++) {
          const i = (y * w + x) * 4;
          if (!d[i + 3]) continue;
          const k = Math.min(
            1,
            Math.max(0, 1 - Math.abs(x / w + (y / h) * 0.2 - center) / 0.06) * (p.amplitude || 0.5),
          );
          for (let ch = 0; ch < 3; ch++)
            d[i + ch] = Math.floor(d[i + ch]! + (255 - d[i + ch]!) * k);
        }
    }
    if (b.chroma) {
      const p = b.chroma,
        dx = Math.round(
          (Math.sin((t - (p.start ?? 0)) * Math.max(1, p.speed ?? 0) * 6.28) *
            (p.amplitude || 3) *
            w) /
            1920,
        ),
        original = d.slice();
      for (let y = 0; y < h; y++)
        for (let x = 0; x < w; x++)
          for (const ch of [0, 2]) {
            const xx = x + (ch === 0 ? dx : -dx),
              i = (y * w + x) * 4;
            if (xx >= 0 && xx < w) d[i + ch] = original[(y * w + xx) * 4 + ch]!;
          }
    }
  }
  ctx.putImageData(pixels, 0, 0);
}

const fontURLs =
  import.meta.env.MODE === "mock"
    ? import.meta.glob<string>("../../../../../internal/onair/fonts/data/*.ttf", {
        eager: true,
        query: "?url",
        import: "default",
      })
    : {};
let ready: Promise<void> | undefined;
export function loadOnAirFonts() {
  return (ready ??= (async () => {
    if (typeof FontFace === "undefined") return;
    const ids = [
      "inter",
      "spacegrotesk",
      "fraunces",
      "playfairdisplay",
      "bungee",
      "fredoka",
      "specialelite",
      "orbitron",
      "vt323",
      "ibmplexmono",
      "archivoblack",
      "notosans",
    ];
    await Promise.all(
      ids.map(async (id) => {
        const url =
          Object.entries(fontURLs).find(([path]) => path.endsWith(`/${id}.ttf`))?.[1] ??
          `/ether/api/onair/fonts/${id}`;
        const response = await fetch(url, { headers: resourceHeaders(url) });
        if (!response.ok) throw new Error(`Font: ${response.status}`);
        const face = new FontFace(`onair-${id}`, await response.arrayBuffer());
        await face.load();
        const fonts =
          typeof document !== "undefined"
            ? document.fonts
            : (globalThis as unknown as { fonts: FontFaceSet }).fonts;
        fonts.add(face);
      }),
    );
  })());
}
const customFonts = new Map<string, Promise<void>>();
export async function loadStyleFonts(look: Look) {
  await loadOnAirFonts();
  await Promise.all(
    Object.values(look.brand.type)
      .filter((id) => /^[a-f0-9]{64}$/.test(id))
      .map((id) => {
        if (!customFonts.has(id))
          customFonts.set(
            id,
            (async () => {
              const url = `/ether/api/onair/assets/${id}`;
              const response = await fetch(url, { headers: resourceHeaders(url) });
              if (!response.ok) throw new Error("Font unavailable");
              const face = new FontFace(`onair-${id}`, await response.arrayBuffer());
              await face.load();
              document.fonts.add(face);
            })(),
          );
        return customFonts.get(id);
      }),
  );
}
/** Resolve the owned style's shared kit and live slots without template lookups. */
export function styleBindings(look: Look, bindings: Bindings = {}): SceneBindings {
  const tokens: Record<string, unknown> = {
    bg: look.brand.palette.background,
    accent: look.brand.palette.accent,
    text: look.brand.palette.text,
    glow: look.brand.palette.glow,
    "type.display": normalizeFont(look.brand.type.display),
    "type.body": normalizeFont(look.brand.type.body),
    "type.mono": normalizeFont(look.brand.type.body),
    energy: look.brand.energy,
    "words.tagline": look.brand.tagline,
    "brand.logo": look.brand.logo ?? "",
  };
  for (const name of ["grain", "flicker", "scanlines", "vignette", "chroma"])
    tokens[`textures.${name}`] = look.brand.textures.some((t) => t.toLowerCase() === name)
      ? 0.07
      : 0;
  for (const control of look.controls) tokens[`control.${control.id}`] = control.value;
  const slots: Record<string, string> = {
    "channel.name": bindings.channel?.name ?? look.name,
    "brand.logo": look.brand.logo ?? "",
  };
  const flatten = (value: unknown, prefix: string) => {
    if (value == null) return;
    if (typeof value !== "object") {
      slots[prefix] = String(value);
      return;
    }
    for (const [key, child] of Object.entries(value))
      flatten(
        child,
        Array.isArray(value) ? `${prefix}[${key}]` : prefix ? `${prefix}.${key}` : key,
      );
  };
  flatten(bindings, "");
  return { tokens, slots };
}
type WorkerRequest = {
  id: number;
  sessionToken: string;
  document: Scene;
  bindings: SceneBindings;
  time: number;
  width: number;
  height: number;
};
type WorkerState = {
  id: number;
  ctx: Context;
  busy: boolean;
  painted?: boolean;
  failed?: boolean;
  pending?: WorkerRequest;
};

const workers = new WeakMap<object, WorkerState>();
const consumers = new Map<number, WorkerState>();
let sharedWorker: Worker | undefined;
let nextID = 0;
export function releaseRenderer(ctx: Context) {
  const state = workers.get(ctx);
  if (!state) return;
  consumers.delete(state.id);
  workers.delete(ctx);
  if (!consumers.size) {
    sharedWorker?.terminate();
    sharedWorker = undefined;
  }
}
function workerFrame(
  ctx: Context,
  document: Scene,
  bindings: SceneBindings,
  time: number,
): boolean {
  if (
    typeof window === "undefined" ||
    typeof Worker === "undefined" ||
    typeof OffscreenCanvas === "undefined"
  )
    return false;
  if (!sharedWorker) {
    sharedWorker = new Worker(new URL("./scene-worker.ts", import.meta.url), { type: "module" });
    sharedWorker.onmessage = (
      event: MessageEvent<{ id: number; bitmap?: ImageBitmap; error?: string }>,
    ) => {
      const state = consumers.get(event.data.id),
        bitmap = event.data.bitmap;
      if (!state) {
        bitmap?.close();
        return;
      }
      state.busy = false;
      if (event.data.error) {
        state.failed = true;
        return;
      }
      if (bitmap) {
        const c = state.ctx;
        c.save();
        c.resetTransform();
        c.clearRect(0, 0, c.canvas.width, c.canvas.height);
        c.drawImage(bitmap, 0, 0);
        c.restore();
        bitmap.close();
      }
      // Continue with the latest requested frame without another main-thread RAF gap.
      if (state.pending && sharedWorker) {
        const pending = state.pending;
        state.pending = undefined;
        state.busy = true;
        sharedWorker.postMessage(pending);
      }
    };
    sharedWorker.onerror = () => {
      for (const state of consumers.values()) {
        state.failed = true;
        state.busy = false;
      }
      sharedWorker?.terminate();
      sharedWorker = undefined;
    };
  }
  let state = workers.get(ctx);
  if (!state) {
    state = { id: ++nextID, ctx, busy: false };
    workers.set(ctx, state);
    consumers.set(state.id, state);
  }
  if (state.failed) return false;
  const request: WorkerRequest = {
    id: state.id,
    sessionToken: getToken(),
    document,
    bindings,
    time,
    width: ctx.canvas.width,
    height: ctx.canvas.height,
  };
  if (!state.busy) {
    state.busy = true;
    sharedWorker.postMessage(request);
  } else state.pending = request;
  // Paint a poster immediately while the shared worker loads its fonts/modules.
  if (!state.painted) {
    state.painted = true;
    return false;
  }
  return true;
}

if (import.meta.hot)
  import.meta.hot.dispose(() => {
    sharedWorker?.terminate();
    sharedWorker = undefined;
    consumers.clear();
  });

const silhouettes = new Map<string, Promise<ImageBitmap>>();
const artwork = new Map<string, Promise<ImageBitmap | undefined>>();
// Resource preparation is outside the pure frame evaluator, just as in Go.
export async function loadSceneAssets(doc: Scene, bindings: SceneBindings): Promise<SceneBindings> {
  const assets = { ...bindings.assets };
  const sources = new Set<string>();
  const masks = new Set<string>();
  const visit = (ls: Layer[]) => {
    for (const l of ls) {
      if (l.src) {
        const src = String(evaluate(l.src, bindings, 0) ?? "");
        if (l.tintBlend === "silhouette") masks.add(src);
        if (
          src &&
          !assets[src] &&
          (/^(\/|https?:|data:|blob:)/.test(src) || /^[a-f0-9]{64}$/.test(src))
        )
          sources.add(src);
      }
      if (l.layers) visit(l.layers);
    }
  };
  visit(doc.layers);
  await Promise.all(
    [...sources].map(async (src) => {
      let pending = artwork.get(src);
      if (!pending) {
        pending = (async () => {
          try {
            const url = /^[a-f0-9]{64}$/.test(src) ? `/ether/api/onair/assets/${src}` : src;
            const response = await fetch(url, { headers: resourceHeaders(url) });
            if (!response.ok) return;
            return await createImageBitmap(await response.blob());
          } catch {
            return;
          }
        })();
        artwork.set(src, pending);
      }
      const image = await pending;
      if (image) {
        if (masks.has(src)) {
          let mask = silhouettes.get(src);
          if (!mask) {
            mask = (async () => {
              const c = new OffscreenCanvas(image.width, image.height),
                x = c.getContext("2d")!;
              x.drawImage(image, 0, 0);
              const pixels = x.getImageData(0, 0, c.width, c.height),
                p = pixels.data;
              let transparent = false;
              for (let i = 3; i < p.length; i += 4)
                if (p[i]! < 250) {
                  transparent = true;
                  break;
                }
              const invert = (p[0]! + p[1]! + p[2]!) / 3 > 128;
              for (let i = 0; i < p.length; i += 4) {
                if (!transparent) {
                  const l = 0.2126 * p[i]! + 0.7152 * p[i + 1]! + 0.0722 * p[i + 2]!;
                  p[i + 3] = invert ? 255 - l : l;
                }
                p[i] = p[i + 1] = p[i + 2] = 255;
              }
              x.putImageData(pixels, 0, 0);
              return createImageBitmap(c);
            })();
            silhouettes.set(src, mask);
          }
          assets[src] = await mask;
        } else assets[src] = image;
      }
    }),
  );
  return { ...bindings, assets };
}

export function renderDocument(ctx: Context, doc: Scene, bindings: SceneBindings, time: number) {
  if (!workerFrame(ctx, doc, bindings, time)) renderScene(ctx, doc, bindings, time);
}
