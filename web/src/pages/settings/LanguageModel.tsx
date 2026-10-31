import { Input, SearchSelect, SettingsSection, Switch } from "@braedonsaunders/appkit-ui";
import type { ReactNode } from "react";
import { ErrorPanel } from "../../components/feedback";
import { SecretInput } from "../../components/fields";
import { t } from "../../i18n";
import {
  findProvider,
  ModelPicker,
  providerOptions,
  useLLMProviders,
  withProvider,
} from "../../components/llm";
import { SettingsRow } from "../../components/SettingsRow";
import type { EtherConfig } from "../../lib/types";

type LLMConfig = EtherConfig["llm"];

function FieldError({ error }: { error?: string }) {
  return error ? (
    <span className="mt-1 block text-xs text-danger" role="alert">
      {error}
    </span>
  ) : null;
}

/**
 * Settings › Integrations › Language model: pick a provider, give it
 * a key (or a local endpoint), then choose from the models it offers.
 */
export function LanguageModelSettings({
  value,
  saved,
  onChange,
  errors,
  footer,
}: {
  value: LLMConfig;
  saved: LLMConfig | undefined;
  onChange: (next: LLMConfig) => void;
  /** Field errors keyed by dotted path ("llm.model"). */
  errors: Record<string, string>;
  footer?: ReactNode;
}) {
  const { providers, error } = useLLMProviders();
  if (error) return <ErrorPanel error={error} />;
  const provider = findProvider(providers, value.provider);
  const set = (patch: Partial<LLMConfig>) => onChange({ ...value, ...patch });
  return (
    <>
      <div id="language-model">
        <SettingsSection
          title={t("Language model")}
          description={t(
            "Powers “Describe it” in Collection Studio, themed Home rows, and the language features below.",
          )}
          footer={footer}
        >
          <SettingsRow
            title={<label htmlFor="llm-enabled">{t("Enabled")}</label>}
            control={
              <Switch
                id="llm-enabled"
                aria-label={t("Enabled")}
                checked={value.enabled}
                onChange={(e) => set({ enabled: e.currentTarget.checked })}
              />
            }
          />
          <SettingsRow
            title={t("Provider")}
            description={<FieldError error={errors["llm.provider"]} />}
            control={
              <div className="sm:w-72">
                <SearchSelect
                  value={value.provider}
                  onChange={(id) => onChange(withProvider(value, saved, id))}
                  options={providerOptions(providers)}
                  ariaLabel={t("Provider")}
                  placeholder={providers ? t("Choose a provider") : t("Loading…")}
                  searchable
                  searchPlaceholder={t("Search…")}
                  noMatchesLabel={t("No matches")}
                  selectLabel={t("Select")}
                  closeLabel={t("Close")}
                />
              </div>
            }
          />
          {provider ? (
            <>
              {provider.local ? (
                <SettingsRow
                  stacked
                  title={<label htmlFor="llm-base-url">{t("Endpoint URL")}</label>}
                  description={
                    <>
                      {provider.requiresBaseUrl
                        ? provider.modelHint
                        : t("Leave empty to use {url}.", { url: provider.baseUrl })}
                      <FieldError error={errors["llm.baseUrl"]} />
                    </>
                  }
                >
                  <Input
                    id="llm-base-url"
                    aria-label={t("Endpoint URL")}
                    aria-invalid={errors["llm.baseUrl"] ? true : undefined}
                    value={value.baseUrl}
                    placeholder={provider.baseUrl || "https://example.com/v1"}
                    onChange={(e) => set({ baseUrl: e.currentTarget.value })}
                    spellCheck={false}
                  />
                </SettingsRow>
              ) : null}
              <SettingsRow
                stacked
                title={<label htmlFor="llm-api-key">{t("API key")}</label>}
                description={
                  <>
                    {provider.needsKey ? null : t("Not needed for most local servers.")}
                    <FieldError error={errors["llm.apiKey"]} />
                  </>
                }
              >
                <SecretInput
                  id="llm-api-key"
                  ariaLabel={t("API key")}
                  value={value.apiKey}
                  onChange={(apiKey) => set({ apiKey })}
                  placeholder={provider.keyHint}
                  invalid={Boolean(errors["llm.apiKey"])}
                />
              </SettingsRow>
              <SettingsRow
                stacked
                title={<label htmlFor="llm-model">{t("Model")}</label>}
                description={
                  <>
                    {provider.requiresBaseUrl ? null : provider.modelHint}
                    <FieldError error={errors["llm.model"]} />
                  </>
                }
              >
                <ModelPicker
                  id="llm-model"
                  cfg={value}
                  provider={provider}
                  value={value.model}
                  onChange={(model) => set({ model })}
                  invalid={Boolean(errors["llm.model"])}
                />
              </SettingsRow>
            </>
          ) : null}
        </SettingsSection>
      </div>
      <div id="speech-to-text">
        <SettingsSection
          title={t("Speech-to-text")}
          description={t(
            "Writes timed subtitles from the audio when a viewer asks. A Whisper-compatible address, such as http://server:1234/v1. Saved with the language model.",
          )}
        >
          <SettingsRow
            stacked
            title={<label htmlFor="speech-base-url">{t("Speech-to-text endpoint")}</label>}
            description={t(
              "The server Ether calls only after a viewer picks this kind of subtitle.",
            )}
          >
            <Input
              id="speech-base-url"
              aria-label={t("Speech-to-text endpoint")}
              value={value.speechBaseUrl ?? ""}
              placeholder="http://192.168.68.85:1234/v1"
              onChange={(e) => set({ speechBaseUrl: e.currentTarget.value })}
              spellCheck={false}
            />
          </SettingsRow>
          <SettingsRow
            stacked
            title={<label htmlFor="speech-model">{t("Speech-to-text model")}</label>}
            description={t("The transcription model that server offers, such as whisper-1.")}
          >
            <Input
              id="speech-model"
              aria-label={t("Speech-to-text model")}
              value={value.speechModel ?? ""}
              placeholder="whisper-1"
              onChange={(e) => set({ speechModel: e.currentTarget.value })}
              spellCheck={false}
            />
          </SettingsRow>
          <SettingsRow
            stacked
            title={<label htmlFor="speech-api-key">{t("Speech-to-text API key")}</label>}
            description={t("Leave empty when the server does not ask for one.")}
          >
            <SecretInput
              id="speech-api-key"
              ariaLabel={t("Speech-to-text API key")}
              value={value.speechApiKey ?? ""}
              onChange={(speechApiKey) => set({ speechApiKey })}
              placeholder=""
            />
          </SettingsRow>
        </SettingsSection>
      </div>
    </>
  );
}
