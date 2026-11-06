import { nameTemplateScenes } from "./editor/scene-names";
import { useEffect, useRef, useState } from "react";
import { Badge, Button, Popover } from "@braedonsaunders/appkit-ui";
import { useLocation, useSearchParams } from "react-router-dom";
import { Drawer } from "../../../components/Drawer";
import { t, tn } from "../../../i18n";
import { tv, type ChannelSummary } from "../model";
import { onair, type Reel, type Bindings } from "./model";
import { studio, type Style, type Template } from "./studio-model";
import { StyleThumbnail } from "./editor/RemixPanel";
import { LookDrawer } from "./LookDrawer";
import { ReelDrawer } from "./ReelDrawer";
import { ScreensDefaults } from "./ScreensDefaults";
import { MoreHorizontal, Copy, Download, Trash2, Upload, Play, ArrowUpRight } from "lucide-react";
import { apiBlob, errorMessage } from "../../../lib/api";
export function useLooks() {
  const location = useLocation(),
    [params] = useSearchParams();
  const [looks, setLooks] = useState<Style[]>([]),
    [reels, setReels] = useState<Reel[]>([]);
  const reload = () => {
    void studio.styles().then(setLooks);
    void onair.reels().then(setReels);
  };
  useEffect(reload, [location.pathname, params.get("channel"), params.get("look")]);
  return { looks, reels, reload };
}
function useTemplates() {
  const [templates, setTemplates] = useState<Template[]>([]);
  useEffect(() => {
    void studio.templates().then(setTemplates);
  }, []);
  return templates;
}
function useSlots(channel?: string) {
  const [b, setB] = useState<Bindings>({});
  useEffect(() => {
    let live = true;
    void (async () => {
      const channels = await tv.channelSummaries();
      const selected = channels.find((c) => c.id === channel) ?? channels[0];
      if (live && selected) setB({ channel: { name: selected.name } });
      const bindings = await onair.bindings({ channel: selected?.id });
      if (live) setB(bindings);
    })().catch(() => {});
    return () => {
      live = false;
    };
  }, [channel]);
  const slots: Record<string, string> = {
    "channel.name": b.channel?.name ?? "",
    "next.title": b.next?.title ?? "",
    "next.poster": b.next?.backdrop ?? "",
    "next.backdrop": b.next?.backdrop ?? "",
    "next.time": b.next?.time ?? "",
    "now.title": b.now?.title ?? "",
    "viewer.name": b.viewer?.name ?? "",
  };
  return Object.fromEntries(Object.entries(slots).filter(([, value]) => value.length > 0));
}
export function blankStyle(): Style {
  return {
    id: "",
    name: t("Untitled style"),
    template: "",
    brand: {
      palette: { background: "#101827", accent: "#65d7ee", text: "#ffffff", glow: "#b9a4ff" },
      type: { display: "inter", body: "inter" },
      tagline: "",
      energy: 0.5,
      textures: [],
      language: "en",
    },
    roles: { ident: { duration: 8 } },
    scenes: [
      {
        id: crypto.randomUUID(),
        name: t("Ident"),
        role: "ident",
        enabled: true,
        weight: 1,
        document: { v: 1, moment: "ident", duration: 8, poster: 4, layers: [] },
        sounds: [],
      },
    ],
    controls: [],
    channelIds: [],
  };
}
export function LooksAside({
  looks,
  query,
  onSelect,
}: {
  looks: Style[];
  reels: Reel[];
  query: string;
  onSelect(kind: string, id: string): void;
}) {
  const slots = useSlots();
  return (
    <div className="space-y-3 p-3">
      {looks
        .filter((l) => l.name.toLowerCase().includes(query.toLowerCase()))
        .map((l) => (
          <button
            key={l.id}
            className="w-full rounded-lg border border-border p-2 text-left"
            onClick={() => onSelect("look", l.id)}
          >
            <StyleThumbnail style={l} slots={slots} />
            <p className="truncate text-sm">{l.name}</p>
            <p className="text-xs text-fg-muted">
              {tn(
                l.channelCount ?? l.channelIds.length,
                "Used by {count} channel",
                "Used by {count} channels",
              )}
            </p>
          </button>
        ))}
    </div>
  );
}
export function LooksGallery({
  looks,
  onSelect,
}: {
  looks: Style[];
  onNew(): void;
  onSelect(id: string): void;
}) {
  const templates = useTemplates(),
    slots = useSlots(),
    [working, setWorking] = useState(false),
    [error, setError] = useState(""),
    [deleted, setDeleted] = useState<string[]>([]),
    [deleting, setDeleting] = useState<Style | null>(null),
    [channels, setChannels] = useState<ChannelSummary[]>([]),
    [menu, setMenu] = useState<string | null>(null),
    file = useRef<HTMLInputElement>(null);
  useEffect(() => {
    void tv.channelSummaries().then(setChannels);
  }, []);
  const create = async (style: Style) => {
    setWorking(true);
    setError("");
    try {
      const saved = await studio.createStyle(
        nameTemplateScenes({ ...structuredClone(style), id: "", channelIds: [] }),
      );
      onSelect(saved.id);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setWorking(false);
    }
  };
  const exportPack = async (style: Style) => {
    setMenu(null);
    try {
      const blob = await apiBlob(studio.exportPack(style.id));
      const url = URL.createObjectURL(blob),
        a = document.createElement("a");
      a.href = url;
      a.download = style.name + ".etherpkg";
      a.click();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
    } catch (e) {
      setError(errorMessage(e));
    }
  };
  return (
    <div className="mx-auto max-w-[1600px] space-y-12 p-4 sm:p-8">
      {error ? (
        <p role="alert" className="rounded-lg bg-danger-subtle p-4 text-danger">
          {error}
        </p>
      ) : null}
      <section className="space-y-6">
        <div>
          <h2 className="text-xl font-semibold tracking-tight">{t("Your styles")}</h2>
          <p className="mt-2 text-sm text-fg-muted">
            {t("Your channel identities, ready for their next moment on air.")}
          </p>
        </div>
        <div className="grid gap-6 sm:grid-cols-2 xl:grid-cols-3">
          {looks
            .filter((l) => !deleted.includes(l.id))
            .map((l) => (
              <article
                key={l.id}
                className="overflow-hidden rounded-2xl border border-border bg-surface shadow-sm"
              >
                <button className="block w-full text-left" onClick={() => onSelect(l.id)}>
                  <StyleThumbnail style={l} slots={slots} />
                </button>
                <div className="space-y-3 p-4">
                  <div className="flex items-start justify-between gap-4">
                    <button
                      className="min-w-0 text-left font-semibold hover:text-primary"
                      onClick={() => onSelect(l.id)}
                    >
                      {l.name}
                    </button>
                    <Popover
                      open={menu === l.id}
                      onOpenChange={(open) => setMenu(open ? l.id : null)}
                      align="end"
                      trigger={
                        <Button
                          size="sm"
                          variant="ghost"
                          aria-label={t("Actions for {name}", { name: l.name })}
                          onClick={() => setMenu(menu === l.id ? null : l.id)}
                        >
                          <MoreHorizontal size={18} />
                        </Button>
                      }
                      className="w-48 space-y-1 p-2"
                    >
                      <Button
                        variant="ghost"
                        className="w-full justify-start"
                        disabled={working}
                        onClick={() => {
                          setMenu(null);
                          void create({ ...l, name: t("{name} copy", { name: l.name }) });
                        }}
                      >
                        <Copy size={16} />
                        {t("Duplicate")}
                      </Button>
                      <Button
                        variant="ghost"
                        className="w-full justify-start"
                        onClick={() => void exportPack(l)}
                      >
                        <Download size={16} />
                        {t("Export pack")}
                      </Button>
                      <Button
                        variant="ghost"
                        className="w-full justify-start text-danger"
                        onClick={() => {
                          setMenu(null);
                          setDeleting(l);
                        }}
                      >
                        <Trash2 size={16} />
                        {t("Delete")}
                      </Button>
                    </Popover>
                  </div>
                  <div className="flex flex-wrap items-center gap-2">
                    <span className="text-xs text-fg-muted">
                      {tn(
                        l.channelCount ?? l.channelIds.length,
                        "Used by {count} channel",
                        "Used by {count} channels",
                      )}
                    </span>
                    {channels.some(
                      (c) => c.enabled && (c.look?.lookId === l.id || l.channelIds.includes(c.id)),
                    ) ? (
                      <Badge>
                        <span className="mr-1 inline-block h-1.5 w-1.5 rounded-full bg-success" />
                        {t("On air")}
                      </Badge>
                    ) : null}
                    {l.pack ? <Badge>{t("From {pack}", { pack: l.pack.name })}</Badge> : null}
                  </div>
                </div>
              </article>
            ))}
        </div>
      </section>
      <section className="space-y-6">
        <div className="flex flex-wrap items-start justify-between gap-4">
          <div>
            <h2 className="text-xl font-semibold tracking-tight">{t("Make it yours")}</h2>
            <p className="mt-2 text-sm text-fg-muted">
              {t("Choose an era. Set the mood. Make every frame your own.")}
            </p>
          </div>
          <div className="flex gap-2">
            <Button variant="outline" disabled={working} onClick={() => file.current?.click()}>
              <Upload size={16} />
              {t("Import pack")}
            </Button>
            <Button disabled={working} onClick={() => void create(blankStyle())}>
              {t("Start from blank")}
            </Button>
          </div>
        </div>
        <input
          ref={file}
          hidden
          type="file"
          accept=".etherpkg"
          onChange={async (e) => {
            const selected = e.target.files?.[0];
            e.target.value = "";
            if (!selected) return;
            setWorking(true);
            try {
              const saved = await studio.importPack(selected);
              onSelect(saved.id);
            } catch (err) {
              setError(errorMessage(err));
            } finally {
              setWorking(false);
            }
          }}
        />
        <div className="grid gap-6 sm:grid-cols-2 xl:grid-cols-3">
          {templates.map((template) => (
            <article
              key={template.id}
              className="group overflow-hidden rounded-2xl border border-border bg-surface"
            >
              <div className="relative">
                <StyleThumbnail style={template.style} slots={slots} />
                <span className="pointer-events-none absolute bottom-3 left-3 flex items-center gap-2 rounded-full bg-black/70 px-3 py-1 text-xs text-white">
                  <Play size={12} />
                  {t("Hover to play")}
                </span>
              </div>
              <div className="space-y-4 p-5">
                <div className="flex flex-wrap gap-2">
                  {templateTags(template.id, template.tags).map((tag) => (
                    <Badge key={tag}>{t(tag)}</Badge>
                  ))}
                </div>
                <div>
                  <h3 className="text-lg font-semibold">{template.name}</h3>
                  <p className="mt-2 min-h-12 text-sm leading-relaxed text-fg-muted">
                    {templateBlurb(template.id, template.blurb)}
                  </p>
                </div>
                <Button
                  variant="outline"
                  className="w-full justify-between"
                  disabled={working}
                  onClick={() =>
                    void create({
                      ...template.style,
                      brand: {
                        ...template.style.brand,
                        tagline: template.style.brand.tagline || templateTagline(template.id),
                      },
                    })
                  }
                >
                  {t("Use template")}
                  <ArrowUpRight size={16} />
                </Button>
              </div>
            </article>
          ))}
        </div>
      </section>
      <ScreensDefaults />
      <Drawer
        open={!!deleting}
        size="sm"
        title={t("Delete style")}
        onClose={() => setDeleting(null)}
      >
        <div className="space-y-4">
          <p>
            {t("Delete {name}? Channels using this style must be reassigned first.", {
              name: deleting?.name ?? "",
            })}
          </p>
          <Button
            variant="destructive"
            disabled={working}
            onClick={async () => {
              if (!deleting) return;
              setWorking(true);
              try {
                await onair.deleteLook(deleting.id);
                setDeleted((v) => [...v, deleting.id]);
                setDeleting(null);
              } catch (e) {
                setError(errorMessage(e));
              } finally {
                setWorking(false);
              }
            }}
          >
            {t("Delete style")}
          </Button>
        </div>
      </Drawer>
    </div>
  );
}
function templateTagline(id: string) {
  const lines: Record<string, string> = {
    clean: t("A little more to discover"),
    kids: t("Totally gooey TV"),
    "late-night": t("Stay a little longer"),
    cinema: t("Every evening, a premiere"),
    retro: t("Stay tuned to the future"),
    grunge: t("Turn it up"),
    aqua: t("A brighter kind of television"),
    flat: t("Right here. Right now."),
    personal: t("This channel is made for you."),
  };
  return lines[id] ?? "";
}
function templateTags(id: string, other: string[]) {
  const tags: Record<string, string[]> = {
    clean: [t("Minimal"), t("Calm")],
    kids: [t("Playful"), t("Colourful")],
    "late-night": [t("After dark"), t("Atmospheric")],
    cinema: [t("Cinematic"), t("Elegant")],
    retro: [t("80s"), t("Synthwave")],
    grunge: [t("90s"), t("Rebellious")],
    aqua: [t("00s"), t("Optimistic")],
    flat: [t("10s"), t("Graphic")],
    personal: [t("Personal"), t("Welcoming")],
  };
  return tags[id] ?? other;
}
export function templateBlurb(id: string, other: string) {
  const blurbs: Record<string, string> = {
    clean: t("Crisp type, quiet motion and room for the programmes to shine."),
    kids: t("Bouncy lettering, gooey shapes and big colours for little adventures."),
    "late-night": t("Moonlit hills, drifting fog and a station that stays up with you."),
    cinema: t("Golden light, velvet shadows and a premiere every evening."),
    retro: t("Chrome type, a synthwave sun and a neon grid: Saturday-night 1984."),
    grunge: t("Torn edges, acid colours and restless type straight from the 90s."),
    aqua: t("Glassy panels, cool blue light and the bright promise of the 2000s."),
    flat: t("Bold colour blocks, sharp geometry and the confident rhythm of the 2010s."),
    personal: t("Aurora skies, a familiar greeting and a channel that feels like home."),
  };
  return blurbs[id] ?? other;
}
export function LookSurfaces({ looks, reload }: { looks: Style[]; reload(): void }) {
  const [params, setParams] = useSearchParams();
  const selected = looks.find((l) => l.id === params.get("look"));
  const close = () => {
    const next = new URLSearchParams(params);
    next.delete("look");
    next.delete("reel");
    setParams(next);
  };
  return (
    <>
      {params.get("look") === "new" ? (
        <Drawer open size="2xl" title={t("New style")} onClose={close}>
          <LooksGallery
            looks={looks}
            onNew={() => {}}
            onSelect={(id) => {
              reload();
              setParams((p) => {
                p.set("look", id);
                return p;
              });
            }}
          />
        </Drawer>
      ) : null}
      {selected ? (
        <LookDrawer
          key={selected.id}
          look={selected}
          initialTab="remix"
          stacked={params.has("channel")}
          onClose={close}
          onSaved={() => reload()}
        />
      ) : null}
      {params.has("reel") ? (
        <ReelDrawer open reelId={params.get("reel") || "new"} onClose={close} onSaved={reload} />
      ) : null}
    </>
  );
}
