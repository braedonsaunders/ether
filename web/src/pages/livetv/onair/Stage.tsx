import { useEffect, useRef, useState } from "react";
import { t } from "../../../i18n";
import {
  renderDocument,
  releaseRenderer,
  loadOnAirFonts,
  loadSceneAssets,
  type SceneBindings,
} from "./scene-renderer";
import { apiBlob } from "../../../lib/api";
import type { SceneVideo, Style, StyleScene } from "./studio-model";
import type { Bindings, Moment } from "./model";
import { Canvas, type CanvasProps } from "./editor/Canvas";
export function styleBindings(style: Style, bindings: Bindings = {}): SceneBindings {
  const slots: Record<string, string> = {};
  const flatten = (value: unknown, prefix = "") => {
    if (value !== null && typeof value === "object")
      Object.entries(value).forEach(([k, v]) => flatten(v, prefix ? `${prefix}.${k}` : k));
    else if (value !== undefined) slots[prefix] = String(value ?? "");
  };
  flatten(bindings);
  return {
    tokens: {
      bg: style.brand.palette.background,
      accent: style.brand.palette.accent,
      text: style.brand.palette.text,
      glow: style.brand.palette.glow,
      "type.display": style.brand.type.display,
      "type.body": style.brand.type.body,
      "brand.logo": style.brand.logo,
      "words.tagline": style.brand.tagline,
      energy: style.brand.energy,
      ...Object.fromEntries(style.brand.textures.map((k) => [`textures.${k}`, 1])),
      ...Object.fromEntries(style.controls.map((c) => [`control.${c.id}`, c.value])),
    },
    slots,
  };
}
export function ScenePoster(p: { scene: StyleScene; bindings: SceneBindings; playing?: boolean }) {
  const canvas = useRef<HTMLCanvasElement>(null);
  useEffect(() => {
    let frame = 0,
      live = true;
    const start = performance.now();
    let bindings = p.bindings;
    const ctx = canvas.current?.getContext("2d");
    const draw = () => {
      if (!live || !ctx) return;
      renderDocument(
        ctx,
        p.scene.document,
        bindings,
        p.playing
          ? ((performance.now() - start) / 1000) % p.scene.document.duration
          : p.scene.document.poster,
      );
      if (p.playing) frame = requestAnimationFrame(draw);
    };
    draw();
    void Promise.all([loadOnAirFonts(), loadSceneAssets(p.scene.document, p.bindings)]).then(
      ([, b]) => {
        bindings = b;
        if (!p.playing) draw();
      },
    );
    return () => {
      live = false;
      cancelAnimationFrame(frame);
      if (ctx) releaseRenderer(ctx);
    };
  }, [p.scene, p.bindings, p.playing]);
  return (
    <canvas
      ref={canvas}
      width={480}
      height={270}
      className="aspect-video w-full rounded bg-black"
      aria-label={t("{name} preview", { name: p.scene.name })}
    />
  );
}
export function LookTile(p: {
  look: Style;
  moment?: Moment;
  channel?: string;
  bindings?: Bindings;
  size?: string;
  playOnHover?: boolean;
}) {
  const [hover, setHover] = useState(false);
  const scene = p.look.scenes.find((s) => s.role === (p.moment ?? "ident") && s.enabled);
  const bindings = styleBindings(p.look, p.bindings);
  return (
    <div
      className={p.size}
      onMouseEnter={() => setHover(p.playOnHover !== false)}
      onMouseLeave={() => setHover(false)}
    >
      {scene && <ScenePoster scene={scene} bindings={bindings} playing={hover} />}
    </div>
  );
}
export function LookStage(
  p: CanvasProps & { sceneVideo?: SceneVideo; playing?: boolean; muted?: boolean },
) {
  const [videoUrl, setVideoUrl] = useState("");
  const videoRef = useRef<HTMLVideoElement>(null);
  useEffect(() => {
    const digest = p.sceneVideo?.assetDigest;
    if (!digest) {
      setVideoUrl("");
      return;
    }
    const controller = new AbortController();
    let url = "";
    void apiBlob(`/ether/api/onair/assets/${digest}`, controller.signal)
      .then((blob) => {
        if (!controller.signal.aborted) {
          url = URL.createObjectURL(blob);
          setVideoUrl(url);
        }
      })
      .catch(() => {});
    return () => {
      controller.abort();
      if (url) URL.revokeObjectURL(url);
    };
  }, [p.sceneVideo?.assetDigest]);
  useEffect(() => {
    const video = videoRef.current;
    if (!video) return;
    const at = p.time + (p.sceneVideo?.start ?? 0);
    if (Math.abs(video.currentTime - at) > 0.15) video.currentTime = at;
    if (p.playing) void video.play().catch(() => {});
    else video.pause();
  }, [p.time, p.playing, videoUrl, p.sceneVideo?.start]);
  const [bindings, setBindings] = useState(p.bindings);
  useEffect(() => {
    let live = true;
    void Promise.all([loadOnAirFonts(), loadSceneAssets(p.document, p.bindings)]).then(([, b]) => {
      if (live) setBindings(b);
    });
    return () => {
      live = false;
    };
  }, [p.document, p.bindings]);
  return (
    <Canvas
      {...p}
      bindings={bindings}
      video={
        videoUrl ? (
          <video
            ref={videoRef}
            src={videoUrl}
            muted={p.muted}
            playsInline
            className={`absolute inset-0 h-full w-full ${p.sceneVideo?.mode === "replace" ? "z-10" : ""}`}
            style={{ objectFit: p.sceneVideo?.fit ?? "cover" }}
            aria-label={t("Scene video")}
          />
        ) : undefined
      }
    />
  );
}
