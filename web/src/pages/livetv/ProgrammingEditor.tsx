import { useEffect, useState } from "react";
import { Badge, Button, Input, Select, Switch } from "@braedonsaunders/appkit-ui";
import { CalendarDays, Clock, Plus, Radio, Trash2 } from "lucide-react";
import { Drawer } from "../../components/Drawer";
import { Field, ChipGroup } from "../../components/fields";
import { TitleSearch } from "../../components/pickers";
import { SettingsRow } from "../../components/SettingsRow";
import { textOf, useRegistry } from "../../lib/extend";
import { api } from "../../lib/api";
import { blankSpec } from "../../lib/rules";
import { useDebouncedValue } from "../../lib/hooks";
import type { Rule } from "../../lib/contract";
import { t, tn } from "../../i18n";
import {
  Block,
  Channel,
  Pin,
  ProgramSource,
  blockLabel,
  minutesOfDay,
  timeOfDay,
  timeZones,
} from "./model";
import type { CollectionChoice } from "./ChannelStudio";
import { ProgrammePicker } from "./ProgrammePicker";
import { WeekGrid } from "./WeekGrid";
import { BreakPatternEditor, breakPatternPreset } from "./onair/BreakPatternEditor";
import { personalRecipe } from "./onair/defaults";
import { ChannelLook, OnAirAsset, Reel, onair } from "./onair/model";

