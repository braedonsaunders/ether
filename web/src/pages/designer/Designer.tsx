import { Badge, Button, EmptyState, SubtabNav, Spinner, toast } from "@braedonsaunders/appkit-ui";
import { RefreshCw, Save, Undo2, Users } from "lucide-react";
import { useMemo, useState } from "react";
import { ErrorPanel } from "../../components/feedback";
import { useViewers, viewerName } from "../../components/pickers";
import { t } from "../../i18n";
import { api, errorMessage } from "../../lib/api";
import type { HomeLayout } from "../../lib/contract";
import { useAsync } from "../../lib/hooks";
import { Workspace, PanelHead } from "../../components/workspace";
import { EMPTY_LAYOUT, overrideCount, prune, sameLayout, type LayerKey } from "../../lib/layout";
import { ContextBar, type ViewContext } from "./ContextBar";
import { HomeCanvas } from "./HomeCanvas";
import { LayerSwitcher } from "./LayerSwitcher";
import { OverrideList } from "./OverrideList";
import { PageSettings } from "./PageSettings";
import { RowPool } from "./RowPool";
import { AnchoredSlots } from "./AnchoredSlots";
import { TuningPanel } from "./TuningPanel";

function initialContext(): ViewContext {
  const now = new Date();
  return { device: "tv", day: now.getDay(), hour: now.getHours(), section: "home" };
}

/**
 * Home: the native Plex Home (each viewer's For You) for everyone, personalization
 * within it, and per-viewer overrides on top. Controls on the left third,
 * the live Plex Home (rendered server-side from the draft) on the right.
 */
