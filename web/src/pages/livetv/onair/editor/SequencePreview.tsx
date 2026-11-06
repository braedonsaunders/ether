import { useEffect, useMemo, useRef, useState } from "react";
import { Play, Pause, RotateCcw, Volume2, VolumeX, X } from "lucide-react";
import { Button } from "@braedonsaunders/appkit-ui";
import { t } from "../../../../i18n";
import { useLocale } from "../../../../i18n/react";
import { onair, type Bindings, type Reel } from "../model";
import { studio, type Style } from "../studio-model";
import { StyleThumbnail } from "./RemixPanel";
import { useMediaURL } from "../useMediaURL";
import { usePreviewAudio } from "./preview-audio";
export function SequencePreview({
  style,
  channelId,
  at,
  onClose,
}: {
  style: Style;
  channelId: string;
  at: string;
  onClose(): void;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  const open = true;
  const [playing, setPlaying] = useState(true),
    [muted, setMuted] = useState(false),
    [time, setTime] = useState(0),
    [bindings, setBindings] = useState<Bindings>({}),
    [reels, setReels] = useState<Reel[]>([]);
  useEffect(() => {
    if (open) dialog.current?.showModal();
  }, [open]);
  useEffect(() => {
    void onair.bindings({ channel: channelId, at }).then(setBindings);
    void onair.reels().then(setReels);
  }, [channelId, at]);
  const { locale } = useLocale();
  const chapters = useMemo(() => {
    const scene = (role: string) => style.scenes.find((x) => x.role === role && x.enabled);
    const reel = reels
      .find((x) => x.channelIds.includes(channelId))
      ?.clips.find((c) => c.status === "ready");
    return [
      { label: t("Ident"), scene: scene("ident") },
      { label: t("Programme"), programme: true },
      { label: t("Break"), scene: scene("break") },
      ...(reel
        ? [
            {
              label: reel.name,
              reel: {
                ...reel,
                preview: `/ether/api/onair/reels/${reels.find((r) => r.clips.some((c) => c.id === reel.id))!.id}/clips/${reel.id}/preview`,
              },
            },
          ]
        : []),
      { label: t("Back"), scene: scene("back") },
      { label: t("Up next"), scene: scene("upnext") },
    ]
      .filter((c) => c.scene || "programme" in c || "reel" in c)
      .map((c) => ({
        ...c,
        duration: c.scene
          ? (style.roles[c.scene.role]?.duration ?? 8)
          : "reel" in c
            ? Math.max(1, (c.reel?.durationMs ?? 8000) / 1000)
            : 6,
      }));
  }, [style, reels, channelId, locale]);
  const total = chapters.reduce((n, c) => n + c.duration, 0);
  let start = 0,
    index = 0;
  for (let i = 0; i < chapters.length; i++) {
    index = i;
    if (time < start + chapters[i]!.duration || i === chapters.length - 1) break;
    start += chapters[i]!.duration;
  }
  const chapter = chapters[index],
    local = time - start;
  useEffect(() => {
    if (!open || !playing) return;
    let frame = 0,
      last = performance.now();
    const tick = (now: number) => {
      setTime((v) => {
        const next = v + (now - last) / 1000;
        if (next >= total) {
          setPlaying(false);
          return total;
        }
        return next;
      });
      last = now;
      frame = requestAnimationFrame(tick);
    };
    frame = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(frame);
  }, [open, playing, total]);
  usePreviewAudio(chapter?.scene, local, open && playing, muted);
  const video = useRef<HTMLVideoElement>(null);
  const videoDigest = open
    ? (chapter?.scene?.video?.assetDigest ??
      (chapter && "reel" in chapter ? (chapter.reel?.sourceDigest ?? "") : ""))
    : "";
  const videoSource = useMediaURL(
    open
      ? videoDigest
        ? studio.media(videoDigest)
        : chapter && "reel" in chapter
          ? (chapter.reel?.preview ?? "")
          : ""
      : "",
  );
  const programmeSource = useMediaURL(bindings.next?.backdrop ?? "", open);
  useEffect(() => {
    const v = video.current;
    if (!v) return;
    const target = local + (chapter?.scene?.video?.start ?? 0);
    if (Math.abs(v.currentTime - target) > 0.25) v.currentTime = target;
    if (open && playing) void v.play().catch(() => {});
    else v.pause();
  }, [open, playing, local, videoSource]);

  const slots = {
    "channel.name": bindings.channel?.name ?? "",
    "next.title": bindings.next?.title ?? "",
    "next.time": bindings.next?.time ?? "",
    "next.poster": bindings.next?.backdrop ?? "",
    "next.backdrop": bindings.next?.backdrop ?? "",
    "now.title": bindings.now?.title ?? "",
  };
  return (
    <>
      {open ? (
        <dialog
          ref={dialog}
          onCancel={(e) => {
            e.preventDefault();
            setPlaying(false);
            onClose();
          }}
          role="dialog"
          aria-modal="true"
          aria-label={t("Sequence preview")}
          className="fixed inset-0 z-50 m-0 flex h-dvh max-h-none w-screen max-w-none flex-col border-0 bg-black p-0 text-white"
          onKeyDown={(e) => {
            if (e.key === "Escape") {
              setPlaying(false);
              onClose();
            }
          }}
        >
          <div className="flex items-center justify-between gap-4 border-b border-white/10 bg-neutral-950 px-4 py-4 sm:px-8">
            <div>
              <p className="text-xs uppercase tracking-widest text-white/50">
                {t("Sequence preview")}
              </p>
              <p className="mt-1 text-sm font-medium">
                {bindings.channel?.name} · {chapter?.label}
              </p>
            </div>
            <Button
              variant="secondary"
              onClick={() => {
                setPlaying(false);
                onClose();
              }}
            >
              <X size={16} />
              {t("Close")}
            </Button>
          </div>
          <div className="relative flex min-h-0 flex-1 items-center justify-center overflow-hidden">
            {chapter?.scene ? (
              <>
                {chapter.scene.video && videoSource ? (
                  <video
                    ref={video}
                    src={videoSource}
                    muted={muted}
                    playsInline
                    className="absolute inset-0 h-full w-full object-contain"
                  />
                ) : null}
                {chapter.scene.video?.mode !== "replace" ? (
                  <div className="relative w-full">
                    <StyleThumbnail
                      style={style}
                      scene={chapter.scene}
                      slots={slots}
                      time={local}
                    />
                  </div>
                ) : null}
              </>
            ) : chapter && "reel" in chapter ? (
              <video
                key={chapter.reel?.id}
                ref={video}
                src={videoSource}
                autoPlay={playing}
                muted={muted}
                controls
                playsInline
                className="h-full w-full object-contain"
              />
            ) : (
              <div className="relative flex h-full w-full items-end bg-neutral-950 p-6 sm:p-12">
                {programmeSource ? (
                  <img
                    alt=""
                    src={programmeSource}
                    className="absolute inset-0 h-full w-full object-cover"
                  />
                ) : null}
                <div className="relative rounded-xl bg-black/70 p-6 backdrop-blur-md">
                  <p className="text-sm">{t("Programme preview")}</p>
                  <p className="text-3xl">{bindings.next?.title}</p>
                </div>
              </div>
            )}
          </div>
          <nav
            aria-label={t("Sequence chapters")}
            className="flex gap-2 overflow-x-auto border-t border-white/10 bg-neutral-950 p-4 sm:px-8"
          >
            {chapters.map((c, i) => {
              const active = index === i;
              return (
                <button
                  key={i}
                  aria-current={active ? "step" : undefined}
                  className={`relative min-w-32 flex-1 overflow-hidden rounded-xl border px-4 py-3 text-left transition-colors ${active ? "border-white/60 bg-white/10" : "border-white/10 hover:border-white/40"}`}
                  onClick={() => setTime(chapters.slice(0, i).reduce((n, x) => n + x.duration, 0))}
                >
                  <span className="flex items-center justify-between gap-4 text-xs text-white/50">
                    <span>{String(i + 1).padStart(2, "0")}</span>
                    <span>{formatTime(c.duration)}</span>
                  </span>
                  <span className="mt-2 block truncate text-sm font-medium">{c.label}</span>
                  {c.scene ? (
                    <span className="mt-1 block truncate text-xs text-white/50">
                      {c.scene.name}
                    </span>
                  ) : null}
                  <span className="absolute inset-x-0 bottom-0 h-0.5 bg-white/10">
                    <span
                      className="block h-full bg-white"
                      style={{
                        width: `${i < index ? 100 : active ? Math.min(100, (local / c.duration) * 100) : 0}%`,
                      }}
                    />
                  </span>
                </button>
              );
            })}
          </nav>
          <div className="flex flex-wrap items-center gap-3 bg-neutral-950 px-4 pb-6 sm:px-8">
            <Button variant="secondary" onClick={() => setPlaying(!playing)}>
              {playing ? <Pause size={16} /> : <Play size={16} />}
              {playing ? t("Pause") : t("Play")}
            </Button>
            <Button
              variant="secondary"
              onClick={() => {
                setTime(0);
                setPlaying(true);
              }}
            >
              <RotateCcw size={16} />
              {t("Replay")}
            </Button>
            <Button variant="secondary" onClick={() => setMuted(!muted)} aria-pressed={muted}>
              {muted ? <VolumeX size={16} /> : <Volume2 size={16} />}
              {muted ? t("Unmute") : t("Mute")}
            </Button>
            <input
              aria-label={t("Sequence position")}
              type="range"
              min={0}
              max={total}
              step={0.1}
              value={time}
              className="min-w-24 flex-1 accent-white"
              onChange={(e) => setTime(+e.target.value)}
            />
            <span className="text-xs tabular-nums text-white/60">
              {formatTime(time)} / {formatTime(total)}
            </span>
          </div>
        </dialog>
      ) : null}
    </>
  );
}

function formatTime(seconds: number) {
  return `${Math.floor(seconds / 60)}:${Math.floor(seconds % 60)
    .toString()
    .padStart(2, "0")}`;
}
