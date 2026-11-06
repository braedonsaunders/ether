import { useEffect, useRef, useState } from "react";
import { Button, Input, Popover, Switch } from "@braedonsaunders/appkit-ui";
import {
  Check,
  ChevronDown,
  Upload,
  Shuffle,
  Sparkles,
  Layers,
  Radio,
  ScanLine,
  CircleDashed,
  Rainbow,
} from "lucide-react";
import { Drawer } from "../../../../components/Drawer";
import { apiBlob, errorMessage } from "../../../../lib/api";
import { useMediaURL } from "../useMediaURL";
import type { Layer } from "../scene-renderer";
import { ColorPicker } from "../../../../components/ColorPicker";
import { t } from "../../../../i18n";
import {
  studio,
  type Style,
  type StyleScene,
  type Brand,
  type LibraryAsset,
  type StyleControl,
} from "../studio-model";
import {
  styleBindings as engineBindings,
  renderDocument,
  releaseRenderer,
  loadSceneAssets,
  loadStyleFonts,
  type SceneBindings,
} from "../scene-renderer";
import { onair, type Bindings } from "../model";
import { DescribeLook } from "../DescribeLook";
import { LibraryPicker } from "../../library/LibraryPicker";

export function styleBindings(style: Style, slots: Record<string, string> = {}): SceneBindings {
  const bindings = engineBindings(style);
  return { ...bindings, slots: { ...bindings.slots, ...slots } };
}
export function StyleThumbnail({
  style,
  scene = style.scenes.find((x) => x.role === "ident"),
  slots = {},
  playing = false,
  time,
}: {
  style: Style;
  scene?: StyleScene;
  slots?: Record<string, string>;
  playing?: boolean;
  time?: number;
}) {
  const ref = useRef<HTMLCanvasElement>(null),
    [hover, setHover] = useState(false);
  const [loaded, setLoaded] = useState<SceneBindings | null>(null);
  const bindingKey = JSON.stringify(styleBindings(style, slots));
  const documentKey = JSON.stringify(scene?.document);
  useEffect(() => {
    let live = true;
    setLoaded(null);
    if (scene)
      void Promise.all([
        loadStyleFonts(style),
        loadSceneAssets(scene.document, JSON.parse(bindingKey) as SceneBindings),
      ])
        .then(([, b]) => {
          if (live) setLoaded(b);
        })
        .catch(() => {});
    return () => {
      live = false;
    };
  }, [bindingKey, documentKey]);
  useEffect(() => {
    const ctx = ref.current?.getContext("2d");
    return () => {
      if (ctx) releaseRenderer(ctx);
    };
  }, []);
  useEffect(() => {
    const ctx = ref.current?.getContext("2d");
    if (!ctx || !scene) return;
    let frame = 0;
    const start = performance.now();
    const bindings = loaded ?? (JSON.parse(bindingKey) as SceneBindings);
    const draw = () =>
      renderDocument(
        ctx,
        scene.document,
        bindings,
        time ??
          (playing || hover
            ? ((performance.now() - start) / 1000) % scene.document.duration
            : scene.document.poster),
      );
    const animate = () => {
      draw();
      frame = requestAnimationFrame(animate);
    };
    draw();
    if (time === undefined && (playing || hover)) frame = requestAnimationFrame(animate);
    return () => cancelAnimationFrame(frame);
  }, [loaded, bindingKey, documentKey, playing, hover, time]);

  return (
    <canvas
      ref={ref}
      width={640}
      height={360}
      className="aspect-video w-full rounded-lg bg-black"
      aria-label={t("{name} preview", { name: scene?.name ?? style.name })}
      tabIndex={0}
      onFocus={() => setHover(!matchMedia("(prefers-reduced-motion: reduce)").matches)}
      onBlur={() => setHover(false)}
      onMouseEnter={() => setHover(!matchMedia("(prefers-reduced-motion: reduce)").matches)}
      onMouseLeave={() => setHover(false)}
    />
  );
}
const fontNames: Record<string, string> = {
  inter: "Inter",
  playfairdisplay: "Playfair Display",
  specialelite: "Special Elite",
  fraunces: "Fraunces",
  bungee: "Bungee",
  fredoka: "Fredoka",
  orbitron: "Orbitron",
  vt323: "VT323",
  spacegrotesk: "Space Grotesk",
  ibmplexmono: "IBM Plex Mono",
  archivoblack: "Archivo Black",
  notosans: "Noto Sans",
};
const fonts = [
  "inter",
  "playfairdisplay",
  "specialelite",
  "fraunces",
  "bungee",
  "fredoka",
  "orbitron",
  "vt323",
  "spacegrotesk",
  "ibmplexmono",
  "archivoblack",
  "notosans",
];
function luminance(hex: string) {
  const c = hex
    .replace("#", "")
    .match(/../g)
    ?.map((x) => parseInt(x, 16) / 255)
    .map((x) => (x <= 0.04045 ? x / 12.92 : ((x + 0.055) / 1.055) ** 2.4)) ?? [0, 0, 0];
  return c[0]! * 0.2126 + c[1]! * 0.7152 + c[2]! * 0.0722;
}
function harmony(hex: string, angle: number) {
  const rgb = hex.match(/[a-f\d]{2}/gi)?.map((x) => parseInt(x, 16) / 255) ?? [0, 0, 0];
  const max = Math.max(...rgb),
    min = Math.min(...rgb),
    d = max - min;
  let h =
    d === 0
      ? 0
      : max === rgb[0]!
        ? ((rgb[1]! - rgb[2]!) / d) % 6
        : max === rgb[1]!
          ? (rgb[2]! - rgb[0]!) / d + 2
          : (rgb[0]! - rgb[1]!) / d + 4;
  h = ((h * 60 + angle + 360) % 360) / 60;
  const x = d * (1 - Math.abs((h % 2) - 1)),
    m = min;
  const c =
    h < 1
      ? [d, x, 0]
      : h < 2
        ? [x, d, 0]
        : h < 3
          ? [0, d, x]
          : h < 4
            ? [0, x, d]
            : h < 5
              ? [x, 0, d]
              : [d, 0, x];
  return (
    "#" +
    c
      .map((v) =>
        Math.round((v + m) * 255)
          .toString(16)
          .padStart(2, "0"),
      )
      .join("")
  );
}
export function RemixPanel(p: {
  style: Style;
  onChange(style: Style): void;
  onOpenScene?(sceneId: string): void;
}) {
  const s = p.style,
    [picker, setPicker] = useState(false),
    [assets, setAssets] = useState<LibraryAsset[]>([]),
    [live, setLive] = useState<Bindings>({});
  useEffect(() => {
    void onair.bindings({ channel: s.channelIds[0] }).then(setLive);
  }, [s.channelIds[0]]);
  useEffect(() => {
    void studio
      .library()
      .then(setAssets)
      .catch(() => {});
  }, []);
  const brand = (patch: Partial<Brand>) =>
    p.onChange({
      ...s,
      brand: { ...s.brand, ...patch },
      ...(patch.textures
        ? {
            scenes: s.scenes.map((scene) => ({
              ...scene,
              document: {
                ...scene.document,
                layers: textureLayers(scene.document.layers, patch.textures!),
                post: patch
                  .textures!.filter((type) => type !== "chroma")
                  .map(
                    (type) =>
                      scene.document.post?.find((effect) => effect.type === type) ?? {
                        type,
                        amount: `{textures.${type}}`,
                      },
                  ),
              },
            })),
          }
        : {}),
    });
  const contrast =
    (Math.max(luminance(s.brand.palette.text), luminance(s.brand.palette.background)) + 0.05) /
    (Math.min(luminance(s.brand.palette.text), luminance(s.brand.palette.background)) + 0.05);
  return (
    <div className="space-y-8">
      <fieldset className="space-y-4 rounded-xl border border-border bg-surface p-4">
        <legend className="font-semibold">{t("Brand kit")}</legend>
        <div className="flex items-center gap-4">
          <LogoPreview logo={s.brand.logo ?? ""} />
          <div className="min-w-0 flex-1">
            <label className="block space-y-2 text-sm">
              {t("Monogram")}
              <Input
                value={s.brand.logo?.length === 64 ? "" : (s.brand.logo ?? "")}
                maxLength={3}
                onChange={(e) => brand({ logo: e.target.value })}
              />
            </label>
            <p className="mt-2 text-xs text-fg-muted">
              {t("Up to 3 letters, used when there’s no logo.")}
            </p>
          </div>
        </div>
        <Button variant="outline" onClick={() => setPicker(true)}>
          {t("Choose logo from Library")}
        </Button>
        <LibraryPicker
          open={picker}
          kinds={["image"]}
          onPick={(a) => {
            brand({ logo: a.digest });
            setPicker(false);
          }}
          onClose={() => setPicker(false)}
        />
        <div className="grid grid-cols-[repeat(auto-fit,minmax(80px,1fr))] gap-2">
          {(Object.keys(s.brand.palette) as (keyof Brand["palette"])[]).map((key) => (
            <ColorPicker
              key={key}
              label={t(
                { background: "Background", accent: "Accent", text: "Text", glow: "Glow" }[key],
              )}
              value={s.brand.palette[key]}
              onChange={(v) => brand({ palette: { ...s.brand.palette, [key]: v } })}
            />
          ))}
        </div>
        <p className="text-xs" role="status">
          {t("Text contrast: {ratio}:1", { ratio: contrast.toFixed(2) })}{" "}
          {contrast >= 4.5 ? t("Good readability") : t("Increase text contrast")}
        </p>
        <div className="grid grid-cols-[repeat(auto-fit,minmax(128px,1fr))] gap-2">
          {[
            { label: t("Complementary"), angles: [180] },
            { label: t("Analogous"), angles: [-30, 30] },
            { label: t("Triad"), angles: [120, 240] },
          ].map((h) => {
            const palette = {
              ...s.brand.palette,
              glow: harmony(s.brand.palette.accent, h.angles[0]!),
              ...(h.angles.length > 1
                ? { background: harmony(s.brand.palette.accent, h.angles[1]!) }
                : {}),
            };
            return (
              <button
                key={h.label}
                className="space-y-2 rounded-lg border border-border p-3 text-left hover:border-primary focus-visible:ring-2 focus-visible:ring-primary"
                onClick={() => brand({ palette })}
              >
                <span className="flex overflow-hidden rounded">
                  {Object.values(palette).map((colour, i) => (
                    <span key={i} className="h-6 flex-1" style={{ background: colour }} />
                  ))}
                </span>
                <span className="text-xs font-medium">{h.label}</span>
              </button>
            );
          })}
        </div>
        <Button
          variant="outline"
          className="min-h-10"
          onClick={() => {
            const accent = harmony(s.brand.palette.accent, 30 + Math.random() * 300);
            brand({ palette: { ...s.brand.palette, accent, glow: harmony(accent, 150) } });
          }}
        >
          <Shuffle size={16} />
          {t("Shuffle palette")}
        </Button>
        <div className="grid grid-cols-[repeat(auto-fit,minmax(200px,1fr))] gap-4">
          {(["display", "body"] as const).map((key) => (
            <FontPicker
              key={key}
              label={key === "display" ? t("Display font") : t("Body font")}
              value={s.brand.type[key]}
              assets={assets}
              onChange={(value) => brand({ type: { ...s.brand.type, [key]: value } })}
            />
          ))}
        </div>
        <label className="block text-sm">
          {t("Tagline")}
          <Input value={s.brand.tagline} onChange={(e) => brand({ tagline: e.target.value })} />
        </label>
        <label className="block text-sm">
          {t("Energy: Calm to lively")}
          <input
            className="w-full accent-primary"
            type="range"
            min={0}
            max={1}
            step={0.01}
            value={s.brand.energy}
            onChange={(e) => brand({ energy: +e.target.value })}
          />
        </label>
        <div className="flex flex-wrap gap-2">
          {Object.entries(textureChoices()).map(([x, choice]) => (
            <Button
              key={x}
              variant={s.brand.textures.includes(x) ? "default" : "outline"}
              className="min-h-10 gap-2"
              aria-pressed={s.brand.textures.includes(x)}
              onClick={() =>
                brand({
                  textures: s.brand.textures.includes(x)
                    ? s.brand.textures.filter((v) => v !== x)
                    : [...s.brand.textures, x],
                })
              }
            >
              {s.brand.textures.includes(x) ? <Check size={16} /> : choice.icon}
              {choice.label}
            </Button>
          ))}
        </div>
      </fieldset>
      {Array.from(new Set(s.controls.map((c) => c.group ?? t("Controls")))).map((group) => (
        <fieldset key={group} className="space-y-4 rounded-xl border border-border bg-surface p-4">
          <legend className="font-semibold">{t(group)}</legend>
          {s.controls
            .filter((c) => (c.group ?? t("Controls")) === group)
            .map((c) => (
              <div key={c.id}>
                <RemixControl
                  control={c}
                  assets={assets}
                  onChange={(value) =>
                    p.onChange({
                      ...s,
                      controls: s.controls.map((x) => (x.id === c.id ? { ...x, value } : x)),
                    })
                  }
                />
              </div>
            ))}
        </fieldset>
      ))}
      <DescribeLook look={s} onChange={p.onChange} />
      <div className="grid grid-cols-2 gap-3">
        {s.scenes.map((scene) => (
          <button
            key={scene.id}
            className="rounded-lg border border-border p-1 text-left"
            onClick={() => p.onOpenScene?.(scene.id)}
          >
            <StyleThumbnail
              style={s}
              scene={scene}
              slots={{ ...engineBindings(s, live).slots, "next.poster": live.next?.backdrop ?? "" }}
            />
            <span className="text-xs">{scene.name}</span>
          </button>
        ))}
      </div>
    </div>
  );
}

