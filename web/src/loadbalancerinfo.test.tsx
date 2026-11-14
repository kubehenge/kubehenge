import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { JSX } from "react";
import { describe, expect, it } from "vitest";

import { LoadBalancerFields, type LoadBalancerValue } from "./LoadBalancer";

const value: LoadBalancerValue = {
  implementation: "metallb",
  addresses: "",
  mode: "l2",
  asn: "",
  peers: "",
  routerID: "",
  authenticated: false,
};

const fields = (distribution: string): JSX.Element => (
  <LoadBalancerFields
    distribution={distribution}
    families="ipv4"
    value={value}
    errors={{}}
    onChange={() => undefined}
    password={null}
  />
);

describe("the load balancer help", () => {
  it("says what a Service gets with each choice", async () => {
    const user = userEvent.setup({ delay: null });
    const { unmount } = render(fields("rke2"));
    await user.click(screen.getByRole("button", { name: "About Load balancer" }));
    expect(screen.getByText(/With none, a Service gets no address/u)).toBeVisible();
    unmount();
    render(fields("k3s"));
    await user.click(screen.getByRole("button", { name: "About Load balancer" }));
    expect(screen.getByText(/With k3s's own, a Service answers on the nodes' own addresses/u)).toBeVisible();
  });
});