export default function Designer() {
  const { viewers, error: viewersError } = useViewers();
  const doc = useAsync(() => api.layout(), []);
  const [draft, setDraft] = useState<HomeLayout>(EMPTY_LAYOUT);
  const [layer, setLayer] = useState<LayerKey>("global");
  const [spotCheck, setSpotCheck] = useState<number | null>(null);
  const [ctx, setCtx] = useState<ViewContext>(initialContext);
  const [saving, setSaving] = useState(false);
  const [nonce, setNonce] = useState(0);
  const [tools, setTools] = useState("layout");
  const [focusRow, setFocusRow] = useState<string | null>(null);

  const saved = doc.data?.layout ?? EMPTY_LAYOUT;
  // Adopt the stored layout in the render that delivers it; an effect could
  // replace an edit made before it ran.
  const [adopted, setAdopted] = useState<typeof doc.data>(undefined);
  if (doc.data && doc.data !== adopted) {
    setAdopted(doc.data);
    setDraft(prune(doc.data.layout));
  }
  const dirty = Boolean(doc.data) && !sameLayout(draft, saved);
  const available = (doc.data?.availableRows ?? []).map((row) =>
    row.kind === "favorites"
      ? { ...row, title: t("Favorites"), description: t("Titles this viewer marked as favorites.") }
      : row,
  );
  const defaults = doc.data?.defaults.maxRows ?? { home: 14, movies: 10, shows: 10 };

  const viewerId = layer === "global" ? spotCheck : layer;
  const overrideTotal = (viewers ?? []).reduce((n, v) => n + overrideCount(draft, v.id), 0);
  const viewerById = useMemo(() => new Map((viewers ?? []).map((v) => [v.id, v])), [viewers]);
  const layerName =
    layer === "global"
      ? t("everyone's Home")
      : viewerById.get(layer)
        ? viewerName(viewerById.get(layer)!)
        : t("Viewer {id}", { id: layer });

  const save = async () => {
    setSaving(true);
    try {
      const next = await api.putLayout(draft);
      doc.setData(next);
      setDraft(prune(next.layout));
      setNonce((n) => n + 1);
      toast.success(t("Home layout saved"), {
        description: t("Plex Homes follow it on their next load."),
      });
    } catch (e) {
      toast.error(t("Couldn't save the layout"), { description: errorMessage(e) });
    } finally {
      setSaving(false);
    }
  };

  const refreshRows = async () => {
    try {
      if (viewerId !== null) await api.refreshViewer(viewerId);
      setNonce((n) => n + 1);
      toast.success(viewerId !== null ? t("Rows rebuilt") : t("Preview refreshed"));
    } catch (e) {
      toast.error(t("Couldn't rebuild rows"), { description: errorMessage(e) });
    }
  };

  const actions = (
    <div className="flex flex-wrap items-center gap-2">
      {dirty ? <Badge variant="warning">{t("Unsaved changes")}</Badge> : null}
      <Button variant="ghost" size="sm" onClick={() => void refreshRows()}>
        <RefreshCw size={14} /> {viewerId !== null ? t("Rebuild rows") : t("Refresh")}
      </Button>
      <Button
        variant="outline"
        size="sm"
        onClick={() => setDraft(prune(saved))}
        disabled={!dirty || saving}
      >
        <Undo2 size={14} /> {t("Discard")}
      </Button>
      <Button size="sm" onClick={() => void save()} disabled={!dirty || saving}>
        <Save size={14} /> {saving ? t("Saving…") : t("Save layout")}
      </Button>
    </div>
  );

  const editing = {
    draft,
    onChange: setDraft,
    layer,
    section: ctx.section,
    viewers: viewers ?? [],
  };

  const switchLayer = (l: LayerKey) => {
    setLayer(l);
    setFocusRow(null);
  };

  const state = viewersError ? (
    <div className="p-6">
      <ErrorPanel error={viewersError} />
    </div>
  ) : doc.error ? (
    <div className="p-6">
      <ErrorPanel error={doc.error} onRetry={() => void doc.reload()} />
    </div>
  ) : !viewers || !doc.data ? (
    <div className="grid flex-1 place-items-center">
      <Spinner size={22} label={t("Loading the Home layout")} />
    </div>
  ) : viewers.length === 0 ? (
    <div className="p-6">
      <EmptyState
        icon={<Users />}
        title={t("No viewers yet")}
        description={t(
          "Share this server from plex.tv; each person appears here once they sign in.",
        )}
      />
    </div>
  ) : null;

  return (
    <Workspace
      title={t("Home")}
      subtitle={t(
        "Shape the native Plex Home: every viewer's personalized For You, with per-viewer overrides on top.",
      )}
      actions={actions}
      asideLabel={t("Designer controls")}
      mainLabel={t("Home preview")}
      mainClassName="p-3 sm:p-6"
      aside={
        viewers ? (
          <>
            <PanelHead>
              <div
                className="flex min-w-0 flex-wrap items-center gap-1"
                aria-label={t("Editing and context")}
              >
                <LayerSwitcher
                  layer={layer}
                  onChange={switchLayer}
                  viewers={viewers}
                  draft={draft}
                />
                <ContextBar ctx={ctx} onChange={setCtx} className="shrink-0" />
              </div>
              <SubtabNav
                ariaLabel={t("Designer tools")}
                active={tools}
                onSelect={setTools}
                tabs={[
                  { key: "layout", label: t("Layout") },
                  { key: "rows", label: t("Rows") },
                  { key: "tuning", label: t("Tuning") },
                  {
                    key: "overrides",
                    label: t("Overrides"),
                    count: overrideTotal || undefined,
                  },
                ]}
              />
            </PanelHead>
            <div className="space-y-5 p-3 sm:p-4">
              {tools === "layout" && (
                <>
                  <PageSettings {...editing} defaults={defaults} />
                  <AnchoredSlots {...editing} available={available} />
                </>
              )}
              {tools === "rows" && (
                <RowPool
                  {...editing}
                  available={available}
                  focusRow={focusRow}
                  onFocusRow={setFocusRow}
                />
              )}
              {tools === "tuning" && <TuningPanel {...editing} />}
              {tools === "overrides" && <OverrideList {...editing} onOpenLayer={switchLayer} />}
            </div>
          </>
        ) : null
      }
      main={
        viewers ? (
          <HomeCanvas
            draft={draft}
            dirty={dirty}
            saved={saved}
            layer={layer}
            layerName={layerName}
            ctx={ctx}
            viewers={viewers}
            viewerId={viewerId}
            onSpotCheck={setSpotCheck}
            refreshNonce={nonce}
            onChange={setDraft}
            onEditRow={(row) => {
              setTools("rows");
              setFocusRow(row);
            }}
          />
        ) : null
      }
    >
      {state}
    </Workspace>
  );
}
