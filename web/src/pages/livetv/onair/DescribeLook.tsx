import { useState } from "react";
import { Button, Textarea } from "@braedonsaunders/appkit-ui";
import { t } from "../../../i18n";
import { errorMessage } from "../../../lib/api";
import { studio, type Style } from "./studio-model";
import { StyleThumbnail } from "./editor/RemixPanel";
export function DescribeLook({
  look,
  onChange,
  sceneId,
}: {
  look: Style;
  onChange(style: Style): void;
  sceneId?: string;
}) {
  const [prompt, setPrompt] = useState(""),
    [working, setWorking] = useState(false),
    [proposal, setProposal] = useState<{ style: Style; summary: string } | null>(null),
    [before, setBefore] = useState<Style | null>(null),
    [error, setError] = useState("");
  const draft = async () => {
    setWorking(true);
    setError("");
    try {
      setProposal(await studio.draft({ prompt, style: look, sceneId }));
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setWorking(false);
    }
  };
  return (
    <section className="space-y-3">
      <label className="block text-sm font-semibold" htmlFor="studio-prompt">
        {look.id ? t("Describe a change") : t("Describe your channel")}
      </label>
      <Textarea
        id="studio-prompt"
        value={prompt}
        maxLength={2000}
        onChange={(e) => setPrompt(e.target.value)}
        placeholder={t("A cosy late-night movie channel with a moonlit horizon")}
      />
      <Button disabled={working || !prompt.trim()} onClick={() => void draft()}>
        {working ? t("Designing your proposal…") : t("Preview proposal")}
      </Button>
      {error ? (
        <p role="alert" className="text-danger">
          {error}
        </p>
      ) : null}
      {proposal ? (
        <div className="space-y-3 rounded-lg border border-border p-3">
          <StyleThumbnail
            style={proposal.style}
            scene={proposal.style.scenes.find((s) => s.id === sceneId) ?? proposal.style.scenes[0]}
            playing
          />
          <p>{proposal.summary}</p>
          <div className="flex gap-2">
            <Button
              onClick={() => {
                setBefore(structuredClone(look));
                onChange(proposal.style);
                setProposal(null);
              }}
            >
              {t("Accept")}
            </Button>
            <Button variant="outline" disabled={working} onClick={() => void draft()}>
              {t("Try again")}
            </Button>
            <Button variant="ghost" onClick={() => setProposal(null)}>
              {t("Discard")}
            </Button>
          </div>
        </div>
      ) : null}
      {before ? (
        <Button
          variant="outline"
          onClick={() => {
            onChange(before);
            setBefore(null);
            setProposal(null);
          }}
        >
          {t("Revert")}
        </Button>
      ) : null}
    </section>
  );
}
