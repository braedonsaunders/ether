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
import { t, tn, tx } from "../../i18n";
import { apiFetch } from "../../lib/api";
import { relativeTime } from "../../lib/format";
import { useAsync } from "../../lib/hooks";

type CompatApp = {
  protocol: string;
  name: string;
  devices: string[];
  videoCodecs: string[] | null;
  subtitles: string[] | null;
  directPlay: string;
  note: string;
  certification: string;
};

type CompatDevice = {
  name: string;
  dolbyVisionProfiles: number[] | null;
  subtitleCodecs: string[] | null;
  notes: string;
};

type ObservedClient = {
  protocol: string;
  name: string;
  lastSeen: string;
};

type Catalog = {
  apps: CompatApp[];
  devices: CompatDevice[];
  observed: ObservedClient[];
};

function protocolLabel(value: string) {
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
}

function plays(app: CompatApp) {
  switch (app.directPlay) {
    case "browser":
      return t("The browser tells Ether which codecs it can play.");
    case "reported":
      return t("This app tells Ether what it can play when someone presses Play.");
    case "stream":
      return t("Ether returns a stream, and the app plays that stream.");
    case "renderer":
      return t("The TV or receiver tells Ether what it can play.");
    case "player":
      return t("The player on the device decides. Ether serves the file.");
    case "findroid":
      return t("Findroid asks to direct-play, with srt and ass subtitle sidecars.");
    case "open":
      return t("The app asks to direct-play and names a transcode fallback.");
    case "listed":
      return app.videoCodecs?.join(", ") || t("Not measured yet.");
    default:
      return t("Not measured yet.");
  }
}

function appNote(note: string) {
  switch (note) {
    case "no-dv5":
      return t("Dolby Vision profile 5 is not decoded.");
    case "capture":
      return t("Documented only. A real capture is still required.");
    case "unsupported-shell":
      return t("The 2.x Apple TV app is a web shell and is not supported.");
    case "h264-ac3":
      return t("Transcode fallback is H.264 and AC3.");
    case "sdk":
      return t("Reference client used to test the protocol, not a television app.");
    default:
      return "";
  }
}

function certificationLabel(value: string) {
  switch (value) {
    case "pending":
      return t("Not certified yet");
    case "unsupported":
      return t("Not supported");
    case "builtin":
      return t("Built in");
    default:
      return value;
  }
}

function deviceNote(note: string) {
  switch (note) {
    case "Text is served separately; image subtitles require explicit client support.":
      return t("Text is served separately; image subtitles require explicit client support.");
    case "Dolby Vision profile 7 uses a compatible HDR base layer; TrueHD requires an audio transcode unless declared supported.":
      return t(
        "Dolby Vision profile 7 uses a compatible HDR base layer; TrueHD requires an audio transcode unless declared supported.",
      );
    case "Browser codec declarations govern playback; text subtitles remain sidecars.":
      return t("Browser codec declarations govern playback; text subtitles remain sidecars.");
    case "The TV or receiver advertises what it can play.":
      return t("The TV or receiver advertises what it can play.");
    default:
      return note;
  }
}

function seenFor(app: CompatApp, observed: ObservedClient[]) {
  return observed.filter(
    (row) =>
      row.protocol === app.protocol &&
      (row.name === app.name || app.name.includes(row.name) || row.name.includes(app.name)),
  );
}

