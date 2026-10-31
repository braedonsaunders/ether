import { Badge } from "@braedonsaunders/appkit-ui";
import { Cpu } from "lucide-react";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { Section } from "../../components/layout";
import { t, tn } from "../../i18n";
import { apiFetch } from "../../lib/api";
import { useAsync, useInterval } from "../../lib/hooks";

type Hardware = {
  hwaccel?: { name: string; encoders: string[] | null; device?: string };
  ffmpeg?: unknown;
  error?: string;
  notice?: string;
  playbackError?: string;
  enhancement?: {
    backend: string;
    setupState?: string;
    available: boolean;
    reason?: string;
    version?: string;
    protocols?: string[];
    active: number;
  };
};

/** Detected transcoding hardware: acceleration, encoders and the bundled FFmpeg. */
export function HardwareStatus() {
  const hw = useAsync(
    async (signal) =>
      (await apiFetch<{ value: Hardware | null }>("/status/hwaccel", { signal })).value ?? {},
    [],
  );
  useInterval(() => {
    if (!hw.loading) void hw.reload();
  }, 5000);
  const data = hw.data;
  const accel = data?.hwaccel;
  const encoders = accel?.encoders ?? [];
  const none = !accel?.name || accel.name === "none";
  const ffmpeg = data?.ffmpeg;

  return (
    <Section
      title={t("Transcoding hardware")}
      description={t(
        "What this machine can use to convert video for devices that can't play it directly.",
      )}
    >
      {hw.error && !data ? (
        <ErrorPanel error={hw.error} onRetry={() => void hw.reload()} />
      ) : !data ? (
        <LoadingRows rows={1} />
      ) : data.error ? (
        <p role="alert" className="text-sm text-danger">
          {data.error}
        </p>
      ) : (
        <div className="space-y-3">
          <div className="flex flex-wrap items-center gap-3">
            <span className="grid size-9 place-items-center rounded-lg bg-primary-subtle text-primary">
              <Cpu size={18} />
            </span>
            <div className="min-w-0">
              <p className="text-sm font-medium text-fg">
                {none
                  ? t("Software only")
                  : t("Hardware acceleration: {name}", { name: accel!.name })}
              </p>
              <p className="text-xs text-fg-muted">
                {[
                  accel?.device
                    ? t("Device {device}", { device: accel.device })
                    : none
                      ? t("Transcoding uses the CPU.")
                      : "",
                  typeof ffmpeg === "string" && ffmpeg ? `FFmpeg ${ffmpeg}` : "",
                ]
                  .filter(Boolean)
                  .join(" · ")}
              </p>
            </div>
          </div>
          <div className="flex flex-wrap gap-1.5" aria-label={t("Hardware encoders")}>
            {encoders.length ? (
              encoders.map((e) => (
                <Badge key={e} variant="secondary" className="font-mono">
                  {e}
                </Badge>
              ))
            ) : (
              <span className="text-xs text-fg-muted">{t("No hardware encoders detected")}</span>
            )}
          </div>
          {ffmpeg && typeof ffmpeg !== "string" ? (
            <details>
              <summary className="cursor-pointer text-xs font-medium text-fg-muted">
                {t("FFmpeg details")}
              </summary>
              <pre className="mt-2 overflow-x-auto whitespace-pre-wrap rounded-md bg-bg-subtle p-3 text-xs">
                {JSON.stringify(ffmpeg, null, 2)}
              </pre>
            </details>
          ) : null}
          {data.playbackError || data.notice ? (
            <p role="alert" className="text-sm text-danger">
              {data.playbackError || data.notice}
            </p>
          ) : null}
          {data.enhancement ? (
            <div
              className="rounded-md border border-border p-3"
              aria-label={t("Server enhancement status")}
            >
              <p className="text-sm font-medium text-fg">
                {t("RTX Video: {status}", {
                  status:
                    data.enhancement.active > 0
                      ? tn(data.enhancement.active, "{count} active", "{count} active")
                      : data.enhancement.available
                        ? t("Detected · idle")
                        : data.enhancement.setupState === "off"
                          ? t("Off")
                          : data.enhancement.setupState === "installing"
                            ? t("Installing runtime")
                            : data.enhancement.setupState === "checking"
                              ? t("Checking support")
                              : t("Unavailable"),
                })}
              </p>
              <p className="mt-1 text-xs text-fg-muted">
                {data.enhancement.reason ||
                  t("Detected support is verified when an eligible stream starts.")}
                {data.enhancement.version ? ` · ${data.enhancement.version}` : ""}
              </p>
            </div>
          ) : null}
        </div>
      )}
    </Section>
  );
}
