import { describe, expect, it } from "vitest";
import { outboundVerdict, type OutboundCheck } from "../pages/settings/OutboundInternet";

function check(part: OutboundCheck): OutboundCheck {
  return part;
}

describe("outboundVerdict", () => {
  it("calls a split tunnel working when the addresses differ", () => {
    expect(
      outboundVerdict(
        check({ proxyEnabled: true, sourceIp: "198.51.100.20", publishedIp: "203.0.113.7" }),
      ),
    ).toBe("split");
  });

  it("warns when both addresses match", () => {
    expect(
      outboundVerdict(
        check({ proxyEnabled: true, sourceIp: "203.0.113.7", publishedIp: "203.0.113.7" }),
      ),
    ).toBe("same");
  });

  it("fails closed when the proxy does not answer", () => {
    expect(
      outboundVerdict(
        check({
          proxyEnabled: true,
          sourceError: "proxy did not answer",
          publishedIp: "203.0.113.7",
        }),
      ),
    ).toBe("down");
  });

  it("does not compare a hostname with an address", () => {
    expect(
      outboundVerdict(
        check({ proxyEnabled: true, sourceIp: "198.51.100.20", publishedIp: "plex.example" }),
      ),
    ).toBe("source");
  });

  it("stays quiet when the proxy is off", () => {
    expect(
      outboundVerdict(
        check({ proxyEnabled: false, sourceIp: "203.0.113.7", publishedIp: "203.0.113.7" }),
      ),
    ).toBe("off");
  });
});
