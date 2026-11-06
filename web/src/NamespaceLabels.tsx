import { useState, type JSX } from "react";

import { labelsLines, labelsOf, parseLabels } from "./namespace-labels";
import type { Cluster } from "./types";
import { Dialog, ErrorBox, Field, useChange } from "./ui";

const labelRows = 4;

/** NamespaceLabelsField takes the labels every namespace Kubehenge makes carries, one key=value per line. */
export function NamespaceLabelsField({
  value,
  onChange,
  error,
}: {
  value: string;
  onChange: (v: string) => void;
  error?: string | undefined;
}): JSX.Element {
  return (
    <Field label="Namespace labels" hint="One key=value per line" error={error}>
      {(id) => (
        <textarea
          id={id}
          rows={labelRows}
          spellCheck={false}
          value={value}
          onChange={(e) => onChange(e.target.value)}
        />
      )}
    </Field>
  );
}

export function NamespaceLabelsDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const kept = labelsLines(labelsOf(c.variables["namespaceLabels"]));
  const [value, setValue] = useState(kept);
  const { busy, error, send } = useChange(c, onClose);
  const parsed = parseLabels(value);
  return (
    <Dialog
      title="Edit namespace labels"
      onClose={onClose}
      onSubmit={() => {
        void send({ variables: { namespaceLabels: parsed.labels } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button
            type="submit"
            className="primary"
            disabled={busy || parsed.problem !== undefined || value.trim() === kept.trim()}
          >
            Save
          </button>
        </>
      }
    >
      <NamespaceLabelsField value={value} onChange={setValue} error={parsed.problem} />
      <ErrorBox message={error} />
    </Dialog>
  );
}