function detectedBreaks(channel: Channel): number {
  const counts = new Map<number, number>();
  const items = (list: Channel["items"] | undefined) => {
    for (const item of list || [])
      counts.set(
        item.key,
        Math.max(
          counts.get(item.key) || 0,
          item.breakPoints?.length || item.breakPointsMs?.length || 0,
        ),
      );
  };
  const sources = (list: ProgramSource[] | undefined) => {
    for (const source of list || []) {
      items(source.items);
      sources(source.sources);
    }
  };
  items(channel.items);
  sources(channel.sources);
  for (const block of [...(channel.blocks || []), ...(channel.pins || [])]) {
    items(block.items);
    sources(block.sources);
  }
  return [...counts.values()].reduce((n, count) => n + count, 0);
}
const id = () => crypto.randomUUID();
const weekdays = [1, 2, 3, 4, 5];
const lookDefault = (): ChannelLook => ({ lookId: "", ident: false, upNext: false, offAir: true });
const newSource = (): ProgramSource => ({
  id: id(),
  kind: "movies",
  name: "",
  weight: 1,
  items: [],
});
function Days({ value, onChange }: { value?: number[]; onChange: (days: number[]) => void }) {
  const labels = [t("Sun"), t("Mon"), t("Tue"), t("Wed"), t("Thu"), t("Fri"), t("Sat")];
  return (
    <Field label={t("Days")}>
      <div className="flex flex-wrap gap-1">
        {labels.map((label, n) => (
          <Button
            key={n}
            size="sm"
            variant={!value?.length || value.includes(n) ? "default" : "outline"}
            aria-pressed={!value?.length || value.includes(n)}
            onClick={() => {
              const days = value?.length ? value : [0, 1, 2, 3, 4, 5, 6];
              const next = days.includes(n) ? days.filter((d) => d !== n) : [...days, n];
              if (next.length) onChange(next);
            }}
          >
            {label}
          </Button>
        ))}
      </div>
    </Field>
  );
}
const presets = () => [
  { key: "morning", label: t("Morning"), start: 360, end: 720 },
  { key: "daytime", label: t("Daytime"), start: 720, end: 1200 },
  { key: "prime", label: t("Prime time"), start: 1200, end: 1380 },
  { key: "late", label: t("Late night"), start: 1380, end: 1440 },
  { key: "overnight", label: t("Overnight"), start: 0, end: 360 },
  { key: "custom", label: t("Custom"), start: 0, end: 1440 },
];
export function ProgrammingEditor({
  channel: c,
  onChange,
  collections,
  schedule = false,
  editRequest,
  onEditHandled,
}: {
  channel: Channel;
  onChange: (patch: Partial<Channel>) => void;
  collections: CollectionChoice[];
  schedule?: boolean;
  editRequest?: { start?: number; blockId?: string; pinId?: string };
  onEditHandled?: () => void;
}) {
  const [editing, setEditing] = useState<
    | { kind: "block"; index: number; value: Block }
    | { kind: "pin"; index: number; value: Pin }
    | null
  >(null);
  const [adding, setAdding] = useState(false);
  const [editingBreaks, setEditingBreaks] = useState(false);
  const blocks = c.blocks ?? [];
  const pins = c.pins ?? [];
  const [error, setError] = useState("");
  useEffect(() => {
    if (!editRequest) return;
    if (editRequest.start !== undefined) {
      const parts = new Intl.DateTimeFormat("en-US", {
        timeZone: c.timezone || "UTC",
        hour: "2-digit",
        minute: "2-digit",
        hourCycle: "h23",
      }).formatToParts(editRequest.start);
      const start =
        Number(parts.find((p) => p.type === "hour")?.value) * 60 +
        Number(parts.find((p) => p.type === "minute")?.value);
      setEditing({
        kind: "pin",
        index: -1,
        value: {
          id: id(),
          name: "",
          start,
          end: Math.min(1440, start + 30),
          days: weekdays,
          sources: [{ ...newSource(), kind: "show", strip: true }],
          items: [],
        },
      });
    } else {
      const pin = pins.findIndex((p) => p.id === editRequest.pinId);
      const block = blocks.findIndex((b) => b.id === editRequest.blockId);
      if (pin >= 0) setEditing({ kind: "pin", index: pin, value: structuredClone(pins[pin]!) });
      else if (block >= 0)
        setEditing({ kind: "block", index: block, value: structuredClone(blocks[block]!) });
    }
    onEditHandled?.();
  }, [editRequest]);

  const openBlock = (i: number) => {
    if (blocks[i]) setEditing({ kind: "block", index: i, value: structuredClone(blocks[i]!) });
  };
  const add = (kind: "block" | "daypart" | "pin" | "special") => {
    setAdding(false);
    setError("");
    if (kind === "block" || kind === "daypart")
      setEditing({
        kind: "block",
        index: -1,
        value: {
          id: id(),
          name: "",
          preset: "custom",
          start: 1200,
          end: 1380,
          days: weekdays,
          items: [],
          shuffle: false,
          sources: [newSource()],
        },
      });
    else
      setEditing({
        kind: "pin",
        index: -1,
        value: {
          id: id(),
          name: "",
          start: 1200,
          end: 1230,
          days: weekdays,
          date: kind === "special" ? new Date().toISOString().slice(0, 10) : undefined,
          sources: [{ ...newSource(), kind: "show", strip: true, order: "order" }],
          items: [],
        },
      });
  };
  const save = () => {
    if (!editing) return;
    const v = editing.value;
    if (!v.name?.trim() || v.start < 0 || v.end <= v.start || v.end > 1440) {
      setError(t("Enter a name and valid start and end times."));
      return;
    }
    if (
      !v.sources?.length &&
      !v.items?.length &&
      !("filter" in v && v.filter) &&
      !("collection" in v && v.collection)
    ) {
      setError(t("Add at least one programming source."));
      return;
    }
    if (editing.kind === "block") {
      const next = [...blocks];
      if (editing.index < 0) next.push(editing.value);
      else next[editing.index] = editing.value;
      onChange({ blocks: next });
    } else {
      const next = [...pins];
      if (editing.index < 0) next.push(editing.value);
      else next[editing.index] = editing.value;
      onChange({ pins: next });
    }
    setEditing(null);
    setError("");
  };
  const update = (patch: Partial<Block & Pin>) => {
    if (editing)
      setEditing({ ...editing, value: { ...editing.value, ...patch } } as typeof editing);
  };
  const v = editing?.value;
  return (
    <div className="space-y-6">
      <Field label={t("Timezone")}>
        <Select
          aria-label={t("Timezone")}
          value={c.timezone || "UTC"}
          onChange={(e) => onChange({ timezone: e.currentTarget.value })}
        >
          {timeZones().map((z) => (
            <option key={z} value={z}>
              {z}
            </option>
          ))}
        </Select>
      </Field>
      <div className="flex items-center justify-between gap-3">
        <h3 className="font-semibold">{t("Dayparts and programme blocks")}</h3>
        <Button size="sm" onClick={() => setAdding(true)}>
          <Plus size={16} />
          {t("Add")}
        </Button>
      </div>
      {schedule && (
        <WeekGrid
          blocks={blocks}
          onChange={(next) => {
            onChange({ blocks: next });
            if (next.length > blocks.length)
              setEditing({
                kind: "block",
                index: next.length - 1,
                value: structuredClone(next[next.length - 1]!),
              });
          }}
          selected={editing?.kind === "block" ? editing.index : null}
          onSelect={(i) => {
            if (i !== null) openBlock(i);
          }}
        />
      )}
      <div className="space-y-2">
        {blocks.map((b, i) => (
          <button
            key={b.id ?? i}
            className="flex w-full items-center gap-3 rounded-lg border border-border bg-surface p-4 text-left hover:bg-bg-subtle"
            onClick={() => openBlock(i)}
          >
            <Clock size={20} className="text-primary" />
            <span className="min-w-0 flex-1">
              <span className="block font-medium">{blockLabel(b)}</span>
              <span className="text-xs text-fg-muted">
                {b.sources
                  ?.map((s) => s.name)
                  .filter(Boolean)
                  .join(" · ") || tn(b.items?.length ?? 0, "{count} title", "{count} titles")}
              </span>
            </span>
            <Badge>{t("Block")}</Badge>
          </button>
        ))}
        {!blocks.length && (
          <p className="text-sm text-fg-muted">
            {t("Add a daypart or programme block to shape the day.")}
          </p>
        )}
      </div>
      {!!(c.items.length || c.collection || c.filter || c.sources?.length) && (
        <section className="space-y-3 rounded-lg border border-border p-4">
          <h3 className="font-medium">{t("Between blocks")}</h3>
          <p className="text-xs text-fg-muted">
            {t("This programming fills time outside your blocks and pins.")}
          </p>
          <Sources
            sources={
              c.sources?.length
                ? c.sources
                : [
                    {
                      id: "base",
                      kind: c.collection ? "collection" : c.filter ? "filter" : "items",
                      items: c.items,
                      collection: c.collection,
                      filter: c.filter,
                    },
                  ]
            }
            collections={collections}
            onChange={(sources) =>
              onChange({ sources, items: [], collection: undefined, filter: undefined })
            }
          />
        </section>
      )}
      <section className="space-y-3">
        <div className="flex items-center justify-between">
          <h3 className="font-semibold">{t("Pinned slots and specials")}</h3>
          <Button variant="outline" size="sm" onClick={() => add("pin")}>
            <Plus size={14} />
            {t("Add pinned slot")}
          </Button>
        </div>
        <p className="text-xs text-fg-muted">{t("Pins take priority over programme blocks.")}</p>
        {pins.map((p, i) => (
          <button
            key={p.id}
            onClick={() => setEditing({ kind: "pin", index: i, value: structuredClone(p) })}
            className="flex w-full items-center gap-3 rounded-lg border border-border bg-surface p-4 text-left"
          >
            <CalendarDays size={20} />
            <span className="flex-1">
              <span className="block font-medium">{p.name}</span>
              <span className="text-xs text-fg-muted">
                {p.date || t("Recurring")} · {timeOfDay(p.start)}–{timeOfDay(p.end)}
              </span>
            </span>
            <Badge>{p.date ? t("Special") : t("Pinned")}</Badge>
          </button>
        ))}
      </section>
      <section className="space-y-4 rounded-lg border border-border p-4">
        <h3 className="font-semibold">{t("Breaks and commercials")}</h3>
        <Breaks
          value={c.look}
          onChange={(look) => onChange({ look })}
          detected={detectedBreaks(c)}
        />
      </section>
      <section className="space-y-4 rounded-lg border border-border p-4">
        <h3 className="flex items-center gap-2 font-semibold">
          <Radio size={18} />
          {t("Creative elements")}
        </h3>
        <Creative value={c.look} onChange={(look) => onChange({ look })} />
      </section>
      {editingBreaks && (
        <Drawer
          open
          onClose={() => setEditingBreaks(false)}
          title={t("Breaks and commercials")}
          size="lg"
        >
          <Breaks value={c.look} onChange={(look) => onChange({ look })} />
        </Drawer>
      )}
      {adding && (
        <Drawer open onClose={() => setAdding(false)} title={t("Add programming")} size="md">
          <div className="grid gap-3">
            {(
              [
                { kind: "daypart", label: t("Daypart") },
                { kind: "block", label: t("Programme block") },
                { kind: "pin", label: t("Pinned slot") },
                { kind: "special", label: t("Dated special") },
              ] as const
            ).map((x) => (
              <Button key={x.kind} variant="outline" onClick={() => add(x.kind)}>
                {x.label}
              </Button>
            ))}
            <Button
              variant="outline"
              onClick={() => {
                setAdding(false);
                setEditingBreaks(true);
              }}
            >
              {t("Break")}
            </Button>
          </div>
        </Drawer>
      )}
      {editing && v && (
        <Drawer
          open
          onClose={() => {
            setEditing(null);
            setError("");
          }}
          title={
            editing.kind === "block"
              ? t("Programme block")
              : v && "date" in v && v.date
                ? t("Dated special")
                : t("Pinned slot")
          }
          size="lg"
          footer={
            <div className="flex w-full justify-between gap-2">
              <Button
                variant="ghost"
                disabled={editing.index < 0}
                onClick={() => {
                  if (editing.kind === "block")
                    onChange({ blocks: blocks.filter((_, i) => i !== editing.index) });
                  else onChange({ pins: pins.filter((_, i) => i !== editing.index) });
                  setEditing(null);
                }}
              >
                <Trash2 size={16} />
                {t("Remove")}
              </Button>
              <div className="flex gap-2">
                <Button variant="outline" onClick={() => setEditing(null)}>
                  {t("Cancel")}
                </Button>
                <Button onClick={save}>{t("Apply")}</Button>
              </div>
            </div>
          }
        >
          <div className="space-y-5">
            {error && (
              <p role="alert" className="text-danger">
                {error}
              </p>
            )}
            <Field label={t("Name")}>
              <Input
                aria-label={t("Block name")}
                value={v.name || ""}
                onChange={(e) => update({ name: e.currentTarget.value })}
              />
            </Field>
            {editing.kind === "block" && (
              <Field label={t("Daypart preset")}>
                <Select
                  aria-label={t("Daypart preset")}
                  value={editing.value.preset || "custom"}
                  onChange={(e) => {
                    const p = presets().find((p) => p.key === e.currentTarget.value)!;
                    update({ preset: p.key, name: p.label, start: p.start, end: p.end });
                  }}
                >
                  {presets().map((p) => (
                    <option key={p.key} value={p.key}>
                      {p.label}
                    </option>
                  ))}
                </Select>
              </Field>
            )}
            {editing.kind === "pin" && (
              <SettingsRow
                title={t("One-off special")}
                control={
                  <Switch
                    aria-label={t("One-off special")}
                    checked={!!editing.value.date}
                    onChange={(e) =>
                      update({
                        date: e.currentTarget.checked
                          ? new Date().toISOString().slice(0, 10)
                          : undefined,
                      })
                    }
                  />
                }
              />
            )}
            {editing.kind === "pin" && editing.value.date ? (
              <Field label={t("Date")}>
                <Input
                  aria-label={t("Date")}
                  type="date"
                  value={editing.value.date}
                  onChange={(e) => update({ date: e.currentTarget.value })}
                />
              </Field>
            ) : (
              <Days value={v.days} onChange={(days) => update({ days })} />
            )}
            <div className="grid grid-cols-2 gap-3">
              <Field label={t("Start time")}>
                <Input
                  aria-label={t("Start time")}
                  type="time"
                  value={timeOfDay(v.start)}
                  onChange={(e) => update({ start: minutesOfDay(e.currentTarget.value) })}
                />
              </Field>
              <Field label={t("End time")}>
                <Input
                  aria-label={t("End time")}
                  type="time"
                  value={timeOfDay(v.end === 1440 ? 0 : v.end)}
                  onChange={(e) => update({ end: minutesOfDay(e.currentTarget.value) || 1440 })}
                />
              </Field>
            </div>
            <Sources
              sources={v.sources ?? (v.items?.length ? [{ kind: "items", items: v.items }] : [])}
              collections={collections}
              onChange={(sources) => update({ sources, items: [] })}
            />
            <Field label={t("No repeats within (days)")}>
              <Input
                aria-label={t("No repeats within (days)")}
                type="number"
                min={0}
                max={30}
                value={v.repeatDays || 0}
                onChange={(e) => update({ repeatDays: Number(e.currentTarget.value) })}
              />
            </Field>
            {editing.kind === "block" && !(v.sources?.length === 1 && v.sources[0]?.strip) && (
              <Field label={t("At the end of the block")}>
                <Select
                  aria-label={t("At the end of the block")}
                  value={editing.value.fit || ""}
                  onChange={(e) => update({ fit: e.currentTarget.value as Block["fit"] })}
                >
                  <option value="">{t("Default")}</option>
                  <option value="shift">{t("Let it run and shift")}</option>
                  <option value="cut">{t("Cut at the boundary")}</option>
                  <option value="pad">{t("Pad with filler or promos")}</option>
                </Select>
              </Field>
            )}
            {editing.kind === "block" && editing.value.fit === "pad" && (
              <Field label={t("Pad with filler or promos")}>
                <ProgrammePicker
                  label={t("Selected titles")}
                  items={editing.value.filler || []}
                  onChange={(filler) => update({ filler })}
                />
              </Field>
            )}
            <SettingsRow
              title={t("Customize breaks for this block")}
              control={
                <Switch
                  aria-label={t("Customize breaks for this block")}
                  checked={!!v.breaks}
                  onChange={(e) =>
                    update({
                      breaks: e.currentTarget.checked
                        ? structuredClone(c.look || lookDefault())
                        : undefined,
                    })
                  }
                />
              }
            />
            {v.breaks ? (
              <Breaks
                value={v.breaks}
                onChange={(breaks) => update({ breaks })}
                detected={detectedBreaks({
                  ...c,
                  items: v.items || [],
                  sources: v.sources,
                  blocks: [],
                  pins: [],
                })}
              />
            ) : (
              <p className="text-xs text-fg-muted">{t("Uses the channel break settings.")}</p>
            )}
          </div>
        </Drawer>
      )}
    </div>
  );
}
function Sources({
  sources,
  onChange,
  collections,
  depth = 0,
}: {
  sources: ProgramSource[];
  onChange: (s: ProgramSource[]) => void;
  collections: CollectionChoice[];
  depth?: number;
}) {
  const segments = useRegistry("onairSegment");
  const set = (i: number, p: Partial<ProgramSource>) =>
    onChange(sources.map((s, n) => (n === i ? { ...s, ...p } : s)));
  return (
    <div className="space-y-3">
      <div className="flex justify-between">
        <h4 className="font-medium">{t("Sources")}</h4>
        <Button variant="outline" size="sm" onClick={() => onChange([...sources, newSource()])}>
          <Plus size={14} />
          {t("Add source")}
        </Button>
      </div>
      {sources.map((s, i) => (
        <section key={s.id || i} className="space-y-3 rounded-lg border border-border p-3">
          <div className="flex gap-2">
            <Select
              className="flex-1"
              aria-label={t("Source type")}
              value={s.kind || (s.collection ? "collection" : s.filter ? "filter" : "items")}
              onChange={(e) => {
                const kind = e.currentTarget.value as ProgramSource["kind"];
                onChange(
                  sources.map((source, n) =>
                    n === i
                      ? {
                          id: source.id,
                          name: source.name,
                          weight: source.weight,
                          kind,
                          items: [],
                          personal: kind === "personal" ? personalRecipe() : undefined,
                        }
                      : source,
                  ),
                );
              }}
            >
              <option value="items">{t("Selected titles")}</option>
              <option value="show">{t("TV show")}</option>
              <option value="movies">{t("Movie pool")}</option>
              <option value="collection">{t("Collection")}</option>
              <option value="filter">{t("Smart filter")}</option>
              <option value="personal">{t("Personal picks")}</option>
              <option value="plugin">{t("Plugin segment")}</option>
              {depth < 2 && <option value="mix">{t("Weighted mix")}</option>}
            </Select>
            <Button
              variant="ghost"
              aria-label={t("Remove source")}
              onClick={() => onChange(sources.filter((_, n) => n !== i))}
            >
              <Trash2 size={16} />
            </Button>
          </div>
          {sources.length > 1 && (
            <Field label={t("Weight")}>
              <Input
                aria-label={t("Weight")}
                type="number"
                min={1}
                value={s.weight || 1}
                onChange={(e) => set(i, { weight: Number(e.currentTarget.value) })}
              />
            </Field>
          )}
          {(!s.kind || ["items", "movies", "collection", "filter"].includes(s.kind)) && (
            <Field label={t("Order")}>
              <Select
                aria-label={t("Order")}
                value={s.order || "order"}
                onChange={(e) => set(i, { order: e.currentTarget.value as "order" | "shuffle" })}
              >
                <option value="order">{t("In order")}</option>
                <option value="shuffle">{t("Shuffled")}</option>
              </Select>
            </Field>
          )}
          {(s.kind === "items" || s.kind === "movies" || !s.kind) && (
            <ProgrammePicker
              label={t("Selected titles")}
              items={s.items || []}
              onChange={(items) => set(i, { items })}
            />
          )}
          {s.kind === "show" && (
            <>
              <TitleSearch
                type="show"
                label={t("Find a TV show")}
                onPick={(card) =>
                  set(i, { showKey: Number(card.key), startDate: undefined, name: card.title })
                }
              />
              {s.name && <Badge>{s.name}</Badge>}
              <div className="grid grid-cols-2 gap-2">
                <Field label={t("Start season")}>
                  <Input
                    type="number"
                    min={1}
                    aria-label={t("Start season")}
                    value={s.startSeason || 1}
                    onChange={(e) =>
                      set(i, { startDate: undefined, startSeason: Number(e.currentTarget.value) })
                    }
                  />
                </Field>
                <Field label={t("Start episode")}>
                  <Input
                    type="number"
                    min={1}
                    aria-label={t("Start episode")}
                    value={s.startEpisode || 1}
                    onChange={(e) =>
                      set(i, { startDate: undefined, startEpisode: Number(e.currentTarget.value) })
                    }
                  />
                </Field>
              </div>
              <SettingsRow
                title={t("Strip: continue episodes each day")}
                control={
                  <Switch
                    aria-label={t("Strip: continue episodes each day")}
                    checked={!!s.strip}
                    onChange={(e) => set(i, { strip: e.currentTarget.checked, order: "order" })}
                  />
                }
              />
              {!s.strip && (
                <Field label={t("Episode order")}>
                  <Select
                    aria-label={t("Episode order")}
                    value={s.order || "order"}
                    onChange={(e) =>
                      set(i, { order: e.currentTarget.value as "order" | "shuffle" })
                    }
                  >
                    <option value="order">{t("In order")}</option>
                    <option value="shuffle">{t("Shuffled")}</option>
                  </Select>
                </Field>
              )}
            </>
          )}
          {s.kind === "collection" && (
            <Field label={t("Collection")}>
              <Select
                aria-label={t("Collection")}
                value={s.collection || ""}
                onChange={(e) =>
                  set(i, {
                    collection: Number(e.currentTarget.value),
                    name: collections.find((c) => c.ratingKey === e.currentTarget.value)?.name,
                  })
                }
              >
                <option value="">{t("Choose a collection")}</option>
                {collections
                  .filter((c) => c.ratingKey)
                  .map((c) => (
                    <option key={c.id} value={c.ratingKey}>
                      {c.name}
                    </option>
                  ))}
              </Select>
            </Field>
          )}
          {s.kind === "filter" && (
            <>
              <GenreChips filter={s.filter} onChange={(filter) => set(i, { filter })} />
              <FilterCount filter={s.filter} />
              <SmartFilter
                filter={s.filter}
                onChange={(filter) => set(i, { filter: { ...s.filter, ...filter } })}
              />
            </>
          )}
          {s.kind === "plugin" && (
            <Field label={t("Plugin segment")}>
              <Select
                aria-label={t("Plugin segment")}
                value={s.segmentId || ""}
                onChange={(e) =>
                  set(i, {
                    segmentId: e.currentTarget.value,
                    name: textOf(
                      segments.descriptors.find((d) => d.id === e.currentTarget.value)?.label,
                    ),
                  })
                }
              >
                <option value="">{t("Choose a segment")}</option>
                {segments.descriptors.map((d) => (
                  <option key={d.id} value={d.id}>
                    {textOf(d.label)}
                  </option>
                ))}
              </Select>
            </Field>
          )}
          {s.kind === "mix" && (
            <Sources
              sources={s.sources || []}
              collections={collections}
              depth={depth + 1}
              onChange={(sources) => set(i, { sources })}
            />
          )}
        </section>
      ))}
    </div>
  );
}
function Breaks({
  value,
  onChange,
  detected = 0,
}: {
  value?: ChannelLook;
  onChange: (v: ChannelLook) => void;
  detected?: number;
}) {
  const [reels, setReels] = useState<Reel[]>([]);
  useEffect(() => {
    let live = true;
    onair
      .reels()
      .then((r) => {
        if (live) setReels(r);
      })
      .catch(() => {});
    return () => {
      live = false;
    };
  }, []);
  const set = (p: Partial<ChannelLook>) => onChange({ ...lookDefault(), ...value, ...p });
  return (
    <div className="space-y-3" id="channel-breaks">
      <Field label={t("Break placement")}>
        <Select
          aria-label={t("Break placement")}
          value={value?.breaks ? value.breakPlacement || "between" : "off"}
          onChange={(e) => {
            const p = e.currentTarget.value;
            set({
              breaks: p === "off" ? "" : p === "between" ? "between" : "inside",
              breakPlacement: p === "off" ? undefined : (p as ChannelLook["breakPlacement"]),
              placement: p === "between" ? "gap" : "insert",
              breakPattern:
                p === "natural" || p === "even"
                  ? { ...(value?.breakPattern || breakPatternPreset("1980s")), inside: true }
                  : value?.breakPattern
                    ? { ...value.breakPattern, inside: false }
                    : undefined,
            });
          }}
        >
          <option value="off">{t("Off")}</option>
          <option value="between">{t("Between programmes only")}</option>
          <option value="natural">{t("Natural break points")}</option>
          <option value="even">{t("Evenly spaced")}</option>
        </Select>
      </Field>
      {value?.breaks && (
        <>
          <Badge>
            {tn(detected, "{count} detected break point", "{count} detected break points")}
          </Badge>
          <BreakPatternEditor
            value={value.breakPattern}
            onChange={(breakPattern) =>
              set({
                breakPattern: breakPattern
                  ? { ...breakPattern, inside: value.breaks === "inside" }
                  : undefined,
              })
            }
          />
          <Field label={t("Commercials from Assets")}>
            <Select
              aria-label={t("Commercials from Assets")}
              value={value.reelId || ""}
              onChange={(e) =>
                set({
                  reelId: e.currentTarget.value,
                  breakContent: e.currentTarget.value ? "reel" : "look",
                })
              }
            >
              <option value="">{t("Style break screen")}</option>
              {reels.map((r) => (
                <option key={r.id} value={r.id}>
                  {r.name}
                </option>
              ))}
            </Select>
          </Field>
          {!value.breakPattern && (
            <Field label={t("Pod length (seconds)")}>
              <Input
                type="number"
                min={4}
                max={600}
                step={4}
                aria-label={t("Pod length (seconds)")}
                value={value.breakSeconds || 120}
                onChange={(e) => set({ breakSeconds: Number(e.currentTarget.value) })}
              />
            </Field>
          )}
        </>
      )}
    </div>
  );
}
function Creative({
  value,
  onChange,
}: {
  value?: ChannelLook;
  onChange: (v: ChannelLook) => void;
}) {
  const [assets, setAssets] = useState<OnAirAsset[]>([]);
  useEffect(() => {
    let live = true;
    onair
      .assets()
      .then((a) => {
        if (live)
          setAssets(a.filter((a) => a.kind === "video" && (!a.status || a.status === "ready")));
      })
      .catch(() => {});
    return () => {
      live = false;
    };
  }, []);
  const set = (p: Partial<ChannelLook>) => onChange({ ...lookDefault(), ...value, ...p });
  return (
    <div className="space-y-3">
      <div className="grid gap-3 sm:grid-cols-2">
        {(
          [
            { key: "ident", label: t("Ident") },
            { key: "upnext", label: t("Up next") },
            { key: "tonight", label: t("Tonight") },
            { key: "break", label: t("Break") },
            { key: "back", label: t("Back") },
          ] as const
        ).map((m) => (
          <Field key={m.key} label={m.label}>
            <Select
              aria-label={m.label}
              value={value?.momentAssets?.[m.key] || ""}
              onChange={(e) =>
                set({ momentAssets: { ...value?.momentAssets, [m.key]: e.currentTarget.value } })
              }
            >
              <option value="">{t("Use style")}</option>
              {assets.map((a) => (
                <option key={a.digest} value={a.digest}>
                  {a.name}
                </option>
              ))}
            </Select>
          </Field>
        ))}
      </div>
      {(
        [
          { key: "ident", label: t("Ident when tuning in") },
          { key: "upNext", label: t("Up next") },
          { key: "tonight", label: t("Coming up tonight") },
        ] as const
      ).map((x) => (
        <SettingsRow
          key={x.key}
          title={x.label}
          control={
            <Switch
              aria-label={x.label}
              checked={x.key === "tonight" ? (value?.tonight ?? !!value?.upNext) : !!value?.[x.key]}
              onChange={(e) => set({ [x.key]: e.currentTarget.checked })}
            />
          }
        />
      ))}
      <SettingsRow
        title={t("Station IDs")}
        control={
          <Switch
            aria-label={t("Station IDs")}
            checked={!!value?.breakPattern?.stationId}
            onChange={(e) =>
              set({
                breakPattern: {
                  preset: "custom",
                  seconds: 120,
                  minSeconds: 60,
                  maxSeconds: 180,
                  spots: 4,
                  minSpots: 2,
                  maxSpots: 6,
                  podsPerHalfHour: 2,
                  inside: true,
                  noRepeatMinutes: 60,
                  bumperStyle: "break-back",
                  clockAlignment: "half",
                  ...value?.breakPattern,
                  stationId: e.currentTarget.checked,
                },
              })
            }
          />
        }
      />
      <SettingsRow
        title={t("Break and return bumpers")}
        control={
          <Switch
            aria-label={t("Break and return bumpers")}
            checked={value?.breakPattern?.bumperStyle === "break-back"}
            onChange={(e) =>
              set({
                breakPattern: {
                  preset: "custom",
                  seconds: 120,
                  minSeconds: 60,
                  maxSeconds: 180,
                  spots: 4,
                  minSpots: 2,
                  maxSpots: 6,
                  podsPerHalfHour: 2,
                  inside: true,
                  noRepeatMinutes: 60,
                  stationId: false,
                  clockAlignment: "half",
                  ...value?.breakPattern,
                  bumperStyle: e.currentTarget.checked ? "break-back" : "none",
                },
              })
            }
          />
        }
      />
    </div>
  );
}

