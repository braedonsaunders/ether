import { styleCopy } from "./naming";
import { stylesName } from "./naming";
import { useEffect, useState } from "react";
import { Badge, Button, EmptyState } from "@braedonsaunders/appkit-ui";
import { Plus, Sparkles, Copy, Palette } from "lucide-react";
import { useLocation, useSearchParams } from "react-router-dom";
import { Drawer } from "../../../components/Drawer";
import { t, tn } from "../../../i18n";
import { onair, type Look, type Reel, type Family } from "./model";
import { LookTile } from "./Stage";
import { LookDrawer } from "./LookDrawer";
import { ReelDrawer } from "./ReelDrawer";
import { AssetsPanel } from "./AssetsPanel";
import { ScreensDefaults } from "./ScreensDefaults";
export function useLooks() {
  const location = useLocation();
  const [params] = useSearchParams();
  const [looks, setLooks] = useState<Look[]>([]),
    [reels, setReels] = useState<Reel[]>([]);
  const reload = () => {
    void onair.looks().then(setLooks);
    void onair.reels().then(setReels);
  };
  useEffect(reload, [location.pathname, params.get("channel"), params.get("look")]);
  return { looks, reels, reload };
}
export function LooksAside({
  looks,
  query,
  onSelect,
}: {
  looks: Look[];
  reels: Reel[];
  query: string;
  onSelect: (kind: string, id: string) => void;
}) {
  const families = useFamilies();
  return (
    <div className="space-y-5 p-3">
      {looks
        .filter((l) => l.name.toLowerCase().includes(query.toLowerCase()))
        .map((l) => (
          <button
            key={l.id}
            className="flex w-full items-center gap-3 rounded-lg p-2 text-left hover:bg-surface-hover"
            onClick={() => onSelect("look", l.id)}
          >
            <div className="w-24 shrink-0">
              <LookTile look={l} playOnHover={false} />
            </div>
            <div className="min-w-0">
              <p className="truncate text-sm font-medium">{l.name}</p>
              <p className="text-xs text-fg-muted">{familyName(l.family, families)}</p>
              <p className="text-xs text-fg-subtle">
                {tn(l.channelCount ?? 0, "Used by {count} channel", "Used by {count} channels")}
              </p>
              {l.pack ? <Badge>{t("From {pack}", { pack: l.pack.name })}</Badge> : null}
            </div>
          </button>
        ))}
    </div>
  );
}
function useFamilies() {
  const [families, setFamilies] = useState<Family[]>([]);
  useEffect(() => {
    void onair.families().then(setFamilies);
  }, []);
  return families;
}
function familyName(id: string, families: Family[]) {
  const name = families.find((f) => f.id === id)?.name ?? id;
  const labels: Record<string, string> = {
    Clean: t("Clean"),
    Kids: t("Kids"),
    "Late night": t("Late night"),
    Cinema: t("Cinema"),
    Retro: t("Retro"),
    Personal: t("Personal"),
    "Grunge 90": t("Grunge 90"),
    "Aqua 00": t("Aqua 00"),
    "Flat 10": t("Flat 10"),
  };
  return labels[name] ?? name;
}
export function LooksGallery({
  looks,
  onNew,
  onSelect,
}: {
  looks: Look[];
  onNew: () => void;
  onSelect: (id: string) => void;
}) {
  const [families, setFamilies] = useState<Family[]>([]);
  useEffect(() => {
    void onair.families().then(setFamilies);
  }, []);
  return (
    <div className="space-y-6 p-4 sm:p-6">
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-semibold">{stylesName()}</h2>
        <span className="text-xs text-fg-muted">
          {tn(looks.length, "{count} style", "{count} styles")}
        </span>
      </div>
      {!looks.length ? (
        <EmptyState
          icon={<Palette />}
          title={styleCopy("Give your channels a style")}
          description={t("Choose a family to get started.")}
          action={<Button onClick={onNew}>{styleCopy("New style")}</Button>}
        />
      ) : null}
      {!looks.length ? (
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-3">
          {families.map((f) => (
            <button
              key={f.id}
              className="overflow-hidden rounded-lg border border-border text-left"
              onClick={async () => {
                const saved = await onair.saveLook({
                  ...f.look,
                  name: styleCopy("Untitled style"),
                  channelIds: [],
                });
                onSelect(saved.id);
              }}
            >
              <LookTile look={f.look} />
              <span className="block p-3 text-sm">{familyName(f.id, families)}</span>
            </button>
          ))}
        </div>
      ) : null}
      <div className="grid grid-cols-1 gap-5 sm:grid-cols-2 xl:grid-cols-2">
        {looks.map((l) => (
          <button
            key={l.id}
            className="group min-w-0 overflow-hidden rounded-xl border border-border bg-surface text-left shadow-sm transition-colors hover:border-primary"
            onClick={() => onSelect(l.id)}
          >
            <LookTile look={l} />
            <div className="flex items-center justify-between gap-3 p-4">
              <div className="min-w-0 flex-1">
                <h3 className="truncate font-semibold tracking-tight" title={l.name}>
                  {l.name}
                </h3>
                <p className="mt-1 text-xs text-fg-muted">{familyName(l.family, families)}</p>
              </div>
              <span className="shrink-0 text-xs text-fg-subtle">
                {tn(l.channelCount ?? 0, "Used by {count} channel", "Used by {count} channels")}
              </span>
            </div>
          </button>
        ))}
        <button
          onClick={onNew}
          className="flex min-h-48 flex-col items-center justify-center gap-3 rounded-xl border border-dashed border-border bg-surface/50 text-fg-muted hover:border-primary hover:text-primary"
        >
          <Plus size={26} />
          <span className="text-sm font-medium">{styleCopy("New style")}</span>
        </button>
      </div>
      <ScreensDefaults />
      <section className="space-y-4">
        <h2 className="text-lg font-semibold">{t("Assets")}</h2>
        <AssetsPanel />
      </section>
    </div>
  );
}
export function LookSurfaces({ looks, reload }: { looks: Look[]; reload: () => void }) {
  const [params, setParams] = useSearchParams();
  const [families, setFamilies] = useState<Family[]>([]),
    [creating, setCreating] = useState<Look | null>(null),
    [initialTab, setInitialTab] = useState("describe");
  useEffect(() => {
    void onair.families().then(setFamilies);
  }, []);
  const lookId = params.get("look");
  const close = () => {
    setCreating(null);
    const next = new URLSearchParams(params);
    next.delete("look");
    next.delete("reel");
    setParams(next);
  };
  const choose = (l: Look, tab = "design") => {
    setInitialTab(tab);
    setCreating({
      ...structuredClone(l),
      id: "",
      name: styleCopy("Untitled style"),
      pack: undefined,
      channelIds: [],
    });
  };
  const selected = creating || looks.find((l) => l.id === lookId);
  return (
    <>
      {lookId === "new" && !creating ? (
        <Drawer
          open
          stacked={params.has("channel")}
          size="lg"
          title={styleCopy("New style")}
          onClose={close}
        >
          <div className="space-y-6">
            <button
              className="flex w-full items-center gap-4 rounded-xl border border-border bg-primary-subtle p-5 text-left"
              onClick={() => families[0] && choose(families[0].look, "describe")}
            >
              <Sparkles className="text-primary" size={24} />
              <span className="text-base font-semibold">{t("Describe it")}</span>
            </button>
            <section className="space-y-3">
              <h3 className="text-sm font-semibold">{t("Start from a family")}</h3>
              <div className="grid grid-cols-2 gap-3">
                {families.map((f) => (
                  <button
                    key={f.id}
                    className="overflow-hidden rounded-lg border border-border text-left hover:border-primary"
                    onClick={() => choose(f.look)}
                  >
                    <LookTile look={f.look} />
                    <span className="block p-3 text-sm font-medium">
                      {familyName(f.id, families)}
                    </span>
                  </button>
                ))}
              </div>
            </section>
            <section className="space-y-3">
              <h3 className="flex items-center gap-2 text-sm font-semibold">
                <Copy size={15} />
                {styleCopy("Duplicate a style")}
              </h3>
              <div className="flex flex-wrap gap-2">
                {looks.map((l) => (
                  <Button
                    key={l.id}
                    variant="outline"
                    size="sm"
                    className="max-w-full"
                    onClick={() => choose(l)}
                  >
                    <span className="truncate">{l.name}</span>
                  </Button>
                ))}
              </div>
            </section>
          </div>
        </Drawer>
      ) : null}
      {selected ? (
        <LookDrawer
          key={selected.id || "new"}
          look={selected}
          initialTab={initialTab}
          stacked={params.has("channel")}
          onClose={close}
          onSaved={(l) => {
            reload();
            if (l) {
              setCreating(null);
              setParams((p) => {
                p.set("look", l.id);
                return p;
              });
            }
          }}
        />
      ) : null}
      {params.has("reel") ? (
        <ReelDrawer open reelId={params.get("reel") || "new"} onClose={close} onSaved={reload} />
      ) : null}
    </>
  );
}
