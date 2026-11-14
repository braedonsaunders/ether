import { Link } from "react-router-dom";
import { Badge, Button, Input, cn } from "@braedonsaunders/appkit-ui";
import {
  AlertTriangle,
  BookOpen,
  CheckCircle2,
  Eye,
  FileCode2,
  FileJson,
  History,
  PanelLeftClose,
  PanelLeftOpen,
  Play,
  TerminalSquare,
} from "lucide-react";
import { Suspense, lazy, useCallback, useEffect, useState, type ReactNode } from "react";
import { t } from "../../i18n";

import { apiFetch, errorMessage } from "../../lib/api";
import type { StudioProject } from "./studio-types";
import { PaneResizer, useStudioPanes } from "./code/panes";

const CodeMirrorPane = lazy(() => import("./code/CodeMirrorPane"));

type Panel = "preview" | "try" | "problems" | "activity";

interface RunResult {
  status: number;
  body?: unknown;
  ms: number;
  units: number;
  logs?: string[] | null;
  hints?: { message: string; severity?: string }[] | null;
}

interface CallRecord {
  id: number;
  route: string;
  method: string;
  status: number;
  ms: number;
  error?: string;
  created: string;
}

/** Requests that exercise each kind of plugin the way Ether will call it. */
type Preset = { label: string; path: string; query?: Record<string, string> };

/** Requests an add-on draft answers, read from its own manifest. */
function addonPresets(manifestText: string | undefined): Preset[] {
  let man: {
    catalogs?: {
      type: string;
      id: string;
      name?: string;
      extra?: { name: string; isRequired?: boolean }[];
    }[];
    resources?: (string | { name: string })[];
    types?: string[];
  } = {};
  try {
    man = JSON.parse(manifestText ?? "{}");
  } catch {
    // A manifest mid-edit still gets the generic requests.
  }
  const out: Preset[] = [];
  for (const c of man.catalogs ?? []) {
    if ((c.extra ?? []).some((e) => e.isRequired && e.name !== "search")) continue;
    const search = (c.extra ?? []).some((e) => e.isRequired && e.name === "search");
    out.push({
      label: t("Catalogue: {name}", { name: c.name || `${c.type}/${c.id}` }),
      path: search
        ? `/catalog/${c.type}/${c.id}/search=night.json`
        : `/catalog/${c.type}/${c.id}.json`,
    });
    if (out.length >= 4) break;
  }
  const names = new Set((man.resources ?? []).map((r) => (typeof r === "string" ? r : r.name)));
  const type = man.types?.includes("movie") ? "movie" : (man.types?.[0] ?? "movie");
  const sample = type === "series" ? "tt0903747:1:1" : "tt0063350";
  if (names.has("meta"))
    out.push({
      label: t("Details for a title"),
      path: `/meta/${type}/${sample.split(":")[0]}.json`,
    });
  if (names.has("stream"))
    out.push({ label: t("Sources for a title"), path: `/stream/${type}/${sample}.json` });
  if (names.has("subtitles"))
    out.push({ label: t("Subtitles for a title"), path: `/subtitles/${type}/${sample}.json` });
  out.push({ label: t("Connection test"), path: "/ether/test" });
  return out;
}

function presets(project: StudioProject): Preset[] {
  if (project.template === "addon") return addonPresets(project.files["manifest.json"]);
  switch (project.template) {
    case "channels":
      return [
        { label: t("Channel lineup"), path: "/ether/channels.json" },
        { label: t("Guide for one channel"), path: "/ether/guide/{channel}.json" },
        { label: t("Stream for one channel"), path: "/stream/tv/{channel}.json" },
        { label: t("Connection test"), path: "/ether/test" },
      ];
    case "video_source":
      return [
        {
          label: t("Sources for a title"),
          path: "/stream/movie/tt0063350.json",
          query: { title: "Night of the Living Dead", year: "1968" },
        },
        { label: t("Open a source"), path: "/ether/open/{source}" },
        { label: t("Connection test"), path: "/ether/test" },
      ];
    default:
      return [{ label: t("Health"), path: "/ether/health" }];
  }
}

