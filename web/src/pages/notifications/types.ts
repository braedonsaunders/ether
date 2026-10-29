import { localized, t } from "../../i18n";
import { newId } from "../../lib/api";

export type Channel = {
  smtpMode?: string;
  subscription?: PushSubscriptionJSON;
  id: string;
  name: string;
  kind: string;
  enabled: boolean;
  events: string[];
  endpoint: string;
  token: string;
  target: string;
  username: string;
  password: string;
  from: string;
  to: string;
  perMinute: number;
  digestMinutes: number;
};

export type Delivery = {
  id: number;
  channel: string;
  created: number;
  status: string;
  error?: string;
  kind?: string;
  attempted?: number;
  due?: number;
};

export type NotificationsData = {
  channels: Channel[];
  events: string[];
  deliveries: Delivery[];
  eventCatalog?: { kind: string; description: string; automatic: boolean }[];
  crashWatcher?: boolean;
};

export type NewsletterSettings = {
  enabled: boolean;
  weekday: number;
  hour: number;
  userId: number;
  nextDue: number;
  lastAttempt: number;
  lastStatus: string;
};

export const CRITICAL_EVENTS = [
  "server.crash",
  "playback.error",
  "provider.outage",
  "indexer.outage",
  "cache.full",
];
export const TRANSPORT_HELP: Record<string, string> = localized(() => ({
  smtp: t(
    "Use your mail provider’s SMTP server and app password. STARTTLS usually uses port 587; implicit TLS uses 465. Authentication is optional for a trusted TLS relay. Recipients can be separated by commas.",
  ),
  ntfy: t(
    "Enter your complete topic URL. Subscribe to the same topic in the ntfy phone app. Add a bearer token for private topics.",
  ),
  gotify: t(
    "Enter your server’s /message URL and an application token. Connect the Gotify app on your phone to receive alerts.",
  ),
  telegram: t(
    "Create a bot with BotFather, message it first, then enter its bot token and the destination chat ID. Group chat IDs may be negative.",
  ),
  discord: t(
    "Create an incoming webhook in the destination Discord channel and paste its full webhook URL. Mentions are disabled.",
  ),
  slack: t("Create an incoming webhook for the destination Slack channel and paste its full URL."),
  webhook: t(
    "Ether sends a JSON POST with kind, title, message and at. Optionally add a bearer token. Only HTTP 2xx responses count as accepted.",
  ),
  webpush: t(
    "This subscription belongs to one browser profile on one device. Use Connect this device on each device you want to receive alerts.",
  ),
}));
const EVENT_LABELS: Record<string, string> = localized(() => ({
  "server.started": t("Server online"),
  "server.stopping": t("Server stopping"),
  "server.crash": t("Server crashed"),
  "provider.recovered": t("Provider recovered"),
  "indexer.recovered": t("Indexer recovered"),
  "cache.recovered": t("Cache space available"),
  "*": t("All events"),
  "playback.start": t("Playback started"),
  "playback.stop": t("Playback ended"),
  "playback.error": t("Playback failed"),
  "playback.failover": t("Playback switched source"),
  "provider.outage": t("Provider unavailable"),
  "indexer.outage": t("Indexer unavailable"),
  "indexer.budget": t("Indexer budget reached"),
  "cache.full": t("Cache full"),
  "cert.renewed": t("Certificate renewed"),
  "update.available": t("Update available"),
  "user.joined": t("Viewer joined"),
  "title.available": t("Watchlist title available"),
  "newsletter.weekly": t("Weekly newsletter"),
  test: t("Test notification"),
}));
export const eventLabel = (kind: string) => EVENT_LABELS[kind] ?? kind;

export const TRANSPORTS = ["webhook", "discord", "slack", "telegram", "smtp", "ntfy", "gotify"];
const TRANSPORT_LABELS: Record<string, string> = localized(() => ({
  webpush: t("Browser push"),
  webhook: t("Webhook"),
  discord: "Discord",
  slack: "Slack",
  telegram: "Telegram",
  smtp: t("Email (SMTP)"),
  ntfy: "ntfy",
  gotify: "Gotify",
}));
export const transportLabel = (kind: string) => TRANSPORT_LABELS[kind] ?? kind;

export const channelName = (c: Channel) => c.name || transportLabel(c.kind);

export const blankChannel = (): Channel => ({
  id: newId("notify"),
  name: "",
  kind: "webhook",
  smtpMode: "starttls",
  enabled: true,
  events: [...CRITICAL_EVENTS],
  endpoint: "",
  token: "",
  target: "",
  username: "",
  password: "",
  from: "",
  to: "",
  perMinute: 5,
  digestMinutes: 0,
});
