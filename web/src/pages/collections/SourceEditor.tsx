import { Badge, Button, Input, SearchSelect } from "@braedonsaunders/appkit-ui";
import { ArrowDown, ArrowUp, Plus, Trash2, X } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";
import { Artwork } from "../../components/Artwork";
import { Segmented } from "../../components/fields";
import { LookupPicker, TitleSearch } from "../../components/pickers";
import { titleSubtitle } from "../../components/Poster";
import { DragHandle, SortableList, arrayMove } from "../../components/Sortable";
import { t, tx } from "../../i18n";
import { searchSelectLabels } from "../../i18n/appkit-labels";
import type { Source, TitleCard } from "../../lib/contract";
import {
  STEVENLU_LISTS,
  TMDB_LISTS,
  describeSource,
  isSourceComplete,
  sourceMeta,
} from "../../lib/rules";
import { useTitle } from "../../lib/titles";

export function SourceEditor({
  source,
  onChange,
  onRemove,
  index,
  region,
  removable,
}: {
  source: Source;
  onChange: (s: Source) => void;
  onRemove: () => void;
  index: number;
  region: string;
  removable: boolean;
}) {
  const meta = sourceMeta(source.kind);
  const n = index + 1;
  const label = t("Source {n}", { n });
  return (
    <div className="rounded-lg border border-border bg-surface p-3" role="group" aria-label={label}>
      <div className="flex items-start justify-between gap-2">
        <div className="min-w-0">
          <p className="text-sm font-medium text-fg">{meta?.label ?? source.kind}</p>
          <p className="text-xs text-fg-muted">
            {isSourceComplete(source) && source.kind !== "catalog"
              ? describeSource(source)
              : meta?.description}
          </p>
        </div>
        {removable ? (
          <Button
            type="button"
            variant="ghost"
            size="icon"
            className="size-8 shrink-0 text-fg-muted hover:text-danger"
            onClick={onRemove}
            aria-label={t("Remove source {n}", { n })}
          >
            <Trash2 size={15} />
          </Button>
        ) : null}
      </div>
      <div className="mt-2 empty:hidden">
        <SourceBody source={source} onChange={onChange} region={region} label={label} />
      </div>
    </div>
  );
}

