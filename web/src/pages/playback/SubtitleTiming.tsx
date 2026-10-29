import { Button, EmptyState, PagedTable, Switch, Textarea } from "@braedonsaunders/appkit-ui";
import { Captions } from "lucide-react";
import { useState } from "react";
import { Drawer } from "../../components/Drawer";
import { Field, NumberInput } from "../../components/fields";
import { ErrorPanel, LoadingRows, MutationError } from "../../components/feedback";
import { Section } from "../../components/layout";
import { t, tx } from "../../i18n";
import { pagedTableLabels } from "../../i18n/appkit-labels";
import { apiFetch } from "../../lib/api";
import { formatDateTime, relativeTime } from "../../lib/format";
import { useAsync } from "../../lib/hooks";

// `title` is the real title resolved by the server from the catalog; it is
// always present and empty when the server could not identify the title,
// in which case the UI says "Unknown title" — never a rating key or the
// cache id (a hash), which are not identification a person can read.
// `fetchedAt` is when the sidecar was downloaded.
type CachedSubtitle = {
  id: string;
  language: string;
  release: string;
  fetchedAt?: string;
  title: string;
  showTitle?: string;
  season?: number;
  episode?: number;
};

const episodeCode = (s: CachedSubtitle) =>
  s.season && s.episode
    ? `S${String(s.season).padStart(2, "0")}E${String(s.episode).padStart(2, "0")}`
    : "";

const displayTitle = (s: CachedSubtitle) => {
  if (!s.title && !s.showTitle) return t("Unknown title");
  if (s.showTitle) return [s.showTitle, episodeCode(s), s.title].filter(Boolean).join(" · ");
  return s.title;
};

const subtitleName = (s: CachedSubtitle) =>
  `${displayTitle(s)} · ${s.language || t("Unknown language")}`;

/**
 * Fix subtitles that drift: pick a downloaded sidecar and create a corrected
 * copy with an offset, a timing multiplier, or an offset estimated from
 * reference text. The original file is kept.
 */
export function SubtitleTiming() {
  const cached = useAsync(
    async (signal) =>
      (await apiFetch<CachedSubtitle[] | null>("/subtitle-sync/", { signal })) ?? [],
    [],
  );
  const [open, setOpen] = useState<CachedSubtitle | null>(null);

  return (
    <Section
      title={t("Subtitle timing")}
      description={t("Create a corrected copy of a downloaded subtitle. Original files are kept.")}
    >
      {cached.error ? (
        <ErrorPanel error={cached.error} onRetry={() => void cached.reload()} />
      ) : !cached.data ? (
        <LoadingRows rows={2} />
      ) : (
        <PagedTable<CachedSubtitle>
          rows={cached.data}
          rowKey={(s) => s.id}
          searchable={cached.data.length > 8}
          pageSize={10}
          onRowClick={setOpen}
          labels={pagedTableLabels({ searchPlaceholder: t("Search subtitles…") })}
          empty={
            <EmptyState
              icon={<Captions />}
              title={t("No downloaded subtitles")}
              description={t("Subtitles appear here after playback finds a matching sidecar.")}
              className="py-6"
            />
          }
          columns={[
            {
              key: "title",
              header: t("Title"),
              search: (s) => `${displayTitle(s)} ${s.showTitle ?? ""} ${s.release} ${s.language}`,
              sortValue: (s) => displayTitle(s).toLowerCase(),
              cell: (s) => (
                <span className="block">
                  <span className="font-medium text-fg">{displayTitle(s)}</span>
                  {s.release ? (
                    <span className="mt-0.5 block break-all text-xs text-fg-subtle">
                      {s.release}
                    </span>
                  ) : null}
                </span>
              ),
            },
            {
              key: "language",
              header: t("Language"),
              sortValue: (s) => s.language,
              cell: (s) => <span className="text-fg-muted">{s.language || t("Unknown")}</span>,
            },
            {
              key: "fetchedAt",
              header: t("Downloaded"),
              sortValue: (s) => s.fetchedAt ?? "",
              cell: (s) => (
                <span className="text-fg-muted" title={formatDateTime(s.fetchedAt)}>
                  {relativeTime(s.fetchedAt)}
                </span>
              ),
            },
            {
              key: "action",
              header: "",
              align: "right",
              cell: (s) => (
                <Button
                  variant="outline"
                  size="sm"
                  aria-label={t("Adjust timing for {name}", { name: subtitleName(s) })}
                  onClick={(e) => {
                    e.stopPropagation();
                    setOpen(s);
                  }}
                >
                  {t("Adjust timing")}
                </Button>
              ),
            },
          ]}
        />
      )}
      {open ? <TimingDrawer subtitle={open} onClose={() => setOpen(null)} /> : null}
    </Section>
  );
}

function TimingDrawer({ subtitle, onClose }: { subtitle: CachedSubtitle; onClose: () => void }) {
  const [offset, setOffset] = useState(0);
  const [rate, setRate] = useState(1);
  const [reference, setReference] = useState("");
  const [apply, setApply] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [synced, setSynced] = useState("");

  const create = async () => {
    setBusy(true);
    setError(null);
    setSynced("");
    try {
      const result = await apiFetch<{ externalUrl: string }>(
        `/subtitle-sync/${encodeURIComponent(subtitle.id.replace(/^ether-subtitle:/, ""))}`,
        {
          method: "POST",
          body: JSON.stringify({ offsetMs: offset, rate, reference, apply }),
        },
      );
      setSynced(result.externalUrl);
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Drawer
      open
      onClose={onClose}
      title={t("Adjust subtitle timing")}
      description={subtitleName(subtitle)}
      size="lg"
      footer={
        <Button
          disabled={busy || !Number.isFinite(offset) || !Number.isFinite(rate) || rate <= 0}
          onClick={() => void create()}
        >
          {busy ? t("Creating…") : t("Create corrected copy")}
        </Button>
      }
    >
      <div className="space-y-5">
        <MutationError error={error} />
        <div className="grid gap-4 sm:grid-cols-2">
          <Field
            label={t("Offset (milliseconds)")}
            hint={t("Positive values show subtitles later.")}
          >
            <NumberInput
              aria-label={t("Offset (milliseconds)")}
              min={-3600000}
              max={3600000}
              value={offset}
              onChange={setOffset}
            />
          </Field>
          <Field label={t("Timing multiplier")} hint={t("1 keeps the original speed.")}>
            <NumberInput
              aria-label={t("Timing multiplier")}
              min={0.5}
              max={2}
              step={0.001}
              value={rate}
              onChange={setRate}
            />
          </Field>
        </div>
        <Field
          label={t("Reference subtitles (optional)")}
          hint={t(
            "Paste embedded subtitle text to estimate a constant offset from matching dialogue.",
          )}
        >
          <Textarea
            aria-label={t("Reference subtitles")}
            rows={5}
            value={reference}
            onChange={(e) => setReference(e.target.value)}
          />
        </Field>
        <label className="flex items-center justify-between gap-3 text-sm text-fg">
          {t("Replace this subtitle for all viewers")}
          <Switch
            aria-label={t("Replace this subtitle for all viewers")}
            checked={apply}
            onChange={(e) => setApply(e.target.checked)}
          />
        </label>
        {synced ? (
          <p
            role="status"
            className="break-all rounded-lg border border-border bg-bg-subtle p-3 text-sm"
          >
            {tx("Corrected copy: {url}", { url: <code>{synced}</code> })}
          </p>
        ) : null}
      </div>
    </Drawer>
  );
}
