import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@braedonsaunders/appkit-ui";
import { Link } from "react-router-dom";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { Section } from "../../components/layout";
import { t, tx } from "../../i18n";
import { apiFetch } from "../../lib/api";
import { useAsync } from "../../lib/hooks";

type Policy = {
  family: string;
  dolbyVisionProfiles: number[] | null;
  subtitleCodecs: string[];
  notes: string;
};

/** Built-in compatibility reference for each family of Plex apps. */
export function DeviceCapabilities() {
  const matrix = useAsync(() => apiFetch<{ policies: Policy[] }>("/playback/policy"), []);
  return (
    <>
      <p className="max-w-3xl text-sm text-fg-muted">
        {tx(
          "This is a reference. Each Plex app reports its actual codecs, display and receiver support when it plays. To choose quality for a device, use {link}.",
          {
            link: (
              <Link to="/releases/rules" className="text-primary underline">
                {t("Client rules")}
              </Link>
            ),
          },
        )}
      </p>
      {matrix.error ? (
        <ErrorPanel error={matrix.error} onRetry={() => void matrix.reload()} />
      ) : !matrix.data ? (
        <LoadingRows />
      ) : (
        <Section title={t("Device capabilities")} bodyClassName="overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("App")}</TableHead>
                <TableHead>Dolby Vision</TableHead>
                <TableHead>{t("Subtitles")}</TableHead>
                <TableHead>{t("Behaviour")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {matrix.data.policies.map((p) => (
                <TableRow key={p.family}>
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
