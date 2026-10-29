import { describe, expect, it } from "vitest";
import { createFakeServer } from "../mock/server";

describe("Apps mock API", () => {
  it("preserves setup choices while saving node-local listener settings", async () => {
    const fake = createFakeServer();
    const fetch = fake.fetch;
    const initial = await (await fetch("/ether/api/frontends")).json();
    expect(
      initial.find((app: { descriptor: { id: string } }) => app.descriptor.id === "plex").enabled,
    ).toBe(true);
    await fetch("/ether/api/frontends/silo/settings", {
      method: "PUT",
      body: JSON.stringify({ port: 8092, discovery: true }),
    });
    await fetch("/ether/api/frontends/silo/enabled", {
      method: "PUT",
      body: JSON.stringify({ enabled: true }),
    });
    const settings = await (await fetch("/ether/api/frontends/silo/settings")).json();
    expect(settings).toEqual({ port: 8092, discovery: true });
    expect(fake.state.config.frontends?.plex?.enabled).toBe(true);
    expect(fake.state.config.frontendPorts?.silo).toBe(8092);
  });
});
