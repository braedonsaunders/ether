import { describe, expect, it } from "vitest";
import { remoteHealth } from "../lib/health";

const claimed = {
  claimed: true,
  ownerUsername: "owner",
  presence: { connected: true },
  remoteAccess: true,
};

describe("remoteHealth", () => {
  it("is an error when plex.tv rejects the server token", () => {
    const h = remoteHealth({
      ...claimed,
      tokenRejected: true,
      lastPublishError: "401 Invalid authentication token",
    });
    expect(h.level).toBe("error");
    expect(h.summary).toMatch(/rejects/);
    expect(h.details.join(" ")).toMatch(/Authorized Devices/);
  });

  it("warns while publishing or friend sync keeps failing", () => {
    expect(remoteHealth({ ...claimed, lastPublishError: "timeout" }).level).toBe("warn");
    expect(remoteHealth({ ...claimed, friendSyncError: "500" }).summary).toMatch(/Shared users/);
  });

  it("points an unclaimed server at the in-page claim action", () => {
    const h = remoteHealth({ claimed: false });
    expect(h.level).toBe("error");
    expect(h.href).toBe("/apps/plex/account");
    expect(h.details.join(" ")).toMatch(/Sign in and claim this server on the Plex page/);
    expect(h.details.join(" ")).not.toMatch(/setup wizard/);
  });

  it("treats enabled remote access as configuration when plex.tv has no verdict", () => {
    const h = remoteHealth(claimed);
    expect(h.level).toBe("ok");
    expect(h.summary).toBe("Direct remote access enabled");
    expect(h.statusLabel).toBe("Configured");
    expect(h.details.join(" ")).not.toMatch(/connectivity check/);
    expect(h.summary).not.toMatch(/Reachable/);
  });

  it("does not call remote access reachable when plex.tv last reported it unreachable", () => {
    const h = remoteHealth({ ...claimed, plexReachable: false });
    expect(h.level).toBe("warn");
    expect(h.summary).toBe("plex.tv's last connectivity check reported the server unreachable.");
    expect(h.summary).not.toMatch(/Reachable/);
    expect(h.details.join(" ")).toContain("Direct remote access enabled");
    expect(h.details.join(" ")).not.toMatch(/Reachable outside/);
    expect(remoteHealth({ ...claimed, plexReachable: false, tokenRejected: true }).level).toBe(
      "error",
    );
    const uri = "https://plex.example.com:443";
    const both = remoteHealth({
      ...claimed,
      plexReachable: false,
      publishedUris: [uri],
      connections: [{ uri, custom: true, protocol: "https" }],
    });
    expect(both.level).toBe("unknown");
    expect(both.summary).toBe("Custom connection not confirmed by plex.tv");
    expect(both.details.join(" ")).toContain(uri);
    expect(both.summary).not.toBe("Custom connection published");
  });

  it("keeps intentional LAN-only off the incomplete rank", () => {
    const h = remoteHealth({ ...claimed, remoteAccess: false });
    expect([h.level, h.summary]).toEqual(["off", "Local network only"]);
    expect(h.countsOverall).toBeUndefined();
  });

  it("labels a published custom route without calling it reachable", () => {
    const uri = "https://plex.example.com:443";
    const h = remoteHealth({
      ...claimed,
      remoteAccess: false,
      publishedUris: [uri, "https://192-168-1-5.abc.plex.direct:32400"],
      resourceConnections: [{ uri, local: false, relay: false }],
      connections: [
        { uri, custom: true, protocol: "https" },
        { uri: "https://192-168-1-5.abc.plex.direct:32400", custom: false, protocol: "https" },
      ],
    });
    expect(h.level).toBe("ok");
    expect(h.summary).toBe("Custom connection published");
    expect(h.statusLabel).toBe("Published");
    expect(h.note).toMatch(/Test remote playback from another network/);
    expect(h.details.join(" ")).toContain(uri);
    expect(h.details.join(" ")).not.toContain("192-168-1-5");
    expect(h.summary).not.toBe("Reachable outside your network");
  });

  it("does not infer a custom route from an https name alone", () => {
    const uri = "https://plex.example.com";
    expect(remoteHealth({ ...claimed, remoteAccess: false, publishedUris: [uri] }).summary).toBe(
      "Local network only",
    );
    expect(
      remoteHealth({
        ...claimed,
        remoteAccess: false,
        publishedUris: [uri],
        connections: [{ uri, custom: false, protocol: "https" }],
      }).summary,
    ).toBe("Local network only");
    expect(
      remoteHealth({
        ...claimed,
        remoteAccess: false,
        publishedUris: ["https://192-168-1-5.abc.plex.direct:32400"],
        connections: [{ uri, custom: true, protocol: "https" }],
      }).summary,
    ).toBe("Local network only");
  });

  it("keeps sign-in and publishing failures ahead of a custom route", () => {
    const published = {
      ...claimed,
      remoteAccess: false,
      publishedUris: ["https://plex.example.com"],
      connections: [{ uri: "https://plex.example.com", custom: true }],
    };
    expect(remoteHealth({ ...published, tokenRejected: true }).level).toBe("error");
    expect(remoteHealth({ ...published, lastPublishError: "timeout" }).level).toBe("warn");
    expect(remoteHealth({ ...published, presence: { connected: false } }).summary).toBe(
      "plex.tv connection lost",
    );
  });

  it("keeps the WAN connectivity verdict separate from custom-route publication", () => {
    const uri = "https://plex.example.com";
    const published = {
      ...claimed,
      remoteAccess: false,
      publishedUris: [uri],
      connections: [{ uri, custom: true, protocol: "https" }],
      resourceConnections: [{ uri, local: false, relay: false }],
    };
    const reachable = remoteHealth({ ...published, plexReachable: true });
    expect(reachable.summary).toBe("Custom connection published");
    expect(reachable.level).toBe("ok");
    expect(reachable.details.join(" ")).toMatch(/plex.tv lists this custom connection/);
    expect(reachable.summary).not.toBe("Reachable outside your network");
    const unreachable = remoteHealth({ ...published, plexReachable: false });
    expect(unreachable.summary).toBe("Custom connection published");
    expect(unreachable.level).toBe("ok");
    expect(unreachable.note).toMatch(/port-mapping check does not verify custom connections/);
    const enabled = remoteHealth({ ...claimed, plexReachable: true });
    expect(enabled.summary).toBe("Direct remote access enabled");
    expect(enabled.statusLabel).toBe("Configured");
    expect(enabled.details.join(" ")).toMatch(/reported the server reachable/);
    expect(enabled.summary).not.toMatch(/Reachable/);
    expect(remoteHealth({ ...claimed, remoteAccess: false, plexReachable: false }).summary).toBe(
      "Local network only",
    );
  });

  it("confirms only the published custom identity offered by plex.tv, independently of local probes", () => {
    const route = {
      ...claimed,
      plexReachable: false,
      publishedUris: ["https://PLEX.example.com:443"],
      connections: [{ uri: "https://plex.example.com/", custom: true }],
      reachability: { reachable: true, uri: "https://plex.example.com/" },
    };
    for (const remoteAccess of [true, false]) {
      for (const uri of ["https://plex.example.com", "https://plex.example.com:443/"]) {
        const h = remoteHealth({ ...route, remoteAccess, resourceConnections: [{ uri }] });
        expect(h.level).toBe("ok");
        expect(h.summary).toBe("Custom connection published");
        expect(h.note).toMatch(/Test remote playback from another network/);
      }
    }
    for (const resourceConnections of [
      [],
      [{ uri: "https://other.example.com" }],
      [{ uri: "http://plex.example.com" }],
      [{ uri: "https://plex.example.com:8443" }],
      [{ uri: "https://plex.example.com", relay: true }],
      [{ uri: "https://plex.example.com", local: true }],
    ]) {
      expect(remoteHealth({ ...route, resourceConnections }).level).toBe("unknown");
    }
  });
});
