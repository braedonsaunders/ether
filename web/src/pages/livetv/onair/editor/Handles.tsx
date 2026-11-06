import type { Layer } from "../scene-renderer";
export function pathNodes(path: string) {
  const tokens = path.match(/[a-df-z]|[-+]?(?:\d*\.\d+|\d+\.?\d*)(?:e[-+]?\d+)?/gi) ?? [];
  const points: {
    x: number;
    y: number;
    xi: number;
    yi: number;
    relative: boolean;
    originX: number;
    originY: number;
  }[] = [];
  let i = 0,
    cmd = "",
    x = 0,
    y = 0,
    sx = 0,
    sy = 0;
  while (i < tokens.length) {
    if (/^[a-z]$/i.test(tokens[i]!)) cmd = tokens[i++]!;
    const c = cmd.toUpperCase(),
      relative = cmd !== c;
    if (c === "Z") {
      x = sx;
      y = sy;
      cmd = "";
      continue;
    }
    const lengths: Record<string, number> = {
      M: 2,
      L: 2,
      T: 2,
      H: 1,
      V: 1,
      C: 6,
      S: 4,
      Q: 4,
      A: 7,
    };
    const count = lengths[c];
    if (!count || i + count > tokens.length) break;
    const start = i;
    const ox = relative ? x : 0,
      oy = relative ? y : 0;
    const pairs =
      c === "A"
        ? [5]
        : c === "H" || c === "V"
          ? []
          : Array.from({ length: count / 2 }, (_, j) => j * 2);
    for (const j of pairs) {
      points.push({
        x: Number(tokens[start + j]) + ox,
        y: Number(tokens[start + j + 1]) + oy,
        xi: start + j,
        yi: start + j + 1,
        relative,
        originX: ox,
        originY: oy,
      });
    }
    if (c === "H") x = Number(tokens[start]) + ox;
    else if (c === "V") y = Number(tokens[start]) + oy;
    else {
      x = Number(tokens[start + count - 2]) + ox;
      y = Number(tokens[start + count - 1]) + oy;
    }
    if (c === "M") {
      sx = x;
      sy = y;
      cmd = relative ? "l" : "L";
    }
    i += count;
  }
  return { tokens, points };
}
export function Handles(p: { layer: Layer; matrix: number[] }) {
  if (p.layer.type !== "path") return null;
  const nodes = pathNodes(p.layer.path ?? "");
  return (
    <g transform={`matrix(${p.matrix.join(" ")})`} stroke="#76a9ff" fill="white" strokeWidth="2">
      {nodes.points.map((n) => (
        <circle
          key={n.xi}
          data-mode={`node:${n.xi}:${n.yi}:${n.originX}:${n.originY}`}
          className="pointer-events-auto cursor-crosshair"
          cx={n.x}
          cy={n.y}
          r="7"
        />
      ))}
    </g>
  );
}
