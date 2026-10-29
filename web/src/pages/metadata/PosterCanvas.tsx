import { Artwork } from "../../components/Artwork";
import { t } from "../../i18n";
import type { TitleCard } from "../../lib/contract";
import { previewURL, type Overlays } from "./model";

export function PosterCanvas({
  title,
  neighbours,
  mode,
  settings,
  user,
  lang,
  nonce,
}: {
  title: TitleCard;
  neighbours: TitleCard[];
  mode: "poster" | "library" | "compare";
  settings: Overlays;
  user: number | null;
  lang: string;
  nonce: number;
}) {
  const poster = (item: TitleCard, label: string, original = false) => (
    <figure className="metadata-poster" key={`${item.key}-${label}`}>
      <Artwork
        src={previewURL(
          original ? { ...settings, enabled: false } : settings,
          item.key,
          user,
          lang,
          nonce,
        )}
        alt={t("{title} {label}", { title: item.title, label })}
        title={t("Artwork unavailable. Try another title.")}
        eager
        className="metadata-poster-image rounded-md"
        imgClassName="object-contain"
      />
      <figcaption title={label}>{label}</figcaption>
    </figure>
  );
  const library = neighbours.length > 0 ? [neighbours[0]!, title, ...neighbours.slice(1)] : [title];
  const count = mode === "compare" ? 2 : mode === "library" ? library.length : 1;
  return (
    <div className={`metadata-stage metadata-stage-${mode}`}>
      <div
        className="metadata-poster-grid"
        style={{ gridTemplateColumns: `repeat(${count}, minmax(0, 1fr))` }}
      >
        {mode === "compare" ? (
          <>
            {poster(title, t("Original"), true)}
            {poster(title, settings.enabled ? t("Your design") : t("Overlays off"))}
          </>
        ) : mode === "library" ? (
          library.map((item) => poster(item, item.title))
        ) : (
          poster(title, settings.enabled ? t("Your design") : t("Original artwork"))
        )}
      </div>
    </div>
  );
}
