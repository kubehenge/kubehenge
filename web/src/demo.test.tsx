import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { fakeServer } from "./fake-server";

const signedIn = {
  "GET /api/session": { body: { user: "admin@pve", csrf: "c1" } },
  "GET /api/clusters": { body: [] },
  "GET /api/runs": { body: [] },
};

// The demo mark stays once any answer carried it, so the real install's case runs first.
describe("the demo mark", () => {
  it("is absent on a real install", async () => {
    window.history.replaceState(null, "", "/");
    fakeServer(signedIn);
    render(<App />);
    await screen.findByText("admin@pve");
    expect(screen.queryByText("Demo")).not.toBeInTheDocument();
  });

  it("shows in the top bar of the demo", async () => {
    window.history.replaceState(null, "", "/");
    fakeServer(signedIn, { "X-Kubehenge-Demo": "1" });
    render(<App />);
    await screen.findByText("admin@pve");
    expect(screen.getByRole("link", { name: /Kubehenge\s*Demo/u })).toBeInTheDocument();
  });

  it("shows on the demo's sign-in page", async () => {
    window.history.replaceState(null, "", "/");
    fakeServer(
      {
        "GET /api/session": { status: 401, body: { error: "not signed in" } },
        "GET /api/realms": { body: [{ realm: "pve", type: "pve" }] },
      },
      { "X-Kubehenge-Demo": "1" },
    );
    render(<App />);
    await expect(screen.findByRole("heading", { name: /Kubehenge\s*Demo/u })).resolves.toBeInTheDocument();
  });
});
