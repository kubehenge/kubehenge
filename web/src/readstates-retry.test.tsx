import { render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { ApplianceActivity, ClusterActivity } from "./Activity";
import { ApplianceUpgrades } from "./ApplianceUpgrades";
import { fakeServer, held } from "./fake-server";
import { InterfaceCertificate } from "./InterfaceCertificate";
import { unreadable, web } from "./readstates-fixtures";
import { Setup } from "./Setup";
import { Templates } from "./Templates";

// tryNow is the retry a failed read offers.
const tryNow = (): Promise<HTMLElement> => screen.findByRole("button", { name: "Try now" });

describe("a retry under every failed read", () => {
  it("offers a retry when the appliance's activity fails", async () => {
    fakeServer({ "GET /api/activity?days=7": unreadable });
    render(<ApplianceActivity />);
    await expect(tryNow()).resolves.toBeInTheDocument();
  });

  it("offers a retry when a cluster's activity fails", async () => {
    fakeServer({ "GET /api/clusters/default/web/activity?days=7": unreadable });
    render(<ClusterActivity cluster={web} />);
    await expect(tryNow()).resolves.toBeInTheDocument();
  });

  it("offers a retry when the appliance's upgrades fail", async () => {
    fakeServer({ "GET /api/appliance/upgrades": unreadable });
    render(<ApplianceUpgrades />);
    await expect(tryNow()).resolves.toBeInTheDocument();
  });

  it("offers a retry when the VM templates fail", async () => {
    fakeServer({ "GET /api/templates": unreadable, "GET /api/release-pins": held().route });
    render(<Templates />);
    await expect(tryNow()).resolves.toBeInTheDocument();
  });

  it("offers a retry when the release pins fail", async () => {
    fakeServer({ "GET /api/templates": held().route, "GET /api/release-pins": unreadable });
    render(<Templates />);
    await expect(tryNow()).resolves.toBeInTheDocument();
  });

  it("offers a retry when the interface certificate fails", async () => {
    fakeServer({ "GET /api/appliance/certificate": unreadable });
    render(<InterfaceCertificate />);
    await expect(tryNow()).resolves.toBeInTheDocument();
  });

  it("offers a retry when the setup status fails on the first run", async () => {
    fakeServer({ "GET /api/setup": unreadable });
    render(<Setup />);
    await expect(tryNow()).resolves.toBeInTheDocument();
  });

  it("says why no template is built while the templates are not read", async () => {
    fakeServer({ "GET /api/templates": unreadable, "GET /api/release-pins": held().route });
    render(<Templates />);
    await expect(screen.findByText("Templates not read.")).resolves.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Build template" })).toBeDisabled();
  });

  it("offers no retry of the appliance upgrades where Kubehenge runs outside the appliance", async () => {
    fakeServer({ "GET /api/appliance/upgrades": { status: 404, body: { error: "upgrades need the appliance" } } });
    render(<ApplianceUpgrades />);
    const box = within(await screen.findByRole("alert"));
    expect(box.getByText(/need the appliance/u)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Try now" })).not.toBeInTheDocument();
  });
});
