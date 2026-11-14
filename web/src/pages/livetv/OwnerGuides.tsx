import { useEffect, useMemo, useRef, useState } from "react";
import { Button, Checkbox, Input, Switch, toast } from "@braedonsaunders/appkit-ui";
import { ArrowDown, ArrowUp, GripVertical, Plus, Trash2 } from "lucide-react";
import { Drawer } from "../../components/Drawer";
import { t, tn } from "../../i18n";
import { apiFetch, errorMessage, uuid } from "../../lib/api";

type Group = { sourceId: string; name: string; count?: number; sourceName?: string };
type Channel = { id: string; name: string; number: string; sourceId?: string; group?: string };
type Guide = {
  id: string;
  name: string;
  groups: Group[];
  channels?: string[];
  hidden?: boolean;
  all?: boolean;
};
type Data = { guides: Guide[]; groups: Group[]; channels: Channel[] };
const key = (g: Group) => JSON.stringify([g.sourceId, g.name]);
const groupKey = (c: Channel) => key({ sourceId: c.sourceId ?? "", name: c.group ?? "" });

/** Only the visible rows mount, including when a provider has 10,000 channels. */
function WindowList({
  rows,
  render,
}: {
  rows: (Group | Channel)[];
  render: (row: Group | Channel, i: number) => React.ReactNode;
}) {
  const [top, setTop] = useState(0);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (ref.current) ref.current.scrollTop = 0;
    setTop(0);
  }, [rows]);
  const start = Math.max(0, Math.floor(top / 44) - 4);
  return (
    <div
      ref={ref}
      className="h-80 overflow-y-auto rounded-lg border border-border"
      onScroll={(e) => setTop(e.currentTarget.scrollTop)}
    >
      <div style={{ height: rows.length * 44, position: "relative" }}>
        <div style={{ position: "absolute", top: start * 44, left: 0, right: 0 }}>
          {rows.slice(start, start + 20).map((row, i) => (
            <div key={"id" in row ? row.id : key(row)} className="flex h-11 items-center px-3">
              {render(row, start + i)}
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}

export function OwnerGuides({ query = "" }: { query?: string }) {
  const [data, setData] = useState<Data | null>(null);
  const [draft, setDraft] = useState<Guide | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [search, setSearch] = useState("");
  const [mode, setMode] = useState<"groups" | "channels">("groups");
  const [combine, setCombine] = useState("");
  const anchor = useRef<number | null>(null);
  const dragged = useRef<number | null>(null);
  useEffect(() => {
    let live = true;
    void apiFetch<Data>("/livetv/guides")
      .then((v) => {
        if (live) setData(v);
      })
      .catch((e) => {
        if (live) setError(errorMessage(e));
      });
    return () => {
      live = false;
    };
  }, []);
  const channels = data?.channels ?? [];
  const grouped = useMemo(() => {
    const m = new Map<string, Channel[]>();
    for (const c of channels) {
      const k = groupKey(c);
      const list = m.get(k) ?? [];
      list.push(c);
      m.set(k, list);
    }
    return m;
  }, [channels]);
  const chosen = useMemo(() => {
    const ids = new Set(draft?.channels ?? []);
    if (draft?.all) for (const c of channels) ids.add(c.id);
    else
      for (const g of draft?.groups ?? []) for (const c of grouped.get(key(g)) ?? []) ids.add(c.id);
    return ids;
  }, [draft, channels, grouped]);
  const rows = useMemo(() => {
    const q = search.trim().toLocaleLowerCase();
    const matches = (c: Channel) =>
      `${c.name} ${c.number} ${c.group ?? ""}`.toLocaleLowerCase().includes(q);
    return mode === "channels"
      ? channels.filter(matches)
      : (data?.groups ?? []).filter(
          (g) =>
            `${g.name} ${g.sourceName ?? ""}`.toLocaleLowerCase().includes(q) ||
            (grouped.get(key(g)) ?? []).some(matches),
        );
  }, [search, mode, channels, data?.groups, grouped]);
  async function persist(guides: Guide[]) {
    setBusy(true);
    setError("");
    try {
      await apiFetch("/livetv/guides", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(guides),
      });
      setData((d) => d && { ...d, guides });
      setDraft(null);
      toast.success(t("Saved"));
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  }
  function edit(g: Guide) {
    setDraft(structuredClone(g));
    setSearch("");
    setCombine("");
    anchor.current = null;
  }
  function move(from: number, to: number) {
    if (!data || busy || from === to || to < 0 || to >= data.guides.length) return;
    const guides = [...data.guides];
    const [g] = guides.splice(from, 1);
    guides.splice(to, 0, g!);
    void persist(guides);
  }
  function select(indices: number[], checked: boolean) {
    if (!draft) return;
    const ids = new Set(chosen);
    for (const i of indices) {
      const row = rows[i];
      if (!row) continue;
      const cs = "id" in row ? [row] : (grouped.get(key(row)) ?? []);
      for (const c of cs) {
        if (checked) ids.add(c.id);
        else ids.delete(c.id);
      }
    }
    // Retain complete groups so newly imported channels join them automatically.
    const groups: Group[] = [];
    for (const g of data?.groups ?? []) {
      const members = grouped.get(key(g)) ?? [];
      if (members.length && members.every((c) => ids.has(c.id))) {
        groups.push({ sourceId: g.sourceId, name: g.name });
        for (const c of members) ids.delete(c.id);
      }
    }
    setDraft({ ...draft, all: false, groups, channels: [...ids] });
  }
  const count = (g: Guide) => {
    if (g.all) return channels.length;
    const ids = new Set(g.channels);
    for (const group of g.groups ?? []) for (const c of grouped.get(key(group)) ?? []) ids.add(c.id);
    return ids.size;
  };
  return (
    <div className="mx-auto max-w-4xl space-y-5 p-5 sm:p-8">
      <div className="flex items-center justify-between gap-4">
        <h2 className="text-lg font-semibold">{t("Guides")}</h2>
        <Button
          disabled={!data || busy}
          onClick={() => edit({ id: uuid(), name: t("New guide"), groups: [] })}
        >
          <Plus size={16} />
          {t("Add guide")}
        </Button>
      </div>
      <p className="text-sm text-fg-muted">
        {t("Organise channel groups into guides for your apps.")}
      </p>
      {error && !draft && (
        <p role="alert" className="text-danger">
          {error}
        </p>
      )}
      {!data && !error && <p>{t("Loading")}</p>}
      <div className="divide-y divide-border rounded-lg border border-border">
        {data?.guides.map(
          (g, index) =>
            g.name.toLocaleLowerCase().includes(query.toLocaleLowerCase()) && (
              <div
                key={g.id}
                draggable={!busy}
                onDragStart={() => {
                  dragged.current = index;
                }}
                onDragOver={(e) => e.preventDefault()}
                onDrop={(e) => {
                  e.preventDefault();
                  if (dragged.current !== null) move(dragged.current, index);
                  dragged.current = null;
                }}
                className="flex items-center gap-3 p-3"
              >
                <GripVertical size={16} className="shrink-0 text-fg-muted" aria-hidden />
                <button
                  className="min-w-0 flex-1 text-left"
                  disabled={busy}
                  onClick={() => edit(g)}
                >
                  <span className="block truncate font-medium">{g.name}</span>
                  <span className="text-xs text-fg-muted">
                    {tn(count(g), "{count} channel", "{count} channels")} ·{" "}
                    {g.hidden ? t("Hidden") : t("Shown")}
                  </span>
                </button>
                <Button
                  variant="ghost"
                  size="icon"
                  disabled={busy || index === 0}
                  aria-label={t("Move up")}
                  onClick={() => move(index, index - 1)}
                >
                  <ArrowUp size={15} />
                </Button>
                <Button
                  variant="ghost"
                  size="icon"
                  disabled={busy || index === data.guides.length - 1}
                  aria-label={t("Move down")}
                  onClick={() => move(index, index + 1)}
                >
                  <ArrowDown size={15} />
                </Button>
              </div>
            ),
        )}
      </div>
      <Drawer
        stacked
        open={!!draft}
        onClose={() => {
          if (!busy) setDraft(null);
        }}
        title={draft?.name || t("Add guide")}
        size="lg"
        footer={
          <Button
            disabled={busy || !draft?.name.trim() || !chosen.size}
            onClick={() => {
              if (!data || !draft) return;
              const exists = data.guides.some((g) => g.id === draft.id);
              void persist(
                exists
                  ? data.guides.map((g) => (g.id === draft.id ? draft : g))
                  : [...data.guides, draft],
              );
            }}
          >
            {t("Save")}
          </Button>
        }
      >
        {draft && (
          <fieldset disabled={busy} className="space-y-4">
            {error && (
              <p role="alert" className="text-danger">
                {error}
              </p>
            )}
            <label className="block space-y-1 text-sm">
              <span>{t("Name")}</span>
              <Input
                value={draft.name}
                maxLength={150}
                onChange={(e) => setDraft({ ...draft, name: e.currentTarget.value })}
              />
            </label>
            <label className="flex items-center gap-2 text-sm">
              <Switch
                checked={!draft.hidden}
                onChange={(e) => setDraft({ ...draft, hidden: !e.currentTarget.checked })}
              />
              {t("Show in apps")}
            </label>
            <label className="flex items-center gap-2 text-sm">
              <Checkbox
                checked={!!draft.all}
                onChange={(e) => setDraft({ ...draft, all: e.currentTarget.checked })}
              />
              {t("All channels")}
            </label>
            <Input
              type="search"
              aria-label={t("Search channels")}
              placeholder={t("Find a channel or group…")}
              value={search}
              onChange={(e) => {
                setSearch(e.currentTarget.value);
                anchor.current = null;
              }}
            />
            <div className="flex flex-wrap items-center gap-2">
              <Button
                size="sm"
                variant={mode === "groups" ? "secondary" : "ghost"}
                onClick={() => {
                  setMode("groups");
                  anchor.current = null;
                }}
              >
                {t("Groups")}
              </Button>
              <Button
                size="sm"
                variant={mode === "channels" ? "secondary" : "ghost"}
                onClick={() => {
                  setMode("channels");
                  anchor.current = null;
                }}
              >
                {t("Channels")}
              </Button>
              <span className="ml-auto text-sm text-fg-muted">
                {tn(chosen.size, "{count} selected", "{count} selected")}
              </span>
            </div>
            <div className="flex items-center gap-2">
              <Button
                size="sm"
                variant="ghost"
                onClick={() =>
                  select(
                    rows.map((_, i) => i),
                    true,
                  )
                }
              >
                {t("Select all results")}
              </Button>
              <Button
                size="sm"
                variant="ghost"
                onClick={() =>
                  select(
                    rows.map((_, i) => i),
                    false,
                  )
                }
              >
                {t("Clear selection")}
              </Button>
              <span className="ml-auto text-xs text-fg-muted">
                {tn(rows.length, "{count} result", "{count} results")}
              </span>
            </div>
            <WindowList
              rows={rows}
              render={(row, i) => {
                const cs = "id" in row ? [row] : (grouped.get(key(row)) ?? []);
                const n = cs.reduce((n, c) => n + Number(chosen.has(c.id)), 0);
                return (
                  <label
                    className="flex w-full min-w-0 items-center gap-3 text-sm"
                    onClickCapture={(e) => {
                      if (e.shiftKey) {
                        e.preventDefault();
                        e.stopPropagation();
                        const start = anchor.current ?? i;
                        select(
                          Array.from(
                            { length: Math.abs(start - i) + 1 },
                            (_, j) => Math.min(start, i) + j,
                          ),
                          n !== cs.length,
                        );
                      } else anchor.current = i;
                    }}
                  >
                    <Checkbox
                      checked={cs.length > 0 && n === cs.length}
                      aria-checked={n > 0 && n < cs.length ? "mixed" : n === cs.length}
                      onChange={(e) => select([i], e.currentTarget.checked)}
                    />
                    <span className="min-w-0 flex-1 truncate">
                      {"id" in row ? `${row.number} · ${row.name}` : row.name || t("Ungrouped")}
                    </span>
                    <span className="shrink-0 text-xs text-fg-muted">
                      {"id" in row ? row.group : `${n} / ${cs.length}`}
                    </span>
                  </label>
                );
              }}
            />
            <div className="flex gap-2 border-t border-border pt-4">
              <select
                aria-label={t("Combine into")}
                className="min-w-0 flex-1 rounded-md border border-border bg-surface p-2"
                value={combine}
                onChange={(e) => setCombine(e.currentTarget.value)}
              >
                <option value="">{t("Choose a guide")}</option>
                {data?.guides
                  .filter((g) => g.id !== draft.id)
                  .map((g) => (
                    <option key={g.id} value={g.id}>
                      {g.name}
                    </option>
                  ))}
              </select>
              <Button
                variant="secondary"
                disabled={!combine}
                onClick={() => {
                  if (!data) return;
                  void persist(
                    data.guides
                      .filter((g) => g.id !== draft.id)
                      .map((g) =>
                        g.id === combine
                          ? {
                              ...g,
                              all: g.all || draft.all,
                              groups: [...(g.groups ?? []), ...draft.groups],
                              channels: [
                                ...new Set([...(g.channels ?? []), ...(draft.channels ?? [])]),
                              ],
                            }
                          : g,
                      ),
                  );
                }}
              >
                {t("Combine into")}
              </Button>
            </div>
            {data?.guides.some((g) => g.id === draft.id) && (
              <Button
                variant="ghost"
                onClick={() => void persist(data.guides.filter((g) => g.id !== draft.id))}
              >
                <Trash2 size={15} />
                {t("Delete")}
              </Button>
            )}
          </fieldset>
        )}
      </Drawer>
    </div>
  );
}
