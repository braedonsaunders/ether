import { Button } from "@braedonsaunders/appkit-ui";
import { ArrowDown, ArrowUp, Shuffle, X } from "lucide-react";
import { useRef, useState } from "react";
import { Artwork } from "../../components/Artwork";
import { TitleSearch } from "../../components/pickers";
import { DragHandle, SortableList } from "../../components/Sortable";
import { api, errorMessage } from "../../lib/api";
import type { TitleCard } from "../../lib/contract";
import { t, tn } from "../../i18n";
import { duration, loopLength, type Item } from "./model";

/** A hand-built running order: search to add, drag (or arrows) to reorder. */
export function ProgrammePicker({
  items,
  onChange,
  label,
  searchLabel,
}: {
  items: Item[];
  onChange: (items: Item[]) => void;
  label: string;
  searchLabel?: string;
}) {
  const [notice, setNotice] = useState("");
  const pending = useRef(false);
  async function pick(card: TitleCard) {
    if (pending.current) return;
    pending.current = true;
    setNotice("");
    try {
      const title = card.runtimeMinutes ? card : await api.title(card.key);
      if (!title.runtimeMinutes || title.runtimeMinutes <= 0)
        throw new Error(
          t(
            "This movie has no running time yet. Choose another movie or add it through a collection after its metadata is refreshed.",
          ),
        );
      onChange([
        ...items,
        {
          key: Number(title.key),
          title: title.title,
          durationMs: title.runtimeMinutes * 60000,
          summary: title.summary,
          rating: title.contentRating,
          thumb: title.poster,
        },
      ]);
    } catch (e) {
      setNotice(errorMessage(e));
    } finally {
      pending.current = false;
    }
  }
  function move(index: number, offset: number) {
    const next = [...items];
    const current = next[index],
      other = next[index + offset];
    if (!current || !other) return;
    next[index] = other;
    next[index + offset] = current;
    onChange(next);
  }
  const ids = items.map((item, i) => `${item.key}-${i}`);
  return (
    <div className="space-y-3" role="group" aria-label={label}>
      <TitleSearch
        type="movie"
        placeholder={t("Find a movie to add…")}
        label={searchLabel}
        excludeKeys={new Set(items.map((i) => String(i.key)))}
        onPick={(title) => void pick(title)}
      />
      {notice && (
        <p role="alert" className="text-sm text-danger">
          {notice}
        </p>
      )}
      {items.length ? (
        <>
          <div className="flex items-center justify-between text-xs text-fg-muted">
            <span>
              {tn(
                items.length,
                "{count} title · {duration} loop",
                "{count} titles · {duration} loop",
                {
                  duration: duration(loopLength(items)),
                },
              )}
            </span>
            {items.length > 1 ? (
              <Button
                variant="ghost"
                size="sm"
                className="h-7 px-2 text-xs"
                onClick={() => onChange([...items].sort(() => Math.random() - 0.5))}
              >
                <Shuffle size={12} /> {t("Shuffle order")}
              </Button>
            ) : null}
          </div>
          <SortableList
            items={items}
            getId={(item) => ids[items.indexOf(item)] ?? String(item.key)}
            onReorder={(next) => onChange(next)}
            className="space-y-1.5"
            renderItem={(item, handle, index) => (
              <div className="flex items-center gap-2 rounded-lg border border-border bg-surface p-1.5 pr-2">
                <DragHandle
                  handle={handle}
                  label={t("Reorder {title}", { title: item.title })}
                  className="size-7"
                />
                <span className="w-5 shrink-0 text-right text-xs tabular-nums text-fg-subtle">
                  {index + 1}
                </span>
                <Artwork
                  src={item.thumb}
                  alt=""
                  title={item.title}
                  className="h-12 w-8 shrink-0 rounded"
                />
                <div className="min-w-0 flex-1">
                  <p className="truncate text-sm font-medium text-fg">{item.title}</p>
                  <p className="text-xs text-fg-muted">
                    {t("{minutes} min", { minutes: Math.round(item.durationMs / 60000) })}
                    {item.rating ? ` · ${item.rating}` : ""}
                  </p>
                </div>
                <div className="flex shrink-0 gap-0.5">
                  <Button
                    size="sm"
                    variant="ghost"
                    className="size-7 p-0"
                    aria-label={t("Move {title} earlier", { title: item.title })}
                    disabled={index === 0}
                    onClick={() => move(index, -1)}
                  >
                    <ArrowUp size={13} />
                  </Button>
                  <Button
                    size="sm"
                    variant="ghost"
                    className="size-7 p-0"
                    aria-label={t("Move {title} later", { title: item.title })}
                    disabled={index === items.length - 1}
                    onClick={() => move(index, 1)}
                  >
                    <ArrowDown size={13} />
                  </Button>
                  <Button
                    size="sm"
                    variant="ghost"
                    className="size-7 p-0 text-fg-subtle hover:text-danger"
                    aria-label={t("Remove {title}", { title: item.title })}
                    onClick={() => onChange(items.filter((_, n) => n !== index))}
                  >
                    <X size={14} />
                  </Button>
                </div>
              </div>
            )}
          />
        </>
      ) : (
        <p className="rounded-lg border border-dashed border-border px-3 py-4 text-center text-xs text-fg-muted">
          {t(
            "Search for movies to build the running order. Use a collection or genres to include TV episodes.",
          )}
        </p>
      )}
    </div>
  );
}
