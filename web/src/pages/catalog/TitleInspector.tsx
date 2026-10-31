import { Badge, Spinner } from "@braedonsaunders/appkit-ui";
import { Link } from "react-router-dom";
import { ArrowUpRight, Film, Star } from "lucide-react";
import { Artwork } from "../../components/Artwork";
import { Drawer } from "../../components/Drawer";
import { ErrorPanel } from "../../components/feedback";
import { t } from "../../i18n";
import { api } from "../../lib/api";
import { formatRating, ratingSource } from "../../lib/contentRatings";
import { fixed } from "../../lib/format";
import { useAsync } from "../../lib/hooks";
import "./catalog.css";

export function TitleInspector({ titleKey, onClose }: { titleKey: string; onClose: () => void }) {
  const state = useAsync(() => api.title(titleKey), [titleKey]);
  const title = state.data;
  return (
    <Drawer
      open
      onClose={onClose}
      title={title?.title ?? t("Title details")}
      size="lg"
      bodyClassName="overflow-y-auto p-0"
    >
      {state.error ? (
        <ErrorPanel className="m-5" error={state.error} onRetry={() => void state.reload()} />
      ) : !title ? (
        <div className="grid min-h-64 place-items-center">
          <Spinner label={t("Loading title details")} />
        </div>
      ) : (
        <>
          <div className="catalog-detail-hero">
            <Artwork
              src={title.backdrop}
              alt=""
              icon={<span />}
              className="absolute inset-0"
              eager
            />
            <div className="catalog-detail-scrim" />
            <div className="catalog-detail-heading">
              <Artwork
                src={title.poster}
                alt={t("{title} poster", { title: title.title })}
                className="aspect-[2/3] w-28 shrink-0 rounded-lg shadow-xl"
                eager
              />
              <div>
                <p className="catalog-eyebrow">
                  {title.type === "show" ? t("TELEVISION") : t("MOVIE")}
                </p>
                <h2>{title.title}</h2>
                <p>
                  {[
                    title.year,
                    title.contentRating ? formatRating(title.contentRating) : null,
                    title.runtimeMinutes
                      ? t("{minutes} min", { minutes: title.runtimeMinutes })
                      : null,
                  ]
                    .filter(Boolean)
                    .join(" · ")}
                </p>
                {title.contentRating && title.contentRatingSource ? (
                  <p className="mt-1 text-xs text-fg-muted">
                    {t("Content rating from {source}", {
                      source: ratingSource(title.contentRatingSource),
                    })}
                  </p>
                ) : null}
                {title.rating ? (
                  <span className="mt-3 inline-flex items-center gap-1.5 text-sm">
                    <Star size={14} />
                    {t("{rating} / 10 · TMDB", { rating: fixed(title.rating, 1) })}
                  </span>
                ) : null}
              </div>
            </div>
          </div>
          <div className="catalog-detail-body">
            <div className="flex flex-wrap gap-2">
              {title.genres?.map((g) => (
                <Badge key={g} variant="secondary">
                  {g}
                </Badge>
              ))}
            </div>
            <section>
              <h3>{t("Overview")}</h3>
              <p>{title.summary || t("No overview is available for this title yet.")}</p>
            </section>
            <dl className="catalog-detail-facts">
              <div>
                <dt>{t("Catalog ID")}</dt>
                <dd className="font-mono">{title.key}</dd>
              </div>
              <div>
                <dt>{t("Media type")}</dt>
                <dd>{title.type === "show" ? t("TV show") : t("Movie")}</dd>
              </div>
              <div>
                <dt>{t("Directed by")}</dt>
                <dd>{title.directors?.map((p) => p.name).join(", ") || t("Not provided")}</dd>
              </div>
              {title.collection ? (
                <div>
                  <dt>{t("Collection")}</dt>
                  <dd>{title.collection.name}</dd>
                </div>
              ) : null}
            </dl>
            {title.cast?.length ? (
              <section>
                <h3>{t("Cast")}</h3>
                <div className="catalog-cast">
                  {title.cast.map((person) => (
                    <div key={`${person.id}-${person.role}`}>
                      <Artwork
                        src={person.image}
                        alt=""
                        title={person.name}
                        className="aspect-[2/3] rounded-md"
                      />
                      <h4>{person.name}</h4>
                      <p>{person.role}</p>
                    </div>
                  ))}
                </div>
              </section>
            ) : null}
            {title.keywords?.length ? (
              <section>
                <h3>{t("Keywords")}</h3>
                <div className="flex flex-wrap gap-1.5">
                  {title.keywords.map((k) => (
                    <Badge key={typeof k === "string" ? k : k.id} variant="outline">
                      {typeof k === "string" ? k : k.name}
                    </Badge>
                  ))}
                </div>
              </section>
            ) : null}
            <div className="catalog-detail-footer">
              <Film size={18} />
              <p>
                {t(
                  "Artwork and ratings shown here come from TMDB. Configure how they appear in Plex under Metadata.",
                )}
              </p>
              <Link to="/metadata" onClick={onClose} aria-label={t("Open Metadata")}>
                {t("Metadata")}
                <ArrowUpRight size={14} />
              </Link>
            </div>
          </div>
        </>
      )}
    </Drawer>
  );
}