/** Built-in app reference, separate from the devices those apps run on. */
export function DeviceCapabilities() {
  const [protocol, setProtocol] = useState("");
  const [device, setDevice] = useState("");
  const matrix = useAsync(() => apiFetch<Catalog>("/playback/policy"), []);
  const apps = matrix.data?.apps ?? [];
  const devices = matrix.data?.devices ?? [];
  const observed = matrix.data?.observed ?? [];
  const protocols = [
    ...new Set(["plex", "jellyfin", "emby", "silo", "stremio", "dlna", "webdav", ...apps.map((app) => app.protocol)]),
  ];
  const deviceNames = [...new Set(devices.map((row) => row.name))];
  const visibleApps = apps.filter(
    (app) =>
      (!protocol || app.protocol === protocol) && (!device || app.devices.includes(device)),
  );
  const visibleDevices = devices.filter((row) => !device || row.name === device);
  const matched = new Set(visibleApps.flatMap((app) => seenFor(app, observed).map((row) => `${row.protocol}:${row.name}`)));
  const unmatched = observed.filter(
    (row) => !matched.has(`${row.protocol}:${row.name}`) && (!protocol || row.protocol === protocol),
  );
  return (
    <>
      <p className="max-w-3xl text-sm text-fg-muted">
        {tx(
          "Apps are separate from the devices they run on. The same Apple TV is an Apple TV in Plex, Infuse, Swiftfin, and Silo. To limit quality for one of them, use {link}.",
          {
            link: (
              <Link to="/releases/devices" className="text-primary underline">
                {t("Device rules")}
              </Link>
            ),
          },
        )}
      </p>
      {matrix.data ? (
        <p className="text-sm">
          {tn(apps.length, "{count} known app.", "{count} known apps.")}{" "}
          {observed.length === 0
            ? t("None of these apps have connected to this server yet.")
            : tn(
                observed.length,
                "{count} client has connected to this server.",
                "{count} clients have connected to this server.",
              )}
        </p>
      ) : null}
      <div className="flex flex-wrap gap-4">
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
        <label className="flex items-center gap-2 text-sm">
          {t("Device")}
          <select aria-label={t("Device")} value={device} onChange={(e) => setDevice(e.currentTarget.value)}>
            <option value="">{t("All devices")}</option>
            {deviceNames.map((name) => (
              <option key={name} value={name}>
                {name}
              </option>
            ))}
          </select>
        </label>
      </div>
      {matrix.error ? (
        <ErrorPanel error={matrix.error} onRetry={() => void matrix.reload()} />
      ) : !matrix.data ? (
        <LoadingRows />
      ) : (
        <>
          <Section title={t("Apps")} bodyClassName="overflow-x-auto">
            {visibleApps.length === 0 ? (
              <p className="text-sm text-fg-muted">{t("No apps match this filter.")}</p>
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>{t("App")}</TableHead>
                    <TableHead>{t("Protocol")}</TableHead>
                    <TableHead>{t("Typical devices")}</TableHead>
                    <TableHead>{t("Plays directly")}</TableHead>
                    <TableHead>{t("Subtitles")}</TableHead>
                    <TableHead>{t("Certification")}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {visibleApps.map((app) => {
                    const seen = seenFor(app, observed);
                    const note = appNote(app.note);
                    return (
                      <TableRow key={`${app.protocol}:${app.name}`}>
                        <TableCell className="font-medium">
                          {app.name}
                          {seen.length > 0 ? (
                            <span className="mt-1 block text-xs font-normal text-fg-muted">
                              {tx("Seen on this server, last {when}", {
                                when: relativeTime(seen[0]!.lastSeen),
                              })}
                            </span>
                          ) : null}
                        </TableCell>
                        <TableCell>{protocolLabel(app.protocol)}</TableCell>
                        <TableCell>{app.devices.join(", ") || t("Any")}</TableCell>
                        <TableCell>
                          {plays(app)}
                          {note ? <span className="mt-1 block text-fg-muted">{note}</span> : null}
                        </TableCell>
                        <TableCell>{app.subtitles?.join(", ") || t("Reported by the app")}</TableCell>
                        <TableCell>{certificationLabel(app.certification)}</TableCell>
                      </TableRow>
                    );
                  })}
                </TableBody>
              </Table>
            )}
          </Section>
          {unmatched.length > 0 ? (
            <Section title={t("Also seen on this server")}>
              <ul className="text-sm">
                {unmatched.map((row) => (
                  <li key={`${row.protocol}:${row.name}`}>
                    {protocolLabel(row.protocol)} · {row.name} · {relativeTime(row.lastSeen)}
                  </li>
                ))}
              </ul>
            </Section>
          ) : null}
          <Section title={t("Devices")} bodyClassName="overflow-x-auto">
            <p className="mb-3 max-w-3xl text-sm text-fg-muted">
              {t(
                "A device is the box or screen. These rows are shared by every app that runs on it, and they are not a Plex list.",
              )}
            </p>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>{t("Device")}</TableHead>
                  <TableHead>{t("Dolby Vision")}</TableHead>
                  <TableHead>{t("Subtitles")}</TableHead>
                  <TableHead>{t("Behaviour")}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {visibleDevices.map((row) => (
                  <TableRow key={row.name}>
                    <TableCell className="font-medium">{row.name}</TableCell>
                    <TableCell>
                      {row.dolbyVisionProfiles?.join(", ") || t("Reported by the app")}
                    </TableCell>
                    <TableCell>{row.subtitleCodecs?.join(", ") || t("Reported by the app")}</TableCell>
                    <TableCell className="text-fg-muted">{deviceNote(row.notes)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </Section>
        </>
      )}
    </>
  );
}
