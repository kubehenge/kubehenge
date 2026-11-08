import { useState, type JSX } from "react";

import { isSecurityGroup, list } from "./network";
import type { Cluster } from "./types";
import { Dialog, ErrorBox, Field, useChange } from "./ui";
import { texts } from "./values";

/** securityGroupsOf reads the securityGroups variable into the text its field edits. */
export const securityGroupsOf = (v: unknown): string => (texts(v) ?? []).join(", ");

/** securityGroupsText is how the settings list the groups. */
export function securityGroupsText(s: string): string {
  const groups = list(s);
  return groups.length > 0 ? groups.join(", ") : "Off";
}

/** securityGroupsProblem names what is wrong with the groups, or nothing. The server checks they exist. */
export function securityGroupsProblem(s: string): string | undefined {
  const groups = list(s);
  if (groups.some((g) => !isSecurityGroup(g))) {
    return "Names of 2 to 18 letters, digits, hyphens and underscores, starting with a letter";
  }
  return new Set(groups).size < groups.length ? "Each group once" : undefined;
}

/** SecurityGroupsField edits the Proxmox VE security groups of a cluster's nodes. */
export function SecurityGroupsField({
  value,
  onChange,
  error,
}: {
  value: string;
  onChange: (s: string) => void;
  error?: string | undefined;
}): JSX.Element {
  return (
    <Field
      label="Firewall security groups"
      hint="Optional, comma-separated"
      info="The nodes accept the cluster's own traffic. The groups must accept the rest, such as ingress."
      error={error}
    >
      {(id) => <input id={id} spellCheck={false} value={value} onChange={(e) => onChange(e.target.value)} />}
    </Field>
  );
}

export function FirewallDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const current = securityGroupsOf(c.variables["securityGroups"]);
  const [value, setValue] = useState(current);
  const [attempted, setAttempted] = useState(false);
  const { busy, error, send } = useChange(c, onClose);
  const found = securityGroupsProblem(value);
  const changed = list(value).join(",") !== list(current).join(",");
  return (
    <Dialog
      title="Edit firewall security groups"
      onClose={onClose}
      onSubmit={() => {
        if (found !== undefined) {
          setAttempted(true);
          return;
        }
        const groups = list(value);
        void send({ variables: { securityGroups: groups.length > 0 ? groups : null } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || !changed}>
            Save
          </button>
        </>
      }
    >
      <SecurityGroupsField value={value} onChange={setValue} error={attempted ? found : undefined} />
      <p className="muted">Saving replaces every node, one at a time.</p>
      <ErrorBox message={error} />
    </Dialog>
  );
}
