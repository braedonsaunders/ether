import { Button, Input, SettingsSection, Spinner, Switch, toast } from "@braedonsaunders/appkit-ui";
import { useState } from "react";
import { Link } from "react-router-dom";
import { ErrorPanel } from "../../components/feedback";
import { t } from "../../i18n";
import { SaveBar } from "../../components/SaveBar";
import { SettingsRow } from "../../components/SettingsRow";
import { TitleSearch } from "../../components/pickers";
import { apiFetch } from "../../lib/api";
import type { TitleCard } from "../../lib/contract";
import { useDiscoverySettings } from "../viewers/sections/WatchlistDefaults";

/**
 * Features that use the owner's language model or speech-to-text, edited in
 * place under Settings › Integrations › Language model.
 */
export function LanguageFeatures({
  textModelReady = false,
  speechReady = false,
}: {
  textModelReady?: boolean;
  speechReady?: boolean;
}) {
  const s = useDiscoverySettings();
  if (s.doc.error) return <ErrorPanel error={s.doc.error} onRetry={() => void s.doc.reload()} />;
  if (!s.draft)
    return (
      <div className="py-6 text-center">
        <Spinner size={18} label={t("Loading language features")} />
      </div>
    );
  const draft = s.draft;
  const set = (patch: Partial<typeof draft>) => s.setDraft({ ...draft, ...patch });
  return (
    <>
      <SettingsSection title={t("Language features")}>
        <FeatureRow
          id="llm-natural-search"
          title={t("Natural-language search")}
          does={t("Viewers can search with a sentence, such as “90s heist movies”.")}
          trigger={t("A viewer types that search in Plex.")}
          checked={draft.naturalSearch}
          onChange={(on) => set({ naturalSearch: on })}
          ready={textModelReady}
          readyLabel={t("Uses your language model — connected ✓")}
          setupHref="/services/llm#language-model"
          setupLabel={t("Needs a language model → Set up")}
        />
        <FeatureRow
          id="llm-recaps"
          title={t("Recaps")}
          does={t(
            "Viewers get a short previously-on summary of episodes they have already finished.",
          )}
          trigger={t("Shown on a season or episode after those episodes are watched.")}
          checked={draft.recaps}
          onChange={(on) => set({ recaps: on })}
          ready={textModelReady}
          readyLabel={t("Uses your language model — connected ✓")}
          setupHref="/services/llm#language-model"
          setupLabel={t("Needs a language model → Set up")}
        />
        <FeatureRow
          id="llm-subtitle-translate"
          title={t("Translate subtitles")}
          does={t("Turns a subtitle track the title already has into another language.")}
          trigger={t(
            "A viewer searches subtitles and picks the translated track. Nothing is created until they pick it.",
          )}
          checked={Boolean(draft.subtitleTranslate)}
          onChange={(on) => set({ subtitleTranslate: on })}
          ready={textModelReady}
          readyLabel={t("Uses your language model — connected ✓")}
          setupHref="/services/llm#language-model"
          setupLabel={t("Needs a language model → Set up")}
        />
        <FeatureRow
          id="llm-speech"
          title={t("Subtitles from the audio")}
          does={t("Writes timed subtitles by listening to the audio.")}
          trigger={t(
            "A viewer searches subtitles and picks the track made from the audio. Nothing is created until they pick it.",
          )}
          checked={Boolean(draft.speechToText)}
          onChange={(on) => set({ speechToText: on })}
          ready={speechReady}
          readyLabel={t("Uses speech-to-text — connected ✓")}
          setupHref="/services/llm#speech-to-text"
          setupLabel={t("Needs speech-to-text → Set up")}
        />
        <FeatureRow
          id="llm-overview"
          title={t("Translate descriptions")}
          does={t("Translates the overview and tagline into the viewer's language.")}
          trigger={t("A viewer opens the title in another language, or you queue a title here.")}
          checked={Boolean(draft.overviewTranslate)}
          onChange={(on) => set({ overviewTranslate: on })}
          ready={textModelReady}
          readyLabel={t("Uses your language model — connected ✓")}
          setupHref="/services/llm#language-model"
          setupLabel={t("Needs a language model → Set up")}
        />
        {draft.overviewTranslate ? (
          <DescriptionQueue saved={Boolean(s.doc.data?.overviewTranslate)} ready={textModelReady} />
        ) : null}
      </SettingsSection>
      <SaveBar
        dirty={s.dirty}
        saving={s.saving}
        error={s.error}
        onSave={() => s.save(t("Language features saved"))}
        onDiscard={s.discard}
      />
    </>
  );
}

function FeatureRow({
  id,
  title,
  does,
  trigger,
  checked,
  onChange,
  ready,
  readyLabel,
  setupHref,
  setupLabel,
}: {
  id: string;
  title: string;
  does: string;
  trigger: string;
  checked: boolean;
  onChange: (on: boolean) => void;
  ready: boolean;
  readyLabel: string;
  setupHref: string;
  setupLabel: string;
}) {
  return (
    <SettingsRow
      title={<label htmlFor={id}>{title}</label>}
      description={
        <span className="block space-y-1">
          <span className="block">{does}</span>
          <span className="block text-fg-muted">{trigger}</span>
          {ready ? (
            <span className="block text-success">{readyLabel}</span>
          ) : (
            <Link to={setupHref} className="underline">
              {setupLabel}
            </Link>
          )}
        </span>
      }
      control={
        <Switch
          id={id}
          aria-label={title}
          checked={checked}
          onChange={(e) => onChange(e.currentTarget.checked)}
        />
      }
    />
  );
}

function DescriptionQueue({ saved, ready }: { saved: boolean; ready: boolean }) {
  const [picked, setPicked] = useState<TitleCard | null>(null);
  const [language, setLanguage] = useState("fr");
  const [busy, setBusy] = useState(false);
  const canQueue = saved && ready && picked && language.trim() !== "";
  return (
    <div className="space-y-3 border-t border-border px-4 py-3">
      <p className="text-sm font-medium text-fg">{t("Queue a title")}</p>
      <TitleSearch onPick={setPicked} placeholder={t("Search titles")} />
      {picked ? <p className="text-sm text-fg">{picked.title}</p> : null}
      <label className="block text-sm text-fg" htmlFor="overview-language">
        {t("Language")}
        <Input
          id="overview-language"
          className="mt-1 w-24"
          aria-label={t("Language")}
          value={language}
          onChange={(e) => setLanguage(e.currentTarget.value)}
          spellCheck={false}
        />
      </label>
      {!saved ? (
        <p className="text-sm text-fg-muted">{t("Save this switch, then queue a title.")}</p>
      ) : null}
      <Button
        type="button"
        variant="outline"
        disabled={!canQueue || busy}
        onClick={() => {
          if (!picked) return;
          setBusy(true);
          void apiFetch("/discovery/translate", {
            method: "POST",
            body: JSON.stringify({ key: picked.key, language: language.trim() }),
          })
            .then(() => toast.success(t("Translation queued.")))
            .catch((e: Error) => toast.error(e.message))
            .finally(() => setBusy(false));
        }}
      >
        {t("Queue translation")}
      </Button>
    </div>
  );
}
