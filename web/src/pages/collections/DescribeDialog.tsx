import { Drawer } from "../../components/Drawer";
import {
  Alert,
  AlertDescription,
  AlertTitle,
  Badge,
  Button,
  Textarea,
} from "@braedonsaunders/appkit-ui";
import { Sparkles, Wand2 } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";
import { Segmented } from "../../components/fields";
import { localized, t, tx } from "../../i18n";
import { api, ApiError, errorMessage } from "../../lib/api";
import type { CollectionSpec } from "../../lib/contract";
import { describeRule, describeSource, normalizeSpec } from "../../lib/rules";

const EXAMPLES = localized(() => [
  t("Cosy British mysteries under two hours"),
  t("Mind-bending sci-fi from the 90s"),
  t("Acclaimed animated films the whole family will like"),
  t("Slow-burn horror with great reviews"),
]);

/**
 * "Describe it": plain English in, an editable collection draft out. The
 * result is never applied silently; the owner reviews it first.
 */
export function DescribeDialog({
  open,
  onClose,
  onApply,
  applyLabel = t("Open as draft"),
  secondary,
}: {
  open: boolean;
  onClose: () => void;
  onApply: (spec: CollectionSpec) => void;
  applyLabel?: string;
  secondary?: { label: string; onApply: (spec: CollectionSpec) => void };
}) {
  const [prompt, setPrompt] = useState("");
  const [media, setMedia] = useState<"movie" | "show" | "mixed">("movie");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [result, setResult] = useState<{ spec: CollectionSpec; explanation: string } | null>(null);

  const generate = async () => {
    setBusy(true);
    setError(null);
    setResult(null);
    try {
      const r = await api.generateCollection(prompt.trim(), media);
      setResult({
        spec: normalizeSpec({ ...r.spec, mediaType: r.spec.mediaType ?? media }),
        explanation: r.explanation,
      });
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  };
  const close = () => {
    setResult(null);
    setError(null);
    onClose();
  };
  const llmOff = error instanceof ApiError && error.status === 501;

  return (
    <Drawer
      open={open}
      onClose={close}
      size="lg"
      title={
        <span className="flex items-center gap-2">
          <Sparkles size={18} className="text-primary" /> {t("Describe a collection")}
        </span>
      }
      description={t(
        "Say what you want in plain English. Ether drafts the sources and rules; you review them before anything is saved.",
      )}
      footer={
        <div className="flex w-full flex-wrap justify-end gap-2">
          <Button variant="ghost" onClick={close}>
            {t("Cancel")}
          </Button>
          {result && secondary ? (
            <Button
              variant="outline"
              onClick={() => {
                secondary.onApply(result.spec);
                close();
              }}
            >
              {secondary.label}
            </Button>
          ) : null}
          {result ? (
            <Button
              onClick={() => {
                onApply(result.spec);
                close();
              }}
            >
              {applyLabel}
            </Button>
          ) : (
            <Button onClick={() => void generate()} disabled={busy || prompt.trim().length < 3}>
              <Wand2 size={15} /> {busy ? t("Drafting…") : t("Draft it")}
            </Button>
          )}
        </div>
      }
    >
      <div className="space-y-4">
        <Textarea
          rows={3}
          value={prompt}
          autoFocus
          aria-label={t("Describe the collection")}
          placeholder={t("e.g. Feel-good 80s adventure films for a Saturday night")}
          onChange={(e) => setPrompt(e.currentTarget.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && (e.metaKey || e.ctrlKey) && prompt.trim().length >= 3)
              void generate();
          }}
        />
        <div className="flex flex-wrap items-center gap-3">
          <Segmented
            ariaLabel={t("Media type")}
            size="sm"
            value={media}
            onChange={setMedia}
            options={[
              { value: "movie", label: t("Movies") },
              { value: "show", label: t("TV") },
              { value: "mixed", label: t("Both") },
            ]}
          />
          <div className="flex flex-wrap gap-1.5">
            {EXAMPLES.map((ex) => (
              <button
                key={ex}
                type="button"
                onClick={() => setPrompt(ex)}
                className="rounded-full border border-border px-2.5 py-0.5 text-xs text-fg-muted hover:border-border-strong hover:text-fg"
              >
                {ex}
              </button>
            ))}
          </div>
        </div>
        {llmOff ? (
          <Alert variant="info">
            <AlertTitle>{t("Describe it needs a language model")}</AlertTitle>
            <AlertDescription>
              {tx(
                "Connect an OpenAI-compatible endpoint (a local Ollama works) in {integrations}, then try again. You can still build collections by hand or from a template.",
                {
                  integrations: (
                    <Link to="/services" className="font-medium underline" onClick={close}>
                      {t("Integrations")}
                    </Link>
                  ),
                },
              )}
            </AlertDescription>
          </Alert>
        ) : error ? (
          <Alert variant="destructive">
            <AlertDescription>{errorMessage(error)}</AlertDescription>
          </Alert>
        ) : null}
        {result ? (
          <div
            className="space-y-3 rounded-lg border border-border bg-bg-subtle p-4"
            aria-live="polite"
          >
            <div>
              <p className="text-base font-semibold text-fg">
                {result.spec.name || t("Untitled collection")}
              </p>
              {result.explanation ? (
                <p className="mt-1 text-sm text-fg-muted">{result.explanation}</p>
              ) : null}
            </div>
            <div className="flex flex-wrap gap-1.5">
              {result.spec.sources.map((s, i) => (
                <Badge key={`s${i}`} variant="info">
                  {describeSource(s)}
                </Badge>
              ))}
              {result.spec.rules.map((r, i) => (
                <Badge key={`r${i}`} variant="default">
                  {describeRule(r)}
                </Badge>
              ))}
              {result.spec.exclude.map((r, i) => (
                <Badge key={`x${i}`} variant="destructive">
                  {t("Not: {rule}", { rule: describeRule(r) })}
                </Badge>
              ))}
            </div>
            <p className="text-xs text-fg-subtle">
              {t("Nothing is saved yet. You can change every rule in the editor.")}
            </p>
          </div>
        ) : null}
      </div>
    </Drawer>
  );
}