/**
 * The code view of Studio: files, a typed editor and the tools to try the
 * draft against the real server before installing it.
 */
export function CodeStudio({
  project,
  file,
  source,
  edits,
  dirty,
  saving,
  epoch,
  preview,
  onFile,
  onChange,
  onSave,
}: {
  project: StudioProject | null;
  file: string;
  source: string;
  edits: Record<string, string>;
  dirty: boolean;
  saving: boolean;
  /** Bumps after every save, so panels re-check the saved draft. */
  epoch: number;
  preview: ReactNode;
  onFile: (f: string) => void;
  onChange: (v: string) => void;
  onSave: () => void;
}) {
  const [panel, setPanel] = useState<Panel>("preview");
  const panes = useStudioPanes();
  const unsaved = dirty || Object.keys(edits).length > 0;
  const [install, setInstall] = useState<{ ok: boolean; message: string } | null>(null);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === "s") {
        e.preventDefault();
        if (unsaved) onSave();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [unsaved, onSave]);

  // The real install check, re-run on every save.
  useEffect(() => {
    if (!project) return;
    let live = true;
    apiFetch(`/plugins/studio/projects/${encodeURIComponent(project.id)}/install/preview`, {
      method: "POST",
      body: "{}",
    })
      .then(() => live && setInstall({ ok: true, message: t("Passes the install check") }))
      .catch((e) => live && setInstall({ ok: false, message: errorMessage(e) }));
    return () => {
      live = false;
    };
  }, [project?.id, epoch]);

  if (!project) {
    return (
      <div className="flex flex-1 items-center justify-center p-8 text-sm text-fg-muted">
        {t("Loading the draft…")}
      </div>
    );
  }
  const files = Object.keys(project.files).sort((a, b) =>
    a === "src/plugin.ts" ? -1 : b === "src/plugin.ts" ? 1 : a.localeCompare(b),
  );
  const diags = project.diagnostics || [];
  const problems = diags.length + (install && !install.ok ? 1 : 0);
  const status = unsaved
    ? { tone: "text-warning", text: t("Unsaved changes") }
    : diags.length
      ? { tone: "text-danger", text: t("Does not compile") }
      : install && !install.ok
        ? { tone: "text-warning", text: t("Compiles; install check has notes") }
        : { tone: "text-success", text: t("Compiles and ready to install") };

  return (
    <div ref={panes.grid} className={panes.className} style={panes.style}>
      <aside
        aria-label={t("Files")}
        className="relative hidden min-h-0 flex-col border-r border-border bg-bg-subtle lg:flex"
      >
        {panes.collapsed ? (
          // Folded: a rail with the way back out and one icon per file.
          <div className="flex flex-col items-center gap-1 py-2">
            <button
              type="button"
              onClick={() => panes.setCollapsed(false)}
              aria-label={t("Show files")}
              title={t("Show files")}
              className="mb-1 rounded-md p-1.5 text-fg-muted hover:bg-surface-hover hover:text-fg"
            >
              <PanelLeftOpen size={16} />
            </button>
            {files.map((f) => (
              <button
                key={f}
                type="button"
                onClick={() => onFile(f)}
                aria-label={f}
                title={f}
                aria-current={file === f ? "page" : undefined}
                className={cn(
                  "relative rounded-md p-1.5 transition",
                  file === f
                    ? "bg-primary/10 text-fg"
                    : "text-fg-muted hover:bg-surface-hover hover:text-fg",
                )}
              >
                {f.endsWith(".ts") ? <FileCode2 size={15} /> : <FileJson size={15} />}
                {(f === file ? dirty : f in edits) ? (
                  <span className="absolute right-1 top-1 size-1.5 rounded-full bg-primary" />
                ) : null}
              </button>
            ))}
          </div>
        ) : (
          <>
            <div className="flex items-center justify-between px-3 pb-1 pt-2">
              <p className="text-[11px] font-semibold uppercase tracking-wider text-fg-subtle">
                {t("Files")}
              </p>
              <button
                type="button"
                onClick={() => panes.setCollapsed(true)}
                aria-label={t("Hide files")}
                title={t("Hide files")}
                className="rounded-md p-1 text-fg-subtle hover:bg-surface-hover hover:text-fg"
              >
                <PanelLeftClose size={15} />
              </button>
            </div>
            <ul className="space-y-0.5 px-1.5">
              {files.map((f) => (
                <li key={f}>
                  <button
                    type="button"
                    onClick={() => onFile(f)}
                    aria-current={file === f ? "page" : undefined}
                    className={cn(
                      "flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left font-mono text-xs transition",
                      file === f
                        ? "bg-primary/10 text-fg"
                        : "text-fg-muted hover:bg-surface-hover hover:text-fg",
                    )}
                  >
                    {f.endsWith(".ts") ? <FileCode2 size={14} /> : <FileJson size={14} />}
                    <span className="min-w-0 flex-1 truncate">{f}</span>
                    {(f === file ? dirty : f in edits) ? (
                      <span
                        className="size-1.5 rounded-full bg-primary"
                        aria-label={t("Not saved")}
                      />
                    ) : null}
                  </button>
                </li>
              ))}
            </ul>
            <div className="mt-auto space-y-2 border-t border-border p-3 text-xs text-fg-muted">
              <p className="font-medium text-fg">{project.name}</p>
              <p className="break-all font-mono text-[11px]">{project.pluginId}</p>
            </div>
            <PaneResizer
              side="right"
              width={panes.left}
              bounds={panes.leftBounds}
              onWidth={panes.setLeft}
              onReset={panes.resetLeft}
              label={t("Resize files")}
            />
          </>
        )}
      </aside>

      <section aria-label={t("Code")} className="flex min-h-[32rem] flex-col bg-surface lg:min-h-0">
        <div className="flex shrink-0 items-center gap-2 border-b border-border bg-bg-subtle px-3 py-1.5">
          <span className="font-mono text-xs text-fg">{file}</span>
          <span className={cn("ml-auto text-xs", status.tone)}>{status.text}</span>
          <Button size="sm" disabled={saving || !unsaved} onClick={onSave}>
            <Play size={13} /> {saving ? t("Saving…") : t("Save and run")}
          </Button>
        </div>
        <div className="min-h-0 flex-1">
          <Suspense fallback={<p className="p-4 text-xs text-fg-muted">{t("Loading editor…")}</p>}>
            <CodeMirrorPane
              key={file}
              file={file === "src/plugin.ts" ? "plugin.ts" : file}
              value={source}
              onChange={onChange}
            />
          </Suspense>
        </div>
        <div className="flex shrink-0 items-center gap-3 border-t border-border bg-bg-subtle px-3 py-1 text-[11px] text-fg-muted">
          <span>{file.endsWith(".ts") ? "TypeScript" : "JSON"}</span>
          <span>⌘S {t("saves, compiles and refreshes the preview")}</span>
        </div>
      </section>

      <section
        aria-label={t("Tools")}
        className="relative flex min-h-[28rem] flex-col border-t border-border lg:min-h-0 lg:border-l lg:border-t-0"
      >
        <PaneResizer
          side="left"
          width={panes.right}
          bounds={panes.rightBounds}
          onWidth={panes.setRight}
          onReset={panes.resetRight}
          label={t("Resize tools")}
        />
        <div
          role="tablist"
          className="flex shrink-0 items-center gap-0.5 border-b border-border bg-bg-subtle px-1.5"
        >
          {(
            [
              ["preview", <Eye size={14} key="i" />, t("Preview")],
              ["try", <TerminalSquare size={14} key="i" />, t("Try it")],
              ["problems", <AlertTriangle size={14} key="i" />, t("Problems")],
              ["activity", <History size={14} key="i" />, t("Activity")],
            ] as [Panel, ReactNode, string][]
          ).map(([key, icon, label]) => (
            <button
              key={key}
              role="tab"
              type="button"
              aria-selected={panel === key}
              onClick={() => setPanel(key)}
              className={cn(
                "-mb-px inline-flex h-9 items-center gap-1.5 border-b-2 px-2.5 text-xs transition",
                panel === key
                  ? "border-primary text-fg"
                  : "border-transparent text-fg-muted hover:text-fg",
              )}
            >
              {icon}
              {label}
              {key === "problems" && problems ? (
                <Badge variant="warning" className="px-1.5 py-0 text-[10px]">
                  {problems}
                </Badge>
              ) : null}
            </button>
          ))}
        </div>
        <div className="app-scroll min-h-0 flex-1 overflow-y-auto p-4">
          {panel === "preview" ? preview : null}
          {panel === "try" ? <TryIt project={project} epoch={epoch} /> : null}
          {panel === "problems" ? (
            <ul className="space-y-2 text-sm">
              {diags.map((d, i) => (
                <li
                  key={i}
                  className="rounded-lg border border-danger/30 bg-danger/5 p-2.5 font-mono text-xs text-danger"
                >
                  {d.file}:{d.line}:{d.column} {d.text}
                </li>
              ))}
              {install ? (
                <li
                  className={cn(
                    "flex items-start gap-2 rounded-lg border p-2.5 text-xs",
                    install.ok
                      ? "border-success/30 bg-success/5 text-fg"
                      : "border-warning/30 bg-warning/5 text-fg",
                  )}
                >
                  {install.ok ? (
                    <CheckCircle2 size={14} className="mt-0.5 shrink-0 text-success" />
                  ) : (
                    <AlertTriangle size={14} className="mt-0.5 shrink-0 text-warning" />
                  )}
                  <span className="break-words">{install.message}</span>
                </li>
              ) : null}
              {!diags.length && install?.ok ? (
                <li className="text-xs text-fg-muted">
                  {t("No problems. Compiles and passes the install check.")}
                </li>
              ) : null}
            </ul>
          ) : null}
          {panel === "activity" ? <Activity project={project} epoch={epoch} /> : null}
        </div>
      </section>
    </div>
  );
}

