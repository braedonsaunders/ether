import { sceneDisplayName } from "./scene-names";
import { useState, useEffect } from "react";
import { Button, Input, Popover, confirmDialog } from "@braedonsaunders/appkit-ui";
import {
  Plus,
  MoreHorizontal,
  Copy,
  Trash2,
  Eye,
  EyeOff,
  ChevronLeft,
  ChevronRight,
  Sparkles,
  LayoutTemplate,
  FilePlus2,
  Pencil,
} from "lucide-react";
import { t } from "../../../../i18n";
import {
  ROLES,
  studio,
  type Style,
  type StyleScene,
  type Template,
  type Role,
} from "../studio-model";
import type { SceneBindings } from "../scene-renderer";
import { Drawer } from "../../../../components/Drawer";
import { DescribeLook } from "../DescribeLook";
import { ScenePoster } from "../Stage";
export function SceneStrip(p: {
  style: Style;
  selected: string;
  bindings: SceneBindings;
  onSelect(id: string): void;
  onChange(style: Style): void;
}) {
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
  const [menu, setMenu] = useState(""),
    [renaming, setRenaming] = useState(""),
    [templatesOpen, setTemplatesOpen] = useState(false);
  const [ai, setAI] = useState("");
  const [templates, setTemplates] = useState<Template[]>([]);
  useEffect(() => {
    void studio.templates().then(setTemplates);
  }, []);
  const add = (role: Role, source?: StyleScene, templateName?: string) => {
    const id = crypto.randomUUID();
    const scene: StyleScene = source
      ? {
          ...structuredClone(source),
          id,
          role,
          name: templateName
            ? t("{template} · {role}", { template: templateName, role: roleNames[role] })
            : t("{name} copy", { name: source.name === role ? roleNames[role] : source.name }),
        }
      : {
          id,
          role,
          name: t("New {role} scene", { role: roleNames[role] }),
          enabled: true,
          weight: 1,
          sounds: [],
          document: {
            v: 1,
            moment: role,
            duration: p.style.roles[role]?.duration ?? 8,
            poster: 3,
            layers: [],
          },
        };
    scene.document.duration = p.style.roles[role]?.duration ?? 8;
    p.onChange({ ...p.style, scenes: [...p.style.scenes, scene] });
    p.onSelect(id);
    return id;
  };
  const change = (scene: StyleScene) =>
    p.onChange({ ...p.style, scenes: p.style.scenes.map((s) => (s.id === scene.id ? scene : s)) });
  const duration = (role: Role, seconds: number) =>
    p.onChange({
      ...p.style,
      roles: { ...p.style.roles, [role]: { duration: seconds } },
      scenes: p.style.scenes.map((s) =>
        s.role === role ? { ...s, document: { ...s.document, duration: seconds } } : s,
      ),
    });
  const name = (scene: StyleScene) => sceneDisplayName(p.style, scene);
  return (
    <div
      className="flex h-[156px] shrink-0 gap-0 overflow-x-auto overflow-y-hidden border-t border-border bg-surface px-2 py-2"
      aria-label={t("Scenes")}
    >
      {ai && (
        <Drawer open stacked size="lg" title={t("Describe a scene")} onClose={() => setAI("")}>
          <DescribeLook look={p.style} sceneId={ai} onChange={p.onChange} />
        </Drawer>
      )}
      {ROLES.map((role) => (
        <section
          key={role}
          className="shrink-0 border-r border-border px-3 last:border-r-0"
          aria-label={roleNames[role]}
        >
          <header className="mb-2 flex h-5 items-center justify-between gap-3">
            <h3 className="whitespace-nowrap text-[11px] font-semibold text-fg-muted">
              {roleNames[role]}
            </h3>
            <div className="flex items-center gap-0.5">
              <button
                className="rounded p-0.5 text-fg-muted hover:bg-surface-hover disabled:opacity-25"
                disabled={(p.style.roles[role]?.duration ?? 8) <= 4}
                aria-label={t("Shorten {role}", { role: roleNames[role] })}
                onClick={() =>
                  duration(role, Math.max(4, (p.style.roles[role]?.duration ?? 8) - 4))
                }
              >
                <ChevronLeft size={12} />
              </button>
              <span className="w-7 text-center text-[10px] tabular-nums text-fg-muted">
                {t("{seconds}s", { seconds: p.style.roles[role]?.duration ?? 8 })}
              </span>
              <button
                className="rounded p-0.5 text-fg-muted hover:bg-surface-hover disabled:opacity-25"
                disabled={(p.style.roles[role]?.duration ?? 8) >= 28}
                aria-label={t("Lengthen {role}", { role: roleNames[role] })}
                onClick={() =>
                  duration(role, Math.min(28, (p.style.roles[role]?.duration ?? 8) + 4))
                }
              >
                <ChevronRight size={12} />
              </button>
            </div>
          </header>
          <div className="flex items-start gap-2">
            {p.style.scenes
              .filter((s) => s.role === role)
              .map((scene) => (
                <article
                  key={scene.id}
                  className={`group relative w-32 shrink-0 rounded-lg ${scene.id === p.selected ? "ring-2 ring-primary ring-offset-2 ring-offset-surface" : ""} ${!scene.enabled ? "opacity-45" : ""}`}
                >
                  <button
                    className="block w-full overflow-hidden rounded-lg border border-border bg-bg-subtle"
                    aria-label={t("Edit {name}", { name: name(scene) })}
                    onClick={() => p.onSelect(scene.id)}
                  >
                    <ScenePoster scene={scene} bindings={p.bindings} />
                  </button>
                  <div className="mt-1 flex min-w-0 items-center gap-1">
                    {renaming === scene.id ? (
                      <Input
                        autoFocus
                        className="h-6 text-xs"
                        value={scene.name}
                        aria-label={t("Scene name")}
                        onChange={(e) => change({ ...scene, name: e.target.value })}
                        onBlur={() => setRenaming("")}
                        onKeyDown={(e) => {
                          if (e.key === "Enter") setRenaming("");
                        }}
                      />
                    ) : (
                      <span
                        className="min-w-0 flex-1 truncate text-[11px] font-medium"
                        title={name(scene)}
                      >
                        {name(scene)}
                      </span>
                    )}
                    <Popover
                      open={menu === scene.id}
                      onOpenChange={(open) => setMenu(open ? scene.id : "")}
                      className="w-56 space-y-1 p-2"
                      trigger={
                        <Button
                          size="sm"
                          variant="ghost"
                          className="h-5 min-w-5 px-0"
                          aria-label={t("Scene actions for {name}", { name: name(scene) })}
                          onClick={() => setMenu(menu === scene.id ? "" : scene.id)}
                        >
                          <MoreHorizontal size={14} />
                        </Button>
                      }
                    >
                      <Button
                        size="sm"
                        variant="ghost"
                        className="w-full justify-start gap-2"
                        onClick={() => {
                          setRenaming(scene.id);
                          setMenu("");
                        }}
                      >
                        <Pencil size={14} />
                        {t("Rename")}
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        className="w-full justify-start gap-2"
                        onClick={() => {
                          add(role, scene);
                          setMenu("");
                        }}
                      >
                        <Copy size={14} />
                        {t("Duplicate")}
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        className="w-full justify-start gap-2"
                        onClick={() => change({ ...scene, enabled: !scene.enabled })}
                      >
                        {scene.enabled ? <EyeOff size={14} /> : <Eye size={14} />}{" "}
                        {scene.enabled ? t("Disable") : t("Enable")}
                      </Button>
                      <label className="block space-y-2 border-y border-border px-2 py-3 text-xs">
                        {t("Rotation weight")}
                        <input
                          className="block w-full accent-primary"
                          type="range"
                          min="1"
                          max="10"
                          step="1"
                          value={scene.weight}
                          aria-label={t("Rotation weight")}
                          onChange={(e) => change({ ...scene, weight: Number(e.target.value) })}
                        />
                        <span className="text-fg-muted">
                          {t("×{weight} in rotation", { weight: scene.weight })}
                        </span>
                      </label>
                      <Button
                        size="sm"
                        variant="ghost"
                        className="w-full justify-start gap-2 text-danger"
                        onClick={async () => {
                          setMenu("");
                          if (
                            await confirmDialog({
                              title: t("Delete scene?"),
                              message: t("This scene will be removed from the style."),
                              confirmLabel: t("Delete"),
                              tone: "danger",
                            })
                          )
                            p.onChange({
                              ...p.style,
                              scenes: p.style.scenes.filter((s) => s.id !== scene.id),
                            });
                        }}
                      >
                        <Trash2 size={14} />
                        {t("Delete")}
                      </Button>
                    </Popover>
                  </div>
                  <span className="block text-[9px] text-fg-muted">
                    {t("×{weight} in rotation", { weight: scene.weight })}
                  </span>
                </article>
              ))}
            <Popover
              open={menu === `add-${role}`}
              onOpenChange={(open) => {
                setMenu(open ? `add-${role}` : "");
                setTemplatesOpen(false);
              }}
              className="w-64 p-2"
              trigger={
                <button
                  className="flex h-[72px] w-12 shrink-0 items-center justify-center rounded-lg border border-dashed border-border text-fg-muted transition-colors hover:border-primary hover:bg-primary-subtle hover:text-primary"
                  aria-label={t("Add {role} scene", { role: roleNames[role] })}
                  onClick={() => {
                    setMenu(menu === `add-${role}` ? "" : `add-${role}`);
                    setTemplatesOpen(false);
                  }}
                >
                  <Plus size={18} />
                </button>
              }
            >
              <Button
                size="sm"
                variant="ghost"
                className="w-full justify-start gap-2"
                onClick={() => {
                  add(role);
                  setMenu("");
                }}
              >
                <FilePlus2 size={15} />
                {t("Blank scene")}
              </Button>
              <Button
                size="sm"
                variant="ghost"
                className="w-full justify-start gap-2"
                onClick={() => setTemplatesOpen(!templatesOpen)}
              >
                <LayoutTemplate size={15} />
                {t("From template")}
                <ChevronRight size={12} className="ml-auto" />
              </Button>
              {templatesOpen && (
                <div className="my-1 max-h-60 overflow-y-auto rounded bg-bg-subtle p-1">
                  {templates.flatMap((template) =>
                    template.style.scenes
                      .filter((s) => s.role === role)
                      .map((source) => (
                        <button
                          key={`${template.id}-${source.id}`}
                          className="block w-full rounded p-2 text-left text-xs hover:bg-surface-hover"
                          onClick={() => {
                            add(role, source, template.name);
                            setMenu("");
                          }}
                        >
                          {template.name}
                        </button>
                      )),
                  )}
                </div>
              )}
              <Button
                size="sm"
                variant="ghost"
                className="w-full justify-start gap-2"
                onClick={() => {
                  setAI(add(role));
                  setMenu("");
                }}
              >
                <Sparkles size={15} />
                {t("Describe with AI")}
              </Button>
            </Popover>
          </div>
        </section>
      ))}
    </div>
  );
}
