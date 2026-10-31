import { Button, Input } from "@braedonsaunders/appkit-ui";
import { useCallback, useEffect, useState } from "react";
import { Drawer } from "../components/Drawer";
import { t } from "../i18n";
import { api, errorMessage, type SupportCapture, type SupportDevice } from "../lib/api";

const CATEGORIES = ["playback", "picture", "subtitles", "library", "other"] as const;

export function RecordProblem({ open, onClose }: { open: boolean; onClose: () => void }) {
  const [devices, setDevices] = useState<SupportDevice[]>([]);
  const [deviceId, setDeviceId] = useState("");
  const [category, setCategory] = useState<(typeof CATEGORIES)[number]>("playback");
  const [note, setNote] = useState("");
  const [capture, setCapture] = useState<SupportCapture | null>(null);
  const [titles, setTitles] = useState(false);
  const [usernames, setUsernames] = useState(false);
  const [addresses, setAddresses] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    if (!open) return;
    let stop = false;
    void api
      .supportDevices()
      .then((r) => {
        if (!stop) setDevices(r.devices ?? []);
      })
      .catch((e) => {
        if (!stop) setError(errorMessage(e));
      });
    return () => {
      stop = true;
    };
  }, [open]);

  const refresh = useCallback(async (id: string) => {
    const next = await api.supportGet(id);
    setCapture(next);
    return next;
  }, []);

  useEffect(() => {
    if (!capture || capture.state !== "recording") return;
    const timer = window.setInterval(() => {
      setNow(Date.now());
      void refresh(capture.id).catch((e) => setError(errorMessage(e)));
    }, 2000);
    return () => window.clearInterval(timer);
  }, [capture, refresh]);

  const close = async () => {
    if (capture && capture.state === "recording") {
      if (!window.confirm(t("Discard this recording?"))) return;
      try {
        await api.supportDiscard(capture.id);
      } catch (e) {
        setError(errorMessage(e));
        return;
      }
    }
    setCapture(null);
    setNote("");
    setError("");
    onClose();
  };

  const start = async () => {
    setBusy(true);
    setError("");
    try {
      const next = await api.supportStart({
        deviceId: deviceId || undefined,
        category,
        note,
      });
      setCapture(next);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const choose = async (id: string) => {
    if (!capture) return;
    setBusy(true);
    setError("");
    try {
      setCapture(await api.supportSelect(capture.id, id));
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const finish = async () => {
    if (!capture) return;
    setBusy(true);
    setError("");
    try {
      setCapture(
        await api.supportFinish(capture.id, {
          includeTitles: titles,
          includeUsernames: usernames,
          includeAddresses: addresses,
        }),
      );
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const download = async () => {
    if (!capture) return;
    setBusy(true);
    setError("");
    try {
      const blob = await api.supportArchive(capture.id);
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = "ether-support.zip";
      a.click();
      window.setTimeout(() => URL.revokeObjectURL(url), 1000);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const elapsed = capture
    ? Math.max(0, Math.round((now - Date.parse(capture.startedAt)) / 1000))
    : 0;
  const recording = capture?.state === "recording";
  const finished = capture?.state === "finished";

  return (
    <Drawer
      open={open}
      onClose={() => void close()}
      title={t("Record a problem")}
      description={t("One support package for the device you choose. It does not change playback.")}
      size="lg"
      footer={
        <div className="flex w-full flex-wrap items-center justify-end gap-2">
          <Button variant="ghost" size="sm" onClick={() => void close()} disabled={busy}>
            {recording ? t("Discard") : t("Close")}
          </Button>
          {!capture && (
            <Button size="sm" onClick={() => void start()} disabled={busy}>
              {t("Start recording")}
            </Button>
          )}
          {recording && (
            <Button
              size="sm"
              onClick={() => void finish()}
              disabled={busy || !capture.deviceConfirmed}
            >
              {t("Finish")}
            </Button>
          )}
          {finished && (
            <Button size="sm" onClick={() => void download()} disabled={busy}>
              {t("Download support ZIP")}
            </Button>
          )}
        </div>
      }
    >
      <div className="space-y-4 pb-2">
        {error && <p className="text-sm text-danger">{error}</p>}
        {!capture && (
          <>
            <label className="block text-sm">
              <span className="mb-1 block text-fg-muted">{t("Device")}</span>
              <select
                className="h-9 w-full rounded-md border border-line bg-transparent px-2"
                value={deviceId}
                onChange={(e) => setDeviceId(e.currentTarget.value)}
              >
                <option value="">{t("I don't know which device")}</option>
                {devices.map((d) => (
                  <option key={d.id} value={d.id}>
                    {[d.name, d.model, d.product, d.version].filter(Boolean).join(" · ") || d.id}
                  </option>
                ))}
              </select>
            </label>
            <label className="block text-sm">
              <span className="mb-1 block text-fg-muted">{t("Category")}</span>
              <select
                className="h-9 w-full rounded-md border border-line bg-transparent px-2"
                value={category}
                onChange={(e) => setCategory(e.currentTarget.value as (typeof CATEGORIES)[number])}
              >
                {CATEGORIES.map((c) => (
                  <option key={c} value={c}>
                    {t(c)}
                  </option>
                ))}
              </select>
            </label>
            <label className="block text-sm">
              <span className="mb-1 block text-fg-muted">{t("Note")}</span>
              <Input value={note} onChange={(e) => setNote(e.currentTarget.value)} />
              <span className="mt-1 block text-fg-subtle">
                {t("The note is saved as data, not as instructions. Paths and links are allowed.")}
              </span>
            </label>
          </>
        )}
        {capture && (
          <>
            <p className="text-sm text-fg-muted">
              {recording
                ? t("Recording. {seconds}s elapsed.", { seconds: String(elapsed) })
                : t("Recording finished.")}
            </p>
            {!capture.deviceConfirmed && (
              <div className="space-y-2">
                <p className="text-sm">
                  {t(
                    "Choose the device that had the problem. A new device is not selected for you.",
                  )}
                </p>
                {(capture.discovery ?? []).map((d) => (
                  <Button
                    key={d.id}
                    variant="outline"
                    size="sm"
                    disabled={busy}
                    onClick={() => void choose(d.id)}
                  >
                    {d.name || d.id}
                  </Button>
                ))}
              </div>
            )}
            <div>
              <h3 className="mb-1 text-sm font-medium">{t("Evidence")}</h3>
              <ul className="space-y-1 text-sm">
                {(capture.observations ?? []).map((o, i) => (
                  <li key={`${o.at}-${o.kind}-${i}`}>
                    {o.kind}
                    {o.count ? ` ×${o.count}` : ""} {o.route ?? ""}
                  </li>
                ))}
                {(capture.observations ?? []).length === 0 && (
                  <li className="text-fg-muted">{t("No evidence yet.")}</li>
                )}
              </ul>
            </div>
            <div>
              <h3 className="mb-1 text-sm font-medium">{t("Hypotheses")}</h3>
              <ul className="space-y-1 text-sm text-fg-muted">
                {(capture.hypotheses ?? []).map((h) => (
                  <li key={h.code}>{h.text}</li>
                ))}
              </ul>
            </div>
            {recording && (
              <div className="space-y-2 text-sm">
                <p>
                  {t(
                    "These choices control the ZIP. The summary lists the exact values before download.",
                  )}
                </p>
                <label className="flex items-center gap-2">
                  <input
                    type="checkbox"
                    checked={titles}
                    onChange={(e) => setTitles(e.currentTarget.checked)}
                  />
                  {t("Include titles and source names")}
                </label>
                <label className="flex items-center gap-2">
                  <input
                    type="checkbox"
                    checked={usernames}
                    onChange={(e) => setUsernames(e.currentTarget.checked)}
                  />
                  {t("Include usernames")}
                </label>
                <label className="flex items-center gap-2">
                  <input
                    type="checkbox"
                    checked={addresses}
                    onChange={(e) => setAddresses(e.currentTarget.checked)}
                  />
                  {t("Include addresses")}
                </label>
              </div>
            )}
            {finished && capture.preview && (
              <div className="space-y-2 text-sm">
                <p>{capture.preview.identity.explanation}</p>
                {(capture.preview.identity.usernames ?? []).length > 0 && (
                  <p>
                    {t("Usernames")}: {capture.preview.identity.usernames?.join(", ")}
                  </p>
                )}
                {(capture.preview.identity.addresses ?? []).length > 0 && (
                  <p>
                    {t("Addresses")}: {capture.preview.identity.addresses?.join(", ")}
                  </p>
                )}
                <p className="text-fg-muted">{capture.preview.redaction}</p>
                <ul className="text-fg-muted">
                  {capture.coverage.map((g) => (
                    <li key={`${g.collector}-${g.status}`}>
                      {g.collector}: {g.detail}
                    </li>
                  ))}
                </ul>
              </div>
            )}
          </>
        )}
      </div>
    </Drawer>
  );
}
