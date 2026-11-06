import { sceneDisplayName } from "./scene-names";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { Button, Input, Select, Textarea, Popover } from "@braedonsaunders/appkit-ui";
import {
  ChevronDown,
  Link2,
  Plus,
  SlidersHorizontal,
  Type,
  Layers,
  Volume2,
  Minus,
} from "lucide-react";
import { t } from "../../../../i18n";
import {
  studio,
  ROLES,
  type Style,
  type StyleScene,
  type ControlKind,
  type LibraryAsset,
} from "../studio-model";
import { evaluate, type Value, type Layer, type SceneBindings } from "../scene-renderer";
import { styleBindings } from "../Stage";
import { layerBounds } from "./scene-bounds";
import { findLayer, updateLayer, sharedTextLayers, updateSharedText } from "./scene-edit";
import { isAnimated, setKeyframe } from "./keyframes";
import { propertyName } from "./Timeline";
import { VideoPicker } from "./VideoPicker";
function Section(p: { title: string; children: ReactNode; open?: boolean }) {
  return (
    <details open={p.open} className="group/section border-b border-border py-4">
      <summary className="mb-3 flex cursor-pointer list-none items-center justify-between text-[11px] font-semibold uppercase tracking-wider text-fg-muted">
        {p.title}
        <ChevronDown size={12} className="transition-transform group-open/section:rotate-180" />
      </summary>
      <div className="space-y-3">{p.children}</div>
    </details>
  );
}
export function Inspector(p: {
  style: Style;
  scene: StyleScene;
  selected: string[];
  onChange(style: Style): void;
  time?: number;
  bindings?: SceneBindings;
}) {
  const [assets, setAssets] = useState<LibraryAsset[]>([]),
    [animProp, setAnimProp] = useState<keyof Layer>("opacity"),
    [binding, setBinding] = useState(""),
    [gradient, setGradient] = useState("");
  const scrub = useRef<{ key: keyof Layer; x: number; value: number; layer: Layer } | null>(null);
  useEffect(() => {
    void studio.library().then(setAssets);
  }, []);
  const roleNames = {
    ident: t("Ident"),
    upnext: t("Up next"),
    tonight: t("Tonight"),
    break: t("Break"),
    back: t("Back from break"),
    offair: t("Off air"),
    welcome: t("Welcome"),
    bug: t("Corner logo"),
    lowerthird: t("Lower third"),
  };
  const layer = findLayer(p.scene.document, p.selected[0] ?? "");
  const stack = layer?.type === "group" ? sharedTextLayers(layer) : [];
  const groupTextKeys = new Set<keyof Layer>([
    "text",
    "font",
    "size",
    "track",
    "weight",
    "maxWidth",
    "maxLines",
    "align",
    "fit",
  ]);
  const sourceFor = (key: keyof Layer) =>
    stack.length && groupTextKeys.has(key) ? stack[0]! : layer!;
  const textLayer = stack[0] ?? layer;
  const bindings = p.bindings ?? styleBindings(p.style),
    time = p.time ?? 0;
  const changeScene = (next: StyleScene) =>
    p.onChange({ ...p.style, scenes: p.style.scenes.map((s) => (s.id === next.id ? next : s)) });
  const duration = (value: number) =>
    p.onChange({
      ...p.style,
      roles: { ...p.style.roles, [p.scene.role]: { duration: value } },
      scenes: p.style.scenes.map((s) =>
        s.role === p.scene.role ? { ...s, document: { ...s.document, duration: value } } : s,
      ),
    });
  if (!layer)
    return (
      <section>
        <div className="mb-1 flex items-center gap-2 text-sm font-semibold">
          <SlidersHorizontal size={16} />
          {t("Scene settings")}
        </div>
        <p className="mb-2 text-xs text-fg-muted">
          {t("Select a layer on the canvas to edit its properties.")}
        </p>
        <Section title={t("Scene")} open>
          <label className="block space-y-1 text-xs text-fg-muted">
            {t("Name")}
            <Input
              className="h-8 text-xs"
              value={sceneDisplayName(p.style, p.scene)}
              aria-label={t("Scene name")}
              onChange={(e) => changeScene({ ...p.scene, name: e.target.value })}
            />
          </label>
          <label className="block space-y-1 text-xs text-fg-muted">
            {t("Role")}
            <Select
              value={p.scene.role}
              aria-label={t("Scene role")}
              onChange={(e) => {
                const role = e.target.value as StyleScene["role"];
                changeScene({
                  ...p.scene,
                  role,
                  document: {
                    ...p.scene.document,
                    moment: role,
                    duration: p.style.roles[role]?.duration ?? 8,
                  },
                });
              }}
            >
              {ROLES.map((role) => (
                <option key={role} value={role}>
                  {roleNames[role]}
                </option>
              ))}
            </Select>
          </label>
          <label className="block space-y-2 text-xs text-fg-muted">
            {t("Rotation weight")}
            <input
              type="range"
              min="1"
              max="10"
              value={p.scene.weight}
              className="w-full accent-primary"
              aria-label={t("Rotation weight")}
              onChange={(e) => changeScene({ ...p.scene, weight: Number(e.target.value) })}
            />
            <span>{t("×{weight} in rotation", { weight: p.scene.weight })}</span>
          </label>
          <div className="flex items-center justify-between text-xs">
            <span className="text-fg-muted">{t("Duration")}</span>
            <div className="flex items-center gap-2 rounded-md border border-border">
              <Button
                size="sm"
                variant="ghost"
                disabled={p.scene.document.duration <= 4}
                aria-label={t("Shorten scene")}
                onClick={() => duration(Math.max(4, p.scene.document.duration - 4))}
              >
                <Minus size={12} />
              </Button>
              <span className="tabular-nums">
                {t("{seconds}s", { seconds: p.scene.document.duration })}
              </span>
              <Button
                size="sm"
                variant="ghost"
                disabled={p.scene.document.duration >= 28}
                aria-label={t("Lengthen scene")}
                onClick={() => duration(Math.min(28, p.scene.document.duration + 4))}
              >
                <Plus size={12} />
              </Button>
            </div>
          </div>
        </Section>
        <Section title={t("Background video")} open>
          <VideoPicker scene={p.scene} onChange={changeScene} />
        </Section>
        <Section title={t("Sound")} open>
          <div className="flex items-center gap-2 text-xs text-fg-muted">
            <Volume2 size={15} />
            {t("{count} sound tracks", { count: p.scene.sounds.length })}
          </div>
          {p.scene.sounds.map((sound) => (
            <div key={sound.id} className="flex items-center justify-between text-xs">
              <span className="truncate">
                {assets.find((a) => a.digest === sound.assetDigest)?.name ?? t("Sound track")}
              </span>
              <span className="text-fg-muted">{t("{seconds}s", { seconds: sound.at })}</span>
            </div>
          ))}
          <p className="text-[11px] text-fg-muted">
            {t("Arrange sound and animation in the timeline.")}
          </p>
        </Section>
      </section>
    );
  const patch = (data: Partial<Layer>) => {
    let document = p.scene.document;
    const shared: Partial<Layer> = {},
      own: Partial<Layer> = {};
    for (const [key, value] of Object.entries(data)) {
      Object.assign(stack.length && groupTextKeys.has(key as keyof Layer) ? shared : own, {
        [key]: value,
      });
    }
    if (Object.keys(shared).length) document = updateSharedText(document, layer.id, shared);
    if (Object.keys(own).length) document = updateLayer(document, layer.id, own);
    changeScene({ ...p.scene, document });
  };
  const set = (key: keyof Layer, value: Value) => {
    let document = p.scene.document;
    for (const target of stack.length && groupTextKeys.has(key) ? stack : [layer])
      document = updateLayer(
        document,
        target.id,
        isAnimated(target[key])
          ? { [key]: setKeyframe(target, key, time, value)[key] }
          : { [key]: value },
      );
    changeScene({ ...p.scene, document });
  };
  const n = (key: keyof Layer, fallback = 0) =>
    Number(evaluate(sourceFor(key)[key] as Value, bindings, time) ?? fallback);
  const labels: Record<string, string> = {
    x: t("X"),
    y: t("Y"),
    w: t("W"),
    h: t("H"),
    rotate: t("Rotation"),
    scale: t("Scale"),
    scaleX: t("Scale X"),
    scaleY: t("Scale Y"),
    opacity: t("Opacity"),
    size: t("Size"),
    weight: t("Weight"),
    track: t("Tracking"),
    maxWidth: t("Max width"),
    maxLines: t("Max lines"),
    blur: t("Blur"),
    glow: t("Glow"),
    strokeWidth: t("Stroke width"),
    count: t("Count"),
    seed: t("Seed"),
    repeat: t("Repeat"),
  };
  const number = (key: keyof Layer) => {
    const size =
      (key === "w" || key === "h") && ["path", "text", "group"].includes(layer.type)
        ? layerBounds(p.scene.document, bindings, time).find((b) => b.id === layer.id)
        : undefined;
    const percent = ["scale", "scaleX", "scaleY", "opacity"].includes(key);
    const factor = key === "rotate" ? 180 / Math.PI : percent ? 100 : 1;
    const value = size
      ? key === "w"
        ? size.w * size.scaleX
        : size.h * size.scaleY
      : n(key, percent ? 1 : key === "weight" ? 400 : key === "size" ? 60 : 0);
    const write = (v: number) => {
      if (size) {
        set(key === "w" ? "scaleX" : "scaleY", v / Math.max(1, key === "w" ? size.w : size.h));
        return;
      }
      set(key, v / factor);
    };
    return (
      <div
        key={key}
        className="flex h-8 min-w-0 items-center gap-1 rounded-md border border-border bg-bg-subtle px-2 focus-within:border-primary"
      >
        <span
          className="shrink-0 cursor-ew-resize select-none text-[10px] text-fg-muted"
          title={t("Drag to adjust")}
          onPointerDown={(e) => {
            scrub.current = { key, x: e.clientX, value: value * factor, layer };
            e.currentTarget.setPointerCapture(e.pointerId);
          }}
          onPointerMove={(e) => {
            if (e.currentTarget.hasPointerCapture(e.pointerId) && scrub.current)
              write(scrub.current.value + (e.clientX - scrub.current.x) * (e.shiftKey ? 10 : 1));
          }}
          onPointerUp={() => {
            scrub.current = null;
          }}
        >
          {labels[key] ?? t("Value")}
        </span>
        <Input
          className="h-6 min-w-0 border-0 bg-transparent px-1 text-right text-xs shadow-none focus:ring-0"
          type="number"
          step={percent ? 1 : key === "rotate" ? 1 : 0.1}
          aria-label={labels[key] ?? t("Value")}
          value={Number.isFinite(value) ? Number((value * factor).toFixed(2)) : 0}
          onChange={(e) => write(Number(e.target.value))}
        />
        <span className="text-[9px] text-fg-muted">
          {key === "rotate"
            ? "°"
            : percent
              ? "%"
              : [
                    "x",
                    "y",
                    "w",
                    "h",
                    "size",
                    "track",
                    "maxWidth",
                    "blur",
                    "glow",
                    "strokeWidth",
                  ].includes(key)
                ? t("px")
                : ""}
        </span>
      </div>
    );
  };
  const tokens = [
    ["bg", t("Background")],
    ["accent", t("Accent")],
    ["text", t("Text colour")],
    ["glow", t("Glow colour")],
    ["type.display", t("Display font")],
    ["type.body", t("Body font")],
    ["brand.logo", t("Brand logo")],
    ["words.tagline", t("Tagline")],
    ["channel.name", t("Channel name")],
    ["next.title", t("Next programme")],
    ["next.time", t("Next start time")],
    ["next.poster", t("Next poster")],
    ["next.backdrop", t("Next backdrop")],
    ["now.title", t("Now playing")],
    ["viewer.name", t("Viewer name")],
    ["clock", t("Clock")],
    ...p.style.controls.map((c) => [`control.${c.id}`, c.label]),
  ];
  const bind = (key: keyof Layer, label: string, kind: ControlKind) => (
    <Popover
      open={binding === key}
      onOpenChange={(open) => setBinding(open ? key : "")}
      className="max-h-80 w-60 overflow-y-auto p-2"
      trigger={
        <button
          className="rounded p-1.5 text-fg-muted hover:bg-surface-hover hover:text-primary"
          aria-label={t("Bind {field}", { field: label })}
          onClick={() => setBinding(binding === key ? "" : key)}
        >
          <Link2 size={12} />
        </button>
      }
    >
      <h4 className="mb-2 px-2 text-xs font-semibold">{t("Data binding")}</h4>
      {tokens.map(([token, name]) => (
        <button
          key={token}
          className="block w-full rounded px-2 py-1.5 text-left text-xs hover:bg-surface-hover"
          onClick={() => {
            set(key, `{${token}}`);
            setBinding("");
          }}
        >
          {name}
        </button>
      ))}
      <Button
        size="sm"
        variant="ghost"
        className="mt-2 w-full justify-start gap-2 border-t border-border"
        onClick={() => {
          const id = `${layer.id}-${key}`.replace(/[^a-zA-Z0-9_-]/g, "-");
          const value = evaluate(sourceFor(key)[key] as Value, bindings, time);
          const document = (
            stack.length && groupTextKeys.has(key) ? updateSharedText : updateLayer
          )(p.scene.document, layer.id, { [key]: `{control.${id}}` });
          p.onChange({
            ...p.style,
            controls: [
              ...p.style.controls.filter((c) => c.id !== id),
              {
                id,
                label,
                kind,
                value: typeof value === "number" || typeof value === "string" ? value : "",
              },
            ],
            scenes: p.style.scenes.map((s) => (s.id === p.scene.id ? { ...s, document } : s)),
          });
          setBinding("");
        }}
      >
        <Plus size={13} />
        {t("Expose as control…")}
      </Button>
    </Popover>
  );
  const text = (key: keyof Layer, label: string, kind: ControlKind = "text") => (
    <div key={key} className="space-y-1">
      <label className="text-[10px] text-fg-muted">{label}</label>
      <div className="flex items-center gap-1">
        <Input
          className="h-8 min-w-0 text-xs"
          aria-label={label}
          value={String(sourceFor(key)[key] ?? "")}
          onChange={(e) => set(key, e.target.value)}
        />
        {bind(key, label, kind)}
      </div>
    </div>
  );
  const choice = (key: keyof Layer, label: string, options: [string, string][]) => (
    <label className="block space-y-1 text-[10px] text-fg-muted">
      {label}
      <Select
        aria-label={label}
        value={String(sourceFor(key)[key] ?? options[0]?.[0] ?? "")}
        onChange={(e) => set(key, e.target.value)}
      >
        {options.map(([value, name]) => (
          <option key={value} value={value}>
            {name}
          </option>
        ))}
      </Select>
    </label>
  );
  const color = (key: "fill" | "stroke" | "tint", label: string) => {
    const raw = layer[key],
      resolved = evaluate(raw, bindings, time);
    const value = typeof resolved === "string" ? resolved : "#ffffff";
    const g = raw && typeof raw === "object" && "stops" in raw ? raw : undefined;
    return (
      <div className="space-y-2">
        <div className="flex h-9 items-center gap-2 rounded-md border border-border bg-bg-subtle px-2">
          <input
            type="color"
            className="h-5 w-5 shrink-0 cursor-pointer overflow-hidden rounded border-0 bg-transparent p-0"
            aria-label={t("Choose {field}", { field: label })}
            value={/^#[\da-f]{6}$/i.test(value) ? value : "#ffffff"}
            onChange={(e) => set(key, e.target.value)}
          />
          <span className="w-10 text-[10px] text-fg-muted">{label}</span>
          <Popover
            open={gradient === key}
            onOpenChange={(open) => setGradient(open ? key : "")}
            className="w-64 space-y-3 p-3"
            trigger={
              <button
                className="min-w-0 flex-1 truncate text-left text-[11px]"
                onClick={() => setGradient(gradient === key ? "" : key)}
              >
                {g
                  ? t("Linear gradient")
                  : typeof raw === "string" && raw.startsWith("{")
                    ? (tokens.find(([token]) => `{${token}}` === raw)?.[1] ?? t("Bound colour"))
                    : value.toUpperCase()}
              </button>
            }
          >
            <h4 className="text-xs font-semibold">{label}</h4>
            {g ? (
              g.stops.map((stop, index) => (
                <div key={index} className="flex gap-2">
                  <Input
                    className="h-8 w-16 text-xs"
                    type="number"
                    step=".01"
                    min="0"
                    max="1"
                    aria-label={t("Stop position")}
                    value={stop[0]}
                    onChange={(e) =>
                      patch({
                        [key]: {
                          ...g,
                          stops: g.stops.map((s, i) =>
                            i === index ? [Number(e.target.value), s[1]] : s,
                          ),
                        },
                      })
                    }
                  />
                  <Input
                    className="h-8 text-xs"
                    aria-label={t("Stop colour")}
                    value={stop[1]}
                    onChange={(e) =>
                      patch({
                        [key]: {
                          ...g,
                          stops: g.stops.map((s, i) => (i === index ? [s[0], e.target.value] : s)),
                        },
                      })
                    }
                  />
                </div>
              ))
            ) : (
              <Input
                className="h-8 text-xs"
                aria-label={label}
                value={String(raw ?? "")}
                onChange={(e) => set(key, e.target.value)}
              />
            )}
            <Button
              size="sm"
              variant="secondary"
              onClick={() =>
                patch({
                  [key]: {
                    linear: [0, 0, 0, 1080],
                    stops: [
                      [0, "{text}"],
                      [0.5, "{accent}"],
                      [1, "{bg}"],
                    ],
                  },
                })
              }
            >
              {t("Use gradient")}
            </Button>
          </Popover>
          {bind(key, label, "color")}
        </div>
      </div>
    );
  };
  return (
    <section>
      <div className="mb-1 flex items-center gap-2 text-sm font-semibold">
        {layer.type === "text" ? <Type size={15} /> : <Layers size={15} />}
        <span className="truncate">{layer.name || t("Layer")}</span>
      </div>
      {p.selected.length > 1 && (
        <p className="text-xs text-fg-muted">
          {t("{count} layers selected", { count: p.selected.length })}
        </p>
      )}
      <Section title={t("Layout")} open>
        <div className="grid grid-cols-2 gap-2">
          {(["x", "y", "w", "h", "rotate", "opacity", "scaleX", "scaleY"] as const).map(number)}
        </div>
      </Section>
      <Section title={t("Appearance")} open>
        {layer.type !== "group" && color("fill", t("Fill"))}
        {layer.type !== "group" && color("stroke", t("Stroke"))}
        <div className="grid grid-cols-2 gap-2">
          {layer.type !== "group" && number("strokeWidth")}
          {number("blur")}
          {number("glow")}
        </div>
        {choice("blend", t("Blend"), [
          ["normal", t("Normal")],
          ["screen", t("Screen")],
          ["multiply", t("Multiply")],
          ["overlay", t("Overlay")],
          ["lighten", t("Lighten")],
          ["difference", t("Difference")],
        ])}
        <div className="grid grid-cols-2 gap-2">
          {choice("join", t("Join"), [
            ["miter", t("Miter")],
            ["round", t("Round")],
            ["bevel", t("Bevel")],
          ])}
          {choice("cap", t("Cap"), [
            ["butt", t("Butt")],
            ["round", t("Round")],
            ["square", t("Square")],
          ])}
        </div>
      </Section>
      {(layer.type === "text" || stack.length > 0) && (
        <Section title={t("Text")} open>
          <div className="flex items-start gap-1">
            <Textarea
              className="min-h-20 text-xs"
              aria-label={t("Text content")}
              value={
                typeof textLayer?.text === "string"
                  ? textLayer.text
                  : String(evaluate(textLayer?.text, bindings, time) ?? "")
              }
              onChange={(e) => set("text", e.target.value)}
            />
            {bind("text", t("Text"), "text")}
          </div>
          <div className="flex items-center gap-1">
            <div className="min-w-0 flex-1">
              {choice("font", t("Font"), [
                ["{type.display}", t("Brand display")],
                ["{type.body}", t("Brand body")],
                ["notosans", t("Noto Sans")],
                ["bebasneue", t("Bebas Neue")],
                ["orbitron", t("Orbitron")],
                ["bungee", t("Bungee")],
                ["pressstart2p", t("Press Start 2P")],
                ["rajdhani", t("Rajdhani")],
                ...assets
                  .filter((a) => a.kind === "font")
                  .map((a) => [a.digest, a.name] as [string, string]),
              ])}
            </div>
            {bind("font", t("Font"), "font")}
          </div>
          <div className="grid grid-cols-2 gap-2">
            {(["size", "weight", "track", "maxWidth", "maxLines"] as const).map(number)}
          </div>
          <div className="grid grid-cols-2 gap-2">
            {choice("align", t("Align"), [
              ["left", t("Left")],
              ["center", t("Centre")],
              ["right", t("Right")],
            ])}
            {choice("fit", t("Fit"), [
              ["shrink", t("Shrink")],
              ["wrap", t("Wrap")],
              ["clip", t("Clip")],
            ])}
          </div>
        </Section>
      )}
      {["image", "art", "logo"].includes(layer.type) && (
        <Section title={t("Image")} open>
          {text("src", t("Source"), "image")}
          {choice("fit", t("Fit"), [
            ["cover", t("Cover")],
            ["contain", t("Contain")],
          ])}
          {color("tint", t("Tint"))}
        </Section>
      )}
      {layer.type === "path" && (
        <Section title={t("Shape")}>
          <Textarea
            className="text-xs"
            aria-label={t("Path")}
            value={layer.path ?? ""}
            onChange={(e) => patch({ path: e.target.value })}
          />
          <p className="text-[11px] text-fg-muted">
            {t("Drag path nodes on the canvas to reshape.")}
          </p>
        </Section>
      )}
      {["particles", "noise"].includes(layer.type) && (
        <Section title={t("Texture")} open>
          <div className="grid grid-cols-2 gap-2">
            {(["count", "seed", "repeat"] as const).map(number)}
          </div>
        </Section>
      )}
      <Section title={t("Animation")}>
        <label className="block space-y-1 text-[10px] text-fg-muted">
          {t("Animated property")}
          <Select
            aria-label={t("Animated property")}
            value={animProp}
            onChange={(e) => setAnimProp(e.target.value as keyof Layer)}
          >
            {[
              "x",
              "y",
              "w",
              "h",
              "scale",
              "scaleX",
              "scaleY",
              "rotate",
              "opacity",
              "fill",
              "stroke",
              "strokeWidth",
              "text",
              "font",
              "size",
              "track",
              "src",
              "tint",
            ].map((key) => (
              <option key={key} value={key}>
                {propertyName(key)}
              </option>
            ))}
          </Select>
        </label>
        <Button
          size="sm"
          variant="secondary"
          className="w-full gap-2"
          onClick={() => {
            let document = p.scene.document;
            for (const id of p.selected) {
              const selected = findLayer(document, id);
              if (selected)
                document = updateLayer(
                  document,
                  id,
                  setKeyframe(
                    selected,
                    animProp,
                    time,
                    (evaluate(selected[animProp] as Value, bindings, time) as Value) ??
                      (["opacity", "scale", "scaleX", "scaleY"].includes(animProp) ? 1 : 0),
                  ),
                );
            }
            changeScene({ ...p.scene, document });
          }}
        >
          <Plus size={13} />
          {t("Add keyframe at {time}s", { time: time.toFixed(2) })}
        </Button>
        <p className="text-[11px] text-fg-muted">
          {t("Add keyframes and motion presets in the timeline.")}
        </p>
        {choice("blend", t("Compositing"), [
          ["normal", t("Normal")],
          ["screen", t("Screen")],
          ["multiply", t("Multiply")],
        ])}
        <label className="block space-y-1 text-[10px] text-fg-muted">
          {t("Reveal")}
          <Select
            value={layer.reveal?.wipe ?? ""}
            aria-label={t("Reveal direction")}
            onChange={(e) =>
              patch({
                reveal: e.target.value ? { wipe: e.target.value, from: 0, to: 1 } : undefined,
              })
            }
          >
            <option value="">{t("None")}</option>
            <option value="ltr">{t("Left to right")}</option>
            <option value="rtl">{t("Right to left")}</option>
          </Select>
        </label>
        {layer.reveal && (
          <div className="grid grid-cols-2 gap-2">
            {(["from", "to"] as const).map((key) => (
              <label key={key} className="space-y-1 text-[10px] text-fg-muted">
                {key === "from" ? t("Start") : t("End")}
                <Input
                  type="number"
                  step=".1"
                  className="h-8 text-xs"
                  value={layer.reveal![key]}
                  aria-label={key === "from" ? t("Reveal start") : t("Reveal end")}
                  onChange={(e) =>
                    patch({ reveal: { ...layer.reveal!, [key]: Number(e.target.value) } })
                  }
                />
              </label>
            ))}
          </div>
        )}
      </Section>
      <Section title={t("Data binding")}>
        <p className="text-[11px] text-fg-muted">
          {t(
            "Use the link beside a field to connect your brand, live data, or an exposed control.",
          )}
        </p>
        {(["fill", "text", "font", "src"] as const)
          .filter((key) => sourceFor(key)[key] !== undefined)
          .map((key) => (
            <div key={key} className="flex items-center justify-between text-xs">
              <span>
                {{ fill: t("Fill"), text: t("Text"), font: t("Font"), src: t("Source") }[key]}
              </span>
              {bind(
                key,
                { fill: t("Fill"), text: t("Text"), font: t("Font"), src: t("Source") }[key],
                key === "fill"
                  ? "color"
                  : key === "font"
                    ? "font"
                    : key === "src"
                      ? "image"
                      : "text",
              )}
            </div>
          ))}
      </Section>
    </section>
  );
}
