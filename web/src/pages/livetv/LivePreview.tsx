import { cn } from "@braedonsaunders/appkit-ui";
import { Loader2, Maximize2, RotateCcw, Volume2, VolumeX, X } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";
import { t } from "../../i18n";
import { API_BASE, errorMessage, sessionXhrSetup } from "../../lib/api";
import { fixed } from "../../lib/format";
import { ChannelLogo } from "./ChannelLogo";
import { tv, type Channel, type PreviewSession } from "./model";

type Phase = "tuning" | "playing" | "error";
type HlsLike = {
  loadSource: (u: string) => void;
  attachMedia: (v: HTMLMediaElement) => void;
  destroy: () => void;
  on: (e: string, fn: (...args: unknown[]) => void) => void;
  levels?: { height?: number; bitrate?: number }[];
  currentLevel?: number;
};

/**
 * A floating mini player for testing a channel from its page: it tunes the
 * saved channel exactly like a Plex client would (same source selection and
 * failover), plays it with hls.js, and stops the session when closed.
 */
export function LivePreview({ channel, onClose }: { channel: Channel; onClose: () => void }) {
  const video = useRef<HTMLVideoElement>(null);
  const [phase, setPhase] = useState<Phase>("tuning");
  const [error, setError] = useState("");
  const [session, setSession] = useState<PreviewSession | null>(null);
  const [muted, setMuted] = useState(false);
  const [quality, setQuality] = useState("");
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    let hls: HlsLike | null = null;
    let id = "";
    let live = true;
    setPhase("tuning");
    setError("");
    setQuality("");
    const fail = (message: string) => {
      if (!live) return;
      setError(message);
      setPhase("error");
    };
    (async () => {
      try {
        const s = await tv.play(channel.id);
        id = s.id;
        if (!live) {
          void tv.stopPreview(s.id).catch(() => undefined);
          return;
        }
        setSession(s);
        const { default: Hls } = await import("hls.js");
        const el = video.current;
        if (!live || !el) return;
        if (!Hls.isSupported()) {
          fail(t("This browser can't play HLS through Media Source Extensions."));
          return;
        }
        const h = new Hls({
          liveSyncDurationCount: 3,
          // Never hands the session to a channel's own (cross-origin) media host.
          xhrSetup: sessionXhrSetup,
        }) as unknown as HlsLike;
        hls = h;
        h.on(Hls.Events.ERROR, (...args: unknown[]) => {
          const data = args[1] as {
            fatal?: boolean;
            details?: string;
            response?: { code?: number };
          };
          if (data?.fatal)
            fail(
              (data.response?.code ?? 0) >= 400
                ? t("The stream returned HTTP {code}.", { code: data.response!.code! })
                : data.details
                  ? t("Playback stopped ({details}).", { details: data.details })
                  : t("Playback stopped (stream error)."),
            );
        });
        h.on(Hls.Events.LEVEL_SWITCHED, () => {
          const l = h.levels?.[h.currentLevel ?? 0];
          if (l)
            setQuality(
              [l.height ? `${l.height}p` : "", l.bitrate ? `${fixed(l.bitrate / 1e6, 1)} Mbps` : ""]
                .filter(Boolean)
                .join(" · "),
            );
        });
        h.attachMedia(el);
        h.loadSource(`${API_BASE}/livetv/${s.playlist}`);
        el.play()?.catch(() => {
          // Autoplay with sound was refused; start muted instead.
          el.muted = true;
          setMuted(true);
          void el.play()?.catch(() => undefined);
        });
      } catch (e) {
        fail(errorMessage(e));
      }
    })();
    return () => {
      live = false;
      hls?.destroy();
      if (id) void tv.stopPreview(id).catch(() => undefined);
    };
  }, [channel.id, attempt]);

  // Stop the stream when the tab is closed, not only when React unmounts.
  useEffect(() => {
    const stop = () => {
      if (session) void tv.stopPreview(session.id).catch(() => undefined);
    };
    window.addEventListener("pagehide", stop);
    return () => window.removeEventListener("pagehide", stop);
  }, [session]);

  const toggleMute = useCallback(() => {
    const el = video.current;
    if (!el) return;
    el.muted = !el.muted;
    setMuted(el.muted);
  }, []);

  return (
    <div
      role="dialog"
      aria-label={t("Watching {channel}", { channel: channel.name })}
      className="fixed bottom-5 right-5 z-50 w-[min(24rem,calc(100vw-2.5rem))] overflow-hidden rounded-2xl border border-white/10 bg-black shadow-2xl ring-1 ring-black/40"
    >
      <div className="group relative aspect-video bg-black">
        <video
          ref={video}
          className="size-full bg-black object-contain"
          playsInline
          muted={muted}
          onPlaying={() => setPhase("playing")}
          aria-label={t("{channel} live", { channel: channel.name })}
        />
        {phase !== "playing" ? (
          <div className="absolute inset-0 grid place-items-center bg-gradient-to-br from-zinc-900 via-black to-zinc-900">
            <div className="flex flex-col items-center gap-3 px-6 text-center">
              <ChannelLogo name={channel.name} logo={channel.logo} seed={channel.id} size={44} />
              {phase === "tuning" ? (
                <p className="flex items-center gap-2 text-sm text-white/80">
                  <Loader2 size={14} className="animate-spin" />{" "}
                  {t("Tuning {channel}…", { channel: channel.name })}
                </p>
              ) : (
                <>
                  <p className="text-sm text-white/90">
                    {error || t("Couldn't play this channel.")}
                  </p>
                  <button
                    type="button"
                    onClick={() => setAttempt((a) => a + 1)}
                    className="inline-flex items-center gap-1.5 rounded-full bg-white/10 px-3 py-1 text-xs font-medium text-white hover:bg-white/20"
                  >
                    <RotateCcw size={12} /> {t("Try again")}
                  </button>
                </>
              )}
            </div>
          </div>
        ) : null}

        {/* top bar */}
        <div className="pointer-events-none absolute inset-x-0 top-0 flex items-center gap-2 bg-gradient-to-b from-black/80 to-transparent px-3 pb-6 pt-2.5">
          <span className="inline-flex items-center gap-1 rounded bg-danger px-1.5 py-0.5 text-[10px] font-bold uppercase tracking-wider text-white">
            <span className="size-1.5 animate-pulse rounded-full bg-white" /> {t("Live")}
          </span>
          <span className="min-w-0 truncate text-xs font-medium text-white">
            {channel.number} · {channel.name}
          </span>
          <div className="pointer-events-auto ml-auto flex items-center gap-1">
            <IconButton label={muted ? t("Unmute") : t("Mute")} onClick={toggleMute}>
              {muted ? <VolumeX size={14} /> : <Volume2 size={14} />}
            </IconButton>
            <IconButton
              label={t("Full screen")}
              onClick={() => void video.current?.requestFullscreen?.().catch(() => undefined)}
            >
              <Maximize2 size={14} />
            </IconButton>
            <IconButton label={t("Stop watching")} onClick={onClose}>
              <X size={15} />
            </IconButton>
          </div>
        </div>

        {/* bottom bar */}
        <div
          className={cn(
            "pointer-events-none absolute inset-x-0 bottom-0 flex items-end gap-2 bg-gradient-to-t from-black/85 to-transparent px-3 pb-2.5 pt-8 text-[11px] text-white/80 transition-opacity",
            phase === "playing" && "opacity-0 group-hover:opacity-100",
          )}
        >
          <span className="min-w-0 flex-1 truncate">
            {session?.programme?.item.title ? (
              <span className="font-medium text-white">{session.programme.item.title}</span>
            ) : null}
          </span>
          {session?.activeSourceName ? (
            <span className="shrink-0 rounded-full bg-white/15 px-2 py-0.5">
              {t("via {source}", { source: session.activeSourceName })}
            </span>
          ) : null}
          {quality ? <span className="shrink-0 tabular-nums">{quality}</span> : null}
        </div>
      </div>
    </div>
  );
}

function IconButton({
  label,
  onClick,
  children,
}: {
  label: string;
  onClick: () => void;
  children: React.ReactNode;
}) {
  return (
    <button
      type="button"
      aria-label={label}
      title={label}
      onClick={onClick}
      className="grid size-7 place-items-center rounded-full text-white/85 transition-colors hover:bg-white/15 hover:text-white focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-white/60"
    >
      {children}
    </button>
  );
}
