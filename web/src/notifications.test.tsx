import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { fakeServer, type Route } from "./fake-server";
import { Notifications } from "./Notifications";

const unset = (purpose: string): Route => ({ body: { purpose, set: false, canChange: true } });
const stored = (purpose: string): Route => ({ body: { purpose, set: true, canChange: true } });
const minutesAgo = (m: number): string => new Date(Date.now() - m * 60_000).toISOString();

describe("notifications", () => {
  it("shows each set target's last sending, and stores ntfy without its optional token", async () => {
    const routes: Record<string, Route> = {
      "GET /api/credentials/notify-webhook": stored("notify-webhook"),
      "GET /api/credentials/notify-ntfy": unset("notify-ntfy"),
      "GET /api/credentials/notify-email": stored("notify-email"),
      "PUT /api/credentials/notify-ntfy": stored("notify-ntfy"),
      "GET /api/notifications": {
        body: [
          { kind: "notify-webhook", at: minutesAgo(3), title: "web: VM paused" },
          { kind: "notify-ntfy" },
          {
            kind: "notify-email",
            at: minutesAgo(5),
            title: "web: VM paused",
            error: "signing in: 535 bad credentials",
          },
        ],
      },
    };
    const calls = fakeServer(routes);
    const user = userEvent.setup({ delay: null });
    render(<Notifications />);

    const webhook = screen.getByRole("group", { name: "Webhook" });
    await expect(within(webhook).findByText("Sent 3m ago")).resolves.toBeInTheDocument();
    expect(within(webhook).getByText("web: VM paused")).toBeInTheDocument();
    const email = screen.getByRole("group", { name: "Email" });
    expect(within(email).getByText("Failed 5m ago")).toBeInTheDocument();
    expect(within(email).getByText("signing in: 535 bad credentials")).toBeInTheDocument();

    const ntfy = screen.getByRole("group", { name: "ntfy" });
    await user.click(await within(ntfy).findByRole("button", { name: "Set…" }));
    await user.type(within(ntfy).getByLabelText("Topic URL"), "https://ntfy.sh/kh");
    await user.click(within(ntfy).getByRole("button", { name: "Store" }));
    await expect(within(ntfy).findByText("Set")).resolves.toBeInTheDocument();
    expect(calls.at(-1)).toMatchObject({
      key: "PUT /api/credentials/notify-ntfy",
      body: { values: { url: "https://ntfy.sh/kh" } },
    });
  });

  it("sends a test and shows how each target took it", async () => {
    const routes: Record<string, Route> = {
      "GET /api/credentials/notify-webhook": stored("notify-webhook"),
      "GET /api/credentials/notify-ntfy": stored("notify-ntfy"),
      "GET /api/credentials/notify-email": unset("notify-email"),
      "GET /api/notifications": {
        body: [{ kind: "notify-webhook" }, { kind: "notify-ntfy" }, { kind: "notify-email" }],
      },
      "POST /api/notifications/test": {
        body: [
          { kind: "notify-webhook", at: minutesAgo(1), title: "Kubehenge test" },
          { kind: "notify-ntfy", at: minutesAgo(1), title: "Kubehenge test", error: "answered 403 Forbidden" },
        ],
      },
    };
    fakeServer(routes);
    const user = userEvent.setup({ delay: null });
    render(<Notifications />);

    await user.click(await screen.findByRole("button", { name: "Send test" }));
    const ntfy = screen.getByRole("group", { name: "ntfy" });
    await expect(within(ntfy).findByText("Failed 1m ago")).resolves.toBeInTheDocument();
    expect(within(ntfy).getByText("answered 403 Forbidden")).toBeInTheDocument();
    expect(within(screen.getByRole("group", { name: "Webhook" })).getByText("Sent 1m ago")).toBeInTheDocument();
  });
});
