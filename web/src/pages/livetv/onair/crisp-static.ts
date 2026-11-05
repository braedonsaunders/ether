// Exact uint64 Signal hash, using 32-bit limbs to keep native-size previews fast.
type U64 = [number, number]; // low, high
function mul(a: U64, b: U64): U64 {
  const a0 = a[0] & 65535,
    a1 = a[0] >>> 16;
  const b0 = b[0] & 65535,
    b1 = b[0] >>> 16;
  const cross = a1 * b0 + a0 * b1 + ((a0 * b0) >>> 16);
  return [
    Math.imul(a[0], b[0]) >>> 0,
    (a1 * b1 + Math.floor(cross / 65536) + Math.imul(a[0], b[1]) + Math.imul(a[1], b[0])) >>> 0,
  ];
}
function xor(a: U64, b: U64): U64 {
  return [(a[0] ^ b[0]) >>> 0, (a[1] ^ b[1]) >>> 0];
}
function shift(a: U64, n: number): U64 {
  return [((a[0] >>> n) | (a[1] << (32 - n))) >>> 0, a[1] >>> n];
}
function hash(a: U64): U64 {
  a = mul(xor(a, shift(a, 30)), [0x1ce4e5b9, 0xbf58476d]);
  a = mul(xor(a, shift(a, 27)), [0x133111eb, 0x94d049bb]);
  return xor(a, shift(a, 31));
}
function integer(n: number): U64 {
  return [n >>> 0, Math.floor(n / 4294967296) >>> 0];
}
const golden: U64 = [0x7f4a7c15, 0x9e3779b9];
function noise(seed: U64, n: number) {
  return (hash(xor(seed, mul(integer(n), golden)))[1] | 0) / 2147483648;
}
function walk(seed: U64, tick: number) {
  const epoch = Math.floor(tick / 960),
    position = (tick % 960) / 30;
  const step = Math.floor(position),
    fraction = position - step;
  const bridge = xor(seed, hash(integer(epoch)));
  let sum = 0,
    total = 0;
  for (let i = 0; i < 32; i++) {
    const v = noise(bridge, i);
    total += v;
    if (i < step) sum += v;
    else if (i === step) sum += fraction * v;
  }
  const start = noise(seed, epoch),
    end = noise(seed, epoch + 1);
  return start + ((end - start) * position) / 32 + (sum - (total * position) / 32) * 0.08;
}
// Owner-selected detail 1.75 matches signal.DefaultStaticParameters.
const grain = Math.max(1, Math.trunc(6 / 1.75));
const frames = new Map<
  string,
  { canvas: OffscreenCanvas | HTMLCanvasElement; pixels: ImageData }
>();
export function crispStatic(seed: number, tick: number, width: number, height: number) {
  const w = Math.max(2, Math.round(width)),
    h = Math.max(2, Math.round(height));
  const key = `${w}:${h}`;
  let f = frames.get(key);
  if (!f) {
    const canvas =
      typeof OffscreenCanvas === "undefined"
        ? document.createElement("canvas")
        : new OffscreenCanvas(w, h);
    canvas.width = w;
    canvas.height = h;
    const context = canvas.getContext("2d")! as
      CanvasRenderingContext2D | OffscreenCanvasRenderingContext2D;
    f = { canvas, pixels: context.createImageData(w, h) };
    if (frames.size >= 8) frames.delete(frames.keys().next().value!);
    frames.set(key, f);
  }
  const s = integer(seed),
    t = tick / 30;
  const bar = Math.trunc(
    (0.5 +
      0.24 * Math.sin(t * 0.173) +
      0.13 * Math.sin(t * 0.071 * Math.SQRT2) +
      0.08 * walk(s, tick)) *
      h,
  );
  const barHeight = Math.max(12, Math.floor(h / 11));
  const clock = xor(s, mul(integer(Math.floor(tick / 2)), [0xde82ef95, 0xd1342543]));
  const data = f.pixels.data;
  for (let y = 0, gy = 0; y < h; y += grain, gy++) {
    const rowSeed = xor(clock, mul(integer(gy), golden));
    for (let x = 0, gx = 0; x < w; x += grain, gx++) {
      const v = 52 + (hash(xor(rowSeed, integer(gx)))[1] >>> 25);
      for (let yy = y; yy < Math.min(h, y + grain); yy++) {
        const distance = Math.abs(yy - bar);
        let offset =
          distance < barHeight ? -Math.trunc(((barHeight - distance) * 28) / barHeight) : 0;
        if (yy % 3 === 0) offset -= 3;
        const q = Math.max(
          0,
          Math.min(255, (298 * (Math.max(16, Math.min(235, v + offset)) - 16) + 128) >> 8),
        );
        for (let xx = x; xx < Math.min(w, x + grain); xx++) {
          const i = (yy * w + xx) * 4;
          data[i] = data[i + 1] = data[i + 2] = q;
          data[i + 3] = 255;
        }
      }
    }
  }
  const bits = hash(xor(s, mul(integer(Math.floor(tick / 3)), [0x78bd642f, 0xa0761d64])));
  // uint64 modulo and shift, used only once per frame.
  const event = (BigInt(bits[1]) << 32n) | BigInt(bits[0]);
  if (event % 29n === 0n) {
    const line = Number((event >> 16n) % BigInt(h));
    for (let y = line; y < Math.min(h, line + Math.max(1, Math.floor(h / 270))); y++)
      for (let x = 0; x < w; x++) {
        const n = hash(xor(bits, integer(Math.floor(x / 12))));
        const v = 145 + Number(((BigInt(n[1]) << 32n) | BigInt(n[0])) % 60n);
        const q = (298 * (v - 16) + 128) >> 8,
          i = (y * w + x) * 4;
        data[i] = data[i + 1] = data[i + 2] = q;
      }
  }
  const context = f.canvas.getContext("2d")! as
    CanvasRenderingContext2D | OffscreenCanvasRenderingContext2D;
  context.putImageData(f.pixels, 0, 0);
  return f.canvas;
}
