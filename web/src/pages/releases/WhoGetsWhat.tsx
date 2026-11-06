import { Badge, SearchSelect, SettingsSection, cn } from "@braedonsaunders/appkit-ui";
import { Link } from "react-router-dom";
import { useState } from "react";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { Segmented } from "../../components/fields";
import { useViewers, viewerName } from "../../components/pickers";
import { MonitorCheck, ShieldCheck, CheckCircle2, ChevronRight } from "lucide-react";
import { t } from "../../i18n";
import { searchSelectLabels } from "../../i18n/appkit-labels";
import { TitlePicker } from "./Search";
import { api } from "../../lib/api";
import { formatNumber } from "../../lib/format";
import { useAsync } from "../../lib/hooks";
import { MEDIA_MOVIE, MEDIA_TV, mediaLabel } from "../../lib/releases";
import type { QualityAnswer, QualityLayer } from "../../lib/types";

type Where = "home" | "away";

function resolutionName(h: number | undefined): string {
  if (!h) return t("Any resolution");
  return h === 2160 ? t("4K") : `${h}p`;
}

function rateName(kbps: number): string {
  return t("{rate} Mbps", { rate: formatNumber(kbps / 1000, 1) });
}

/** The big line: "1080p · HDR off", plus the bitrate cap when there is one. */
export function answerHeadline(a: QualityAnswer): string {
  if (a.blocked) return t("Can't play away from home");
  const hdr = a.dolbyVision ? t("HDR and Dolby Vision") : a.hdr ? t("HDR on") : t("HDR off");
  const parts = [resolutionName(a.maxHeight), hdr];
  if (a.bitrateKbps) parts.push(t("Up to {rate}", { rate: rateName(a.bitrateKbps) }));
  return parts.join(" · ");
}

export function layerText(l: QualityLayer, _viewer: string): string {
  switch (l.kind) {
    case "profile":
      return l.source === "rule"
        ? t("Rule {n} · {profile}", { n: l.rule ?? 0, profile: l.profile ?? "" })
        : l.source === "remote"
          ? t("Default away")
          : t("Default at home");
    case "device":
      if (!l.decisive) return t("No limit");
      return (
        [
          l.maxHeight ? t("Up to {height}", { height: resolutionName(l.maxHeight) }) : "",
          l.hdr === false ? t("No HDR") : l.dolbyVision === false ? t("No Dolby Vision") : "",
        ]
          .filter(Boolean)
          .join(" · ") || t("No limit")
      );
    case "remote-cap":
    case "viewer-cap":
      return l.bitrateKbps ? rateName(l.bitrateKbps) : t("No limit");
    case "remote-blocked":
      return t("Away playback disabled");
    case "rule-height":
      return l.ignored
        ? t("Device resolution is fixed")
        : t("Rule {n} · up to {height}", { n: l.rule ?? 0, height: resolutionName(l.maxHeight) });
    case "android-lift":
      return t("Rule {n} lifts the device limit", { n: l.rule ?? 0 });
  }
}

