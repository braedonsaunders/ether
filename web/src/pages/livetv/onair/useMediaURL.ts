import { useEffect, useState } from "react";
import { apiBlob } from "../../../lib/api";
/** Resolves an authenticated /ether/api media path to an object URL for <video>/<audio>/<img>. */
export function useMediaURL(url: string, enabled = true) {
  const [resolved, setResolved] = useState("");
  useEffect(() => {
    setResolved("");
    if (!enabled || !url) return;
    if (!url.startsWith("/ether/api/")) {
      setResolved(url);
      return;
    }
    const abort = new AbortController();
    let objectURL = "";
    void apiBlob(url, abort.signal)
      .then((blob) => {
        if (abort.signal.aborted) return;
        objectURL = URL.createObjectURL(blob);
        setResolved(objectURL);
      })
      .catch(() => {});
    return () => {
      abort.abort();
      if (objectURL) URL.revokeObjectURL(objectURL);
    };
  }, [url, enabled]);
  return resolved;
}
