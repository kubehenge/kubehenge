import { render, screen, within } from "@testing-library/react";
import type { JSX } from "react";
import { describe, expect, it } from "vitest";

import { newForm, setLoadBalancer, type Form } from "./create-form";
import { LoadBalancerFields, recommendedImplementation, type LoadBalancerValue } from "./LoadBalancer";

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