function RemixControl({
  control: c,
  assets,
  onChange,
}: {
  control: StyleControl;
  assets: LibraryAsset[];
  onChange(v: string | number | boolean): void;
}) {
  const [open, setOpen] = useState(false);
  if (c.kind === "color")
    return <ColorPicker label={t(c.label)} value={String(c.value)} onChange={onChange} />;
  if (c.kind === "image")
    return (
      <div className="space-y-2">
        <p className="text-sm">{t(c.label)}</p>
        {c.value ? (
          <img
            className="h-20 object-contain"
            alt={t(c.label)}
            src={studio.thumb(String(c.value))}
          />
        ) : null}
        <Button variant="outline" onClick={() => setOpen(true)}>
          {t("Choose image")}
        </Button>
        <LibraryPicker
          open={open}
          kinds={["image"]}
          onClose={() => setOpen(false)}
          onPick={(a) => {
            onChange(a.digest);
            setOpen(false);
          }}
        />
      </div>
    );
  if (c.kind === "font")
    return (
      <FontPicker label={t(c.label)} value={String(c.value)} assets={assets} onChange={onChange} />
    );
  if (c.kind === "number")
    return (
      <label className="block space-y-3 text-sm">
        <span className="flex justify-between gap-4">
          <span>{t(c.label)}</span>
          <output className="tabular-nums text-fg-muted">
            {Number(c.value).toLocaleString(undefined, { maximumFractionDigits: 3 })}
          </output>
        </span>
        <input
          className="w-full accent-primary"
          type="range"
          min={c.min ?? 0}
          max={c.max ?? 100}
          step={c.step ?? 1}
          value={Number(c.value)}
          onChange={(e) => onChange(+e.target.value)}
        />
      </label>
    );
  if (c.kind === "toggle")
    return (
      <label className="flex min-h-10 items-center justify-between gap-4 text-sm">
        <span>{t(c.label)}</span>
        <Switch checked={Boolean(c.value)} onChange={(e) => onChange(e.currentTarget.checked)} />
      </label>
    );

  return (
    <label className="block text-sm">
      {t(c.label)}
      <Input
        type="text"

        value={String(c.value)}
        min={c.min}
        max={c.max}
        step={c.step}
        onChange={(e) => onChange(e.target.value)}
      />
    </label>
  );
}

