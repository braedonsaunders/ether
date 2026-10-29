// Message extraction and catalogue checks shared by `pnpm i18n` and the
// catalogue test. English source strings are the keys; see src/i18n/index.ts.
import { readdirSync, readFileSync, existsSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import {
  DEFAULT_LOCALE,
  SUPPORTED_LOCALES,
  messageProblems,
  systemMessageKey,
} from "@braedonsaunders/appkit-i18n";
import ts from "typescript";

export const webRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
export const srcRoot = path.join(webRoot, "src");
export const localesDir = path.join(srcRoot, "i18n", "locales");

const SKIP_DIRS = new Set(["test", "mock", "node_modules"]);

/** Every shipped source file: no tests, no in-browser mock backend. */
export function sourceFiles(dir = srcRoot) {
  const out = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      if (!SKIP_DIRS.has(entry.name)) out.push(...sourceFiles(full));
    } else if (/\.tsx?$/.test(entry.name) && !/\.test\.tsx?$/.test(entry.name)) {
      out.push(full);
    }
  }
  return out.sort();
}

function parse(file) {
  return ts.createSourceFile(
    file,
    readFileSync(file, "utf8"),
    ts.ScriptTarget.Latest,
    true,
    file.endsWith(".tsx") ? ts.ScriptKind.TSX : ts.ScriptKind.TS,
  );
}

function literal(node) {
  if (!node) return null;
  if (ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node)) return node.text;
  return null;
}

function where(sf, node) {
  const { line } = sf.getLineAndCharacterOfPosition(node.getStart(sf));
  return `${path.relative(webRoot, sf.fileName)}:${line + 1}`;
}

/**
 * Collect messages: `t`, `tx` and `msg` take the English as their first
 * argument; `tn(count, one, other)` is keyed by `other`.
 * Returns { messages: Map<key, {one?: string, refs: string[]}>, dynamic: string[] }.
 */
export function extract(files = sourceFiles()) {
  const messages = new Map();
  const dynamic = [];
  const add = (key, ref, one) => {
    const entry = messages.get(key) ?? { refs: [] };
    if (one !== undefined) entry.one = one;
    entry.refs.push(ref);
    messages.set(key, entry);
  };
  for (const file of files) {
    const sf = parse(file);
    const visit = (node) => {
      if (ts.isCallExpression(node) && ts.isIdentifier(node.expression)) {
        const name = node.expression.text;
        if (name === "t" || name === "tx" || name === "msg") {
          const text = literal(node.arguments[0]);
          if (text !== null) add(text, where(sf, node));
          else if (name !== "msg") dynamic.push(where(sf, node));
        } else if (name === "tn") {
          const one = literal(node.arguments[1]);
          const other = literal(node.arguments[2]);
          if (one !== null && other !== null) add(other, where(sf, node), one);
          else dynamic.push(where(sf, node));
        }
      }
      ts.forEachChild(node, visit);
    };
    visit(sf);
  }
  return { messages, dynamic };
}

export function localeCodes() {
  return SUPPORTED_LOCALES.filter((locale) => locale !== DEFAULT_LOCALE);
}

/** A locale's catalogue file: translations keyed by `systemMessageKey(english)`. */
export function readCatalog(locale) {
  const file = path.join(localesDir, `${locale}.json`);
  return existsSync(file) ? JSON.parse(readFileSync(file, "utf8")) : {};
}

/** Source messages by catalogue key; throws if two sources share a key. */
export function keyed(messages) {
  const byKey = new Map();
  for (const [source, message] of messages) {
    const key = systemMessageKey(source);
    const clash = byKey.get(key);
    if (clash && clash.source !== source) {
      throw new Error(
        `message key collision: ${JSON.stringify(clash.source)} / ${JSON.stringify(source)}`,
      );
    }
    byKey.set(key, { source, ...message });
  }
  return byKey;
}

/**
 * The English reference catalogue, `locales/en.json`: every key with its
 * source copy (plural messages as `{ one, other }`). Not loaded at runtime;
 * it lets a reviewer read any catalogue side by side with the English.
 */
