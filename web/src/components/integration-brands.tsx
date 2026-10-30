import emby from "../assets/brands/emby.svg?no-inline";
import jellyfin from "../assets/brands/jellyfin.svg?no-inline";
import plex from "../assets/brands/plex.svg?no-inline";
import simkl from "../assets/brands/simkl.svg?no-inline";
import tmdb from "../assets/brands/themoviedatabase.svg?no-inline";
import trakt from "../assets/brands/trakt.svg?no-inline";

/**
 * Real vendored marks for IntegrationAppCard's `logo` slot.
 * The card does not choose one from `identity.id`; the caller passes it.
 * Color follows the surrounding text (`currentColor` via a mask).
 */
function BrandMark({ src, color }: { src: string; color: string }) {
  return (
    <span
      aria-hidden
      className="block size-7 bg-current"
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

export function PlexMark() {
  return <BrandMark src={plex} color="#e5a00d" />;
}
export function JellyfinMark() {
  return <BrandMark src={jellyfin} color="#aa5cc3" />;
}
export function EmbyMark() {
  return <BrandMark src={emby} color="#52b54b" />;
}
export function SimklMark() {
  return <BrandMark src={simkl} color="currentColor" />;
}
export function TmdbMark() {
  return <BrandMark src={tmdb} color="#01b4e4" />;
}
export function TraktMark() {
  return <BrandMark src={trakt} color="#ed1c24" />;
}