function Answer({ answer, viewer }: { answer: QualityAnswer; viewer: string }) {
  const labels: Record<string, string> = {
    profile: t("Profile"),
    device: t("Device"),
    "rule-height": t("Rule"),
    "android-lift": t("Rule"),
    "remote-cap": t("Away limit"),
    "viewer-cap": t("Viewer limit"),
    "remote-blocked": t("Access"),
  };
  return (
    <div className="space-y-5 border-t border-border p-5" aria-live="polite">
      <div className="flex items-center gap-4 rounded-xl border border-primary/20 bg-primary/5 p-5">
        <MonitorCheck size={36} className="shrink-0 text-primary" />
        <div className="space-y-2">
          <p className="text-xs font-medium text-fg-muted">{t("Available quality")}</p>
          <div className="flex flex-wrap items-center gap-2" data-testid="quality-media">
            {answer.mediaType ? (
              <Badge variant="secondary">{mediaLabel(answer.mediaType)}</Badge>
            ) : null}
          </div>
          <div data-testid="quality-headline" className="flex flex-wrap items-center gap-2">
            <span className="text-3xl font-semibold tracking-tight">
              {answer.blocked ? t("Can't play away from home") : resolutionName(answer.maxHeight)}
            </span>
            {!answer.blocked ? (
              <>
                <Badge variant="secondary">{answer.hdr ? t("HDR") : t("SDR")}</Badge>
                {answer.dolbyVision ? <Badge variant="secondary">{t("Dolby Vision")}</Badge> : null}
                {answer.bitrateKbps ? (
                  <Badge variant="outline">
                    {t("Up to {rate}", { rate: rateName(answer.bitrateKbps) })}
                  </Badge>
                ) : null}
              </>
            ) : null}
          </div>
        </div>
      </div>
      <ol className="flex flex-col gap-6 sm:flex-row" aria-label={t("What decided it")}>
        {answer.layers.map((original, i) => {
          const profile = answer.layers.find((step) => step.kind === "profile");
          const hdrCap =
            original.kind === "device" &&
            ((profile?.hdr === true && original.hdr === false) ||
              (profile?.dolbyVision === true && original.dolbyVision === false));
          const l = hdrCap
            ? {
                ...original,
                decisive: true,
                maxHeight: original.decisive ? original.maxHeight : undefined,
              }
            : original;
          return (
            <li
              key={`${l.kind}-${i}`}
              className={cn(
                "relative min-w-0 flex-1 space-y-3 rounded-xl border border-border bg-surface p-4",
                l.ignored && "opacity-60",
                l.decisive && "border-primary/40",
              )}
            >
              {i > 0 ? (
                <ChevronRight
                  aria-hidden
                  size={18}
                  className="absolute -left-6 top-1/2 hidden text-fg-muted sm:block"
                />
              ) : null}
              <div className="flex items-center gap-2 text-sm font-medium">
                <span className="text-fg-muted">{i + 1}</span>
                {labels[l.kind] ?? t("Limit")}
              </div>
              <p className={cn("text-sm text-fg-muted", l.ignored && "line-through")}>
                {layerText(l, viewer)}
              </p>
              {l.decisive ? (
                <Badge variant="info">
                  <ShieldCheck size={12} />
                  {t("Sets the limit")}
                </Badge>
              ) : (
                <CheckCircle2
                  aria-label={t("No additional limit")}
                  size={16}
                  className="text-success"
                />
              )}
              {l.edited ? <Badge variant="secondary">{t("Edited")}</Badge> : null}
              <div>
                <Link
                  className="text-sm text-primary underline"
                  to={
                    l.kind === "device"
                      ? "/playback/devices/apps"
                      : l.kind === "viewer-cap" || l.kind === "remote-blocked"
                        ? "/viewers"
                        : l.kind === "remote-cap"
                          ? "/quality/profiles"
                          : l.kind === "profile" && l.source !== "rule"
                            ? "/quality/profiles"
                            : "/quality/rules"
                  }
                >
                  {l.decisive ? t("Edit") : t("Set limit")}
                </Link>
              </div>
            </li>
          );
        })}
      </ol>
      <p className="text-xs text-fg-muted">
        {t("Apps can ask for less in their own quality settings.")}
      </p>
    </div>
  );
}

/**
 * "Who gets what": pick a viewer, an app and At home or Away, and see the
 * quality ceiling that combination gets and which setting decided it. The
 * answer comes from the server's playback code.
 */
