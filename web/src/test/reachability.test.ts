import { describe, expect, it } from "vitest";
import {
  awaySummary,
  claimSummary,
  homeSummary,
  isLoopbackAddress,
  usableAddresses,
} from "../pages/apps/reachability";

describe("app reachability copy", () => {
  it("ignores loopback and unspecified binds", () => {
    expect(isLoopbackAddress("127.0.0.1:32400")).toBe(true);
    expect(isLoopbackAddress("http://[::1]:32400")).toBe(true);
    expect(isLoopbackAddress("0.0.0.0:32400")).toBe(true);
    expect(
      usableAddresses({
        addresses: ["127.0.0.1:32400"],
        connectUrls: ["http://192.168.1.20:32400"],
      }),
    ).toEqual(["http://192.168.1.20:32400"]);
  });

  it("says home reachability is unchecked when only this computer is listening", () => {
    expect(homeSummary({ addresses: ["127.0.0.1:32400"], lan: "unknown" }).detail).toMatch(
      /only listening on this computer/,
    );
  });

  it("names the address people use when home reachability was not checked", () => {
    expect(
      homeSummary({
        addresses: ["127.0.0.1:32400"],
        connectUrls: ["http://192.168.1.20:32400"],
        lan: "unknown",
      }).detail,
    ).toMatch(/Not checked yet.*192\.168\.1\.20:32400/);
  });

  it("says remote access is off instead of unknown", () => {
    expect(awaySummary({ wan: "unknown" }, { remoteAccess: false }).detail).toBe(
      "Not checked yet: remote access is off.",
    );
  });

  it("keeps home-network apps from sounding like a failed remote check", () => {
    expect(awaySummary({ wan: "unknown" }, { homeNetwork: true }).detail).toMatch(/home network/);
  });

  it("names the Plex account that claimed the server", () => {
    expect(claimSummary(true, "alex")).toBe("Claimed by the Plex account alex.");
    expect(claimSummary(false)).toMatch(/Not claimed/);
  });
});