function FilterCount({ filter }: { filter?: Record<string, unknown> }) {
  const value = useDebouncedValue(JSON.stringify(filter || {}), 400);
  const [count, setCount] = useState<number | null>(null);
  useEffect(() => {
    const ctl = new AbortController();
    const f = JSON.parse(value) as Record<string, unknown>;
    const rules: Rule[] = [];
    if (Array.isArray(f.genres) && f.genres.length)
      rules.push({ field: "genre", op: "in", value: f.genres });
    if (f.yearFrom) rules.push({ field: "year", op: "gte", value: f.yearFrom });
    if (f.yearTo) rules.push({ field: "year", op: "lte", value: f.yearTo });
    if (f.studio) rules.push({ field: "studio", op: "is", value: f.studio });
    if (f.maxContentRating)
      rules.push({ field: "contentRating", op: "lte", value: f.maxContentRating });
    if (f.minRuntime)
      rules.push({ field: "runtime", op: "gte", value: Number(f.minRuntime) / 60000000000 });
    if (f.maxRuntime)
      rules.push({ field: "runtime", op: "lte", value: Number(f.maxRuntime) / 60000000000 });
    if (Array.isArray(f.keywords))
      for (const keyword of f.keywords) rules.push({ field: "keyword", op: "is", value: keyword });
    setCount(null);
    api
      .previewCollection(
        { ...blankSpec(f.type === "show" ? "show" : "movie"), rules },
        ctl.signal,
        1,
      )
      .then((r) => setCount(r.total))
      .catch(() => {});
    return () => ctl.abort();
  }, [value]);
  return count === null ? null : <Badge>{tn(count, "{count} title", "{count} titles")}</Badge>;
}

