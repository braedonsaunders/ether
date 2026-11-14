import { useEffect, useState } from "react";
import { Button } from "@braedonsaunders/appkit-ui";
import { apiFetch } from "../../../lib/api";
import { t } from "../../../i18n";

type Job = { path: string; state: string; progress?: string; error?: string };
export function ActionJobs({ id }: { id: string }) {
  const [jobs, setJobs] = useState<Job[]>([]);
  const [error, setError] = useState("");
  useEffect(() => {
    let live = true;
    let timer: ReturnType<typeof setTimeout>;
    setJobs([]);
    async function load() {
      try {
        const next = await apiFetch<Job[]>(`/plugins/${encodeURIComponent(id)}/jobs`);
        if (live) {
          setJobs(next);
          setError("");
        }
      } catch (e) {
        if (live) setError(String(e));
      }
      if (live) timer = setTimeout(load, 2000);
    }
    void load();
    return () => {
      live = false;
      clearTimeout(timer);
    };
  }, [id]);
  if (!jobs.length && !error) return null;
  return (
    <div className="space-y-3 rounded-xl border border-border bg-surface p-4">
      <h3 className="text-sm font-medium">{t("Background jobs")}</h3>
      {error ? (
        <p role="alert" className="text-xs text-danger">
          {error}
        </p>
      ) : null}
      {jobs.map((job) => (
        <div key={job.path} className="flex items-center justify-between gap-3 text-sm">
          <div>
            <p className="break-all font-mono text-xs">{job.path}</p>
            <p>
              {job.state === "running"
                ? t("Running…")
                : job.state === "done"
                  ? t("Completed")
                  : job.state === "canceled"
                    ? t("Cancelled")
                    : t("Failed")}
            </p>
            {job.progress ? (
              <p className="text-xs text-fg-muted" role="status">
                {job.progress}
              </p>
            ) : null}
            {job.error ? <p className="text-xs text-danger">{job.error}</p> : null}
          </div>
          {job.state === "running" ? (
            <Button
              size="sm"
              variant="outline"
              onClick={() => {
                void apiFetch(`/plugins/${encodeURIComponent(id)}/jobs/cancel`, {
                  method: "POST",
                  body: JSON.stringify({ path: job.path }),
                }).catch((e) => setError(String(e)));
              }}
            >
              {t("Cancel")}
            </Button>
          ) : null}
        </div>
      ))}
    </div>
  );
}
