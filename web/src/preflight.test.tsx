import { render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { AdvancedFields, PreflightFindings, formOf, missingContent } from "./Setup";
import type { Storage } from "./types";

const none: string[] = [];
const wantImport = { content: "import", offered: none };

const storage = (node: string, name: string, held: { content: string[]; active?: boolean }): Storage => ({
  storage: name,
  node,
  shared: false,
  content: held.content,
  active: held.active ?? true,
  avail: 1,
  total: 1,
  usable: true,
  allocatable: false,
});

describe("the pre-flight findings", () => {
  it("lists errors and warnings apart, with the ranges the cluster gets", () => {
    render(
      <PreflightFindings
        findings={{
          errors: ["Storage local on node pve1 lacks the import content type."],
          warnings: ["3 management VMs share 1 Proxmox VE nodes."],
          podRanges: ["10.244.0.0/16"],
          serviceRanges: ["10.96.0.0/12"],
        }}
      />,
    );
    const errors = screen.getByRole("alert");
    expect(within(errors).getByText("Storage local on node pve1 lacks the import content type.")).toBeInTheDocument();
    expect(within(screen.getByRole("list", { name: "Warnings" })).getAllByRole("listitem")).toHaveLength(1);
    expect(screen.getByText("10.244.0.0/16")).toBeInTheDocument();
    expect(screen.getByText("10.96.0.0/12")).toBeInTheDocument();
  });

  it("shows a command the server marks in backticks as code", () => {
    render(
      <PreflightFindings
        findings={{
          errors: ["Not reachable through the cluster: pve3 (HTTP 596). Run `pvecm updatecerts --force` there."],
          warnings: ["Run `qm unlock 100` first."],
        }}
      />,
    );
    const errors = within(screen.getByRole("alert"));
    expect(errors.getByText("pvecm updatecerts --force").tagName).toBe("CODE");
    expect(screen.getByRole("alert")).not.toHaveTextContent("`");
    expect(within(screen.getByRole("list", { name: "Warnings" })).getByText("qm unlock 100").tagName).toBe("CODE");
  });

  it("shows nothing before Proxmox VE has been asked", () => {
    const { container } = render(<PreflightFindings findings={undefined} />);
    expect(container).toBeEmptyDOMElement();
  });
});

const showAdvanced = (warnings: string[]): void => {
  const set = vi.fn<() => void>();
  const shown = vi.fn<() => string | undefined>();
  const onOpen = vi.fn<() => void>();
  render(
    <AdvancedFields form={formOf()} set={set} shown={shown} fixed={false} open warnings={warnings} onOpen={onOpen} />,
  );
};

describe("the range warnings", () => {
  const overlap = "The pod range 10.0.0.0/16 overlaps the network 10.0.0.0/24 on bridge vmbr0 of node pve1.";

  it("sit beside the range field they are about, with Advanced open", () => {
    showAdvanced([overlap]);
    expect(screen.getByRole("status")).toHaveTextContent(overlap);
    expect(screen.getAllByRole("status")).toHaveLength(1);
  });

  it("are absent without a warning", () => {
    showAdvanced([]);
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });
});

describe("a missing content type", () => {
  const storages = [
    storage("pve1", "local", { content: ["iso", "import"] }),
    storage("pve2", "local", { content: ["iso"] }),
  ];

  it("names the nodes that lack it and where to add it", () => {
    expect(missingContent(storages, ["pve1", "pve2"], wantImport)).toBe(
      "No import storage on pve2. Add one under Datacenter, Storage, Edit, Content.",
    );
  });

  it("counts an inactive storage as missing", () => {
    expect(missingContent([storage("pve1", "nfs", { content: ["import"], active: false })], ["pve1"], wantImport)).toBe(
      "No import storage on pve1. Add one under Datacenter, Storage, Edit, Content.",
    );
  });

  it("says nothing while storages are offered or unread", () => {
    expect(missingContent(storages, ["pve1"], { content: "import", offered: ["local"] })).toBeUndefined();
    expect(missingContent(undefined, ["pve1"], wantImport)).toBeUndefined();
  });
});
