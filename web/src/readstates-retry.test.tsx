import { render, screen } from "@testing-library/react";
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
});
