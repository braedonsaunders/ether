import { t } from "../../i18n";
import { errorMessage } from "../../lib/api";

/** Provider transport details remain in server logs; the console explains the remedy. */
export function sourceImportError(error: unknown, previous = true): string {
  const raw = typeof error === "string" ? error : errorMessage(error);
  const mismatch = /^login (\d+): playlist does not match channel "(.*)"$/.exec(raw);
  if (mismatch)
    return t("Login {number} does not include channel {channel}.", {
      number: mismatch[1],
      channel: mismatch[2],
    });
  const status = Number(/HTTP\s+(\d{3})/i.exec(raw)?.[1] ?? 0);
  let cause: string;
  if (status === 401 || status === 403 || /refused the credentials/i.test(raw))
    cause = t("The provider rejected the login. Check the username and password.");
  else if (status === 429)
    cause = t("The provider is handling too many requests. Try again shortly.");
  else if (status >= 500)
    cause = t("The provider answered {status} (temporarily unavailable). Try again shortly.", {
      status,
    });
  else if (status)
    cause = t("The provider answered {status}. Check the source settings and try again.", {
      status,
    });
  else if (/no such host|name resolution|lookup /i.test(raw))
    cause = t("The provider could not be found. Check the server address.");
  else if (/connection refused/i.test(raw))
    cause = t("The provider refused the connection. Check the address and try again.");
  else if (/certificate|\btls\b|x509/i.test(raw))
    cause = t("A secure connection to the provider could not be established.");
  else if (/timeout|timed out|deadline|within \d|stopped sending/i.test(raw))
    cause = t("The provider took too long to respond. Try again shortly.");
  else cause = t("Check the source settings and try again.");

  const message = /^playlist:/i.test(raw)
    ? t("Couldn't download the playlist. {cause}", { cause })
    : /^guide:/i.test(raw)
      ? t("Couldn't download the guide. {cause}", { cause })
      : /^Xtream account:/i.test(raw)
        ? t("Couldn't check the provider account. {cause}", { cause })
        : /^Xtream (categories|channels|channel list|category list)/i.test(raw)
          ? t("Couldn't download the channel list. {cause}", { cause })
          : t("Couldn't import this source. {cause}", { cause });
  return `${message} ${previous ? t("Your previous import is still in use.") : t("No channels were changed.")}`;
}
