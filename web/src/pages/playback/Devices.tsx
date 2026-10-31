import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@braedonsaunders/appkit-ui";
import { useState } from "react";
import { Link } from "react-router-dom";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { Section } from "../../components/layout";
import { t, tx } from "../../i18n";
import { apiFetch } from "../../lib/api";
import { useAsync } from "../../lib/hooks";

type Policy = {
  family: string;
  protocol?: string;
  dolbyVisionProfiles: number[] | null;
  subtitleCodecs: string[];
  notes: string;
};

/** Built-in compatibility reference and families reported by front ends. */
export function DeviceCapabilities() {
  const [protocol, setProtocol] = useState("");
  const matrix = useAsync(() => apiFetch<{ policies: Policy[] }>("/playback/policy"), []);
  const protocols = [
    ...new Set([
      "plex",
      "jellyfin",
      "emby",
      "silo",
      ...(matrix.data?.policies.map((p) => p.protocol ?? "plex") ?? []),
    ]),
  ];
  const protocolLabel = (value: string) => {
    switch (value) {
      case "plex":
        return "Plex";
      case "jellyfin":
        return "Jellyfin";
      case "emby":
        return "Emby";
      case "silo":
        return "Silo";
      case "stremio":
        return "Stremio";
      case "webdav":
        return t("Files");
      case "dlna":
        return "DLNA";
      default:
        return value;
    }
  };
  return (
    <>
      <p className="max-w-3xl text-sm text-fg-muted">
        {tx(
          "Each app tells Ether what it can play when someone presses Play. This table is the built-in reference. To limit quality for a device, use {link}.",
          {
            link: (
              <Link to="/releases/rules" className="text-primary underline">
                {t("Client rules")}
              </Link>
            ),
          },
        )}
      </p>
      <label className="flex items-center gap-2 text-sm">
        {t("Protocol")}
        <select
          aria-label={t("Protocol")}
          value={protocol}
          onChange={(e) => setProtocol(e.currentTarget.value)}
        >
          <option value="">{t("All protocols")}</option>
          {protocols.map((value) => (
            <option key={value} value={value}>
              {protocolLabel(value)}
            </option>
          ))}
        </select>
      </label>
      {matrix.error ? (
        <ErrorPanel error={matrix.error} onRetry={() => void matrix.reload()} />
      ) : !matrix.data ? (
        <LoadingRows />
      ) : (
        <Section title={t("App compatibility")} bodyClassName="overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("Protocol")}</TableHead>
                <TableHead>{t("App")}</TableHead>
                <TableHead>Dolby Vision</TableHead>
                <TableHead>{t("Subtitles")}</TableHead>
                <TableHead>{t("Behaviour")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {matrix.data.policies
                .filter((p) => !protocol || (p.protocol ?? "plex") === protocol)
                .map((p) => (
                  <TableRow key={`${p.protocol ?? "plex"}:${p.family}`}>
                    <TableCell>{protocolLabel(p.protocol ?? "plex")}</TableCell>
                    <TableCell className="font-medium">{p.family}</TableCell>
                    <TableCell>
                      {p.dolbyVisionProfiles?.join(", ") || t("Reported by the app")}
                    </TableCell>
                    <TableCell>{p.subtitleCodecs.join(", ")}</TableCell>
                    <TableCell className="text-fg-muted">{p.notes}</TableCell>
                  </TableRow>
                ))}
            </TableBody>
          </Table>
        </Section>
      )}
    </>
  );
}