function SmartFilter({
  filter,
  onChange,
}: {
  filter?: Record<string, unknown>;
  onChange: (patch: Record<string, unknown>) => void;
}) {
  const decade = typeof filter?.yearFrom === "number" ? String(filter.yearFrom) : "";
  const keywords = Array.isArray(filter?.keywords) ? (filter.keywords as string[]).join(", ") : "";
  const minutes = (value: unknown) => {
    const n = Number(value);
    if (!n) return "";
    return String(n > 10_000 ? Math.round(n / 60_000_000_000) : n);
  };
  return (
    <div className="grid gap-4 sm:col-span-2 sm:grid-cols-2">
      <Field label={t("Decade")}>
        <Select
          aria-label={t("Decade")}
          value={decade}
          onChange={(e) => {
            const year = Number(e.currentTarget.value);
            onChange(
              year
                ? { yearFrom: year, yearTo: year + 9 }
                : { yearFrom: undefined, yearTo: undefined },
            );
          }}
        >
          <option value="">{t("Any decade")}</option>
          {[2020, 2010, 2000, 1990, 1980, 1970, 1960].map((year) => (
            <option key={year} value={year}>
              {t("{year}s", { year })}
            </option>
          ))}
        </Select>
      </Field>
      <Field label={t("Age level")} hint={t("Nothing more mature than this rating.")}>
        <Select
          aria-label={t("Age level")}
          value={String(filter?.maxContentRating ?? "")}
          onChange={(e) => onChange({ maxContentRating: e.currentTarget.value || undefined })}
        >
          <option value="">{t("Any age")}</option>
          <option value="G">{t("All ages")}</option>
          <option value="PG">{t("Parental guidance")}</option>
          <option value="PG-13">{t("Teen")}</option>
          <option value="R">{t("Mature")}</option>
        </Select>
      </Field>
      <Field label={t("Studio or network")} htmlFor="smart-studio">
        <Input
          id="smart-studio"
          value={String(filter?.studio ?? "")}
          onChange={(e) => onChange({ studio: e.currentTarget.value || undefined })}
          placeholder={t("Studio or network")}
        />
      </Field>
      <Field
        label={t("Keyword")}
        htmlFor="smart-keyword"
        hint={t("Separate with commas. A title must match every keyword.")}
      >
        <Input
          id="smart-keyword"
          value={keywords}
          onChange={(e) =>
            onChange({
              keywords: e.currentTarget.value
                .split(",")
                .map((s) => s.trim())
                .filter(Boolean),
            })
          }
          placeholder={t("space, heist")}
        />
      </Field>
      <Field label={t("Shortest")} htmlFor="smart-min" hint={t("Minutes")}>
        <Input
          id="smart-min"
          type="number"
          min={0}
          value={minutes(filter?.minRuntime)}
          onChange={(e) =>
            onChange({
              minRuntime: e.currentTarget.value
                ? Number(e.currentTarget.value) * 60_000_000_000
                : undefined,
            })
          }
        />
      </Field>
      <Field label={t("Longest")} htmlFor="smart-max" hint={t("Minutes")}>
        <Input
          id="smart-max"
          type="number"
          min={0}
          value={minutes(filter?.maxRuntime)}
          onChange={(e) =>
            onChange({
              maxRuntime: e.currentTarget.value
                ? Number(e.currentTarget.value) * 60_000_000_000
                : undefined,
            })
          }
        />
      </Field>
    </div>
  );
}

