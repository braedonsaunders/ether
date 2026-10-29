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
import { apiFetch } from "../../lib/api";
import { useAsync } from "../../lib/hooks";

type Policy = {
  family: string;
  protocol?: string;
  dolbyVisionProfiles: number[] | null;
  subtitleCodecs: string[];
  notes: string;
};

/** Built-in compatibility reference for each family of Plex apps. */
export function DeviceCapabilities() {
  const [protocol, setProtocol] = useState("");
  const matrix = useAsync(() => apiFetch<{ policies: Policy[] }>("/playback/policy"), []);
  return (
    <>
      <p className="max-w-3xl text-sm text-fg-muted">
        This is a reference. Each app reports its actual codecs, display and receiver support when
        it plays. To choose quality for a device, use{" "}
        <Link to="/releases/rules" className="text-primary underline">
          Client rules
        </Link>
        .
      </p>
      <label className="flex items-center gap-2 text-sm">
        Protocol
        <select
          aria-label="Protocol"
          value={protocol}
          onChange={(e) => setProtocol(e.currentTarget.value)}
        >
          <option value="">All protocols</option>
          <option value="plex">Plex</option>
          <option value="jellyfin">Jellyfin</option>
          <option value="emby">Emby</option>
          <option value="silo">Silo</option>
        </select>
      </label>
      {matrix.error ? (
        <ErrorPanel error={matrix.error} onRetry={() => void matrix.reload()} />
      ) : !matrix.data ? (
        <LoadingRows />
      ) : (
        <Section title="Device capabilities" bodyClassName="overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Protocol</TableHead>
                <TableHead>App</TableHead>
                <TableHead>Dolby Vision</TableHead>
                <TableHead>Subtitles</TableHead>
                <TableHead>Behaviour</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {matrix.data.policies
                .filter((p) => !protocol || (p.protocol ?? "plex") === protocol)
                .map((p) => (
                  <TableRow key={`${p.protocol ?? "plex"}:${p.family}`}>
                    <TableCell>{p.protocol ?? "plex"}</TableCell>
                    <TableCell className="font-medium">{p.family}</TableCell>
                    <TableCell>
                      {p.dolbyVisionProfiles?.join(", ") || "Reported by the app"}
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
