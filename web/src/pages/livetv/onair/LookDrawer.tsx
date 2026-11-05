import { api } from "../../../lib/api";
import type { EtherUser } from "../../../lib/types";
import { styleCopy } from "./naming";
import { MomentVideoPicker } from "./MomentVideoPicker";
import { useUnsavedGuard } from "../../../components/SaveBar";
import { useEffect, useRef, useState } from "react";
import { Badge, Button, Input, Popover, confirmDialog, toast } from "@braedonsaunders/appkit-ui";
import { MoreHorizontal, Undo2, Redo2 } from "lucide-react";
import { Drawer } from "../../../components/Drawer";
import { SubtabNav } from "../../../components/layout";
import { ToolbarPicker } from "../../../components/ToolbarPicker";
import { t } from "../../../i18n";
import { tv, type ChannelSummary } from "../model";
import { onair, type Look, type Bindings, type Moment } from "./model";
import { LookStage } from "./Stage";
import { DesignPanel } from "./DesignPanel";
import { DescribeLook } from "./DescribeLook";
import { LayersPanel } from "./LayersPanel";
import { StyleScreensEditor } from "./StyleScreens";
import { AssetsPanel } from "./AssetsPanel";
export function LookDrawer({
  look,
  onClose,
  onSaved,
  initialTab = "describe",
  stacked,
}: {
  look: Look;
  onClose: () => void;
  onSaved: (l?: Look) => void;
  initialTab?: string;
  stacked?: boolean;
}) {
  const [draft, setDraft] = useState(look),
    [saved, setSaved] = useState(look),
    [tab, setTab] = useState(initialTab),
    [menu, setMenu] = useState(false),
    [moment, setMoment] = useState<Moment>("ident"),
    [layer, setLayer] = useState("Title"),
    [channel, setChannel] = useState(look.channelIds[0] || ""),
    [at, setAt] = useState("now"),
    [custom, setCustom] = useState(() =>
      new Date(Date.now() - new Date().getTimezoneOffset() * 60000).toISOString().slice(0, 16),
    ),
    [viewer, setViewer] = useState(""),
    [channels, setChannels] = useState<ChannelSummary[]>([]),
    [bindings, setBindings] = useState<Bindings>({}),
    [render, setRender] = useState(look.render?.done ?? 7),
    [saving, setSaving] = useState(false);
  const [viewers, setViewers] = useState<EtherUser[]>([]);
  useEffect(() => {
    void api
      .users()
      .then((users) => {
        setViewers(users);
        setViewer(String(users.find((u) => u.isOwner)?.id ?? users[0]?.id ?? ""));
      })
      .catch(() => {});
  }, []);
  const [useOpen, setUseOpen] = useState(false);
  const [useChannels, setUseChannels] = useState<string[]>(look.channelIds);
  const history = useRef<Look[]>([]),
    future = useRef<Look[]>([]);
  const current = useRef(draft);
  current.current = draft;
  const change = (next: Look) => {
    history.current.push(current.current);
    future.current = [];
    current.current = next;
    setDraft(next);
  };
  const undo = () => {
    const next = history.current.pop();
    if (next) {
      future.current.push(current.current);
      current.current = next;
      setDraft(next);
    }
  };
  const redo = () => {
    const next = future.current.pop();
    if (next) {
      history.current.push(current.current);
      current.current = next;
      setDraft(next);
    }
  };
  const dirty = !draft.id || JSON.stringify(draft) !== JSON.stringify(saved);
  useUnsavedGuard(dirty);
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (saving) return;
      if ((e.metaKey || e.ctrlKey) && e.key === "z") {
        e.preventDefault();
        if (e.shiftKey) redo();
        else undo();
      }
    };
    document.addEventListener("keydown", handler);
    return () => document.removeEventListener("keydown", handler);
  }, [saving]);
  useEffect(() => {
    void tv
      .channelSummaries()
      .then((rows) => {
        setChannels(rows);
        setChannel((id) =>
          rows.some((c) => c.id === id)
            ? id
            : (rows.find((c) => c.kind === "virtual")?.id ?? rows[0]?.id ?? ""),
        );
      })
      .catch(() => {});
  }, []);
  const previewAt =
    at === "custom"
      ? Number.isNaN(Date.parse(custom))
        ? new Date().toISOString()
        : new Date(custom).toISOString()
      : at === "tonight"
        ? new Date(new Date().setHours(20, 0, 0, 0)).toISOString()
        : at === "morning"
          ? new Date(new Date().setHours(32, 0, 0, 0)).toISOString()
          : new Date().toISOString();
  useEffect(() => {
    void onair.bindings({ channel, at: previewAt, viewer, moment }).then(setBindings);
  }, [channel, at, custom, viewer, moment]);
  useEffect(() => {
    if (render >= 7) return;
    const id = setInterval(() => {
      void onair.look(draft.id).then((l) => setRender(l.render?.done ?? 7));
    }, 650);
    return () => clearInterval(id);
  }, [render, draft.id]);
  const close = async () => {
    if (
      !dirty ||
      (await confirmDialog({
        title: t("Discard unsaved changes?"),
        message: styleCopy("Your changes to this style haven’t been saved."),
        confirmLabel: t("Discard"),
        tone: "danger",
      }))
    )
      onClose();
  };
  const save = async () => {
    setSaving(true);
    try {
      const result = await onair.saveLook(draft);
      setDraft(result);
      setSaved(result);
      setRender(0);
      onSaved(result);
    } catch {
      toast.error(styleCopy("Couldn’t save style"));
    } finally {
      setSaving(false);
    }
  };
  const action = (label: string, fn: () => void) => (
    <button
      disabled={!saved.id}
      className="block w-full rounded-md px-3 py-2 text-left text-sm hover:bg-surface-hover disabled:opacity-50"
      onClick={() => {
        setMenu(false);
        fn();
      }}
    >
      {label}
    </button>
  );
  return (
    <Drawer
      stacked={stacked}
      open
      onClose={() => void close()}
      size="2xl"
      panelClassName="max-sm:[&>header]:flex-wrap max-sm:[&>header]:gap-2 max-sm:[&>header>div:first-child]:w-full max-sm:[&>header>div:last-child]:w-full max-sm:[&>header>div:last-child]:justify-end"
      title={
        <Input
          aria-label={styleCopy("Style name")}
          className="w-full sm:max-w-48 border-transparent bg-transparent font-semibold"
          value={draft.name}
          onChange={(e) => change({ ...draft, name: e.target.value })}
        />
      }
      bodyClassName="flex min-h-0 flex-1 flex-col overflow-y-auto p-0 lg:overflow-hidden"
      headerActions={
        <div className="flex flex-wrap items-center gap-1 sm:gap-2">
          {dirty ? <Badge>{t("Unsaved")}</Badge> : null}
          {render < 7 ? <Badge>{t("Rendering {done} of 7", { done: render })}</Badge> : null}
          <Button variant="ghost" size="sm" onClick={() => setDraft(saved)}>
            {t("Discard")}
          </Button>
          <Button size="sm" disabled={saving} onClick={() => void save()}>
            {styleCopy("Save style")}
          </Button>
          <Popover
            open={menu}
            onOpenChange={setMenu}
            align="end"
            className="w-52 p-1"
            trigger={
              <Button
                variant="ghost"
                size="sm"
                aria-label={styleCopy("Style actions")}
                onClick={() => setMenu(!menu)}
              >
                <MoreHorizontal size={16} />
              </Button>
            }
          >
            {action(t("Use on channels…"), () => setUseOpen(true))}
            {action(
              t("Duplicate"),
              () =>
                void onair.duplicateLook(saved.id).then((l) => {
                  onSaved(l);
                  onClose();
                }),
            )}
            {action(
              t("Delete"),
              () =>
                void (async () => {
                  if (
                    await confirmDialog({
                      title: t("Delete {name}?", { name: draft.name }),
                      message: styleCopy(
                        "Channels using this style will lose their on-air graphics.",
                      ),
                      confirmLabel: t("Delete"),
                      tone: "danger",
                    })
                  ) {
                    await onair.deleteLook(saved.id);
                    onSaved();
                    onClose();
                  }
                })(),
            )}
          </Popover>
        </div>
      }
    >
      <div inert={saving} className="grid min-h-0 flex-1 grid-cols-1 lg:grid-cols-3">
        <aside className="order-2 min-w-0 border-t border-border bg-surface lg:order-1 lg:overflow-y-auto lg:border-r lg:border-t-0">
          <div className="sticky top-0 z-10 bg-surface px-5 pt-3">
            <SubtabNav
              active={tab}
              onSelect={setTab}
              ariaLabel={styleCopy("Style inspector")}
              tabs={["describe", "design", "layers", "screens", "assets"].map((key) => ({
                key,
                label: t(
                  {
                    describe: "Describe",
                    design: "Design",
                    layers: "Layers",
                    screens: "Screens",
                    assets: "Assets",
                  }[key]!,
                ),
              }))}
            />
          </div>
          <div className="p-5">
            {tab === "design" ? (
              <div className="space-y-5">
                <DesignPanel look={draft} onChange={change} />
                <MomentVideoPicker look={draft} moment={moment} onChange={change} />
              </div>
            ) : tab === "describe" ? (
              <DescribeLook look={draft} onChange={change} />
            ) : tab === "layers" ? (
              <LayersPanel
                moment={moment}
                selected={layer}
                onSelect={setLayer}
                look={draft}
                onChange={change}
              />
            ) : tab === "screens" ? (
              <StyleScreensEditor look={draft} onChange={change} />
            ) : (
              <AssetsPanel />
            )}
          </div>
        </aside>
        <section className="order-1 min-w-0 bg-bg-subtle p-3 sm:p-6 lg:order-2 lg:col-span-2 lg:overflow-y-auto">
          <div className="mb-5 flex flex-wrap items-center gap-2 text-xs text-fg-muted">
            <span>{t("Preview on")}</span>
            <ToolbarPicker
              ariaLabel={t("Preview on")}
              className="max-w-full"
              value={channel}
              onChange={setChannel}
              options={
                channels.length
                  ? channels.map((c) => ({ value: c.id, label: c.name }))
                  : [{ value: "v-movie-night", label: t("Movie Night") }]
              }
            />
            <span>{t("at")}</span>
            <ToolbarPicker
              ariaLabel={t("Preview time")}
              value={at}
              onChange={setAt}
              options={[
                { value: "now", label: t("Now") },
                { value: "tonight", label: t("Tonight") },
                { value: "morning", label: t("Tomorrow morning") },
                { value: "custom", label: t("Custom") },
              ]}
            />
            {at === "custom" ? (
              <Input
                type="datetime-local"
                aria-label={t("Custom time")}
                value={custom}
                onChange={(e) => setCustom(e.target.value)}
              />
            ) : null}
            {draft.family === "personal" ? (
              <>
                <span>{t("Preview as")}</span>
                <ToolbarPicker
                  ariaLabel={t("Preview as")}
                  value={viewer}
                  onChange={setViewer}
                  options={viewers.map((v) => ({
                    value: String(v.id),
                    label: v.title || v.username,
                  }))}
                />
              </>
            ) : null}
            <div className="ml-auto flex">
              <Button variant="ghost" size="sm" aria-label={t("Undo")} onClick={undo}>
                <Undo2 size={14} />
              </Button>
              <Button variant="ghost" size="sm" aria-label={t("Redo")} onClick={redo}>
                <Redo2 size={14} />
              </Button>
            </div>
          </div>
          <p className="mb-3 text-sm text-fg-muted">
            {styleCopy("Style · {name}", { name: draft.name })}
          </p>
          <LookStage
            look={draft}
            compact={tab === "layers"}
            moment={moment}
            bindings={bindings}
            onMomentChange={setMoment}
            onSelectLayer={(s) => {
              setLayer(s);
              setTab("layers");
            }}
          />
        </section>
      </div>
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
                toast.success(styleCopy("Style applied"));
              }}
            >
              {styleCopy("Apply style")}
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
