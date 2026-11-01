import { useState } from "react";
import { SettingsRow } from "../../components/SettingsRow";
import { t } from "../../i18n";
import {
  chosenAddress,
  homeSummary,
  usableAddresses,
  type ReachabilityStatus,
} from "./reachability";

/** The At home line names the address the owner is using, and keeps the rest behind a disclosure. */
export function HomeStatusRow({
  status,
  address,
}: {
  status: ReachabilityStatus | null | undefined;
  address?: string;
}) {
  const [showOther, setShowOther] = useState(false);
  const chosen = chosenAddress(status, address);
  const others = usableAddresses(status).filter((item) => item !== chosen);
  const home = homeSummary(status, chosen);
  return (
    <SettingsRow
      title={home.title}
      description={
        <>
          <span>{home.detail}</span>
          {others.length > 0 ? (
            <span className="mt-1 block">
              <button
                type="button"
                className="text-primary underline"
                aria-expanded={showOther}
                onClick={() => setShowOther((value) => !value)}
              >
                {t("Other addresses")}
              </button>
              {showOther ? (
                <ul className="mt-1 space-y-1">
                  {others.map((item) => (
                    <li key={item}>
                      <code>{item}</code>
                    </li>
                  ))}
                </ul>
              ) : null}
            </span>
          ) : null}
        </>
      }
    />
  );
}