function textureChoices() {
  return {
    grain: { label: t("Grain"), icon: <Layers size={16} /> },
    flicker: { label: t("Flicker"), icon: <Radio size={16} /> },
    scanlines: { label: t("Scanlines"), icon: <ScanLine size={16} /> },
    vignette: { label: t("Vignette"), icon: <CircleDashed size={16} /> },
    chroma: { label: t("Chroma"), icon: <Rainbow size={16} /> },
  };
}
function textureLayers(layers: Layer[], textures: string[]): Layer[] {
  return layers.map((layer) => ({
    ...layer,
    ...(layer.type === "text"
      ? {
          behave: [
            ...(layer.behave ?? []).filter((b) => !b.chroma),
            ...(textures.includes("chroma") ? [{ chroma: { amplitude: 3, speed: 1 } }] : []),
          ],
        }
      : {}),
    ...(layer.layers ? { layers: textureLayers(layer.layers, textures) } : {}),
  }));
}
function LogoPreview({ logo }: { logo: string }) {
  const asset = /^[a-f0-9]{64}$/.test(logo);
  const src = useMediaURL(asset ? studio.thumb(logo) : "");
  return (
    <div className="flex h-20 w-20 shrink-0 items-center justify-center overflow-hidden rounded-xl border border-border bg-bg-subtle p-2">
      {asset && src ? (
        <img src={src} alt={t("Current logo")} className="max-h-full max-w-full object-contain" />
      ) : (
        <span className="text-2xl font-semibold tracking-wide" aria-label={t("Monogram preview")}>
          {logo || <Sparkles size={24} className="text-fg-subtle" />}
        </span>
      )}
    </div>
  );
}
function FontPicker({
  label,
  value,
  assets,
  onChange,
}: {
  label: string;
  value: string;
  assets: LibraryAsset[];
  onChange(value: string): void;
}) {
  const [open, setOpen] = useState(false),
    [upload, setUpload] = useState(false),
    [license, setLicense] = useState(""),
    [selectedFile, setSelectedFile] = useState<File | null>(null),
    [error, setError] = useState(""),
    [busy, setBusy] = useState(false),
    [added, setAdded] = useState<{ id: string; name: string }[]>([]);
  const library = assets
    .filter((a) => a.kind === "font")
    .map((a) => ({ id: a.digest, name: a.name }));
  const choices = [
    ...fonts.map((id) => ({ id, name: fontNames[id]! })),
    ...library,
    ...added.filter((a) => !library.some((x) => x.id === a.id)),
  ];
  useEffect(() => {
    let live = true;
    const faces: FontFace[] = [];
    for (const font of [...library, ...added]) {
      void apiBlob(studio.media(font.id))
        .then(async (blob) => {
          const face = new FontFace(`onair-${font.id}`, await blob.arrayBuffer());
          await face.load();
          if (live) {
            document.fonts.add(face);
            faces.push(face);
          }
        })
        .catch(() => {});
    }
    return () => {
      live = false;
      faces.forEach((face) => document.fonts.delete(face));
    };
  }, [JSON.stringify(library), JSON.stringify(added)]);
  const name = choices.find((f) => f.id === value)?.name ?? value;
  return (
    <div className="space-y-2">
      <p className="text-sm font-medium">{label}</p>
      <Popover
        open={open}
        onOpenChange={setOpen}
        className="w-80 max-w-[calc(100vw-2rem)] p-2"
        trigger={
          <Button
            variant="outline"
            className="h-auto min-h-20 w-full justify-between p-3 text-left"
            onClick={() => setOpen(!open)}
          >
            <span className="min-w-0 flex-1 space-y-2">
              <span className="block text-xs text-fg-muted">{name}</span>
              <span
                className="block truncate text-2xl font-normal"
                style={{ fontFamily: `onair-${value}` }}
              >
                {t("Aa 012")}
              </span>
            </span>
            <ChevronDown size={16} />
          </Button>
        }
      >
        <div role="listbox" aria-label={label} className="max-h-80 overflow-y-auto">
          {choices.map((font) => (
            <button
              key={font.id}
              role="option"
              aria-selected={value === font.id}
              className={`flex min-h-20 w-full items-center justify-between rounded-lg p-3 text-left hover:bg-surface-hover ${value === font.id ? "bg-primary-subtle" : ""}`}
              onClick={() => {
                onChange(font.id);
                setOpen(false);
              }}
            >
              <span>
                <span className="block text-xs text-fg-muted">{font.name}</span>
                <span className="mt-1 block text-2xl" style={{ fontFamily: `onair-${font.id}` }}>
                  {t("On air tonight")}
                </span>
              </span>
              {value === font.id ? <Check size={16} /> : null}
            </button>
          ))}
        </div>
        <Button
          variant="ghost"
          className="mt-2 min-h-10 w-full justify-start border-t border-border"
          onClick={() => {
            setOpen(false);
            setUpload(true);
          }}
        >
          <Upload size={16} />
          {t("Upload font")}
        </Button>
      </Popover>
      <Drawer open={upload} size="sm" title={t("Upload font")} onClose={() => setUpload(false)}>
        <form
          className="space-y-4"
          onSubmit={async (e) => {
            e.preventDefault();
            if (!selectedFile) return;
            setBusy(true);
            setError("");
            try {
              const asset = await onair.uploadAsset(selectedFile, "font", license);
              setAdded((v) => [...v, { id: asset.digest, name: asset.name }]);
              onChange(asset.digest);
              setUpload(false);
              setSelectedFile(null);
            } catch (err) {
              setError(errorMessage(err));
            } finally {
              setBusy(false);
            }
          }}
        >
          <label className="block space-y-2 text-sm">
            {t("Font file")}
            <Input
              type="file"
              accept=".ttf,.otf"
              required
              onChange={(e) => setSelectedFile(e.target.files?.[0] ?? null)}
            />
          </label>
          <label className="block space-y-2 text-sm">
            {t("Licence")}
            <Input
              required
              value={license}
              onChange={(e) => setLicense(e.target.value)}
              placeholder={t("For example, OFL-1.1")}
            />
          </label>
          <p className="text-xs leading-relaxed text-fg-muted">
            {t(
              "The licence stays with this font in your Library and determines whether it can be shared in a pack.",
            )}
          </p>
          {error ? (
            <p role="alert" className="text-danger">
              {error}
            </p>
          ) : null}
          <Button type="submit" disabled={busy || !selectedFile || !license.trim()}>
            <Upload size={16} />
            {busy ? t("Uploading…") : t("Upload font")}
          </Button>
        </form>
      </Drawer>
    </div>
  );
}
