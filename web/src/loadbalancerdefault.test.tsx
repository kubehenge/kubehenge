import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState, type JSX } from "react";
import { describe, expect, it } from "vitest";

import { newForm, setLoadBalancer, type Form } from "./create-form";
import { NetworkStep } from "./CreateSteps";
import { LoadBalancerFields, recommendedImplementation, type LoadBalancerValue } from "./LoadBalancer";
import type { ClusterClass } from "./types";

const fields = (
  distribution: string,
  addresses: string,
  implementation: LoadBalancerValue["implementation"],
): JSX.Element => (
  <LoadBalancerFields
    distribution={distribution}
    families="ipv4"
    value={{ implementation, addresses, mode: "l2", asn: "", peers: "", routerID: "", authenticated: false }}
    errors={{}}
    onChange={() => {
      // The labels are read, not changed.
    }}
    password={null}
  />
);

const choice = (): HTMLElement => screen.getByLabelText("Load balancer");
const optionLabels = (): string[] =>
  within(choice())
    .getAllByRole("option")
    .map((o) => o.textContent ?? "");

/** A form that applies what setLoadBalancer sets, as the wizard's state does. */
function typing(form: Form): { form: Form; patch: (p: Partial<LoadBalancerValue>) => void } {
  const state = { form };
  return {
    get form() {
      return state.form;
    },
    patch(p) {
      setLoadBalancer(
        (k, v) => {
          state.form = { ...state.form, [k]: v };
        },
        p,
        state.form.loadBalancerPicked,
      );
    },
  };
}

const k3sClass: ClusterClass = {
  name: "kubehenge-k3s",
  distribution: "k3s",
  versionFormat: { pattern: ".*" },
  annotations: {},
  variables: [
    { name: "controlPlaneEndpoint", required: true, schema: { type: "string" } },
    { name: "network", required: true, schema: { type: "object", properties: {} } },
    { name: "loadBalancer", required: false, schema: { type: "object" } },
  ],
};

const noProblems = new Map<string, string>();

function Step(): JSX.Element {
  const [form, setForm] = useState<Form>(newForm());
  return (
    <NetworkStep
      form={form}
      set={(k, v) => {
        setForm((f) => ({ ...f, [k]: v }));
      }}
      shown={() => noProblems.get("name")}
      cls={k3sClass}
      bridgeOptions={[]}
    />
  );
}

describe("the load balancer in the wizard's Network step", () => {
  it("moves from k3s's own to MetalLB when a range is entered, until the user picks", async () => {
    const user = userEvent.setup();
    render(<Step />);
    expect(choice()).toHaveDisplayValue("k3s's own (recommended)");
    await user.type(screen.getByLabelText("Load-balancer addresses"), "10.0.0.100-10.0.0.120");
    expect(choice()).toHaveDisplayValue("MetalLB (recommended)");
    await user.selectOptions(choice(), "kube-vip");
    await user.clear(screen.getByLabelText("Load-balancer addresses"));
    expect(choice()).toHaveDisplayValue("kube-vip");
  });

  it("keeps a pick of k3s's own, which has no range to enter", async () => {
    const user = userEvent.setup();
    render(<Step />);
    await user.selectOptions(choice(), "MetalLB");
    await user.type(screen.getByLabelText("Load-balancer addresses"), "10.0.0.100");
    await user.selectOptions(choice(), "k3s's own");
    expect(choice()).toHaveDisplayValue("k3s's own");
    expect(screen.queryByLabelText("Load-balancer addresses")).toBeNull();
  });
});

describe("the load balancer default", () => {
  it("recommends the distribution's own with no range, and MetalLB with one", () => {
    expect(recommendedImplementation("")).toBe("");
    expect(recommendedImplementation("  \n")).toBe("");
    expect(recommendedImplementation("10.0.0.100-10.0.0.120")).toBe("metallb");
  });

  it("marks k3s's own as recommended while the selection is k3s's own", () => {
    render(fields("k3s", "", ""));
    expect(optionLabels()).toStrictEqual(["k3s's own (recommended)", "MetalLB", "kube-vip"]);
    expect(choice()).toHaveDisplayValue("k3s's own (recommended)");
  });

  it("marks MetalLB as recommended once a range is entered", () => {
    render(fields("k3s", "10.0.0.100-10.0.0.120", "metallb"));
    expect(optionLabels()).toStrictEqual(["k3s's own", "MetalLB (recommended)", "kube-vip"]);
    expect(choice()).toHaveDisplayValue("MetalLB (recommended)");
  });

  it("marks nothing without a range on a distribution that has no load balancer of its own", () => {
    render(fields("rke2", "", ""));
    expect(optionLabels()).toStrictEqual(["None", "MetalLB", "kube-vip"]);
  });

  it("follows the range until the load balancer is picked", () => {
    const t = typing(newForm());
    t.patch({ addresses: "10.0.0.100-10.0.0.120" });
    expect(t.form.loadBalancer).toBe("metallb");
    t.patch({ addresses: "" });
    expect(t.form.loadBalancer).toBe("");
  });

  it("keeps a load balancer the user picked, whatever the range", () => {
    const t = typing(newForm());
    t.patch({ implementation: "" });
    t.patch({ addresses: "10.0.0.100-10.0.0.120" });
    expect(t.form.loadBalancer).toBe("");
    const kubeVip = typing(newForm());
    kubeVip.patch({ implementation: "kube-vip" });
    kubeVip.patch({ addresses: "10.0.0.100-10.0.0.120" });
    kubeVip.patch({ addresses: "" });
    expect(kubeVip.form.loadBalancer).toBe("kube-vip");
  });
});
