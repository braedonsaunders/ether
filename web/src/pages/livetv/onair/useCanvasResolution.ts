import { useLayoutEffect, useState, type RefObject } from "react";
/** Size the backing surface from the actual CSS box, including drawer expansion. */
export function useCanvasResolution(ref: RefObject<HTMLCanvasElement | null>) {
  const [revision, setRevision] = useState(0);
  useLayoutEffect(() => {
    const canvas = ref.current;
    if (!canvas) return;
    const measure = () => {
      const box = canvas.getBoundingClientRect();
      const ratio = Math.min(2, window.devicePixelRatio || 1);
      const width = Math.max(1, Math.round(box.width * ratio)),
        height = Math.max(1, Math.round(box.height * ratio));
      if (canvas.width !== width || canvas.height !== height) {
        canvas.width = width;
        canvas.height = height;
        setRevision((n) => n + 1);
      }
    };
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(canvas);
    return () => observer.disconnect();
  }, [ref]);
  return revision;
}
