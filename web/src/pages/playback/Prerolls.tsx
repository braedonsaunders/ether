import {
  Badge,
  Button,
  EmptyState,
  PagedTable,
  Switch,
  Textarea,
  toast,
} from "@braedonsaunders/appkit-ui";
import { Clapperboard, Download, Plus } from "lucide-react";
import { useCallback, useEffect, useMemo, useState } from "react";
import { Drawer } from "../../components/Drawer";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { Field, Segmented } from "../../components/fields";
import { Section } from "../../components/layout";
import { useViewers, viewerName } from "../../components/pickers";
import { t, tn } from "../../i18n";
import { pagedTableLabels } from "../../i18n/appkit-labels";
import { useLocale } from "../../i18n/react";
import { api } from "../../lib/api";
import type { CollectionSpec } from "../../lib/contract";
import { useAsync } from "../../lib/hooks";
import { PrerollDrawer } from "./PrerollDrawer";
import { ErrorNote, PrerollThumb } from "./PrerollParts";
import { PrerollSimulation } from "./PrerollSimulation";
import { prerollInput, prerollsApi, type Preroll, type Selection } from "./prerolls-api";
import {
  clipLength,
  describeFrequency,
  describePlaysBefore,
  describeWho,
  plainError,
  problemSentence,
  videoState,
  type PlainError,
} from "./prerolls-copy";

/** How often the list is re-read while a video converts. */
const CONVERTING_POLL_MS = 3000;

type DrawerState = { open: boolean; id?: string; n: number };

/**
 * Owner pre-rolls: the table, the selection mode, the "What would play?"
 * simulator, and the drawers that add, edit and import them.
 */
