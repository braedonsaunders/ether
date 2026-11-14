export type * from "./types.ts";
export { PluginHttpError } from "./types.ts";
export {
  plugin,
  PluginRouteError,
  type FileRangeResponse,
  type FileRequest,
  type PluginContext,
  type PluginDefinition,
  type PluginInstance,
  type PluginRoutes,
} from "./router.ts";
export {
  ether,
  createHostedClient,
  createHostedHostClient,
  createRemoteHostClient,
  isHostedRuntime,
  type CatalogTitleSummary,
  type EtherHost,
  type FolderEntry,
  type FolderHash,
  type FolderHashJob,
  type DataRange,
  type FetchCacheRequest,
  type FetchCacheResult,
  type HostFetchRequest,
  type HostFetchResponse,
  type RemoteHostOptions,
  type ViewerWatchEntry,
} from "./host.ts";
export {
  collectSettingFields,
  decodeSettings,
  lintSettingsSchema,
  type DecodedSettings,
} from "./settings.ts";
export {
  createI18n,
  lintCopy,
  tRef,
  type I18nHelper,
  type LocaleStrings,
  type StringsCatalog,
} from "./i18n.ts";
export {
  serve,
  dispatchOverTunnelFrame,
  type ServeHandle,
  type ServeOptions,
  type TunnelFrame,
} from "./serve.ts";
export {
  clearSavedCredential,
  CredentialError,
  noCredentialMessage,
  pairUrlFromBase,
  readSavedCredential,
  resolveCredential,
  resolveStateDir,
  serverBase,
  tunnelUrlFromBase,
  writeSavedCredential,
  PAIR_EXCHANGE_PATH,
  TUNNEL_PATH,
  type PairingInputs,
} from "./pairing.ts";
export {
  createMockHostClient,
  createTestHarness,
  assertValidViewPage,
  assertValidViewSpec,
  fixtures,
  type MockHostClient,
  type MockHostState,
  type RecordedLog,
  type TestHarness,
} from "./testing.ts";
export {
  candidateRef,
  streamUrl,
  type CatalogProvider,
  type FrontendAdapter,
  type KindMatcher,
  type KindResolver,
  type Matcher,
  type SetupTester,
} from "./kinds.ts";
export { archiveList, parseFeed, scrapeLinks, scrapeText, type ArchiveMember, type ParsedFeed } from "./kit.ts";
export {
  createCatalogTemplate,
  createSubtitlesTemplate,
  createScrobblerTemplate,
  createViewTemplate,
  createRankerTemplate,
  createConditionTemplate,
  type TemplateOptions,
} from "./templates.ts";
