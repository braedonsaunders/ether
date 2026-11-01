import { Badge } from "@braedonsaunders/appkit-ui";
import { Cpu } from "lucide-react";
import type { ReactNode } from "react";
import apple from "../../assets/brands/apple.svg?no-inline";
import amd from "../../assets/brands/amd.svg?no-inline";
import intel from "../../assets/brands/intel.svg?no-inline";
import nvidia from "../../assets/brands/nvidia.svg?no-inline";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { Section } from "../../components/layout";
import { t, tn } from "../../i18n";
import { apiFetch } from "../../lib/api";
import { useAsync, useInterval } from "../../lib/hooks";
import { codecLabel, encodeCodecs } from "./hardwareCodecs";

type Hardware = {
  hwaccel?: { name: string; encoders: string[] | null; device?: string };
  ffmpeg?: unknown;
  error?: string;
  notice?: string;
  playbackError?: string;
  filtersProbed?: boolean;
  toneMap?: string[];
  colour?: string[];
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

function BrandMark({ src, color }: { src: string; color: string }) {
  return (
    <span
      aria-hidden
      className="block size-8 bg-current"
      style={{
        color,
        maskImage: `url("${src}")`,
        maskPosition: "center",
        maskSize: "contain",
        maskRepeat: "no-repeat",
        WebkitMaskImage: `url("${src}")`,
        WebkitMaskPosition: "center",
        WebkitMaskSize: "contain",
        WebkitMaskRepeat: "no-repeat",
      }}
    />
  );
}

function vendorMark(name: string | undefined): ReactNode {
  switch (name) {
    case "nvenc":
      return <BrandMark src={nvidia} color="#76B900" />;
    case "qsv":
      return <BrandMark src={intel} color="#0071C5" />;
    case "vaapi":
      return <BrandMark src={amd} color="#ED1C24" />;
    case "videotoolbox":
      return <BrandMark src={apple} color="currentColor" />;
    default:
      return <Cpu size={22} aria-hidden />;
  }
}

function vendorCopy(name: string | undefined): { title: string; detail: string } {
  switch (name) {
    case "videotoolbox":
      return {
        title: t("Apple VideoToolbox"),
        detail: t("This Mac converts video in hardware."),
      };
    case "nvenc":
      return {
        title: t("NVIDIA NVENC"),
        detail: t("This computer's NVIDIA card converts video."),
      };
    case "qsv":
      return {
        title: t("Intel Quick Sync"),
        detail: t("This computer's Intel graphics convert video."),
      };
    case "vaapi":
      return {
        title: t("AMD VA-API"),
        detail: t("This computer converts video through VA-API."),
      };
    default:
      return { title: t("Software only"), detail: t("Transcoding uses the CPU.") };
  }
}

function toneMapName(path: string): string {
  switch (path) {
    case "zscale":
      return t("Software");
    case "libplacebo":
      return "libplacebo";
    case "opencl":
      return "OpenCL";
    case "vaapi":
      return "VA-API";
    case "videotoolbox":
      return "VideoToolbox";
    case "cuda":
      return "NVIDIA";
    default:
      return path;
  }
}

/** Detected transcoding hardware: the graphics vendor, codecs, and HDR paths. */
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
  const ffmpeg = typeof data?.ffmpeg === "string" ? data.ffmpeg : "";
  const codecs = encodeCodecs(encoders);
  const toneMap = data?.toneMap ?? [];
  const colour = data?.colour ?? [];
  const copy = vendorCopy(none ? "none" : accel?.name);

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
        <div className="space-y-4">
          <div className="flex items-center gap-4">
            <span className="grid size-14 shrink-0 place-items-center rounded-2xl bg-bg-subtle text-fg">
              {vendorMark(none ? undefined : accel?.name)}
            </span>
            <div className="min-w-0">
              <p className="text-base font-semibold text-fg">{copy.title}</p>
              <p className="text-sm text-fg-muted">{copy.detail}</p>
              <p className="mt-1 text-xs text-fg-muted">
                {[
                  accel?.device ? t("Device {device}", { device: accel.device }) : "",
                  ffmpeg ? t("FFmpeg {version}", { version: ffmpeg }) : "",
                ]
                  .filter(Boolean)
                  .join(" · ")}
              </p>
            </div>
          </div>
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="rounded-lg border border-border bg-bg-subtle p-3">
              <p className="text-xs font-medium text-fg-muted">{t("Encodes")}</p>
              <div className="mt-2 flex flex-wrap gap-1.5" aria-label={t("Hardware encoders")}>
                {codecs.length ? (
                  codecs.map((codec) => (
                    <Badge key={codec} variant="secondary">
                      {codecLabel(codec)}
                    </Badge>
                  ))
                ) : (
                  <span className="text-sm text-fg-muted">
                    {t("No hardware encoders detected")}
                  </span>
                )}
              </div>
            </div>
            <div className="rounded-lg border border-border bg-bg-subtle p-3">
              <p className="text-xs font-medium text-fg-muted">{t("HDR")}</p>
              <div className="mt-2 flex flex-wrap gap-1.5" aria-label={t("HDR")}>
                {toneMap.length ? (
                  <Badge variant="secondary">{t("HDR10")}</Badge>
                ) : (
                  <span className="text-sm text-fg-muted">
                    {t("No HDR conversion on this computer.")}
                  </span>
                )}
              </div>
              {toneMap.length ? (
                <p className="mt-2 text-xs text-fg-muted">
                  {t("HDR to SDR uses {paths}.", {
                    paths: toneMap.map(toneMapName).join(", "),
                  })}
                </p>
              ) : null}
              {colour.length ? (
                <p className="mt-1 text-xs text-fg-muted">{t("Colour conversion is available.")}</p>
              ) : null}
            </div>
          </div>
          {data.filtersProbed === false && !data.notice && !data.playbackError ? (
            <p className="text-sm text-fg-muted">{t("Optional filters were not read.")}</p>
          ) : null}
          {data.playbackError || data.notice ? (
            <p role="alert" className="text-sm text-danger">
              {data.playbackError || data.notice}
            </p>
          ) : null}
          {data.enhancement ? (
            <div
              className="rounded-lg border border-border p-3"
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
