import { useEffect, useState } from "react";
import { Button, Select, Switch, Input } from "@braedonsaunders/appkit-ui";
import { t } from "../../../../i18n";
import { studio, type LibraryAsset, type StyleScene } from "../studio-model";
import { LibraryPicker } from "../../library/LibraryPicker";
import { apiBlob } from "../../../../lib/api";

export function VideoPicker({
  scene,
  onChange,
}: {
  scene: StyleScene;
  onChange(scene: StyleScene): void;
}) {
  const [picker, setPicker] = useState(false);
  const [frames, setFrames] = useState<string[]>([]);
  const [assets, setAssets] = useState<LibraryAsset[]>([]);
  useEffect(() => {
    void studio.library().then((a) => setAssets(a.filter((a) => a.kind === "video")));
  }, []);
  const video = scene.video;
  useEffect(() => {
    setFrames([]);
    if (!video) return;
    let cancelled = false,
      url = "";
    const media = document.createElement("video");
    media.muted = true;
    media.preload = "auto";
    void apiBlob(studio.media(video.assetDigest))
      .then(async (blob) => {
        if (cancelled) return;
        url = URL.createObjectURL(blob);
        media.src = url;
        await new Promise<void>((resolve, reject) => {
          media.onloadedmetadata = () => resolve();
          media.onerror = () => reject(new Error("Video decode failed"));
        });
        const canvas = document.createElement("canvas");
        canvas.width = 160;
        canvas.height = 90;
        const ctx = canvas.getContext("2d")!;
        const result: string[] = [];
        for (let i = 0; i < 6 && !cancelled; i++) {
          media.currentTime = Math.max(0.001, ((media.duration - 0.05) * i) / 6);
          await new Promise<void>((resolve) => {
            media.onseeked = () => resolve();
          });
          if (cancelled) return;
          ctx.drawImage(media, 0, 0, 160, 90);
          result.push(canvas.toDataURL("image/jpeg"));
        }
        if (!cancelled) setFrames(result);
      })
      .catch(() => setFrames([]));
    return () => {
      cancelled = true;
      media.pause();
      media.removeAttribute("src");
      media.load();
      if (url) URL.revokeObjectURL(url);
    };
  }, [video?.assetDigest]);

  const patch = (p: Partial<NonNullable<StyleScene["video"]>>) => {
    if (video) onChange({ ...scene, video: { ...video, ...p } });
  };
  return (
    <section className="space-y-3">
      <Button onClick={() => setPicker(true)}>{t("Choose scene video")}</Button>
      <LibraryPicker
        open={picker}
        kinds={["video"]}
        onPick={(asset) => {
          setPicker(false);
          setAssets((old) => [...old, asset]);
          onChange({
            ...scene,
            video: { assetDigest: asset.digest, mode: "background", fit: "cover", start: 0 },
          });
        }}
        onClose={() => setPicker(false)}
      />
      {video && (
        <>
          <Select
            aria-label={t("Video mode")}
            value={video.mode}
            onChange={(e) => patch({ mode: e.target.value as "background" | "replace" })}
          >
            <option value="background">{t("Background")}</option>
            <option value="replace">{t("Replace scene")}</option>
          </Select>
          <Select
            aria-label={t("Video fit")}
            value={video.fit}
            onChange={(e) => patch({ fit: e.target.value as "cover" | "contain" })}
          >
            <option value="cover">{t("Cover")}</option>
            <option value="contain">{t("Contain")}</option>
          </Select>
          <div className="flex overflow-hidden h-16">
            {frames.map((frame, i) => (
              <img key={i} alt="" className="w-1/6 object-cover" src={frame} />
            ))}
          </div>
          <label>
            {t("Start offset")}
            <Input
              type="range"
              min={0}
              max={(assets.find((a) => a.digest === video.assetDigest)?.durationMs ?? 30000) / 1000}
              step={0.01}
              value={video.start}
              onChange={(e) => patch({ start: Number(e.target.value) })}
            />
          </label>
          <label>
            {t("Crossfade")}
            <Switch
              checked={!!video.crossfade}
              onChange={(e) => patch({ crossfade: e.target.checked })}
            />
          </label>
          <Button onClick={() => onChange({ ...scene, video: undefined })}>
            {t("Remove video")}
          </Button>
        </>
      )}
    </section>
  );
}
