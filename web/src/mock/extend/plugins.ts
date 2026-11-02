/** Mock plugin host: installed plugins, catalogue, studio (WS7, WS10, WS11). */
import type { Descriptor } from "../../lib/extend";
import type { ExtendHandler } from "./index";

export function createPluginsMock(): {
  handle: ExtendHandler;
  descriptors: () => Descriptor[];
  version: () => number;
} {
  return {
    handle: () => null,
    descriptors: () => [],
    version: () => 1,
  };
}
