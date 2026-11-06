import { sceneDisplayName } from "./editor/scene-names";
import { useEffect, useMemo, useRef, useState } from "react";
import {
  Tooltip,
  Select,
  Button,
  Input,
  Popover,
  confirmDialog,
  toast,
} from "@braedonsaunders/appkit-ui";
import { Drawer } from "../../../components/Drawer";
import { SubtabNav } from "../../../components/layout";
import { useUnsavedGuard } from "../../../components/SaveBar";
import { t } from "../../../i18n";
import { api, apiBlob } from "../../../lib/api";
import type { EtherUser } from "../../../lib/types";
import { tv, type ChannelSummary } from "../model";
import { onair, type Bindings } from "./model";
import { studio, type Style, type StyleScene } from "./studio-model";
import { LookStage, styleBindings } from "./Stage";
import { LayersPanel } from "./LayersPanel";
import { StyleScreensEditor } from "./StyleScreens";
import { BugControls } from "./BugControls";
import {
  Undo2,
  Redo2,
  MoreHorizontal,
  ChevronDown,
  ZoomIn,
  ZoomOut,
  Scan,
  Shield,
  Volume2,
  VolumeX,
  Play,
  Eye,
  GripHorizontal,
  Layers,
  SlidersHorizontal,
  MousePointer2,
} from "lucide-react";
import { ToolButton, InsertBar } from "./editor/InsertBar";
import { Inspector } from "./editor/Inspector";
import { SceneStrip } from "./editor/SceneStrip";
import { Timeline } from "./editor/Timeline";
import { useStageClock } from "./editor/clock";
import { usePreviewAudio } from "./editor/preview-audio";
import { RemixPanel } from "./editor/RemixPanel";
import { SequencePreview } from "./editor/SequencePreview";
export function LookDrawer({
  look,
  onClose,
  onSaved,
  initialTab = "remix",
  stacked,
}: {
  look: Style;
  onClose(): void;
  onSaved(style?: Style): void;
  initialTab?: string;
  stacked?: boolean;
}) {
  const [draft, setDraft] = useState(look),
    [saved, setSaved] = useState(look),
    [tab, setTab] = useState(initialTab),
    [mobilePanel, setMobilePanel] = useState("stage"),
    [rail, setRail] = useState(264),
    [timelineHeight, setTimelineHeight] = useState(212),
    [previewOpen, setPreviewOpen] = useState(false),
    [scenePicker, setScenePicker] = useState(false),
    [sceneId, setSceneId] = useState(
      look.scenes.find((s) => s.role === "ident")?.id ?? look.scenes[0]?.id ?? "",
    ),
    [selected, setSelected] = useState<string[]>([]),
    [channels, setChannels] = useState<ChannelSummary[]>([]),
    [channel, setChannel] = useState(look.channelIds[0] ?? ""),
    [viewers, setViewers] = useState<EtherUser[]>([]),
    [viewer, setViewer] = useState(""),
    [at, setAt] = useState(() =>
      new Date(Date.now() - new Date().getTimezoneOffset() * 60000).toISOString().slice(0, 16),
    ),
    [bound, setBound] = useState<Bindings>({}),
    [pen, setPen] = useState(false),
    [safe, setSafe] = useState(false),
    [zoom, setZoom] = useState(1),
    [saving, setSaving] = useState(false),
    [menu, setMenu] = useState(false),
    [useOpen, setUseOpen] = useState(false),
    [useChannels, setUseChannels] = useState(look.channelIds),
    [sequence, setSequence] = useState(false),
    [muted, setMuted] = useState(true),
    [mix, setMix] = useState<{ muted: string[]; solo: string }>({ muted: [], solo: "" });
  const resizeRail = useRef({ x: 0, width: 264 });
  const resizeTimeline = useRef({ y: 0, height: 212 });
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
  const gestureActive = useRef(false);
  const history = useRef<Style[]>([]),
    future = useRef<Style[]>([]),
    current = useRef(draft);
  current.current = draft;
  const change = (next: Style) => {
    if (!gestureActive.current) history.current.push(current.current);
    future.current = [];
    current.current = next;
    setDraft(next);
  };
  const undo = () => {
    const next = history.current.pop();
    if (next) {
      future.current.push(current.current);
      setDraft(next);
    }
  };
  const redo = () => {
    const next = future.current.pop();
    if (next) {
      history.current.push(current.current);
      setDraft(next);
    }
  };
  const scene = draft.scenes.find((s) => s.id === sceneId) ?? draft.scenes[0];
  const clock = useStageClock(scene?.document.duration ?? 8);
  useEffect(() => {
    clock.seek(scene?.document.poster ?? 0);
  }, [scene?.id]);
  const bindings = useMemo(() => styleBindings(draft, bound), [draft, bound]);
  const audioScene = useMemo(
    () =>
      scene
        ? {
            ...scene,
            sounds: scene.sounds.filter(
              (s) => !mix.muted.includes(s.id) && (!mix.solo || mix.solo === s.id),
            ),
          }
        : undefined,
    [scene, mix],
  );
  usePreviewAudio(audioScene, clock.time, clock.playing, muted);
  const updateScene = (next: StyleScene) =>
    change({ ...draft, scenes: draft.scenes.map((s) => (s.id === next.id ? next : s)) });
  const dirty = !draft.id || JSON.stringify(draft) !== JSON.stringify(saved);
  useUnsavedGuard(dirty);
  useEffect(() => {
    void tv.channelSummaries().then((rows) => {
      setChannels(rows);
      setChannel((id) => (rows.some((c) => c.id === id) ? id : (rows[0]?.id ?? "")));
    });
    void api.users().then((rows) => {
      setViewers(rows);
      setViewer(String(rows.find((u) => u.isOwner)?.id ?? rows[0]?.id ?? ""));
    });
  }, []);
  useEffect(() => {
    if (channel && Number.isFinite(Date.parse(at)))
      void onair
        .bindings({
          channel,
          at: new Date(at).toISOString(),
          viewer,
          moment: scene?.role ?? "ident",
        })
        .then(setBound);
  }, [channel, at, viewer, scene?.role]);
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      const editing = (e.target as HTMLElement).closest(
        'input,textarea,select,button,[contenteditable="true"]',
      );
      if (!editing && !e.metaKey && !e.ctrlKey && !e.altKey) {
        if (e.code === "Space") {
          e.preventDefault();
          clock.setPlaying((value) => !value);
          return;
        }
        if (e.key === "-" || e.key === "+" || e.key === "=") {
          e.preventDefault();
          setZoom((value) => Math.max(0.25, Math.min(3, value + (e.key === "-" ? -0.1 : 0.1))));
          return;
        }
      }

      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "z") {
        e.preventDefault();
        if (e.shiftKey) redo();
        else undo();
      }
    };
    document.addEventListener("keydown", handler);
    return () => document.removeEventListener("keydown", handler);
  }, []);
  const close = async () => {
    if (
      !dirty ||
      (await confirmDialog({
        title: t("Discard unsaved changes?"),
        message: t("Your changes to this style haven’t been saved."),
        confirmLabel: t("Discard"),
        tone: "danger",
      }))
    )
      onClose();
  };
  const save = async () => {
    setSaving(true);
    try {
      const result = draft.id ? await studio.saveStyle(draft) : await studio.createStyle(draft);
      setDraft(result);
      setSaved(result);
      onSaved(result);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : t("Couldn’t save style"));
    } finally {
      setSaving(false);
    }
  };
  return (
    <Drawer
      open
      stacked={stacked}
      size="2xl"
      initialFullscreen
      onClose={() => void close()}
      title={
        <div className="flex min-w-0 items-center gap-3">
          <Input
            className="w-36 border-transparent bg-transparent px-0 font-semibold focus:border-border"
            aria-label={t("Style name")}
            value={draft.name}
            onChange={(e) => change({ ...draft, name: e.target.value })}
          />
          <span className="hidden h-5 border-l border-border sm:block" />
          {scene && (
            <Popover
              open={scenePicker}
              onOpenChange={setScenePicker}
              className="max-h-96 w-72 overflow-y-auto p-1"
              trigger={
                <Button
                  size="sm"
                  variant="ghost"
                  className="inline-flex min-w-0 gap-2"
                  onClick={() => setScenePicker(!scenePicker)}
                >
                  <span className="text-fg-muted">{roleNames[scene.role]}</span>
                  <span className="hidden text-fg-muted sm:inline">·</span>
                  <span className="hidden max-w-48 truncate sm:inline">
                    {sceneDisplayName(draft, scene)}
                  </span>
                  <ChevronDown size={12} />
                </Button>
              }
            >
              {draft.scenes.map((item) => (
                <button
                  key={item.id}
                  className="flex w-full gap-3 rounded p-2 text-left text-xs hover:bg-surface-hover"
                  onClick={() => {
                    setSceneId(item.id);
                    setSelected([]);
                    setScenePicker(false);
                  }}
                >
                  <span className="w-24 shrink-0 text-fg-muted">{roleNames[item.role]}</span>
                  <span className="truncate">{sceneDisplayName(draft, item)}</span>
                </button>
              ))}
            </Popover>
          )}
        </div>
      }
      subtabs={
        <div className="flex h-10 shrink-0 items-center justify-between border-b border-border px-4">
          <SubtabNav
            active={tab}
            onSelect={setTab}
            ariaLabel={t("Style workspace")}
            tabs={[
              { key: "remix", label: t("Remix") },
              { key: "design", label: t("Design") },
              { key: "screens", label: t("Screens") },
            ]}
          />
          <span className="hidden text-[10px] text-fg-muted sm:block">
            {t("1920 × 1080 · Live television")}
          </span>
        </div>
      }
      panelClassName="max-sm:[&>header]:flex-wrap max-sm:[&>header>div:first-child]:w-full max-sm:[&>header>div:last-child]:w-full max-sm:[&>header>div:last-child]:justify-end"
      bodyClassName="flex min-h-0 flex-1 flex-col overflow-y-auto p-0 lg:overflow-hidden"
      headerActions={
        <div className="flex flex-wrap items-center gap-2">
          {dirty && (
            <Tooltip content={t("Unsaved changes")}>
              <span
                role="status"
                aria-label={t("Unsaved changes")}
                className="mx-1 h-2 w-2 rounded-full bg-amber-400"
              />
            </Tooltip>
          )}
          <ToolButton
            label={t("Undo")}
            shortcut="⌘Z"
            disabled={!history.current.length}
            onClick={undo}
          >
            <Undo2 size={16} />
          </ToolButton>
          <ToolButton
            label={t("Redo")}
            shortcut="⇧⌘Z"
            disabled={!future.current.length}
            onClick={redo}
          >
            <Redo2 size={16} />
          </ToolButton>
          <Button size="sm" disabled={saving} onClick={() => void save()}>
            {t("Save")}
          </Button>
          <Popover
            open={menu}
            onOpenChange={setMenu}
            trigger={
              <Button
                size="sm"
                variant="ghost"
                aria-label={t("Style actions")}
                onClick={() => setMenu(!menu)}
              >
                <MoreHorizontal size={18} />
              </Button>
            }
          >
            <div className="flex flex-col gap-2 p-2">
              <Button variant="ghost" onClick={() => setUseOpen(true)}>
                {t("Use on channels…")}
              </Button>
              <Button
                variant="ghost"
                onClick={() =>
                  void studio
                    .createStyle({ ...draft, id: "", name: t("{name} copy", { name: draft.name }) })
                    .then(onSaved)
                }
              >
                {t("Duplicate")}
              </Button>
              <Button
                variant="ghost"
                onClick={async () => {
                  try {
                    const blob = await apiBlob(studio.exportPack(saved.id));
                    const url = URL.createObjectURL(blob);
                    const link = document.createElement("a");
                    link.href = url;
                    link.download = `${draft.name}.etherstyle`;
                    link.click();
                    setTimeout(() => URL.revokeObjectURL(url), 1000);
                  } catch (e) {
                    toast.error(e instanceof Error ? e.message : t("Couldn’t export style"));
                  }
                }}
              >
                {t("Export pack")}
              </Button>
              <Button
                variant="ghost"
                onClick={async () => {
                  if (
                    await confirmDialog({
                      title: t("Delete {name}?", { name: draft.name }),
                      message: t("Channels using this style will lose their on-air graphics."),
                      confirmLabel: t("Delete"),
                      tone: "danger",
                    })
                  ) {
                    await onair.deleteLook(saved.id);
                    onSaved();
                    onClose();
                  }
                }}
              >
                {t("Delete")}
              </Button>
            </div>
          </Popover>
        </div>
      }
    >
      <div className="flex shrink-0 items-center justify-center gap-1 border-b border-border p-1 lg:hidden">
        {[
          { id: "stage", label: t("Canvas"), Icon: MousePointer2 },
          { id: "layers", label: tab === "remix" ? t("Remix") : t("Layers"), Icon: Layers },
          { id: "inspector", label: t("Inspector"), Icon: SlidersHorizontal },
        ]
          .filter((i) => tab === "design" || i.id !== "inspector")
          .map(({ id, label, Icon }) => (
            <Button
              key={id}
              size="sm"
              variant={mobilePanel === id ? "secondary" : "ghost"}
              onClick={() => setMobilePanel(id)}
            >
              <Icon size={14} />
              {label}
            </Button>
          ))}
      </div>
      <div
        inert={saving}
        className="flex min-h-0 flex-1 flex-col overflow-y-auto lg:grid lg:overflow-hidden"
        style={{
          gridTemplateColumns:
            tab === "design" ? `${rail}px minmax(0,1fr) 296px` : `${rail}px minmax(0,1fr)`,
        }}
      >
        <aside
          className={`${mobilePanel === "layers" ? "block" : "hidden"} relative min-h-0 overflow-y-auto border-r border-border bg-surface lg:block`}
        >
          <div className="p-4">
            {tab === "design" && scene ? (
              <LayersPanel
                document={scene.document}
                selected={selected}
                onSelect={(ids) => {
                  setSelected(ids);
                  clock.setPlaying(false);
                }}
                onChange={(document) => updateScene({ ...scene, document })}
              />
            ) : tab === "screens" ? (
              <div className="space-y-5">
                <StyleScreensEditor look={draft} onChange={change} />
                <BugControls
                  value={draft.bug ?? {}}
                  onChange={(bug) => change({ ...draft, bug })}
                />
              </div>
            ) : (
              <RemixPanel
                style={draft}
                onChange={change}
                onOpenScene={(id) => {
                  setSceneId(id);
                  setTab("design");
                  setMobilePanel("stage");
                }}
              />
            )}
          </div>
          <div
            role="separator"
            tabIndex={0}
            aria-label={t("Resize layers panel")}
            aria-orientation="vertical"
            aria-valuenow={rail}
            aria-valuemin={200}
            aria-valuemax={360}
            className="absolute inset-y-0 right-0 hidden w-1 cursor-col-resize hover:bg-primary/40 focus:bg-primary/40 lg:block"
            onPointerDown={(e) => {
              resizeRail.current = { x: e.clientX, width: rail };
              e.currentTarget.setPointerCapture(e.pointerId);
            }}
            onPointerMove={(e) => {
              if (e.currentTarget.hasPointerCapture(e.pointerId))
                setRail(
                  Math.max(
                    200,
                    Math.min(360, resizeRail.current.width + e.clientX - resizeRail.current.x),
                  ),
                );
            }}
            onKeyDown={(e) => {
              if (e.key === "ArrowLeft" || e.key === "ArrowRight") {
                e.preventDefault();
                setRail((w) =>
                  Math.max(200, Math.min(360, w + (e.key === "ArrowRight" ? 16 : -16))),
                );
              }
            }}
          />
        </aside>
        <main
          className={`${mobilePanel === "stage" ? "flex" : "hidden"} min-h-0 min-w-0 flex-col bg-bg-subtle lg:flex`}
        >
          {scene && (
            <div className="shrink-0 overflow-x-auto">
              <InsertBar
                scene={scene}
                onChange={updateScene}
                onSelect={setSelected}
                onPen={() => setPen(true)}
                onSelectTool={() => setPen(false)}
                pen={pen}
                bindings={bindings.slots}
              >
                <span className="mx-1 h-5 border-l border-border" />
                <ToolButton
                  label={t("Zoom out")}
                  shortcut="−"
                  onClick={() => setZoom((z) => Math.max(0.25, z - 0.1))}
                >
                  <ZoomOut size={15} />
                </ToolButton>
                <button
                  className="w-10 shrink-0 text-center text-[11px] tabular-nums text-fg-muted"
                  aria-label={t("Reset zoom")}
                  onClick={() => setZoom(1)}
                >
                  {Math.round(zoom * 100)}%
                </button>
                <ToolButton
                  label={t("Zoom in")}
                  shortcut="+"
                  onClick={() => setZoom((z) => Math.min(3, z + 0.1))}
                >
                  <ZoomIn size={15} />
                </ToolButton>
                <ToolButton label={t("Fit to viewport")} onClick={() => setZoom(1)}>
                  <Scan size={15} />
                </ToolButton>
                <ToolButton label={t("Safe areas")} active={safe} onClick={() => setSafe(!safe)}>
                  <Shield size={15} />
                </ToolButton>
                <ToolButton
                  label={muted ? t("Enable preview sound") : t("Mute preview sound")}
                  active={!muted}
                  onClick={() => setMuted(!muted)}
                >
                  {muted ? <VolumeX size={15} /> : <Volume2 size={15} />}
                </ToolButton>
                <Popover
                  open={previewOpen}
                  onOpenChange={setPreviewOpen}
                  className="w-72 space-y-4 p-4"
                  trigger={
                    <span>
                      <ToolButton
                        label={t("Preview as")}
                        onClick={() => setPreviewOpen(!previewOpen)}
                      >
                        <Eye size={15} />
                        <span className="hidden max-w-32 truncate text-[10px] min-[1500px]:inline">
                          {t("{channel} · {time} · {viewer}", {
                            channel: channels.find((c) => c.id === channel)?.name ?? "",
                            time: new Date(at).toLocaleTimeString(undefined, {
                              hour: "numeric",
                              minute: "2-digit",
                            }),
                            viewer: viewers.find((v) => String(v.id) === viewer)?.title ?? "",
                          })}
                        </span>
                      </ToolButton>
                    </span>
                  }
                >
                  <h3 className="text-sm font-semibold">{t("Preview as")}</h3>
                  <label className="block space-y-1 text-xs">
                    {t("Channel")}
                    <Select
                      aria-label={t("Preview channel")}
                      value={channel}
                      onChange={(e) => setChannel(e.target.value)}
                    >
                      {channels.map((c) => (
                        <option key={c.id} value={c.id}>
                          {c.name}
                        </option>
                      ))}
                    </Select>
                  </label>
                  <label className="block space-y-1 text-xs">
                    {t("Time")}
                    <Input
                      type="datetime-local"
                      value={at}
                      aria-label={t("Preview time")}
                      onChange={(e) => setAt(e.target.value)}
                    />
                  </label>
                  <label className="block space-y-1 text-xs">
                    {t("Viewer")}
                    <Select
                      aria-label={t("Preview viewer")}
                      value={viewer}
                      onChange={(e) => setViewer(e.target.value)}
                    >
                      {viewers.map((v) => (
                        <option key={v.id} value={String(v.id)}>
                          {v.title || v.username}
                        </option>
                      ))}
                    </Select>
                  </label>
                  <p className="text-xs text-fg-muted">
                    {t("{channel} · {time} · {viewer}", {
                      channel: channels.find((c) => c.id === channel)?.name ?? "",
                      time: new Date(at).toLocaleString(undefined, {
                        hour: "numeric",
                        minute: "2-digit",
                      }),
                      viewer: viewers.find((v) => String(v.id) === viewer)?.title ?? "",
                    })}
                  </p>
                </Popover>
                <Button
                  size="sm"
                  className="ml-auto h-8 shrink-0 gap-2"
                  aria-label={t("Play the sequence")}
                  disabled={!channel || !Number.isFinite(Date.parse(at))}
                  onClick={() => setSequence(true)}
                >
                  <Play size={13} fill="currentColor" />
                  <span className="hidden min-[1400px]:inline">{t("Play the sequence")}</span>
                </Button>
              </InsertBar>
            </div>
          )}
          {scene ? (
            <>
              <div className="relative min-h-[220px] flex-1 overflow-hidden lg:min-h-0">
                <LookStage
                  fit
                  onGestureStart={() => {
                    if (!gestureActive.current) {
                      history.current.push(current.current);
                      future.current = [];
                      gestureActive.current = true;
                    }
                  }}
                  onGestureEnd={() => {
                    gestureActive.current = false;
                  }}
                  sceneVideo={scene.video}
                  playing={clock.playing}
                  muted={muted}
                  pen={pen}
                  onPenDone={() => setPen(false)}
                  document={scene.document}
                  bindings={bindings}
                  time={clock.time}
                  selected={selected}
                  onSelect={(ids) => {
                    setSelected(ids);
                    if (ids.length) {
                      clock.setPlaying(false);
                      setTab("design");
                    }
                  }}
                  onChange={(document) => updateScene({ ...scene, document })}
                  safe={safe}
                  zoom={zoom}
                />
                <span className="pointer-events-none absolute bottom-2 left-3 text-[10px] text-fg-muted">
                  {t("{role} · {name}", {
                    role: roleNames[scene.role],
                    name: sceneDisplayName(draft, scene),
                  })}
                </span>
              </div>
              {tab === "design" && (
                <div
                  className="relative shrink-0 border-t border-border bg-surface"
                  style={{ height: timelineHeight }}
                >
                  <div
                    role="separator"
                    tabIndex={0}
                    aria-label={t("Resize timeline")}
                    aria-orientation="horizontal"
                    aria-valuenow={timelineHeight}
                    aria-valuemin={140}
                    aria-valuemax={400}
                    className="absolute -top-1 left-0 z-10 flex h-2 w-full cursor-row-resize justify-center hover:bg-primary/20 focus:bg-primary/20"
                    onPointerDown={(e) => {
                      resizeTimeline.current = { y: e.clientY, height: timelineHeight };
                      e.currentTarget.setPointerCapture(e.pointerId);
                    }}
                    onPointerMove={(e) => {
                      if (e.currentTarget.hasPointerCapture(e.pointerId))
                        setTimelineHeight(
                          Math.max(
                            140,
                            Math.min(
                              400,
                              resizeTimeline.current.height + resizeTimeline.current.y - e.clientY,
                            ),
                          ),
                        );
                    }}
                    onKeyDown={(e) => {
                      if (e.key === "ArrowUp" || e.key === "ArrowDown") {
                        e.preventDefault();
                        setTimelineHeight((h) =>
                          Math.max(140, Math.min(400, h + (e.key === "ArrowUp" ? 16 : -16))),
                        );
                      }
                    }}
                  >
                    <GripHorizontal size={14} className="text-fg-muted" />
                  </div>
                  <div className="h-full overflow-auto px-3 py-2">
                    <Timeline
                      scene={scene}
                      duration={scene.document.duration}
                      time={clock.time}
                      playing={clock.playing}
                      onPlayingChange={clock.setPlaying}
                      onTime={clock.seek}
                      loop={clock.loop}
                      onLoopChange={clock.setLoop}
                      rate={clock.rate}
                      onRateChange={clock.setRate}
                      selected={selected}
                      onSelect={setSelected}
                      onChange={updateScene}
                      onPreviewMixChange={(muted, solo) => setMix({ muted, solo })}
                    />
                  </div>
                </div>
              )}
            </>
          ) : (
            <div className="grid flex-1 place-items-center p-8 text-sm text-fg-muted">
              {t("Add a scene to start designing.")}
            </div>
          )}
          <SceneStrip
            style={draft}
            selected={scene?.id ?? ""}
            bindings={bindings}
            onSelect={(id) => {
              setSceneId(id);
              setSelected([]);
            }}
            onChange={change}
          />
        </main>
        {tab === "design" && scene && (
          <aside
            className={`${mobilePanel === "inspector" ? "block" : "hidden"} min-h-0 overflow-y-auto border-l border-border bg-surface p-4 lg:block`}
          >
            <Inspector
              bindings={bindings}
              time={clock.time}
              style={draft}
              scene={scene}
              selected={selected}
              onChange={change}
            />
          </aside>
        )}
      </div>
      {sequence && (
        <SequencePreview
          style={draft}
          channelId={channel}
          at={new Date(at).toISOString()}
          onClose={() => setSequence(false)}
        />
      )}
      {useOpen ? (
        <Drawer
          open
          size="md"
          title={t("Use on channels")}
          onClose={() => setUseOpen(false)}
          footer={
            <Button
              onClick={async () => {
                for (const id of new Set([...saved.channelIds, ...useChannels])) {
                  const c = await tv.channel(id);
                  await tv.saveChannel({
                    ...c,
                    look: useChannels.includes(id)
                      ? { ...c.look, lookId: saved.id, ident: true, upNext: true, offAir: true }
                      : c.look?.lookId === saved.id
                        ? undefined
                        : c.look,
                  });
                }
                setUseOpen(false);
                toast.success(t("Style applied"));
              }}
            >
              {t("Apply style")}
            </Button>
          }
        >
          <div className="space-y-3">
            {channels
              .filter((c) => c.kind === "virtual")
              .map((c) => (
                <label
                  key={c.id}
                  className="flex items-center gap-3 rounded-lg border border-border p-3 text-sm"
                >
                  <input
                    type="checkbox"
                    className="accent-primary"
                    checked={useChannels.includes(c.id)}
                    onChange={(e) =>
                      setUseChannels((s) =>
                        e.target.checked ? [...s, c.id] : s.filter((id) => id !== c.id),
                      )
                    }
                  />
                  {c.name}
                </label>
              ))}
          </div>
        </Drawer>
      ) : null}
    </Drawer>
  );
}
