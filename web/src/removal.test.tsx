import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { fakeServer, type Route } from "./fake-server";
import { FleetProvider } from "./fleet";
import { DangerZone } from "./RemoveManagement";

const cluster = (name: string): object => ({
  namespace: "clusters",
  name,
  class: "k3s",
  distribution: "k3s",
  version: "v1.34.0",
  generation: 1,
  created: "2026-01-01T00:00:00Z",
  deleting: false,
  phase: "Provisioned",
  available: true,
  conditions: [],
  controlPlane: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
});

describe("removing Kubehenge", () => {
  it("leaves the clusters rule to the disabled button, not the dialog", async () => {
    fakeServer({});
    const user = userEvent.setup({ delay: null });
    render(<DangerZone running={false} onStarted={vi.fn<() => void>()} />);
    await user.click(screen.getByRole("button", { name: "Remove…" }));
    expect(screen.queryByText(/Delete every cluster first/u)).not.toBeInTheDocument();
  });

  it("removes on Enter only once the word is typed", async () => {
    const routes: Record<string, Route> = { "POST /api/setup/remove": { status: 202, body: {} } };
    const calls = fakeServer(routes);
    const started = vi.fn<() => void>();
    const user = userEvent.setup({ delay: null });
    render(<DangerZone running={false} onStarted={started} />);

    await user.click(screen.getByRole("button", { name: "Remove…" }));
    expect(screen.getByRole("dialog", { name: "Remove Kubehenge" })).toBeInTheDocument();
    await user.type(screen.getByLabelText("State passphrase"), "correct horse battery staple{Enter}");
    expect(calls.filter((c) => c.key === "POST /api/setup/remove")).toHaveLength(0);

    await user.type(screen.getByLabelText("Type remove to confirm"), "remove{Enter}");
    await waitFor(() => expect(started).toHaveBeenCalledWith());
    expect(calls.filter((c) => c.key === "POST /api/setup/remove").map((c) => c.body)).toStrictEqual([
      { passphrase: "correct horse battery staple", confirm: "remove", removeBackups: false },
    ]);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("offers no removal while clusters exist, and says how many to delete", async () => {
    fakeServer({
      "GET /api/clusters": { body: [cluster("web"), cluster("ci")] },
      "GET /api/fleet/health": { body: [] },
      "GET /api/runs": { body: [] },
    });
    render(
      <FleetProvider>
        <DangerZone running={false} onStarted={vi.fn<() => void>()} />
      </FleetProvider>,
    );
    await expect(screen.findByText("Delete 2 clusters first")).resolves.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Remove…" })).toBeDisabled();
  });

  it("offers no removal while a run is going on", () => {
    fakeServer({});
    render(<DangerZone running onStarted={vi.fn<() => void>()} />);
    expect(screen.getByRole("button", { name: "Remove…" })).toBeDisabled();
  });
});