function GenreChips({
  filter,
  onChange,
}: {
  filter?: Record<string, unknown>;
  onChange: (v: Record<string, unknown>) => void;
}) {
  const [genres, setGenres] = useState<{ key: string; title: string }[]>([]);
  const type = filter?.type === "show" ? "show" : "movie";
  useEffect(() => {
    let live = true;
    api
      .facets(type, "genre")
      .then((values) => {
        if (live && Array.isArray(values)) setGenres(values);
      })
      .catch(() => {});
    return () => {
      live = false;
    };
  }, [type]);
  const selected = Array.isArray(filter?.genres) ? filter.genres.map(String) : [];
  const options = [
    ...genres.map((g) => ({ value: g.key, label: g.title })),
    ...selected
      .filter((v) => !genres.some((g) => g.key === v))
      .map((v) => ({ value: v, label: v })),
  ];
  return (
    <>
      <Field label={t("Catalogue")}>
        <Select
          aria-label={t("Catalogue")}
          value={type}
          onChange={(e) => onChange({ ...filter, type: e.currentTarget.value })}
        >
          <option value="movie">{t("Movies")}</option>
          <option value="show">{t("TV shows")}</option>
        </Select>
      </Field>
      <Field label={t("Genres")}>
        <ChipGroup
          ariaLabel={t("Genres")}
          value={selected}
          options={options}
          onChange={(genres) => onChange({ ...filter, type, genres })}
        />
      </Field>
    </>
  );
}