export function WhoGetsWhat() {
  const { viewers, error: viewersError } = useViewers();
  const clients = useAsync(() => api.qualityClients(), []);
  const [pickedViewer, setViewer] = useState(0);
  const [pickedClient, setClient] = useState("");
  const [where, setWhere] = useState<Where>("home");
  const [title, setTitle] = useState<{ key: string; name: string } | null>(null);
  const [media, setMedia] = useState<"" | "movie" | "tv">("");

  const viewerId = pickedViewer || viewers?.find((v) => v.isOwner)?.id || viewers?.[0]?.id || 0;
  const clientId = pickedClient || clients.data?.[0]?.id || "";
  const remote = where === "away";
  const answer = useAsync<QualityAnswer | null>(
    (signal) =>
      viewerId
        ? api.explainQuality(
            {
              userId: viewerId,
              ...(clientId ? { clientId } : {}),
              ...(title ? { titleKey: title.key } : {}),
              ...(!title && media ? { mediaType: media } : {}),
              remote,
            },
            signal,
          )
        : Promise.resolve(null),
    [viewerId, clientId, remote, title?.key, media],
  );
  const viewer = viewers?.find((v) => v.id === viewerId);

  const failed = viewersError ?? clients.error;
  return (
    <SettingsSection
      title={t("Check quality")}
      description={t("See the quality a viewer gets on an app, and why.")}
    >
      {failed ? (
        <ErrorPanel error={failed} onRetry={() => void clients.reload()} />
      ) : !viewers || !clients.data ? (
        <LoadingRows rows={3} />
      ) : (
        <>
          <div data-setting="check-quality" className="grid gap-4 px-5 py-4 sm:grid-cols-3">
            <div className="min-w-0 space-y-2">
              <p className="text-xs font-medium text-fg-muted">{t("Viewer")}</p>{" "}
              <div className="w-full">
                <SearchSelect
                  {...searchSelectLabels()}
                  value={String(viewerId)}
                  onChange={(v) => setViewer(Number(v))}
                  options={viewers.map((v) => ({ value: String(v.id), label: viewerName(v) }))}
                  ariaLabel={t("Viewer")}
                />
              </div>
            </div>
            <div className="min-w-0 space-y-2">
              <p className="text-xs font-medium text-fg-muted">{t("App")}</p>{" "}
              <div className="w-full">
                <SearchSelect
                  {...searchSelectLabels()}
                  value={clientId}
                  onChange={setClient}
                  options={[
                    { value: "any", label: t("Any app") },
                    ...clients.data.map((c) => ({ value: c.id, label: c.name })),
                  ]}
                  ariaLabel={t("App")}
                />
              </div>
            </div>
            <div className="min-w-0 space-y-2">
              <p className="text-xs font-medium text-fg-muted">{t("Where")}</p>{" "}
              <Segmented
                ariaLabel={t("Where")}
                size="sm"
                value={where}
                onChange={setWhere}
                options={[
                  { value: "home", label: t("At home") },
                  { value: "away", label: t("Away") },
                ]}
              />
            </div>
          </div>
          <div className="px-5 pb-4 space-y-2">
            <p className="text-xs font-medium text-fg-muted">{t("Movie or episode (optional)")}</p>
            {title ? (
              <div className="flex items-center gap-2 text-sm">
                <span>{title.name}</span>
                <button
                  type="button"
                  className="text-primary underline"
                  onClick={() => setTitle(null)}
                >
                  {t("Clear")}
                </button>
              </div>
            ) : (
              <TitlePicker onPick={(key, name) => setTitle({ key, name })} />
            )}
            {!title ? (
              <div className="flex flex-wrap items-center gap-2 pt-1">
                <span className="text-xs text-fg-muted">{t("Media")}</span>
                <Segmented
                  ariaLabel={t("Media")}
                  size="sm"
                  value={media}
                  onChange={setMedia}
                  options={[
                    { value: "", label: t("Both") },
                    { value: MEDIA_MOVIE, label: t("Movies") },
                    { value: MEDIA_TV, label: t("Shows") },
                  ]}
                />
              </div>
            ) : null}
            {title && (
              <p className="text-xs text-fg-muted">
                {t("Quality limits apply to this title; available releases may differ.")}
              </p>
            )}
          </div>
          {answer.error ? (
            <ErrorPanel error={answer.error} onRetry={() => void answer.reload()} />
          ) : answer.data ? (
            <Answer answer={answer.data} viewer={viewer ? viewerName(viewer) : t("This viewer")} />
          ) : (
            <LoadingRows rows={2} />
          )}
        </>
      )}
    </SettingsSection>
  );
}