function TryIt({ project, epoch }: { project: StudioProject; epoch: number }) {
  const options = presets(project);
  const [path, setPath] = useState(options[0]!.path);
  const [query, setQuery] = useState(
    Object.entries(options[0]!.query ?? {})
      .map(([k, v]) => `${k}=${v}`)
      .join("&"),
  );
  const [result, setResult] = useState<RunResult | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const run = useCallback(async () => {
    setBusy(true);
    setError("");
    try {
      const q = Object.fromEntries(new URLSearchParams(query));
      setResult(
        await apiFetch<RunResult>(
          `/plugins/studio/projects/${encodeURIComponent(project.id)}/run`,
          {
            method: "POST",
            body: JSON.stringify({ method: "GET", path, query: q }),
          },
        ),
      );
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  }, [project.id, path, query]);
  useEffect(() => setResult(null), [epoch]);
  const body = result ? JSON.stringify(result.body, null, 2) : "";
  return (
    <div className="space-y-3">
      <div className="flex flex-wrap gap-1.5">
        {options.map((o) => (
          <button
            key={o.path}
            type="button"
            onClick={() => {
              setPath(o.path);
              setQuery(
                Object.entries(o.query ?? {})
                  .map(([k, v]) => `${k}=${v}`)
                  .join("&"),
              );
            }}
            className={cn(
              "rounded-full border px-2.5 py-1 text-xs transition",
              path === o.path
                ? "border-primary/50 bg-primary/10 text-fg"
                : "border-border text-fg-muted hover:text-fg",
            )}
          >
            {o.label}
          </button>
        ))}
      </div>
      <div className="space-y-2">
        <Input
          aria-label={t("Route")}
          value={path}
          onChange={(e) => setPath(e.currentTarget.value)}
          className="font-mono text-xs"
        />
        <Input
          aria-label={t("Query")}
          value={query}
          placeholder="title=…&year=…"
          onChange={(e) => setQuery(e.currentTarget.value)}
          className="font-mono text-xs"
        />
        {/\{(channel|source)\}/.test(path) ? (
          <p className="text-[11px] text-fg-muted">
            {t("Replace {channel} or {source} with an id from an earlier response.", {
              channel: "{channel}",
              source: "{source}",
            })}
          </p>
        ) : null}
        <Button size="sm" disabled={busy} onClick={() => void run()}>
          <Play size={13} /> {busy ? t("Running…") : t("Send")}
        </Button>
      </div>
      {error ? (
        <p role="alert" className="text-sm text-danger">
          {error}
        </p>
      ) : null}
      {result ? (
        <div className="space-y-2">
          <div className="flex flex-wrap items-center gap-2 text-xs">
            <Badge variant={result.status < 400 ? "success" : "destructive"}>
              HTTP {result.status}
            </Badge>
            <span className="text-fg-muted">{result.ms} ms</span>
            <span className="text-fg-muted">
              {t("{units} budget units", { units: result.units })}
            </span>
          </div>
          {result.hints?.map((h, i) => (
            <p key={i} className="rounded-md bg-warning/10 p-2 text-xs text-fg">
              {h.message}
            </p>
          ))}
          <pre className="max-h-96 overflow-auto rounded-lg border border-border bg-bg-subtle p-3 font-mono text-[11px] leading-relaxed text-fg">
            {body.length > 60000 ? body.slice(0, 60000) + "\n…" : body}
          </pre>
          {result.logs?.length ? (
            <pre className="max-h-40 overflow-auto rounded-lg border border-border bg-bg-subtle p-3 font-mono text-[11px] text-fg-muted">
              {result.logs.join("\n")}
            </pre>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

function Activity({ project, epoch }: { project: StudioProject; epoch: number }) {
  const [calls, setCalls] = useState<CallRecord[] | null>(null);
  useEffect(() => {
    let live = true;
    apiFetch<{ calls: CallRecord[] }>(
      `/plugins/studio/projects/${encodeURIComponent(project.id)}/calls`,
    )
      .then((r) => live && setCalls([...(r.calls ?? [])].reverse()))
      .catch(() => live && setCalls([]));
    return () => {
      live = false;
    };
  }, [project.id, epoch]);
  if (!calls) return <p className="text-xs text-fg-muted">{t("Loading…")}</p>;
  if (!calls.length)
    return (
      <p className="text-xs text-fg-muted">
        {t("Every call Ether makes to the draft shows up here.")}
      </p>
    );
  return (
    <ul className="divide-y divide-border rounded-lg border border-border">
      {calls.slice(0, 50).map((c) => (
        <li key={c.id} className="flex items-center gap-2 px-3 py-2 text-xs">
          <Badge
            variant={c.status < 400 && !c.error ? "success" : "destructive"}
            className="shrink-0"
          >
            {c.status || "ERR"}
          </Badge>
          <span className="min-w-0 flex-1 truncate font-mono">
            {c.method} {c.route}
          </span>
          <span className="shrink-0 text-fg-muted">{c.ms} ms</span>
          {c.error ? <span className="w-full truncate text-danger">{c.error}</span> : null}
        </li>
      ))}
    </ul>
  );
}

export function ReferenceButton({ template: _template }: { template: string }) {
  return (
    <Link
      to="/plugins/reference/getting-started"
      className="inline-flex items-center gap-2 rounded-lg border border-border px-3 py-2 text-sm"
    >
      <BookOpen size={17} aria-hidden />
      {t("Reference")}
    </Link>
  );
}
export function Reference({ template: _template }: { template: string }) {
  return <ReferenceButton template={_template} />;
}
