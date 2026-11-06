import { useMediaURL } from "../onair/useMediaURL";
import { useEffect, useId, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { Button, Checkbox, Input, Select, confirmDialog } from "@braedonsaunders/appkit-ui";
import { AudioLines, Film, Image, Type, Play, Pause, Volume2 } from "lucide-react";
import { SubtabNav } from "../../../components/layout";
import { Drawer } from "../../../components/Drawer";
import { t, tn } from "../../../i18n";
import { apiFetch, sessionXhrSetup, errorMessage } from "../../../lib/api";
import { formatBytes } from "../../../lib/format";
import { studio, type LibraryAsset, type AssetKind } from "../onair/studio-model";
import { onair } from "../onair/model";
import { LinkImport } from "../onair/LinkImport";

export function Waveform({ asset, progress = 0 }: { asset: LibraryAsset; progress?: number }) {
  const clip = useId().replace(/:/g, "");
  const [peaks, setPeaks] = useState<number[]>([]);
  useEffect(() => {
    let live = true;
    void studio
      .waveform(asset.digest)
      .then((v) => {
        if (live) setPeaks(v.peaks);
      })
      .catch(() => {});
    return () => {
      live = false;
    };
  }, [asset.digest]);
  return (
    <svg
      viewBox="0 0 1000 160"
      className="h-24 w-full text-primary"
      role="img"
      aria-label={t("Audio waveform")}
    >
      <defs>
        <clipPath id={clip}>
          <rect width={Math.max(0, Math.min(1, progress)) * 1000} height="160" />
        </clipPath>
      </defs>
      <g opacity=".3">
        {peaks.map((p, i) => (
          <path
            key={i}
            d={`M${(i * 1000) / Math.max(1, peaks.length)} ${80 - p * 78}v${Math.max(1, p * 156)}`}
            stroke="currentColor"
          />
        ))}
      </g>
      <g clipPath={`url(#${clip})`}>
        {peaks.map((p, i) => (
          <path
            key={i}
            d={`M${(i * 1000) / Math.max(1, peaks.length)} ${80 - p * 78}v${Math.max(1, p * 156)}`}
            stroke="currentColor"
          />
        ))}
      </g>
    </svg>
  );
}
let activeAudio: HTMLAudioElement | null = null;
function AudioPreview({
  asset,
  source,
  full,
}: {
  asset: LibraryAsset;
  source: string;
  full: boolean;
}) {
  const audio = useRef<HTMLAudioElement>(null);
  const [playing, setPlaying] = useState(false),
    [time, setTime] = useState(0),
    [duration, setDuration] = useState((asset.durationMs ?? 0) / 1000),
    [error, setError] = useState(false);
  useEffect(() => {
    const node = audio.current;
    return () => {
      node?.pause();
      if (activeAudio === node) activeAudio = null;
    };
  }, []);
  const seek = (next: number) => {
    if (audio.current && duration > 0) {
      audio.current.currentTime = Math.max(0, Math.min(duration, next));
      setTime(audio.current.currentTime);
    }
  };
  const toggle = () => {
    const node = audio.current;
    if (!node) return;
    if (playing) node.pause();
    else {
      if (activeAudio && activeAudio !== node) activeAudio.pause();
      activeAudio = node;
      if (node.ended) node.currentTime = 0;
      void node.play().catch(() => {
        setError(true);
        setPlaying(false);
      });
    }
  };
  const clock = (value: number) =>
    `${Math.floor(value / 60)}:${String(Math.floor(value % 60)).padStart(2, "0")}`;
  return (
    <div
      className="flex h-full min-h-32 flex-col justify-center gap-2 bg-gradient-to-br from-primary/5 to-transparent px-4 py-3"
      onClick={(e) => e.stopPropagation()}
    >
      <audio
        ref={audio}
        src={source || undefined}
        preload="metadata"
        className="hidden"
        aria-label={asset.name}
        onLoadedMetadata={(e) => {
          if (Number.isFinite(e.currentTarget.duration)) setDuration(e.currentTarget.duration);
        }}
        onTimeUpdate={(e) => setTime(e.currentTarget.currentTime)}
        onPlay={() => {
          setPlaying(true);
          setError(false);
        }}
        onPause={() => setPlaying(false)}
        onEnded={() => setPlaying(false)}
      />
      <div className="relative overflow-hidden rounded-lg bg-primary/5">
        <div
          role="slider"
          tabIndex={duration > 0 ? 0 : -1}
          aria-label={t("Seek audio")}
          aria-valuemin={0}
          aria-valuemax={duration}
          aria-valuenow={Math.min(time, duration)}
          aria-valuetext={t("{elapsed} of {duration}", {
            elapsed: clock(time),
            duration: clock(duration),
          })}
          className="relative cursor-pointer px-2 outline-none focus-visible:ring-2 focus-visible:ring-primary"
          onPointerDown={(e) => {
            e.stopPropagation();
            const rect = e.currentTarget.getBoundingClientRect();
            seek(((e.clientX - rect.left) / rect.width) * duration);
            e.currentTarget.setPointerCapture(e.pointerId);
          }}
          onPointerMove={(e) => {
            if (e.currentTarget.hasPointerCapture(e.pointerId)) {
              const rect = e.currentTarget.getBoundingClientRect();
              seek(((e.clientX - rect.left) / rect.width) * duration);
            }
          }}
          onKeyDown={(e) => {
            if (["ArrowLeft", "ArrowRight", "Home", "End"].includes(e.key)) {
              e.preventDefault();
              seek(
                e.key === "Home"
                  ? 0
                  : e.key === "End"
                    ? duration
                    : time + (e.key === "ArrowRight" ? 5 : -5),
              );
            }
          }}
        >
          <div
            className="pointer-events-none absolute inset-y-0 left-0 bg-primary/10 transition-[width] duration-100"
            style={{ width: `${duration ? (time / duration) * 100 : 0}%` }}
          />
          <Waveform asset={asset} progress={duration ? time / duration : 0} />
        </div>
        <Button
          size="sm"
          className="absolute left-3 top-1/2 h-10 w-10 -translate-y-1/2 rounded-full p-0 shadow-lg"
          disabled={!source}
          aria-label={
            playing
              ? t("Pause {name}", { name: asset.name })
              : t("Play {name}", { name: asset.name })
          }
          onClick={toggle}
        >
          {playing ? <Pause size={18} /> : <Play size={18} fill="currentColor" />}
        </Button>
      </div>
      <div className="flex items-center justify-between gap-4 text-[10px] tabular-nums text-fg-muted">
        <span>{playing ? t("Playing") : t("Audio preview")}</span>
        <span>
          {t("{elapsed} / {duration}", { elapsed: clock(time), duration: clock(duration) })}
        </span>
      </div>
      {full && (
        <label className="flex items-center gap-3 text-xs text-fg-muted">
          <Volume2 size={15} />
          {t("Volume")}
          <input
            className="flex-1 accent-primary"
            type="range"
            min="0"
            max="1"
            step=".01"
            defaultValue="1"
            aria-label={t("Audio preview volume")}
            onChange={(e) => {
              if (audio.current) audio.current.volume = Number(e.target.value);
            }}
          />
        </label>
      )}
      {error && (
        <p role="alert" className="text-xs text-danger">
          {t("Couldn’t play this audio.")}
        </p>
      )}
    </div>
  );
}
function Preview({ asset: a, full = false }: { asset: LibraryAsset; full?: boolean }) {
  const [sample, setSample] = useState(t("Aa"));
  const [hovered, setHovered] = useState(false);
  const preview = useMediaURL(
    (a as LibraryAsset & { url?: string }).url ?? studio.media(a.digest),
    a.kind === "image" ? full : a.kind !== "video" || full || hovered,
  );
  const thumbnail = useMediaURL(
    (a as LibraryAsset & { thumbnail?: string }).thumbnail ?? studio.thumb(a.digest),
    a.kind === "image" || a.kind === "video",
  );
  useEffect(() => {
    if (a.kind !== "font" || !preview) return;
    const face = new FontFace(`library-${a.digest}`, `url(${preview})`);
    let live = true;
    void face
      .load()
      .then((f) => {
        if (live) document.fonts.add(f);
      })
      .catch(() => {});
    return () => {
      live = false;
      document.fonts.delete(face);
    };
  }, [a.digest, a.kind, preview]);
  if (a.kind === "video")
    return (
      <video
        className="h-full w-full object-cover"
        src={preview || undefined}
        poster={thumbnail || undefined}
        controls={full}
        muted={!full}
        loop={!full}
        playsInline
        tabIndex={0}
        preload="metadata"
        onLoadedData={(e) => {
          if (!full && hovered) void e.currentTarget.play().catch(() => {});
        }}
        onMouseEnter={(e) => {
          setHovered(true);
          if (!full) void e.currentTarget.play().catch(() => {});
        }}
        onMouseLeave={(e) => {
          setHovered(false);
          if (!full) e.currentTarget.pause();
        }}
        onFocus={(e) => {
          setHovered(true);
          if (!full) void e.currentTarget.play().catch(() => {});
        }}
        onBlur={(e) => {
          setHovered(false);
          if (!full) e.currentTarget.pause();
        }}
      />
    );
  if (a.kind === "audio") return <AudioPreview asset={a} source={preview} full={full} />;
  if (a.kind === "font")
    return (
      <div className="flex h-full flex-col items-center justify-center gap-4 p-4 text-center">
        {full && (
          <Input
            aria-label={t("Sample text")}
            value={sample}
            onChange={(e) => setSample(e.target.value)}
          />
        )}
        <p className="break-words text-6xl" style={{ fontFamily: `library-${a.digest}` }}>
          {sample}
        </p>
        {full && (
          <p className="mt-6 break-words text-2xl" style={{ fontFamily: `library-${a.digest}` }}>
            {t("ABCDEFGHIJKLMNOPQRSTUVWXYZ abcdefghijklmnopqrstuvwxyz 0123456789 !?@#$%&*()")}
          </p>
        )}
      </div>
    );
  return full ? (
    <a href={preview} target="_blank" rel="noreferrer" aria-label={t("Open full size image")}>
      <img src={preview || undefined} alt={a.name} className="max-h-[60vh] w-full object-contain" />
    </a>
  ) : (
    <img
      src={thumbnail || undefined}
      alt=""
      loading="lazy"
      className="h-full w-full object-cover"
    />
  );
}
const kinds: AssetKind[] = ["video", "audio", "image", "font"];
export function LibraryGrid({
  allowed = kinds,
  onPick,
}: {
  allowed?: AssetKind[];
  onPick?: (a: LibraryAsset) => void;
}) {
  const [params, setParams] = useSearchParams();
  const [loading, setLoading] = useState(true);
  const [assets, setAssets] = useState<(LibraryAsset & { readOnly?: boolean })[]>([]),
    [error, setError] = useState("");
  const [selected, setSelected] = useState<string[]>([]),
    [pending, setPending] = useState<File[]>([]),
    [license, setLicense] = useState("");
  const [uploads, setUploads] = useState<{ name: string; progress: number; status: string }[]>([]);
  const kind = params.get("kind") ?? "",
    query = params.get("q") ?? "",
    sort = params.get("sort") ?? "newest";
  const change = (key: string, value: string) =>
    setParams((p) => {
      if (value) p.set(key, value);
      else p.delete(key);
      return p;
    });
  const reload = async () => {
    try {
      const library = await studio.library();
      if (!onPick) {
        const reels = await onair.reels();
        for (const reel of reels)
          for (const clip of reel.clips) {
            library.push({
              ...clip,
              digest: `reel-${reel.id}-${clip.id}`,
              kind: "video",
              mime: "video/mp4",
              size: 0,
              createdAt: 0,
              usedBy: [{ kind: "reel", id: reel.id, name: reel.name }],
              url: clip.preview,
              thumbnail: clip.thumbnail,
              readOnly: true,
            } as LibraryAsset);
          }
      }
      setAssets(library);
      setError("");
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setLoading(false);
    }
  };
  useEffect(() => {
    void reload();
  }, []);
  useEffect(() => {
    if (!assets.some((a) => a.status && !["ready", "error"].includes(a.status))) return;
    const id = setInterval(() => void reload(), 1500);
    return () => clearInterval(id);
  }, [assets]);
  const upload = async (file: File, lic = "") => {
    const assetKind: AssetKind = /\.(ttf|otf)$/i.test(file.name)
      ? "font"
      : file.type.startsWith("video/")
        ? "video"
        : file.type.startsWith("audio/")
          ? "audio"
          : "image";
    if (assetKind === "font" && !lic) {
      setPending((p) => [...p, file]);
      return;
    }
    if (!allowed.includes(assetKind)) {
      setError(t("This file type is not available here."));
      return;
    }
    setUploads((p) => [...p, { name: file.name, progress: 0, status: t("Uploading…") }]);
    try {
      if (import.meta.env.MODE === "mock") {
        const data = new FormData();
        data.append("file", file);
        data.append("kind", assetKind);
        data.append("license", lic);
        await apiFetch("/onair/assets", { method: "POST", body: data });
      } else
        await new Promise<void>((resolve, reject) => {
          const xhr = new XMLHttpRequest();
          xhr.open("POST", "/ether/api/onair/assets");
          sessionXhrSetup(xhr, "/ether/api/onair/assets");
          xhr.upload.onprogress = (e) => {
            if (e.lengthComputable)
              setUploads((p) =>
                p.map((x) =>
                  x.name === file.name
                    ? { ...x, progress: Math.round((e.loaded / e.total) * 100) }
                    : x,
                ),
              );
          };
          xhr.upload.onload = () =>
            setUploads((p) =>
              p.map((x) => (x.name === file.name ? { ...x, status: t("Processing media…") } : x)),
            );
          xhr.onload = () =>
            xhr.status >= 200 && xhr.status < 300
              ? resolve()
              : reject(new Error(t("Upload failed")));
          xhr.onerror = () => reject(new Error(t("Upload failed")));
          const data = new FormData();
          data.append("file", file);
          data.append("kind", assetKind);
          data.append("license", lic);
          xhr.send(data);
        });
      setUploads((p) => p.filter((x) => x.name !== file.name));
      await reload();
    } catch (e) {
      setError(errorMessage(e));
      setUploads((p) =>
        p.map((x) => (x.name === file.name ? { ...x, status: t("Upload failed") } : x)),
      );
    }
  };
  const remove = async (list: LibraryAsset[]) => {
    if (!list.length) return;
    if (
      !(await confirmDialog({
        title: tn(list.length, "Delete {count} asset?", "Delete {count} assets?"),
        message: t("These files will be permanently removed from the Library."),
        confirmLabel: t("Delete"),
        tone: "danger",
      }))
    )
      return;
    try {
      for (const a of list) await onair.deleteAsset(a.digest);
      setSelected([]);
      await reload();
    } catch (e) {
      setError(errorMessage(e));
    }
  };
  const visible = assets
    .filter(
      (a) =>
        allowed.includes(a.kind) &&
        (!kind || a.kind === kind) &&
        a.name.toLocaleLowerCase().includes(query.toLocaleLowerCase()),
    )
    .sort((a, b) =>
      sort === "name"
        ? a.name.localeCompare(b.name)
        : sort === "size"
          ? b.size - a.size
          : sort === "used"
            ? b.usedBy.length - a.usedBy.length
            : b.createdAt - a.createdAt,
    );
  const active = assets.find((a) => a.digest === params.get("asset"));
  return (
    <section
      className="space-y-5"
      onDragOver={(e) => e.preventDefault()}
      onDrop={(e) => {
        e.preventDefault();
        for (const file of e.dataTransfer.files) void upload(file);
      }}
    >
      <div className="flex flex-wrap items-center gap-3">
        <div className="flex-1">
          <h2 className="text-xl font-semibold">{t("Your station’s creative Library")}</h2>
          <p className="text-sm text-fg-muted">
            {t("Bumpers, stings, logos and type. Make every moment yours.")}
          </p>
        </div>
        <label className="cursor-pointer rounded-lg border border-border px-4 py-2 text-sm focus-within:ring-2 focus-within:ring-primary">
          {t("Upload files")}
          <input
            className="sr-only"
            aria-label={t("Upload files")}
            type="file"
            multiple
            accept="image/*,audio/*,video/*,.ttf,.otf"
            onChange={(e) => {
              for (const f of e.target.files ?? []) void upload(f);
              e.target.value = "";
            }}
          />
        </label>
        <LinkImport onImported={reload} />
      </div>
      <div className="flex flex-wrap gap-2">
        {[
          ["", t("All")],
          ["video", t("Video")],
          ["audio", t("Audio")],
          ["image", t("Images")],
          ["font", t("Fonts")],
        ]
          .filter(([k]) => !k || allowed.includes(k as AssetKind))
          .map(([k, label]) => (
            <Button
              key={k}
              size="sm"
              variant={kind === k ? "default" : "outline"}
              aria-pressed={kind === k}
              onClick={() => change("kind", k!)}
            >
              {label}
            </Button>
          ))}
        <Input
          aria-label={t("Search Library")}
          placeholder={t("Search Library")}
          value={query}
          onChange={(e) => change("q", e.target.value)}
          className="min-w-40 flex-1"
        />
        <div className="w-44 shrink-0">
          <Select
            aria-label={t("Sort assets")}
            value={sort}
            onChange={(e) => change("sort", e.target.value)}
          >
            <option value="newest">{t("Newest")}</option>
            <option value="name">{t("Name")}</option>
            <option value="size">{t("Size")}</option>
            <option value="used">{t("Most used")}</option>
          </Select>
        </div>
      </div>
      {error && (
        <p role="alert" className="text-danger">
          {error}
        </p>
      )}
      {uploads.map((u, i) => (
        <div key={i} role="status">
          <p>
            {t("{name}: {status} ({progress}%)", {
              name: u.name,
              status: u.status,
              progress: u.progress,
            })}
          </p>
          <progress className="w-full" max={100} value={u.progress} />
        </div>
      ))}
      {pending.length > 0 && (
        <div className="space-y-3 rounded-xl border border-border p-4">
          <p>{t("Choose a licence before uploading fonts.")}</p>
          <Select
            aria-label={t("Font licence")}
            value={license}
            onChange={(e) => setLicense(e.target.value)}
          >
            <option value="">{t("Choose a licence")}</option>
            {[
              ["OFL", t("OFL")],
              ["Apache", t("Apache")],
              ["Owned by me", t("Owned by me")],
              ["Other", t("Other")],
            ].map(([v, l]) => (
              <option key={v} value={v}>
                {l}
              </option>
            ))}
          </Select>
          {pending.map((f) => (
            <p key={f.name}>{f.name}</p>
          ))}
          <Button
            disabled={!license}
            onClick={() => {
              for (const f of pending) void upload(f, license);
              setPending([]);
            }}
          >
            {t("Upload fonts")}
          </Button>
          <Button variant="ghost" onClick={() => setPending([])}>
            {t("Cancel")}
          </Button>
        </div>
      )}
      {!onPick && selected.length > 0 && (
        <div className="flex items-center gap-3">
          <span>{tn(selected.length, "{count} selected", "{count} selected")}</span>
          <Button
            variant="outline"
            disabled={
              !assets.some((a) => selected.includes(a.digest) && !a.usedBy.length && !a.readOnly)
            }
            onClick={() =>
              void remove(assets.filter((a) => selected.includes(a.digest) && !a.usedBy.length))
            }
          >
            {t("Delete unused selected")}
          </Button>
          <Button variant="ghost" onClick={() => setSelected([])}>
            {t("Clear selection")}
          </Button>
        </div>
      )}
      {loading && <p role="status">{t("Loading Library…")}</p>}
      {!loading && !visible.length && (
        <div className="rounded-2xl border border-dashed border-border p-12 text-center">
          <h3 className="text-lg font-semibold">
            {assets.length ? t("No matching assets") : t("Give your station its signature")}
          </h3>
          <p className="mx-auto mt-3 max-w-lg text-fg-muted">
            {t(
              "Drop video bumpers, audio stings, image logos or your favourite licensed fonts here. Everything you add is ready to use in your styles.",
            )}
          </p>
        </div>
      )}
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-3 2xl:grid-cols-4">
        {visible.map((a) => {
          const Icon = { audio: AudioLines, video: Film, image: Image, font: Type }[a.kind];
          return (
            <article
              key={a.digest}
              onClick={(e) => {
                if (!(e.target as HTMLElement).closest("button,input,a,audio,video")) {
                  if (onPick) onPick(a);
                  else change("asset", a.digest);
                }
              }}
              className="cursor-pointer overflow-hidden rounded-xl border border-border bg-surface shadow-sm"
            >
              <div className="aspect-video overflow-hidden bg-bg-subtle">
                <Preview asset={a} />
              </div>
              <div className="space-y-2 p-3">
                <div className="flex items-center gap-2">
                  <Icon size={16} aria-hidden />
                  <button
                    className="min-w-0 flex-1 truncate text-left font-medium hover:text-primary focus-visible:ring-2"
                    onClick={() => (onPick ? onPick(a) : change("asset", a.digest))}
                  >
                    {a.name}
                  </button>
                  {!onPick && !a.readOnly && (
                    <Checkbox
                      aria-label={t("Select {name}", { name: a.name })}
                      checked={selected.includes(a.digest)}
                      onChange={(e) =>
                        setSelected((p) =>
                          e.target.checked ? [...p, a.digest] : p.filter((d) => d !== a.digest),
                        )
                      }
                    />
                  )}
                </div>
                <div className="flex justify-between text-xs text-fg-muted">
                  <span>
                    {a.readOnly
                      ? t("{seconds} s", { seconds: ((a.durationMs ?? 0) / 1000).toFixed(1) })
                      : a.durationMs
                        ? t("{seconds} s · {size}", {
                            seconds: (a.durationMs / 1000).toFixed(1),
                            size: formatBytes(a.size),
                          })
                        : formatBytes(a.size)}
                  </span>
                  <span className="rounded-full bg-bg-subtle px-2">
                    {tn(a.usedBy.length, "Used by {count}", "Used by {count}")}
                  </span>
                </div>
                {a.status && a.status !== "ready" && (
                  <p role="status" className="text-xs">
                    {a.status === "error"
                      ? a.error
                      : t("Preparing media… {progress}%", { progress: a.progress ?? 0 })}
                  </p>
                )}
              </div>
            </article>
          );
        })}
      </div>
      {active && !onPick && active.readOnly && (
        <Drawer open title={active.name} onClose={() => change("asset", "")} size="lg">
          <Preview asset={active} full />
          {active.usedBy.map((u) => (
            <Link
              key={u.id}
              className="mt-4 block text-primary"
              to={`/livetv/looks?reel=${encodeURIComponent(u.id)}`}
            >
              {t("Used in reel {name}", { name: u.name })}
            </Link>
          ))}
        </Drawer>
      )}
      {active && !onPick && !active.readOnly && (
        <AssetDrawer
          key={active.digest}
          asset={active}
          onClose={() => change("asset", "")}
          onChanged={async (a) => {
            await reload();
            if (a) change("asset", a.digest);
          }}
          onDelete={() => remove([active])}
        />
      )}
    </section>
  );
}
function AssetDrawer({
  asset: a,
  onClose,
  onChanged,
  onDelete,
}: {
  asset: LibraryAsset;
  onClose: () => void;
  onChanged: (a?: LibraryAsset) => Promise<void>;
  onDelete: () => Promise<void>;
}) {
  const [params, setParams] = useSearchParams();
  const tab = params.get("assetTab") ?? "preview";
  const [name, setName] = useState(a.name),
    [license, setLicense] = useState(a.license ?? ""),
    [attribution, setAttribution] = useState(a.attribution ?? ""),
    [error, setError] = useState(""),
    [busy, setBusy] = useState(false),
    [start, setStart] = useState(0),
    [end, setEnd] = useState((a.durationMs ?? 0) / 1000);
  const run = async (fn: () => Promise<LibraryAsset | void>) => {
    setBusy(true);
    setError("");
    try {
      const v = await fn();
      await onChanged(v || undefined);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Drawer open onClose={onClose} title={a.name} size="lg">
      <div className="space-y-6">
        <SubtabNav
          ariaLabel={t("Asset sections")}
          active={tab}
          onSelect={(v) =>
            setParams((p) => {
              p.set("assetTab", v);
              return p;
            })
          }
          tabs={[
            { key: "preview", label: t("Preview") },
            { key: "details", label: t("Details") },
            { key: "usage", label: t("Used by") },
          ]}
        />
        {tab === "preview" && <Preview asset={a} full />}
        {error && (
          <p role="alert" className="text-danger">
            {error}
          </p>
        )}
        {tab === "details" && (
          <>
            <label className="block space-y-2">
              {t("Name")}
              <Input value={name} onChange={(e) => setName(e.target.value)} />
            </label>
            <label className="block space-y-2">
              {t("Licence")}
              <Input value={license} onChange={(e) => setLicense(e.target.value)} />
            </label>
            <label className="block space-y-2">
              {t("Attribution")}
              <Input value={attribution} onChange={(e) => setAttribution(e.target.value)} />
            </label>
            {a.source && (
              <a className="text-primary" href={a.source} target="_blank" rel="noreferrer">
                {t("Original source")}
              </a>
            )}
            <Button
              disabled={busy || !name.trim()}
              onClick={() =>
                void run(() =>
                  apiFetch<LibraryAsset>(`/onair/assets/${a.digest}`, {
                    method: "PATCH",
                    body: JSON.stringify({ name, license, attribution }),
                  }),
                )
              }
            >
              {t("Save details")}
            </Button>
            <label className="block cursor-pointer text-primary">
              {t("Replace file")}
              <input
                type="file"
                className="mt-2 block w-full text-sm"
                disabled={busy}
                accept={a.kind === "font" ? ".ttf,.otf" : `${a.kind}/*`}
                onChange={(e) => {
                  const f = e.target.files?.[0];
                  if (f) void run(() => studio.replaceAsset(a.digest, f));
                }}
              />
            </label>
          </>
        )}
        {tab === "preview" && (a.kind === "audio" || a.kind === "video") && (
          <fieldset className="space-y-3 rounded-lg border border-border p-4">
            <legend>{t("Save a clip")}</legend>
            <label className="block">
              {t("Start (seconds)")}
              <Input
                type="number"
                min={0}
                max={end}
                step={0.1}
                value={start}
                onChange={(e) => setStart(Number(e.target.value))}
              />
            </label>
            <label className="block">
              {t("End (seconds)")}
              <Input
                type="number"
                min={start}
                max={(a.durationMs ?? 0) / 1000}
                step={0.1}
                value={end}
                onChange={(e) => setEnd(Number(e.target.value))}
              />
            </label>
            <Button
              disabled={busy || start < 0 || end <= start || end > (a.durationMs ?? 0) / 1000}
              onClick={() =>
                void run(() =>
                  apiFetch<LibraryAsset>(`/onair/assets/${a.digest}/trim`, {
                    method: "POST",
                    body: JSON.stringify({ start, end, name: t("{name} clip", { name }) }),
                  }),
                )
              }
            >
              {t("Save as new clip")}
            </Button>
          </fieldset>
        )}
        {tab === "usage" && (
          <>
            <section className="space-y-2">
              <h3 className="font-medium">{t("Used by")}</h3>
              {a.usedBy.map((u, i) => (
                <Link
                  key={i}
                  className="block text-primary"
                  to={
                    u.kind === "channel"
                      ? `/livetv?channel=${encodeURIComponent(u.id)}`
                      : `/livetv/looks?${u.kind === "reel" ? "reel" : "look"}=${encodeURIComponent(u.id)}${u.scene ? `&scene=${encodeURIComponent(u.scene)}` : ""}`
                  }
                >
                  {u.name}
                </Link>
              ))}
              <p className="text-sm text-fg-muted">
                {a.usedBy.length
                  ? tn(
                      a.usedBy.length,
                      "Used by {count} record. Remove it from there first.",
                      "Used by {count} records. Remove it from them first.",
                    )
                  : t("Not used yet")}
              </p>
            </section>
            <Button
              variant="outline"
              disabled={busy || a.usedBy.length > 0}
              onClick={() => void run(onDelete)}
            >
              {t("Delete asset")}
            </Button>
          </>
        )}
      </div>
    </Drawer>
  );
}
export default function Library() {
  return <LibraryGrid />;
}
