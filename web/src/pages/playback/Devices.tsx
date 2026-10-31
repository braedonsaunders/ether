import {
  Badge,
  Button,
  SubtabNav,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@braedonsaunders/appkit-ui";
import { useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { Section } from "../../components/layout";
import { t, tn, tx } from "../../i18n";
import { api, apiFetch } from "../../lib/api";
import { relativeTime } from "../../lib/format";
import { useAsync } from "../../lib/hooks";
import { BoolRow, RecordDrawer, TextRow } from "../sources/form";
import { codecLabel } from "./hardwareCodecs";

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
  id?: string;
  name: string;
  dolbyVisionProfiles: number[] | null;
  subtitleCodecs: string[] | null;
  notes: string;
  videoCodecs?: string[] | null;
  maxHeight?: number;
  hdr10?: boolean;
  dolbyVision?: boolean;
  builtinMaxHeight?: number;
  builtinHdr10?: boolean;
  builtinDolbyVision?: boolean;
};

type CapabilityOverride = {
  id?: string;
  product?: string;
  dolbyVisionProfiles?: number[];
  subtitleCodecs?: string[];
  videoCodecs?: string[];
  maxHeight?: number;
  hdr10?: boolean;
  dolbyVision?: boolean;
  notes?: string;
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
  overrides?: CapabilityOverride[];
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

function overridden(row: CompatDevice) {
  return (
    (row.maxHeight ?? 0) !== (row.builtinMaxHeight ?? 0) ||
    Boolean(row.hdr10) !== Boolean(row.builtinHdr10) ||
    Boolean(row.dolbyVision) !== Boolean(row.builtinDolbyVision)
  );
}

function CapabilityBadges({ row }: { row: CompatDevice }) {
  const badges = [
    ...(row.videoCodecs ?? []).map((codec) => codecLabel(codec)),
    row.maxHeight ? t("Up to {height}p", { height: row.maxHeight }) : "",
    row.hdr10 ? t("HDR") : "",
    row.dolbyVision ? t("Dolby Vision") : "",
    ...(row.dolbyVisionProfiles ?? []).map((profile) => t("Profile {profile}", { profile })),
  ].filter(Boolean);
  if (!badges.length) {
    return <span className="text-fg-muted">{t("Reported by the app")}</span>;
  }
  return (
    <span className="flex flex-wrap gap-1">
      {badges.map((label) => (
        <Badge key={label} variant="secondary">
          {label}
        </Badge>
      ))}
    </span>
  );
}

function listText(values: string[] | null | undefined) {
  return values?.join(", ") ?? "";
}

function parseList(value: string) {
  return value
    .split(",")
    .map((part) => part.trim())
    .filter(Boolean);
}

function parseProfiles(value: string) {
  return parseList(value)
    .map((part) => Number(part))
    .filter((n) => Number.isInteger(n) && n >= 0);
}

/** Built-in app reference, separate from the devices those apps run on. */
export function DeviceCapabilities() {
  const navigate = useNavigate();
  const { section } = useParams();
  const view = section === "devices" ? "devices" : "apps";
  const [protocol, setProtocol] = useState("");
  const [device, setDevice] = useState("");
  const [editing, setEditing] = useState<CompatDevice | null>(null);
  const [draft, setDraft] = useState({
    profiles: "",
    subtitles: "",
    video: "",
    height: "",
    hdr: false,
    dolby: false,
    product: "",
  });
  const [baseline, setBaseline] = useState(draft);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<unknown>(null);
  const matrix = useAsync(() => apiFetch<Catalog>("/playback/policy"), []);
  const apps = matrix.data?.apps ?? [];
  const devices = matrix.data?.devices ?? [];
  const observed = matrix.data?.observed ?? [];
  const protocols = [
    ...new Set([
      "plex",
      "jellyfin",
      "emby",
      "silo",
      "stremio",
      "dlna",
      "webdav",
      ...apps.map((app) => app.protocol),
    ]),
  ];
  const deviceNames = [...new Set(devices.map((row) => row.name))];
  const visibleApps = apps.filter(
    (app) => (!protocol || app.protocol === protocol) && (!device || app.devices.includes(device)),
  );
  const visibleDevices = devices.filter((row) => !device || row.name === device);
  const matched = new Set(
    visibleApps.flatMap((app) =>
      seenFor(app, observed).map((row) => `${row.protocol}:${row.name}`),
    ),
  );
  const unmatched = observed.filter(
    (row) =>
      !matched.has(`${row.protocol}:${row.name}`) && (!protocol || row.protocol === protocol),
  );
  const openDevice = (row: CompatDevice) => {
    const saved = matrix.data?.overrides?.find((item) => item.id === row.id);
    const next = {
      profiles: listText(row.dolbyVisionProfiles?.map(String) ?? null),
      subtitles: listText(row.subtitleCodecs),
      video: listText(row.videoCodecs),
      height: row.maxHeight ? String(row.maxHeight) : "",
      hdr: Boolean(row.hdr10),
      dolby: Boolean(row.dolbyVision),
      product: saved?.product ?? "",
    };
    setDraft(next);
    setBaseline(next);
    setSaveError(null);
    setEditing(row);
  };
  const saveDevice = async () => {
    if (!editing?.id) return;
    setSaving(true);
    setSaveError(null);
    const saved = matrix.data?.overrides?.find((item) => item.id === editing.id);
    const override: CapabilityOverride = { ...saved, id: editing.id };
    if (draft.profiles !== baseline.profiles)
      override.dolbyVisionProfiles = parseProfiles(draft.profiles);
    if (draft.subtitles !== baseline.subtitles)
      override.subtitleCodecs = parseList(draft.subtitles);
    if (draft.video !== baseline.video) override.videoCodecs = parseList(draft.video);
    const height = Number(draft.height);
    const builtinHeight = editing.builtinMaxHeight ?? editing.maxHeight ?? 0;
    if (draft.height !== baseline.height) {
      if (Number.isInteger(height) && height > 0 && height !== builtinHeight)
        override.maxHeight = height;
      else delete override.maxHeight;
    }
    if (draft.hdr !== baseline.hdr) override.hdr10 = draft.hdr;
    if (draft.dolby !== baseline.dolby) override.dolbyVision = draft.dolby;
    if (draft.product !== baseline.product) {
      if (draft.product.trim()) override.product = draft.product.trim();
      else delete override.product;
    }
    const others = (matrix.data?.overrides ?? []).filter((item) => item.id !== editing.id);
    const changed = Object.keys(override).some((key) => key !== "id");
    const overrides = changed ? [...others, override] : others;
    try {
      await api.putSection("clients", { overrides });
      setEditing(null);
      await matrix.reload();
    } catch (err) {
      setSaveError(err);
    } finally {
      setSaving(false);
    }
  };
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
      <SubtabNav
        ariaLabel={t("App compatibility sections")}
        active={view}
        onSelect={(key) => navigate(`/playback/devices/${key}`)}
        tabs={[
          { key: "apps", label: t("Apps") },
          { key: "devices", label: t("Devices") },
        ]}
      />
      <p className="max-w-3xl text-sm text-fg-muted">
        {view === "apps"
          ? t("An app is the program someone presses Play in.")
          : t(
              "A device is the box or screen. Every app on it shares these limits. Open a row to change what Ether assumes.",
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
          <select
            aria-label={t("Device")}
            value={device}
            onChange={(e) => setDevice(e.currentTarget.value)}
          >
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
          {view === "apps" ? (
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
                          <TableCell>
                            {app.subtitles?.join(", ") || t("Reported by the app")}
                          </TableCell>
                          <TableCell>
                            <Badge
                              variant={
                                app.certification === "unsupported" ? "warning" : "secondary"
                              }
                            >
                              {certificationLabel(app.certification)}
                            </Badge>
                          </TableCell>
                        </TableRow>
                      );
                    })}
                  </TableBody>
                </Table>
              )}
            </Section>
          ) : null}
          {view === "apps" && unmatched.length > 0 ? (
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
          {view === "devices" ? (
            <Section title={t("Devices")} bodyClassName="overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>{t("Device")}</TableHead>
                    <TableHead>{t("Plays")}</TableHead>
                    <TableHead>{t("Subtitles")}</TableHead>
                    <TableHead>{t("Notes")}</TableHead>
                    <TableHead />
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {visibleDevices.map((row) => (
                    <TableRow key={row.name}>
                      <TableCell className="font-medium">
                        <span className="flex flex-wrap items-center gap-2">
                          {row.name}
                          {overridden(row) ? <Badge variant="warning">{t("Changed")}</Badge> : null}
                        </span>
                      </TableCell>
                      <TableCell>
                        <CapabilityBadges row={row} />
                      </TableCell>
                      <TableCell>
                        {row.subtitleCodecs?.join(", ") || t("Reported by the app")}
                      </TableCell>
                      <TableCell className="text-fg-muted">{deviceNote(row.notes)}</TableCell>
                      <TableCell className="text-right">
                        {row.id ? (
                          <Button
                            variant="outline"
                            size="sm"
                            aria-label={t("Edit {name}", { name: row.name })}
                            onClick={() => openDevice(row)}
                          >
                            {t("Edit")}
                          </Button>
                        ) : null}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </Section>
          ) : null}
          <RecordDrawer
            open={editing != null}
            title={editing ? tx("Edit {name}", { name: editing.name }) : ""}
            description={t("What the app reports at playback replaces this.")}
            onClose={() => setEditing(null)}
            onSave={() => void saveDevice()}
            saving={saving}
            saveLabel={t("Save")}
            error={saveError}
          >
            <TextRow
              label={t("Dolby Vision profiles")}
              value={draft.profiles}
              placeholder={t("Comma-separated")}
              onChange={(profiles) => setDraft({ ...draft, profiles })}
            />
            <TextRow
              label={t("Subtitles")}
              value={draft.subtitles}
              placeholder={t("Comma-separated")}
              onChange={(subtitles) => setDraft({ ...draft, subtitles })}
            />
            <TextRow
              label={t("Video codecs")}
              value={draft.video}
              placeholder={t("Comma-separated")}
              onChange={(video) => setDraft({ ...draft, video })}
            />
            <TextRow
              label={t("Maximum resolution")}
              help={
                editing?.builtinMaxHeight
                  ? t("Built in is {height}p.", { height: editing.builtinMaxHeight })
                  : undefined
              }
              value={draft.height}
              onChange={(height) => setDraft({ ...draft, height })}
            />
            <BoolRow
              label={t("HDR")}
              help={editing?.builtinHdr10 ? t("Built in: on.") : t("Built in: off.")}
              value={draft.hdr}
              onChange={(hdr) => setDraft({ ...draft, hdr })}
            />
            <BoolRow
              label={t("Dolby Vision")}
              help={editing?.builtinDolbyVision ? t("Built in: on.") : t("Built in: off.")}
              value={draft.dolby}
              onChange={(dolby) => setDraft({ ...draft, dolby })}
            />
            <TextRow
              label={t("Only when the app name contains")}
              help={t("Leave this blank to change the device for every app.")}
              value={draft.product}
              onChange={(product) => setDraft({ ...draft, product })}
            />
          </RecordDrawer>
        </>
      )}
    </>
  );
}
