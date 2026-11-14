import {
  useEffect,
  useMemo,
  useRef,
  useState,
  isValidElement,
  type ReactElement,
  createElement,
} from "react";
import { Link, useParams, useSearchParams, useLocation } from "react-router-dom";
import ReactMarkdown, { type Components } from "react-markdown";
import remarkGfm from "remark-gfm";
import { Button, buttonVariants } from "@braedonsaunders/appkit-ui";
import { DomainPage } from "../../components/layout";
import { t } from "../../i18n";
import reference from "./developer-reference.json";
import guides from "./reference-guides.json";
import "./reference.css";

const corpus = reference.map((section) => ({
  ...section,
  search: `${section.path}\n${section.code}`.toLowerCase(),
}));
function matches(path: string, _code: string, patterns: string[]) {
  return patterns.some((pattern) =>
    pattern.startsWith("@host:")
      ? (new RegExp(pattern.slice(6), "i").test(path.split("#")[1] ?? "") &&
          /host|contract|sdk/.test(path)) ||
        (pattern.startsWith("@host:") && path.includes("sdk/typescript/src/host.ts"))
      : path.startsWith(pattern),
  );
}
// Heading IDs share Markdown's source line positions, including duplicate titles.
function destination(text: string, prefix: string, fallback: string, terms: string[]) {
  const lower = text.toLowerCase();
  const hits = terms.map((term) => lower.indexOf(term)).filter((at) => at >= 0);
  const at = hits.length ? Math.min(...hits) : 0;
  let heading = fallback,
    anchor = prefix,
    offset = 0,
    fence = "";
  for (const [index, line] of text.split("\n").entries()) {
    if (offset > at) break;
    const marker = line.match(/^\s*(`{3,}|~{3,})/);
    if (marker) {
      if (!fence) fence = marker[1]![0]!;
      else if (marker[1]![0] === fence) fence = "";
    }
    const match = !fence && line.match(/^#{1,6}\s+(.+?)\s*#*$/);
    if (match) {
      heading = match[1]!;
      anchor = `${prefix}:heading:${index + 1}`;
    }
    offset += line.length + 1;
  }
  return { heading, anchor };
}
function snippet(text: string, terms: string[]) {
  const plain = text
    .replace(/^\s*(`{3,}|~{3,}).*$/gm, "")
    .replace(/!?(\[([^\]]+)\])\([^)]*\)/g, "$2")
    .replace(/^\s*#{1,6}\s+/gm, "")
    .replace(/`+/g, "")
    .replace(/\*\*|__/g, "")
    .replace(/\s+/g, " ")
    .trim();
  const lower = plain.toLowerCase();
  const hits = terms.map((term) => lower.indexOf(term)).filter((at) => at >= 0);
  const at = hits.length ? Math.min(...hits) : 0;
  let start = Math.max(0, at - 100),
    end = Math.min(plain.length, at + 380);
  // Keep complete whitespace-delimited words, including code identifiers.
  while (start > 0 && !/\s/.test(plain[start - 1]!)) start--;
  while (end < plain.length && !/\s/.test(plain[end]!)) end++;
  return `${start ? "… " : ""}${plain.slice(start, end).trim()}${end < plain.length ? " …" : ""}`;
}
function Highlight({ text, terms }: { text: string; terms: string[] }) {
  const escaped = terms.map((term) => term.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"));
  const parts = text.split(new RegExp(`(${escaped.join("|")})`, "gi"));
  return (
    <>
      {parts.map((part, i) =>
        terms.includes(part.toLowerCase()) ? (
          <mark key={i} className="rounded bg-amber-300/20 px-0.5 text-inherit font-semibold">
            {part}
          </mark>
        ) : (
          part
        ),
      )}
    </>
  );
}
function Copy({ value }: { value: string }) {
  const [state, setState] = useState<"idle" | "done" | "error">("idle");
  return (
    <Button
      size="sm"
      variant="outline"
      onClick={() => {
        if (!navigator.clipboard) {
          setState("error");
          return;
        }
        void navigator.clipboard.writeText(value).then(
          () => setState("done"),
          () => setState("error"),
        );
      }}
    >
      {state === "done" ? t("Copied") : state === "error" ? t("Copy failed") : t("Copy")}
    </Button>
  );
}
function Markdown({ text, anchorPrefix }: { text: string; anchorPrefix: string }) {
  const headings = Object.fromEntries(
    [1, 2, 3, 4, 5, 6].map((level) => [
      `h${level}`,
      (({ node, children }) =>
        createElement(
          `h${level}`,
          {
            id: `${anchorPrefix}:heading:${node?.position?.start.line}`,
            className: "scroll-mt-20",
          },
          children,
        )) as Components["h1"],
    ]),
  ) as Components;
  return (
    <ReactMarkdown
      remarkPlugins={[remarkGfm]}
      components={{
        ...headings,
        a: ({ href, children }) =>
          href?.startsWith("/plugins/reference/") ? (
            <Link to={href}>{children}</Link>
          ) : (
            <a
              href={
                href?.startsWith("http") || href?.startsWith("#")
                  ? href
                  : `https://github.com/braedonsaunders/ether/tree/main/${href?.replace(/^(\.\.\/)+/, "") ?? ""}`
              }
            >
              {children}
            </a>
          ),
        pre: ({ children }) => {
          const value = isValidElement(children)
            ? String((children as ReactElement<{ children?: unknown }>).props.children ?? "")
            : String(children);
          return (
            <div>
              <pre>{children}</pre>
              <Copy value={value} />
            </div>
          );
        },
      }}
    >
      {text}
    </ReactMarkdown>
  );
}
export default function DeveloperReference() {
  const location = useLocation();
  const { topic = "getting-started" } = useParams();
  const [params, setParams] = useSearchParams();
  const [query, setQuery] = useState(() => params.get("q") ?? "");
  useEffect(() => {
    setQuery(params.get("q") ?? "");
  }, [location.pathname, location.search]);
  useEffect(() => {
    if (query === (params.get("q") ?? "")) return;
    const timer = window.setTimeout(
      () => setParams(query ? { q: query } : {}, { replace: true }),
      150,
    );
    return () => window.clearTimeout(timer);
  }, [query, location.pathname, params, setParams]);
  const input = useRef<HTMLInputElement>(null);
  const article = useRef<HTMLElement>(null);
  const guide = guides.find((g) => g.id === topic);
  const terms = query.toLowerCase().trim().split(/\s+/).filter(Boolean);
  const results = useMemo(
    () =>
      terms.length
        ? [
            ...guides
              .filter((g) =>
                terms.every((term) => `${g.title} ${g.body}`.toLowerCase().includes(term)),
              )
              .map((g) => ({
                path: g.title,
                code: g.body,
                topic: g.id,
                topicTitle: g.title,
                ...destination(g.body, `guide:${g.id}`, g.title, terms),
              })),
            ...corpus
              .filter((s) => terms.every((term) => s.search.includes(term)))
              .map((s) => {
                const owner = guides.find((g) => matches(s.path, s.code, g.patterns))!;
                const label = s.path.split("#")[1] ?? s.path.split("/").at(-1) ?? s.path;
                return {
                  ...s,
                  topic: owner.id,
                  topicTitle: owner.title,
                  ...(s.path.endsWith(".md") && owner.id !== "all-contract"
                    ? destination(s.code, s.path, label, terms)
                    : { heading: label, anchor: s.path }),
                };
              }),
          ]
        : [],
    [query],
  ); // Local immutable corpus; no server or indexer calls.
  useEffect(() => {
    const key = (e: KeyboardEvent) => {
      if (
        ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") ||
        (e.key === "/" &&
          !(
            e.target instanceof HTMLInputElement ||
            e.target instanceof HTMLTextAreaElement ||
            (e.target instanceof HTMLElement && e.target.isContentEditable)
          ))
      ) {
        e.preventDefault();
        e.stopImmediatePropagation();
        input.current?.focus();
        input.current?.select();
      }
      if (e.key === "Escape" && document.activeElement === input.current) {
        setQuery("");
        setParams({}, { replace: true });
        input.current?.blur();
      }
    };
    window.addEventListener("keydown", key, true);
    return () => window.removeEventListener("keydown", key, true);
  }, [setParams]);
  useEffect(() => {
    if (!location.hash) return;
    let id: string;
    try {
      id = decodeURIComponent(location.hash.slice(1));
    } catch {
      return;
    }
    const target = document.getElementById(id);
    if (target instanceof HTMLDetailsElement) target.open = true;
    const details = target?.closest("details");
    if (details) details.open = true;
    target?.scrollIntoView({ block: "start" });
  }, [topic, location.hash, query]);
  const sections = guide ? corpus.filter((s) => matches(s.path, s.code, guide.patterns)) : [];
  return (
    <DomainPage
      title={t("Developer reference")}
      base="/plugins"
      actions={
        <Link
          className={buttonVariants({ variant: "outline", size: "sm" })}
          to="/plugins/installed"
        >
          {t("Plugins")}
        </Link>
      }
    >
      <div className="space-y-2">
        <label className="block text-sm font-medium" htmlFor="reference-search">
          {t("Search API reference")}
        </label>
        <input
          ref={input}
          id="reference-search"
          type="search"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder={t("Search")}
          className="w-full rounded-lg border border-border bg-surface px-4 py-3 text-sm"
        />
        <p className="text-xs text-fg-muted">
          {t("Keyboard shortcut")}: <kbd>⌘ / Ctrl K</kbd>
        </p>
      </div>
      <div className="grid items-start gap-6 lg:grid-cols-[15rem_minmax(0,1fr)]">
        <nav
          aria-label={t("Topics")}
          className="max-h-[65vh] overflow-auto rounded-xl border border-border p-3 lg:sticky lg:top-4"
        >
          {[...new Set(guides.map((g) => g.group))].map((group) => (
            <div key={group} className="mb-5">
              <p className="mb-2 px-3 text-xs font-semibold text-fg-muted">{group}</p>
              {guides
                .filter((g) => g.group === group)
                .map((g) => (
                  <Link
                    key={g.id}
                    to={`/plugins/reference/${g.id}`}
                    aria-current={!terms.length && topic === g.id ? "page" : undefined}
                    className={`block rounded-lg px-3 py-2 text-sm hover:bg-surface ${!terms.length && topic === g.id ? "bg-surface font-semibold text-accent" : "text-fg-muted"}`}
                  >
                    {g.title}
                  </Link>
                ))}
            </div>
          ))}
        </nav>
        <article ref={article} className="dev-reference min-w-0 space-y-6" lang="en">
          {terms.length ? (
            <>
              <h2 className="text-xl font-semibold">
                {t("Search results")} <span className="text-fg-muted">({results.length})</span>
              </h2>
              {results.length === 0 ? <p>{t("No results")}</p> : null}
              {results.slice(0, 100).map((s) => {
                return (
                  <section key={s.path} className="rounded-xl border border-border p-4">
                    <Link
                      className="break-all font-medium text-accent"
                      to={`/plugins/reference/${s.topic}#${encodeURIComponent(s.anchor)}`}
                    >
                      {s.topicTitle} › {s.heading}
                    </Link>
                    <p className="mt-3 whitespace-pre-wrap break-words text-sm text-fg-muted">
                      <Highlight text={snippet(s.code, terms)} terms={terms} />
                    </p>
                  </section>
                );
              })}
              {results.length > 100 ? <p>{t("Refine your search")}</p> : null}
            </>
          ) : guide ? (
            <>
              <h1 id={`guide:${guide.id}`} className="scroll-mt-20 text-2xl font-semibold">
                {guide.title}
              </h1>
              <Markdown text={guide.body} anchorPrefix={`guide:${guide.id}`} />
              {sections.map((s) =>
                s.path.endsWith(".md") && guide.id !== "all-contract" ? (
                  <section key={s.path} id={s.path} className="scroll-mt-20">
                    <h2 className="break-all text-base font-semibold">{s.path}</h2>
                    <Markdown text={s.code} anchorPrefix={s.path} />
                  </section>
                ) : (
                  <details
                    key={s.path}
                    id={s.path}
                    className="scroll-mt-20 rounded-xl border border-border p-4"
                  >
                    <summary className="cursor-pointer break-all font-mono text-xs">
                      {s.path}
                    </summary>
                    <div className="mt-3">
                      <Copy value={s.code} />
                      <pre className="mt-3">
                        <code>{s.code}</code>
                      </pre>
                    </div>
                  </details>
                ),
              )}
            </>
          ) : (
            <>
              <h1>{t("No results")}</h1>
              <Link to="/plugins/reference/getting-started">{t("Reference")}</Link>
            </>
          )}
        </article>
      </div>
    </DomainPage>
  );
}
