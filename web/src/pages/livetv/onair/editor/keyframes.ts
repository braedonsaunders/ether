import { evaluate, type Layer, type Value, type SceneBindings } from "../scene-renderer";
export type Ease = "linear" | "in" | "out" | "inOut" | "back" | "elastic" | "spring";
export const isAnimated = (v: unknown): v is { keys: [number, Value, string?][] } =>
  !!v && typeof v === "object" && "keys" in v && Array.isArray(v.keys);
export function valueAt(
  value: Value | undefined,
  time: number,
  bindings: SceneBindings = { tokens: {}, slots: {} },
): Value {
  return evaluate(value, bindings, time) as Value;
}
export function setKeyframe(layer: Layer, prop: keyof Layer, time: number, value: Value): Layer {
  const old = layer[prop];
  const keys: [number, Value, string?][] = isAnimated(old)
    ? old.keys.map((k) => [...k])
    : time > 0
      ? [[0, (old ?? value) as Value]]
      : [];
  const at = Math.max(0, Math.min(30, time));
  const existing = keys.find((k) => Math.abs(k[0] - at) < 0.00001);
  if (existing) existing[1] = value;
  else keys.push([at, value]);
  return { ...layer, [prop]: { keys: keys.sort((a, b) => a[0] - b[0]) } };
}
export function moveKeyframe(layer: Layer, prop: keyof Layer, from: number, to: number): Layer {
  const v = layer[prop];
  if (!isAnimated(v)) return layer;
  const key = v.keys.find((k) => k[0] === from);
  if (!key) return layer;
  const at = Math.max(0, Math.min(30, to));
  return {
    ...layer,
    [prop]: {
      keys: [...v.keys.filter((k) => k[0] !== from && k[0] !== at), [at, key[1], key[2]]].sort(
        (a, b) => Number(a[0]) - Number(b[0]),
      ),
    },
  };
}
export function removeKeyframe(layer: Layer, prop: keyof Layer, time: number): Layer {
  const v = layer[prop];
  if (!isAnimated(v)) return layer;
  const keys = v.keys.filter((k) => k[0] !== time);
  return { ...layer, [prop]: keys.length > 1 ? { keys } : (keys[0]?.[1] ?? v.keys[0]?.[1]) };
}
export function setEase(layer: Layer, prop: keyof Layer, time: number, ease: Ease): Layer {
  const v = layer[prop];
  if (!isAnimated(v)) return layer;
  return {
    ...layer,
    [prop]: { keys: v.keys.map((k) => (k[0] === time ? [k[0], k[1], ease] : [...k])) },
  };
}
