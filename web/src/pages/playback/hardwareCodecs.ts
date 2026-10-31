const ORDER = ["h264", "hevc", "av1", "vp9"] as const;

export type VideoCodec = (typeof ORDER)[number];

/** Codec badges implied by ffmpeg encoder names. Unknown names are ignored. */
export function encodeCodecs(encoders: string[] | null | undefined): VideoCodec[] {
  const found = new Set<VideoCodec>();
  for (const raw of encoders ?? []) {
    const name = raw.toLowerCase();
    if (name.includes("av1")) found.add("av1");
    else if (name.includes("vp9")) found.add("vp9");
    else if (name.includes("265") || name.includes("hevc")) found.add("hevc");
    else if (name.includes("264") || name.includes("avc")) found.add("h264");
  }
  return ORDER.filter((codec) => found.has(codec));
}

/** Short badge text for a registry or ffmpeg codec id. */
export function codecLabel(code: string): string {
  switch (code.toLowerCase()) {
    case "h264":
    case "avc":
      return "H.264";
    case "hevc":
    case "h265":
      return "H.265/HEVC";
    case "av1":
      return "AV1";
    case "vp9":
      return "VP9";
    default:
      return code;
  }
}