function SourceBody({
  source,
  onChange,
  region,
  label,
}: {
  source: Source;
  onChange: (s: Source) => void;
  region: string;
  label: string;
}) {
  switch (source.kind) {
    case "catalog":
      return null;
    case "tmdbList": {
      const lists = TMDB_LISTS.filter((l) => l.media.includes(source.mediaType));
      return (
        <div className="flex flex-wrap items-center gap-2">
          <Segmented
            ariaLabel={t("{label} media", { label })}
            size="sm"
            value={source.mediaType}
            onChange={(m) =>
              onChange({
                ...source,
                mediaType: m,
                list: TMDB_LISTS.find((l) => l.value === source.list)?.media.includes(m)
                  ? source.list
                  : "popular",
              })
            }
            options={[
              { value: "movie", label: t("Movies") },
              { value: "show", label: t("TV") },
            ]}
          />
          <div className="min-w-48 flex-1">
            <SearchSelect
              value={source.list}
              onChange={(v) => v && onChange({ ...source, list: v as typeof source.list })}
              options={lists.map((l) => ({ value: l.value, label: l.label }))}
              ariaLabel={t("{label} chart", { label })}
              searchable={false}
              {...searchSelectLabels()}
            />
          </div>
        </div>
      );
    }
    case "tmdbCollection":
    case "tmdbKeyword":
    case "tmdbCompany":
    case "tmdbNetwork":
    case "tmdbPerson":
    case "watchProvider": {
      const lookup = sourceMeta(source.kind)?.lookup ?? "keyword";
      const picked =
        source.id > 0 ? [{ id: source.id, name: source.label ?? `#${source.id}` }] : [];
      return (
        <div className="space-y-2">
          <LookupPicker
            kind={lookup}
            multiple={false}
            region={source.kind === "watchProvider" ? source.region || region : region}
            value={picked}
            ariaLabel={t("{label} search", { label })}
            onChange={(p) =>
              onChange({ ...source, id: p[0]?.id ?? 0, label: p[0]?.name } as Source)
            }
          />
          {source.kind === "watchProvider" ? (
            <label className="flex items-center gap-2 text-xs text-fg-muted">
              {t("Region")}
              <Input
                value={source.region ?? ""}
                placeholder={region}
                maxLength={2}
                className="h-8 w-16 uppercase"
                aria-label={t("{label} region", { label })}
                onChange={(e) =>
                  onChange({ ...source, region: e.currentTarget.value.toUpperCase() })
                }
              />
            </label>
          ) : null}
        </div>
      );
    }
    case "traktList":
    case "letterboxdList":
    case "mdblist":
    case "anilistList": {
      const placeholder = {
        traktList: "https://trakt.tv/users/…/lists/…",
        letterboxdList: "https://letterboxd.com/…/list/…",
        mdblist: "https://mdblist.com/lists/…",
        anilistList: "https://anilist.co/user/…/animelist/Completed",
      }[source.kind];
      const bad = source.url.trim() !== "" && !isSourceComplete(source);
      return (
        <div className="space-y-1">
          <Input
            type="url"
            value={source.url}
            placeholder={placeholder}
            aria-label={t("{label} URL", { label })}
            aria-invalid={bad || undefined}
            onChange={(e) => onChange({ ...source, url: e.currentTarget.value })}
          />
          {bad ? (
            <p className="text-xs text-danger">
              {t("Enter a full https:// link to a public list.")}
            </p>
          ) : source.kind === "traktList" ? (
            <p className="text-xs text-fg-muted">
              {tx("Uses the Trakt account connected in {link}.", {
                link: (
                  <Link to="/services/trakt" className="text-primary underline">
                    {t("Integrations")}
                  </Link>
                ),
              })}
            </p>
          ) : source.kind === "mdblist" ? (
            <p className="text-xs text-fg-muted">
              {tx("Uses the MDBList account connected in {link}.", {
                link: (
                  <Link to="/services/ratings" className="text-primary underline">
                    {t("Integrations")}
                  </Link>
                ),
              })}
            </p>
          ) : null}
        </div>
      );
    }
    case "stevenlu":
      return (
        <SearchSelect
          value={source.list}
          onChange={(v) => v && onChange({ ...source, list: v as typeof source.list })}
          options={STEVENLU_LISTS}
          ariaLabel={t("{label} list", { label })}
          searchable={false}
          {...searchSelectLabels()}
        />
      );
    case "tmdbDiscover":
      return (
        <DiscoverParams
          params={source.params}
          onChange={(params) => onChange({ ...source, params })}
          label={label}
        />
      );
    case "titles":
      return <HandPicked keys={source.keys} onChange={(keys) => onChange({ ...source, keys })} />;
  }
}

function DiscoverParams({
  params,
  onChange,
  label,
}: {
  params: Record<string, string>;
  onChange: (p: Record<string, string>) => void;
  label: string;
}) {
  const entries = Object.entries(params);
  const [k, setK] = useState("");
  const [v, setV] = useState("");
  return (
    <div className="space-y-2">
      {entries.length ? (
        <div className="flex flex-wrap gap-1.5">
          {entries.map(([key, value]) => (
            <Badge key={key} variant="secondary" className="gap-1 font-mono text-[11px] text-fg">
              {key}={value}
              <button
                type="button"
                aria-label={t("Remove {key}", { key })}
                onClick={() => {
                  const n = { ...params };
                  delete n[key];
                  onChange(n);
                }}
                className="rounded-full p-0.5 hover:bg-surface-hover"
              >
                <X size={11} />
              </button>
            </Badge>
          ))}
        </div>
      ) : (
        <p className="text-xs text-fg-muted">
          {tx("Add TMDB discover parameters, e.g. {first} or {second}.", {
            first: <code>with_genres=27</code>,
            second: <code>vote_average.gte=7</code>,
          })}
        </p>
      )}
      <form
        className="flex gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (k.trim() && v.trim()) {
            onChange({ ...params, [k.trim()]: v.trim() });
            setK("");
            setV("");
          }
        }}
      >
        <Input
          value={k}
          onChange={(e) => setK(e.currentTarget.value)}
          placeholder={t("parameter")}
          aria-label={t("{label} parameter name", { label })}
          className="font-mono text-xs"
        />
        <Input
          value={v}
          onChange={(e) => setV(e.currentTarget.value)}
          placeholder={t("value")}
          aria-label={t("{label} parameter value", { label })}
          className="font-mono text-xs"
        />
        <Button
          type="submit"
          variant="outline"
          size="icon"
          className="shrink-0"
          aria-label={t("Add parameter")}
          disabled={!k.trim() || !v.trim()}
        >
          <Plus size={15} />
        </Button>
      </form>
    </div>
  );
}

