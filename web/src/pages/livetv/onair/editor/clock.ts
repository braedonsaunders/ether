import { useEffect, useRef, useState } from "react";
export function useStageClock(
  duration: number,
  opts: { loop?: boolean; rate?: number; playing?: boolean } = {},
) {
  const [time, setTime] = useState(0),
    [playing, setPlaying] = useState(opts.playing ?? false),
    [loop, setLoop] = useState(opts.loop ?? true),
    [rate, setRate] = useState(opts.rate ?? 1);
  const current = useRef(time);
  current.current = time;
  useEffect(() => {
    if (!playing) return;
    let frame = 0,
      last = performance.now();
    const tick = (now: number) => {
      let next = current.current + ((now - last) / 1000) * rate;
      last = now;
      if (next >= duration) {
        if (loop) next %= duration;
        else {
          next = duration;
          setPlaying(false);
        }
      }
      current.current = next;
      setTime(next);
      if (next < duration || loop) frame = requestAnimationFrame(tick);
    };
    frame = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(frame);
  }, [duration, playing, loop, rate]);
  return {
    time,
    playing,
    setPlaying,
    seek: (t: number) => setTime(Math.max(0, Math.min(duration, t))),
    loop,
    setLoop,
    rate,
    setRate,
  };
}