export function sourceCatalog(messages) {
  const out = {};
  for (const [key, m] of keyed(messages)) {
    out[key] = m.one === undefined ? m.source : { one: m.one, other: m.source };
  }
  return out;
}

export function problems(locale, message, translation) {
  return messageProblems({ locale, source: message.source, one: message.one, translation });
}

/** { missing: string[], stale: string[], invalid: string[] } for one locale (sources, keys). */
export function checkCatalog(locale, messages) {
  const catalog = readCatalog(locale);
  const byKey = keyed(messages);
  const missing = [...byKey].filter(([key]) => !(key in catalog)).map(([, m]) => m.source);
  const stale = Object.keys(catalog).filter((key) => !byKey.has(key));
  const invalid = [];
  for (const [key, value] of Object.entries(catalog)) {
    const message = byKey.get(key);
    if (!message) continue;
    for (const p of problems(locale, message, value)) {
      invalid.push(`${JSON.stringify(message.source)}: ${p}`);
    }
  }
  return { missing, stale, invalid };
}

export function sourceCatalogIsCurrent(messages) {
  const file = path.join(localesDir, "en.json");
  return existsSync(file) && readFileSync(file, "utf8") === formatCatalog(sourceCatalog(messages));
}

/** Stable JSON matching Prettier's output for a flat object. */
export function formatCatalog(catalog) {
  const sorted = Object.fromEntries(
    Object.entries(catalog).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0)),
  );
  return `${JSON.stringify(sorted, null, 2)}\n`;
}

// ---------------------------------------------------------------------------
// Audit: likely user-visible English that is not wrapped yet.

const TEXT_ATTRS = new Set([
  "alt",
  "aria-description",
  "aria-label",
  "aria-placeholder",
  "aria-valuetext",
  "ariaLabel",
  "cancelLabel",
  "clearLabel",
  "closeLabel",
  "confirmLabel",
  "description",
  "detail",
  "empty",
  "emptyLabel",
  "emptyText",
  "heading",
  "help",
  "hint",
  "label",
  "message",
  "noMatchesLabel",
  "noneLabel",
  "placeholder",
  "searchPlaceholder",
  "searchingLabel",
  "selectAllLabel",
  "selectLabel",
  "sheetTitle",
  "subtitle",
  "summary",
  "text",
  "title",
  "tooltip",
]);

const TEXT_PROPS = new Set([
  ...TEXT_ATTRS,
  "body",
  "caption",
  "confirm",
  "cancel",
  "error",
  "group",
  "info",
  "lead",
  "name",
  "note",
  "reason",
  "success",
  "warning",
]);

const WRAPPERS = new Set(["t", "tn", "tx", "msg"]);
const IGNORE_CALLS = new Set([
  "cn",
  "clsx",
  "require",
  "import",
  "fetch",
  "get",
  "set",
  "has",
  "delete",
  "querySelector",
  "getItem",
  "setItem",
  "removeItem",
  "addEventListener",
  "removeEventListener",
  "startsWith",
  "endsWith",
  "includes",
  "split",
  "replace",
  "join",
  "test",
  "match",
  "matchMedia",
  "createElement",
  "navigate",
  "useParams",
  "useSearchParams",
  "append",
  "padStart",
  "encodeURIComponent",
  "Error",
]);