/** Hand-picked titles: search to add, drag (or use the arrows) to set the order. */
function HandPicked({ keys, onChange }: { keys: string[]; onChange: (k: string[]) => void }) {
  const [known, setKnown] = useState<Record<string, TitleCard>>({});
  const move = (i: number, d: -1 | 1) => {
    const j = i + d;
    if (j < 0 || j >= keys.length) return;
    onChange(arrayMove(keys, i, j));
  };
  return (
    <div className="space-y-3">
      <TitleSearch
        excludeKeys={new Set(keys)}
        onPick={(title) => {
          setKnown((k) => ({ ...k, [title.key]: title }));
          onChange([...keys, title.key]);
        }}
      />
      {keys.length === 0 ? (
        <p className="text-xs text-fg-muted">{t("No titles yet. Search above to add some.")}</p>
      ) : (
        <SortableList
          items={keys}
          getId={(k) => k}
          onReorder={(next) => onChange(next)}
          className="space-y-1.5"
          renderItem={(key, handle, i) => (
            <PickedRow
              keyId={key}
              hint={known[key]}
              index={i}
              handle={handle}
              onUp={() => move(i, -1)}
              onDown={() => move(i, 1)}
              first={i === 0}
              last={i === keys.length - 1}
              onRemove={() => onChange(keys.filter((k) => k !== key))}
            />
          )}
        />
      )}
      {keys.length > 1 ? (
        <p className="text-[11px] text-fg-subtle">
          {t(
            "Drag the handle or use the arrows to reorder. Sort by “Source order” to keep this order.",
          )}
        </p>
      ) : null}
    </div>
  );
}

function PickedRow({
  keyId,
  hint,
  index,
  handle,
  onUp,
  onDown,
  onRemove,
  first,
  last,
}: {
  keyId: string;
  hint?: TitleCard;
  index: number;
  handle: Parameters<typeof DragHandle>[0]["handle"];
  onUp: () => void;
  onDown: () => void;
  onRemove: () => void;
  first: boolean;
  last: boolean;
}) {
  const loaded = useTitle(hint ? undefined : keyId);
  const card = hint ?? loaded;
  const name = card?.title ?? t("Title {id}", { id: keyId });
  return (
    <div className="flex items-center gap-2 rounded-md border border-border-subtle bg-bg-subtle p-1.5">
      <DragHandle handle={handle} label={t("Reorder {name}", { name })} />
      <span className="w-5 text-right text-xs tabular-nums text-fg-subtle">{index + 1}</span>
      <Artwork src={card?.poster} alt="" title="" className="h-12 w-8 shrink-0 rounded" />
      <span className="min-w-0 flex-1">
        <span className="block truncate text-sm text-fg">{name}</span>
        {card ? (
          <span className="block truncate text-xs text-fg-muted">{titleSubtitle(card)}</span>
        ) : null}
      </span>
      <Button
        type="button"
        variant="ghost"
        size="icon"
        className="size-7"
        onClick={onUp}
        disabled={first}
        aria-label={t("Move {name} up", { name })}
      >
        <ArrowUp size={14} />
      </Button>
      <Button
        type="button"
        variant="ghost"
        size="icon"
        className="size-7"
        onClick={onDown}
        disabled={last}
        aria-label={t("Move {name} down", { name })}
      >
        <ArrowDown size={14} />
      </Button>
      <Button
        type="button"
        variant="ghost"
        size="icon"
        className="size-7 hover:text-danger"
        onClick={onRemove}
        aria-label={t("Remove {name}", { name })}
      >
        <X size={14} />
      </Button>
    </div>
  );
}
