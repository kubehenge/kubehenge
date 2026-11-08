import type { JSX } from "react";

import { familyChoiceOf, familyChoices, familyLabels, type FamilyChoice } from "./network";
import { Field } from "./ui";

/** FamiliesField chooses a cluster's address families, or shows them where they are fixed. */
export function FamiliesField({
  value,
  hint,
  error,
  fixed = false,
  onChoose,
}: {
  value: FamilyChoice;
  hint: string | undefined;
  error?: string | undefined;
  fixed?: boolean;
  onChoose: (choice: FamilyChoice) => void;
}): JSX.Element {
  return (
    <Field label="Address families" hint={hint} error={error}>
      {(id) =>
        fixed ? (
          <input id={id} readOnly value={familyLabels[value]} />
        ) : (
          <select
            id={id}
            value={value}
            onChange={(e) => {
              onChoose(familyChoiceOf(e.target.value));
            }}
          >
            {familyChoices.map((choice) => (
              <option key={choice} value={choice}>
                {familyLabels[choice]}
              </option>
            ))}
          </select>
        )
      }
    </Field>
  );
}
