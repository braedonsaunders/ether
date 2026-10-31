import { describe, expect, it } from "vitest";
import { connectionText } from "../pages/sources/Usenet";

describe("connectionText", () => {
  it("shows a learned server limit next to the busy count", () => {
    expect(
      connectionText(
        {
          configuredConnections: 50,
          maxConnections: 47,
          activeConnections: 12,
          limitLearned: true,
        },
        50,
      ),
    ).toEqual(["12 of 50 connections busy", "using 47 of 50 connections (server limit learned)"]);
  });

  it("uses this node's share in a cluster", () => {
    expect(
      connectionText(
        {
          configuredConnections: 30,
          nodeShare: 10,
          maxConnections: 8,
          activeConnections: 3,
          limitLearned: true,
        },
        30,
      ),
    ).toEqual([
      "3 of 10 connections busy",
      "this node: 10 of 30 connections",
      "using 8 of 10 connections (server limit learned)",
    ]);
  });

  it("says nothing about limits the server did not impose", () => {
    expect(
      connectionText({ configuredConnections: 20, maxConnections: 20, activeConnections: 0 }, 20),
    ).toEqual(["0 of 20 connections busy"]);
  });
});
