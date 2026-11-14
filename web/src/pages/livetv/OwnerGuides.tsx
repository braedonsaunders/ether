import { useState } from "react";
import { Button, Checkbox, Input, Switch, toast } from "@braedonsaunders/appkit-ui";
import { ArrowDown, ArrowUp, Plus, Trash2 } from "lucide-react";
import { Drawer } from "../../components/Drawer";
import { t } from "../../i18n";
import { apiFetch, errorMessage, uuid } from "../../lib/api";

type Group = { sourceId: string; name: string };
type Guide = { id: string; name: string; groups: Group[]; hidden?: boolean; all?: boolean };
type Data = { guides: Guide[]; groups: (Group & { count: number; sourceName?: string })[] };
const same = (a: Group, b: Group) => a.sourceId === b.sourceId && a.name === b.name;

/** Guide drafts are saved together so combining or reordering is atomic. */
export function OwnerGuides() {
  const [open, setOpen] = useState(false);
  const [data, setData] = useState<Data | null>(null);
  const [selected, setSelected] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [search, setSearch] = useState("");
  const active = data?.guides.find((g) => g.id === selected);
  const change = (patch: Partial<Guide>) =>
    setData(
      (d) =>
        d && { ...d, guides: d.guides.map((g) => (g.id === selected ? { ...g, ...patch } : g)) },
    );
  async function load() {
    setOpen(true);
    setData(null);
    setError("");
    try {
      const value = await apiFetch<Data>("/livetv/guides");
      setData(value);
      setSelected(value.guides[0]?.id ?? "");
    } catch (e) {
      setError(errorMessage(e));
    }
  }
  async function save() {
    if (!data) return;
    setBusy(true);
    setError("");
    try {
      await apiFetch("/livetv/guides", {
        headers: { "Content-Type": "application/json" },
        method: "PUT",
        body: JSON.stringify(data.guides),
      });
      toast.success(t("Saved"));
      setOpen(false);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  }
  function move(index: number, step: number) {
    setData((d) => {
      if (!d || index + step < 0 || index + step >= d.guides.length) return d;
      const guides = [...d.guides];
      [guides[index], guides[index + step]] = [guides[index + step]!, guides[index]!];
      return { ...d, guides };
    });
  }
  return (
    <>
      <Button variant="ghost" size="sm" onClick={() => void load()}>
        {t("Guides")}
      </Button>
      <Drawer
        open={open}
        onClose={() => {
          if (!busy) setOpen(false);
        }}
        title={t("Guides")}
        size="lg"
        description={t("Organise channel groups into guides for your apps.")}
        footer={
          <Button
            disabled={
              !data ||
              busy ||
              data.guides.some((g) => !g.name.trim() || (!g.all && !g.groups.length))
            }
            onClick={() => void save()}
          >
            {t("Save")}
          </Button>
        }
      >
        {error && (
          <p role="alert" className="text-danger">
            {error}
          </p>
        )}
        {!data && !error ? <p>{t("Loading")}</p> : null}
        {data && (
          <div className="space-y-5">
            <p className="text-sm text-fg-muted">
              {t(
                "Plex guide tabs depend on the app. Emby uses tags. Jellyfin shows standard programme categories.",
              )}
            </p>
            <div className="space-y-2">
              {data.guides.map((g, index) => (
                <div key={g.id} className="flex items-center gap-2">
                  <Button
                    className="min-w-0 flex-1 justify-start truncate"
                    variant={g.id === selected ? "secondary" : "ghost"}
                    onClick={() => setSelected(g.id)}
                  >
                    {g.name}
                  </Button>
                  <Button
                    variant="ghost"
                    size="icon"
                    disabled={index === 0}
                    aria-label={t("Move up")}
                    onClick={() => move(index, -1)}
                  >
                    <ArrowUp size={15} />
                  </Button>
                  <Button
                    variant="ghost"
                    size="icon"
                    disabled={index === data.guides.length - 1}
                    aria-label={t("Move down")}
                    onClick={() => move(index, 1)}
                  >
                    <ArrowDown size={15} />
                  </Button>
                </div>
              ))}
              <Button
                variant="ghost"
                onClick={() => {
                  const id = uuid();
                  setData({
                    ...data,
                    guides: [...data.guides, { id, name: t("New guide"), groups: [] }],
                  });
                  setSelected(id);
                }}
              >
                <Plus size={15} />
                {t("Add guide")}
              </Button>
            </div>
            {active && (
              <fieldset disabled={busy} className="space-y-4 border-t border-border pt-4">
                <label className="block space-y-1 text-sm">
                  <span>{t("Name")}</span>
                  <Input
                    value={active.name}
                    maxLength={150}
                    onChange={(e) => change({ name: e.currentTarget.value })}
                  />
                </label>
                <label className="flex items-center gap-2 text-sm">
                  <Switch
                    checked={!active.hidden}
                    onChange={(e) => change({ hidden: !e.currentTarget.checked })}
                  />
                  {t("Show in apps")}
                </label>
                <label className="flex items-center gap-2 text-sm">
                  <Checkbox
                    checked={!!active.all}
                    onChange={(e) => change({ all: e.currentTarget.checked })}
                  />
                  {t("All channels")}
                </label>
                {!active.all && (
                  <>
                    <Input
                      aria-label={t("Search groups")}
                      placeholder={t("Search groups")}
                      value={search}
                      onChange={(e) => setSearch(e.currentTarget.value)}
                    />
                    <div className="max-h-64 space-y-2 overflow-y-auto">
                      {data.groups
                        .filter((g) =>
                          g.name.toLocaleLowerCase().includes(search.toLocaleLowerCase()),
                        )
                        .map((g) => (
                          <label
                            key={JSON.stringify([g.sourceId, g.name])}
                            className="flex items-center gap-2 text-sm"
                          >
                            <Checkbox
                              checked={active.groups.some((a) => same(a, g))}
                              onChange={(e) =>
                                change({
                                  groups: e.currentTarget.checked
                                    ? [...active.groups, { sourceId: g.sourceId, name: g.name }]
                                    : active.groups.filter((a) => !same(a, g)),
                                })
                              }
                            />
                            <span>{g.name || t("Ungrouped")}</span>
 <span className="text-xs text-fg-muted">{g.sourceName}</span>
                            <span className="ml-auto text-fg-muted">{g.count}</span>
                          </label>
                        ))}
                    </div>
                  </>
                )}
                <label className="block space-y-1 text-sm">
                  <span>{t("Combine into")}</span>
                  <select
                    className="w-full rounded-md border border-border bg-surface p-2"
                    value=""
                    onChange={(e) => {
                      const target = e.currentTarget.value;
                      if (!target) return;
                      setData({
                        ...data,
                        guides: data.guides
                          .filter((g) => g.id !== active.id)
                          .map((g) =>
                            g.id === target
                              ? {
                                  ...g,
                                  all: g.all || active.all,
                                  groups: [
                                    ...g.groups,
                                    ...active.groups.filter(
                                      (a) => !g.groups.some((b) => same(a, b)),
                                    ),
                                  ],
                                }
                              : g,
                          ),
                      });
                      setSelected(target);
                    }}
                  >
                    <option value="">{t("Choose a guide")}</option>
                    {data.guides
                      .filter((g) => g.id !== active.id)
                      .map((g) => (
                        <option key={g.id} value={g.id}>
                          {g.name}
                        </option>
                      ))}
                  </select>
                </label>
                <Button
                  variant="ghost"
                  onClick={() => {
                    setData({ ...data, guides: data.guides.filter((g) => g.id !== active.id) });
                    setSelected(data.guides.find((g) => g.id !== active.id)?.id ?? "");
                  }}
                >
                  <Trash2 size={15} />
                  {t("Delete")}
                </Button>
              </fieldset>
            )}
          </div>
        )}
      </Drawer>
    </>
  );
}