export function Prerolls() {
  const { locale } = useLocale();
  const [items, setItems] = useState<Preroll[] | null>(null);
  const [selection, setSelection] = useState<Selection>("priority");
  const [loadError, setLoadError] = useState<unknown>(null);
  const [drawer, setDrawer] = useState<DrawerState>({ open: false, n: 0 });
  const [importing, setImporting] = useState(false);
  const { viewers } = useViewers();
  const collections = useAsync(() => api.collections(), []);

  const reload = useCallback(async () => {
    try {
      const result = await prerollsApi.list();
      setItems(result.prerolls ?? []);
      setSelection(result.selection === "shuffle" ? "shuffle" : "priority");
      setLoadError(null);
    } catch (e) {
      setLoadError(e);
    }
  }, []);
  useEffect(() => {
    void reload();
  }, [reload]);

  // A converting video finishes on the server; re-read until none is left.
  const converting = items?.some((p) => p.video.state === "processing") ?? false;
  useEffect(() => {
    if (!converting) return;
    const timer = setTimeout(() => void reload(), CONVERTING_POLL_MS);
    return () => clearTimeout(timer);
  }, [converting, items, reload]);

  const nameOfViewer = useCallback(
    (id: number) => {
      const v = viewers?.find((x) => x.id === id);
      return v ? viewerName(v) : t("Viewer {id}", { id });
    },

    [viewers, locale],
  );
  const nameOfCollection = useCallback(
    (id: string) =>
      (collections.data as CollectionSpec[] | undefined)?.find((c) => c.id === id)?.name ?? id,
    [collections.data],
  );

  const merge = (p: Preroll) =>
    setItems((list) => {
      const current = list ?? [];
      return current.some((x) => x.id === p.id)
        ? current.map((x) => (x.id === p.id ? p : x))
        : [...current, p];
    });

  async function toggle(p: Preroll, enabled: boolean) {
    merge({ ...p, enabled });
    try {
      merge(await prerollsApi.update(p.id, prerollInput({ ...p, enabled })));
    } catch (e) {
      merge(p);
      const err = plainError(
        e,
        enabled
          ? t("Ether couldn't turn on {name}.", { name: p.name })
          : t("Ether couldn't turn off {name}.", { name: p.name }),
      );
      toast.error(err.message, { description: err.detail });
    }
  }

  async function choose(next: Selection) {
    const before = selection;
    setSelection(next);
    try {
      await prerollsApi.selection(next);
    } catch (e) {
      setSelection(before);
      const err = plainError(e, t("Ether couldn't change how pre-rolls are picked."));
      toast.error(err.message, { description: err.detail });
    }
  }

  const open = (id?: string) => setDrawer((d) => ({ open: true, id, n: d.n + 1 }));
  const entry = drawer.id ? items?.find((p) => p.id === drawer.id) : undefined;

  const columns = useMemo(
    () => [
      {
        key: "video",
        header: t("Video"),
        search: (p: Preroll) => p.name,
        sortValue: (p: Preroll) => p.name.toLowerCase(),
        cell: (p: Preroll) => <VideoCell preroll={p} />,
      },
      {
        key: "before",
        header: t("Plays before"),
        cell: (p: Preroll) => (
          <span className="block max-w-64 text-sm text-fg">
            {describePlaysBefore(p, nameOfCollection)}
          </span>
        ),
      },
      {
        key: "who",
        header: t("Who"),
        cell: (p: Preroll) => (
          <span className="block max-w-56 text-sm text-fg-muted">
            {describeWho(p, nameOfViewer)}
          </span>
        ),
      },
      {
        key: "often",
        header: t("How often"),
        cell: (p: Preroll) => (
          <span className="text-sm text-fg-muted">{describeFrequency(p.frequency)}</span>
        ),
      },
      {
        key: "plays",
        header: t("Last 30 days"),
        align: "right" as const,
        sortValue: (p: Preroll) => p.plays30d,
        cell: (p: Preroll) => (
          <span className="whitespace-nowrap text-sm tabular-nums text-fg-muted">
            {tn(p.plays30d, "{count} play", "{count} plays")}
          </span>
        ),
      },
      {
        key: "on",
        header: t("On"),
        align: "right" as const,
        cell: (p: Preroll) => (
          // The row opens the drawer; the switch only turns the pre-roll on or off.
          <span
            className="inline-flex"
            onClick={(e) => e.stopPropagation()}
            onKeyDown={(e) => e.stopPropagation()}
          >
            <Switch
              checked={p.enabled}
              aria-label={t("Play {name}", { name: p.name })}
              onChange={(e) => void toggle(p, e.currentTarget.checked)}
            />
          </span>
        ),
      },
    ],

    [locale, nameOfCollection, nameOfViewer, items],
  );

  return (
    <div className="space-y-6">
      <Section
        title={t("Pre-rolls")}
        description={t(
          "Your own short videos, played before the feature: a welcome, a seasonal sting, a cinema intro.",
        )}
        actions={
          <>
            <Button type="button" variant="outline" size="sm" onClick={() => setImporting(true)}>
              <Download size={14} aria-hidden /> {t("Import pre-roll paths")}
            </Button>
            <Button type="button" size="sm" onClick={() => open()}>
              <Plus size={14} aria-hidden /> {t("Add pre-roll")}
            </Button>
          </>
        }
      >
        {loadError && !items ? (
          <ErrorPanel error={loadError} onRetry={() => void reload()} />
        ) : !items ? (
          <LoadingRows rows={3} />
        ) : items.length === 0 ? (
          <EmptyState
            icon={<Clapperboard />}
            title={t("No pre-rolls yet")}
            description={t(
              "Add a short video and Ether plays it before movies or episodes you choose.",
            )}
            action={
              <Button type="button" onClick={() => open()}>
                <Plus size={14} aria-hidden /> {t("Add pre-roll")}
              </Button>
            }
            className="py-10"
          />
        ) : (
          <>
            <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
              <span className="text-sm font-medium text-fg">{t("When several match")}</span>
              <Segmented<Selection>
                ariaLabel={t("When several match")}
                size="sm"
                value={selection}
                onChange={(v) => void choose(v)}
                options={[
                  { value: "priority", label: t("Highest priority plays") },
                  { value: "shuffle", label: t("Shuffle") },
                ]}
              />
              <span className="text-xs text-fg-muted">
                {selection === "priority"
                  ? t("One pre-roll plays per title. Ties are picked at random.")
                  : t("One pre-roll plays per title, picked at random from those that match.")}
              </span>
            </div>
            <PagedTable<Preroll>
              rows={items}
              rowKey={(p) => p.id}
              searchable={items.length > 8}
              pageSize={10}
              onRowClick={(p) => open(p.id)}
              labels={pagedTableLabels({ searchPlaceholder: t("Search pre-rolls…") })}
              empty={<EmptyState title={t("No pre-rolls yet")} className="py-6" />}
              columns={columns}
            />
          </>
        )}
      </Section>
      {items && items.length > 0 ? <PrerollSimulation prerolls={items} /> : null}
      {drawer.n > 0 ? (
        <PrerollDrawer
          key={drawer.n}
          open={drawer.open}
          entry={entry}
          isNew={!drawer.id}
          selection={selection}
          viewers={viewers}
          collections={(collections.data as CollectionSpec[] | undefined) ?? null}
          onClose={() => setDrawer((d) => ({ ...d, open: false }))}
          onSaved={(p) => {
            merge(p);
            setDrawer((d) => ({ ...d, id: p.id }));
          }}
          onDeleted={(id) => setItems((list) => (list ?? []).filter((p) => p.id !== id))}
        />
      ) : null}
      <ImportDrawer
        open={importing}
        onClose={() => setImporting(false)}
        onImported={(created) => {
          for (const p of created) merge(p);
        }}
      />
    </div>
  );
}