function looksLikeCopy(text) {
  const s = text.trim();
  if (!/[A-Za-z]{2,}/.test(s)) return false;
  if (/^(https?:|\/|#|\.|data:|mailto:)/.test(s)) return false; // urls, paths, selectors
  if (/^[a-z0-9_.:\-/[\]=]+$/.test(s)) return false; // keys, ids, enum values, class names
  if (/^[A-Z0-9_]+$/.test(s)) return false; // CONSTANTS
  if (
    /^[\w-]+(\s[\w:/[\]().%-]+)*$/.test(s) &&
    /(^|\s)(flex|grid|text-|bg-|px-|py-|p-|m[trblxy]?-|w-|h-|gap-|rounded|border|items-|justify-|font-|size-|min-|max-|inline|block|hidden|absolute|relative|truncate|shrink|overflow|hover:|focus)/.test(
      s,
    )
  )
    return false; // tailwind
  return true;
}

const COMPARISONS = new Set([
  ts.SyntaxKind.EqualsEqualsEqualsToken,
  ts.SyntaxKind.ExclamationEqualsEqualsToken,
  ts.SyntaxKind.EqualsEqualsToken,
  ts.SyntaxKind.ExclamationEqualsToken,
]);

/** Already wrapped, or used as data rather than shown (keys, comparisons, types). */
function insideWrapper(node) {
  for (let child = node, p = node.parent; p; child = p, p = p.parent) {
    if (ts.isCallExpression(p)) {
      const callee = ts.isIdentifier(p.expression)
        ? p.expression.text
        : ts.isPropertyAccessExpression(p.expression)
          ? p.expression.name.text
          : "";
      if (WRAPPERS.has(callee) || IGNORE_CALLS.has(callee)) return true;
    }
    if (ts.isNewExpression(p) || ts.isTypeNode(p)) return true;
    if (ts.isImportDeclaration(p) || ts.isExportDeclaration(p)) return true;
    if (ts.isElementAccessExpression(p) && p.argumentExpression === child) return true;
    if (ts.isCaseClause(p) && p.expression === child) return true;
    if (ts.isBinaryExpression(p) && COMPARISONS.has(p.operatorToken.kind)) return true;
    if (ts.isFunctionLike(p) || ts.isSourceFile(p)) return false;
  }
  return false;
}

function isStringish(node) {
  return (
    ts.isStringLiteral(node) ||
    ts.isNoSubstitutionTemplateLiteral(node) ||
    ts.isTemplateExpression(node)
  );
}

/** A bare string in a spot that usually renders: JSX child, ternary, return, toast. */
function copyContext(node) {
  const p = node.parent;
  if (!p) return false;
  if (ts.isJsxExpression(p)) return !ts.isJsxAttribute(p.parent);
  if (ts.isConditionalExpression(p) || ts.isReturnStatement(p)) return true;
  if (ts.isBinaryExpression(p) && p.operatorToken.kind === ts.SyntaxKind.QuestionQuestionToken)
    return true;
  return (
    ts.isCallExpression(p) &&
    ts.isPropertyAccessExpression(p.expression) &&
    ts.isIdentifier(p.expression.expression) &&
    p.expression.expression.text === "toast"
  );
}

/** Inside a JSX attribute that is not user-visible text (className, variant…). */
function inOtherAttr(node, sf) {
  for (let p = node.parent; p; p = p.parent) {
    if (ts.isJsxAttribute(p)) return !TEXT_ATTRS.has(p.name.getText(sf));
    if (ts.isFunctionLike(p) || ts.isJsxElement(p) || ts.isJsxSelfClosingElement(p)) return false;
  }
  return false;
}

/** Likely untranslated copy: [{ ref, text }]. Heuristic; review each hit. */
export function audit(files = sourceFiles()) {
  const hits = [];
  for (const file of files) {
    const sf = parse(file);
    const visit = (node) => {
      let text = null;
      if (ts.isJsxText(node)) {
        if (looksLikeCopy(node.text)) text = node.text.trim().replace(/\s+/g, " ");
      } else if (ts.isJsxAttribute(node) && node.initializer) {
        const name = node.name.getText(sf);
        const init = node.initializer;
        const value = ts.isStringLiteral(init)
          ? init.text
          : ts.isJsxExpression(init) && init.expression
            ? literal(init.expression)
            : null;
        if (value !== null && TEXT_ATTRS.has(name) && looksLikeCopy(value)) text = value;
      } else if (ts.isPropertyAssignment(node) && literal(node.initializer) !== null) {
        const name = node.name.getText(sf).replace(/["']/g, "");
        const value = literal(node.initializer);
        if (TEXT_PROPS.has(name) && looksLikeCopy(value) && !insideWrapper(node.initializer)) {
          text = value;
        }
      } else if (isStringish(node) && copyContext(node)) {
        const raw = ts.isTemplateExpression(node) ? node.getText(sf).slice(1, -1) : node.text;
        if (looksLikeCopy(raw) && !insideWrapper(node) && !inOtherAttr(node, sf)) text = raw;
      }
      if (text !== null) hits.push({ ref: where(sf, node), text });
      ts.forEachChild(node, visit);
    };
    visit(sf);
  }
  return hits;
}

// ---------------------------------------------------------------------------
// Memos that build translated copy must recompute when the language changes.

const COPY_CALL = /\b(t|tn|tx|getLocale|searchTerms)\(/;

/**
 * `useMemo` calls whose result holds translated copy but whose dependencies
 * omit `locale`: they would keep the previous language after a switch. Copy
 * is detected through direct `t()` calls, `localized` tables, and
 * module-level functions that produce copy (followed through imports). A
 * memo that only reads such code without keeping copy can opt out with an
 * `// i18n: memo holds no copy` comment on the line above.
 * Returns ["file:line (reason)"].
 */
export function staleMemos(files = sourceFiles()) {
  const parsed = files.map(parse);

  // Identifiers a node references, ignoring property names.
  const referenced = (root) => {
    const names = new Set();
    const walk = (n) => {
      if (ts.isIdentifier(n)) {
        const p = n.parent;
        const isProperty =
          (ts.isPropertyAccessExpression(p) && p.name === n) ||
          (ts.isPropertyAssignment(p) && p.name === n) ||
          (ts.isBindingElement(p) && p.propertyName === n);
        if (!isProperty) names.add(n.text);
      }
      ts.forEachChild(n, walk);
    };
    walk(root);
    return names;
  };

  // Per file: names visible at module scope, and top-level declarations.
  const decls = []; // { name, node, sf, visible }
  const visibleByFile = new Map();
  for (const sf of parsed) {
    const visible = new Set();
    const own = [];
    for (const st of sf.statements) {
      if (ts.isImportDeclaration(st) && st.importClause) {
        const { name, namedBindings } = st.importClause;
        if (name) visible.add(name.text);
        if (namedBindings && ts.isNamedImports(namedBindings)) {
          for (const el of namedBindings.elements) visible.add(el.name.text);
        }
      } else if (ts.isFunctionDeclaration(st) && st.name && st.body) {
        visible.add(st.name.text);
        own.push({ name: st.name.text, node: st.body });
      } else if (ts.isVariableStatement(st)) {
        for (const d of st.declarationList.declarations) {
          if (ts.isIdentifier(d.name) && d.initializer) {
            visible.add(d.name.text);
            own.push({ name: d.name.text, node: d.initializer });
          }
        }
      }
    }
    visibleByFile.set(sf, visible);
    for (const d of own) decls.push({ ...d, sf, visible, refs: referenced(d.node) });
  }

  // Module-level names that produce copy, followed through visible references.
  const copy = new Set();
  for (const d of decls) {
    const text = d.node.getText(d.sf);
    if (COPY_CALL.test(text) || /\blocalized\b/.test(text)) copy.add(d.name);
  }
  for (let grew = true; grew;) {
    grew = false;
    for (const d of decls) {
      if (copy.has(d.name) || /^[A-Z][a-z]/.test(d.name)) continue; // components render, not memoise
      if ([...d.refs].some((x) => x !== d.name && d.visible.has(x) && copy.has(x))) {
        copy.add(d.name);
        grew = true;
      }
    }
  }

  const stale = [];
  for (const sf of parsed) {
    const visible = visibleByFile.get(sf);
    const visit = (node) => {
      if (ts.isCallExpression(node) && /^(React\.)?useMemo$/.test(node.expression.getText(sf))) {
        const fn = node.arguments[0];
        const deps = node.arguments[1]?.getText(sf) ?? "";
        // `// i18n: memo holds no copy` on the line above opts a memo out.
        const above = sf.text.slice(0, node.getStart(sf)).split("\n").slice(-2).join("\n");
        if (fn && !/\blocale\b/.test(deps) && !/i18n: memo holds no copy/.test(above)) {
          const reason = COPY_CALL.test(fn.getText(sf))
            ? "t()"
            : [...referenced(fn)].find(
                (x) => visible.has(x) && copy.has(x) && !/^[A-Z][a-z]/.test(x),
              );
          if (reason) stale.push(`${where(sf, node)} (${reason})`);
        }
      }
      ts.forEachChild(node, visit);
    };
    visit(sf);
  }
  return stale;
}
