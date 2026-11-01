import { describe, expect, it } from "vitest";
import { codecLabel, encodeCodecs } from "./hardwareCodecs";

describe("encode codecs", () => {
  it("maps ffmpeg encoder names to badge codecs", () => {
    expect(
      encodeCodecs([
        "h264_videotoolbox",
        "hevc_videotoolbox",
        "libx264",
        "av1_nvenc",
        "libvpx-vp9",
      ]),
    ).toEqual(["h264", "hevc", "av1", "vp9"]);
    expect(encodeCodecs([])).toEqual([]);
    expect(codecLabel("hevc")).toBe("H.265/HEVC");
  });
});