function VideoCell({ preroll: p }: { preroll: Preroll }) {
  const state = videoState(p);
  return (
    <span className="flex min-w-0 items-center gap-3">
      <PrerollThumb preroll={p} className="w-24" />
      <span className="min-w-0">
        <span className="block truncate font-medium text-fg">{p.name}</span>
        <span className="mt-0.5 block text-xs text-fg-muted">
          {state === "ready" ? (
            clipLength(p.video.durationMs)
          ) : state === "processing" ? (
            <Badge variant="info" className="whitespace-nowrap">
              {t("Converting video…")}
            </Badge>
          ) : state === "failed" ? (
            <Badge
              variant="destructive"
              className="whitespace-nowrap"
              title={problemSentence(p.video.problem)}
            >
              {t("Video problem")}
            </Badge>
          ) : (
            <Badge variant="warning" className="whitespace-nowrap">
              {t("No video yet")}
            </Badge>
          )}
        </span>
        {!p.enabled ? (
          <span className="mt-0.5 block text-xs text-fg-subtle">{t("Turned off")}</span>
        ) : null}
      </span>
    </span>
  );
}

/** Import the "Movie pre-roll video" value from Plex Media Server's Extras settings. */
function ImportDrawer({
  open,
  onClose,
  onImported,
}: {
  open: boolean;
  onClose: () => void;
  onImported: (created: Preroll[]) => void;
}) {
  const [paths, setPaths] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<PlainError | null>(null);
  useEffect(() => {
    if (open) setError(null);
  }, [open]);
  async function run() {
    setBusy(true);
    setError(null);
    try {
      const created = (await prerollsApi.importPms(paths.trim())).created ?? [];
      onImported(created);
      toast.success(
        created.length
          ? tn(created.length, "Imported {count} pre-roll", "Imported {count} pre-rolls")
          : t("Nothing to import"),
        {
          description: created.length
            ? t("Ether is converting the videos so every app can play them.")
            : undefined,
        },
      );
      setPaths("");
      onClose();
    } catch (e) {
      setError(plainError(e, t("Ether couldn't import those pre-rolls.")));
    } finally {
      setBusy(false);
    }
  }
  return (
    <Drawer
      open={open}
      onClose={onClose}
      size="md"
      title={t("Import pre-roll paths")}
      description={t(
        "Copy pre-roll file paths from another Plex server. No server connection is added.",
      )}
      footer={
        <div className="flex w-full justify-end gap-2">
          <Button type="button" variant="outline" size="sm" onClick={onClose}>
            {t("Cancel")}
          </Button>
          <Button
            type="button"
            size="sm"
            disabled={busy || !paths.trim()}
            onClick={() => void run()}
          >
            <Download size={14} aria-hidden /> {busy ? t("Importing…") : t("Import")}
          </Button>
        </div>
      }
    >
      <div className="space-y-5">
        <ol className="list-decimal space-y-1.5 pl-5 text-sm text-fg-muted">
          <li>{t("In Plex Media Server, open Settings, then Extras.")}</li>
          <li>{t("Copy everything in “Movie pre-roll video”.")}</li>
          <li>
            {t("Paste it below. Ether reads the same files, so they must be on this server.")}
          </li>
        </ol>
        <Field
          label={t("Movie pre-roll video")}
          htmlFor="preroll-import"
          hint={t(
            "Each video becomes its own pre-roll. Ether plays one per title: videos listed first win, and videos separated by commas take turns at random.",
          )}
        >
          <Textarea
            id="preroll-import"
            rows={4}
            value={paths}
            spellCheck={false}
            className="font-mono text-xs"
            placeholder="/media/prerolls/intro.mp4;/media/prerolls/sting.mp4"
            onChange={(e) => setPaths(e.currentTarget.value)}
          />
        </Field>
        <ErrorNote error={error} />
      </div>
    </Drawer>
  );
}
