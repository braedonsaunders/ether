import { Button, EmptyState, PagedTable, Switch, Textarea } from "@braedonsaunders/appkit-ui";
import { Captions } from "lucide-react";
import { useState } from "react";
import { Drawer } from "../../components/Drawer";
import { Field, NumberInput } from "../../components/fields";
import { ErrorPanel, LoadingRows, MutationError } from "../../components/feedback";
import { Section } from "../../components/layout";
import { apiFetch } from "../../lib/api";
import { formatDateTime, relativeTime } from "../../lib/format";
import { useAsync } from "../../lib/hooks";

// `key` is the title's Plex ratingKey when the backend could recover it
// from its selection index; the cache itself stores no title name, so the
// release name below is the human-readable identifier. `fileName` is the
// sidecar file on disk and `fetchedAt` when it was downloaded.
type CachedSubtitle = {
  id: string;
  language: string;
  release: string;
  fetchedAt?: string;
  key?: string;
  fileName?: string;
};

const subtitleName = (s: CachedSubtitle) =>
  `${s.release || "Downloaded subtitle"} · ${s.language || "Unknown language"}`;

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
      title="Subtitle timing"
      description="Create a corrected copy of a downloaded subtitle. Original files are kept."
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
          labels={{ searchPlaceholder: "Search subtitles…" }}
          empty={
            <EmptyState
              icon={<Captions />}
              title="No downloaded subtitles"
              description="Subtitles appear here after playback finds a matching sidecar."
              className="py-6"
            />
          }
          columns={[
            {
              key: "release",
              header: "Release",
              search: (s) => `${s.release} ${s.language} ${s.key ?? ""} ${s.fileName ?? ""}`,
              sortValue: (s) => (s.release || "").toLowerCase(),
              cell: (s) => (
                <span className="block">
                  <span className="break-all font-medium text-fg">
                    {s.release || "Downloaded subtitle"}
                  </span>
                  {s.key || s.fileName ? (
                    <span className="mt-0.5 block break-all text-xs text-fg-subtle">
                      {[s.key ? `Title key ${s.key}` : null, s.fileName]
                        .filter(Boolean)
                        .join(" · ")}
                    </span>
                  ) : null}
                </span>
              ),
            },
            {
              key: "language",
              header: "Language",
              sortValue: (s) => s.language,
              cell: (s) => <span className="text-fg-muted">{s.language || "Unknown"}</span>,
            },
            {
              key: "fetchedAt",
              header: "Downloaded",
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
                  aria-label={`Adjust timing for ${subtitleName(s)}`}
                  onClick={(e) => {
                    e.stopPropagation();
                    setOpen(s);
                  }}
                >
                  Adjust timing
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
      title="Adjust subtitle timing"
      description={subtitleName(subtitle)}
      size="lg"
      footer={
        <Button
          disabled={busy || !Number.isFinite(offset) || !Number.isFinite(rate) || rate <= 0}
          onClick={() => void create()}
        >
          {busy ? "Creating…" : "Create corrected copy"}
        </Button>
      }
    >
      <div className="space-y-5">
        <MutationError error={error} />
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Offset (milliseconds)" hint="Positive values show subtitles later.">
            <NumberInput
              aria-label="Offset (milliseconds)"
              min={-3600000}
              max={3600000}
              value={offset}
              onChange={setOffset}
            />
          </Field>
          <Field label="Timing multiplier" hint="1 keeps the original speed.">
            <NumberInput
              aria-label="Timing multiplier"
              min={0.5}
              max={2}
              step={0.001}
              value={rate}
              onChange={setRate}
            />
          </Field>
        </div>
        <Field
          label="Reference subtitles (optional)"
          hint="Paste embedded subtitle text to estimate a constant offset from matching dialogue."
        >
          <Textarea
            aria-label="Reference subtitles"
            rows={5}
            value={reference}
            onChange={(e) => setReference(e.target.value)}
          />
        </Field>
        <label className="flex items-center justify-between gap-3 text-sm text-fg">
          Replace this subtitle for all viewers
          <Switch
            aria-label="Replace this subtitle for all viewers"
            checked={apply}
            onChange={(e) => setApply(e.target.checked)}
          />
        </label>
        {synced ? (
          <p
            role="status"
            className="break-all rounded-lg border border-border bg-bg-subtle p-3 text-sm"
          >
            Corrected copy: <code>{synced}</code>
          </p>
        ) : null}
      </div>
    </Drawer>
  );
}
