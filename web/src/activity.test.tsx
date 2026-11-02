import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { ApplianceActivity, ClusterActivity } from "./Activity";
import { fakeServer } from "./fake-server";
import type { Activity } from "./types";

const minutesAgo = (n: number): string => new Date(Date.now() - n * 60_000).toISOString();

const appliance: Activity = {
  since: minutesAgo(7 * 24 * 60),
  entries: [
    {
      at: minutesAgo(2),
      user: "admin@pve",
      what: "changed cluster",
      cluster: "default/web",
      details: { requestId: "ab12" },
    },
    { at: minutesAgo(9), user: "mallory@pve", what: "sign-in refused", details: { address: "10.0.0.9" } },
    { at: minutesAgo(12), user: "bob@pve", what: "headlamp sign-in", cluster: "default/web", failed: true },
  ],
  events: {
    items: [
      {
        source: "management",
        type: "Warning",
        reason: "FailedCreate",
        message: "no space left",
        object: "Machine web-md-0-a",
        count: 3,
        last: minutesAgo(4),
      },
    ],
  },
};

describe("activity", () => {
  it("lists who did what, with the cluster and the details, and the day's events", async () => {
    fakeServer({ "GET /api/activity?days=7": { body: appliance } });
    render(<ApplianceActivity />);

    const row = within(await screen.findByRole("row", { name: /Changed cluster/u }));
    expect(row.getByText("admin@pve")).toBeInTheDocument();
    expect(row.getByRole("link", { name: "web" })).toHaveAttribute("href", "/clusters/default/web");
    expect(row.queryByText(/ab12/u)).toBeNull();
    expect(row.queryByText("Failed")).toBeNull();
    expect(within(screen.getByRole("row", { name: /Headlamp sign-in/u })).getByText("Failed")).toBeInTheDocument();
    expect(screen.getByText("address 10.0.0.9")).toBeInTheDocument();
    expect(screen.getByText("no space left")).toBeInTheDocument();
  });

  it("reads the window chosen, and a cluster's own activity", async () => {
    const calls = fakeServer({
      "GET /api/clusters/default/web/activity?days=7": { body: { ...appliance, entries: [] } },
      "GET /api/clusters/default/web/activity?days=30": { body: appliance },
    });
    render(<ClusterActivity cluster={{ namespace: "default", name: "web" }} />);

    await expect(screen.findByText("Nothing done")).resolves.toBeInTheDocument();
    expect(screen.queryByRole("columnheader", { name: "Cluster" })).toBeNull();
    await userEvent.selectOptions(screen.getByLabelText(/Show/u), "Last 30 days");
    await expect(screen.findByText("Changed cluster")).resolves.toBeInTheDocument();
    expect(calls.map((c) => c.key)).toContain("GET /api/clusters/default/web/activity?days=30");
  });
});
