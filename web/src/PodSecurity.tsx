import type { JSX } from "react";

import { Field } from "./ui";
import { oneOf, record, text } from "./values";

/** The Pod Security Standard levels Pod Security Admission enforces; privileged is Kubernetes' own default. */
export const admissionLevels = ["privileged", "baseline", "restricted"] as const;
export type AdmissionLevel = (typeof admissionLevels)[number];

/** The levels Kyverno's policies enforce; off leaves Kyverno out of the cluster. */
export const kyvernoLevels = ["off", "baseline", "restricted"] as const;
export type KyvernoLevel = (typeof kyvernoLevels)[number];

export interface PodSecurity {
  admission: AdmissionLevel;
  kyverno: KyvernoLevel;
}

const levelNames: Record<AdmissionLevel | KyvernoLevel, string> = {
  off: "Off",
  privileged: "Privileged: nothing enforced",
  baseline: "Baseline",
  restricted: "Restricted",
};

/** podSecurityOf reads a cluster's podSecurity variable, with the class's defaults for what it leaves out. */
export function podSecurityOf(v: unknown): PodSecurity {
  const r = record(v);
  const admission = text(r["admission"]);
  const kyverno = text(r["kyverno"]);
  return {
    admission: oneOf(admissionLevels, admission) ? admission : "privileged",
    kyverno: oneOf(kyvernoLevels, kyverno) ? kyverno : "off",
  };
}

/** podSecurityVariable is the variable for a choice, or undefined for the defaults, which need none. */
export function podSecurityVariable(p: PodSecurity): Partial<PodSecurity> | undefined {
  const v: Partial<PodSecurity> = {};
  if (p.admission !== "privileged") {
    v.admission = p.admission;
  }
  if (p.kyverno !== "off") {
    v.kyverno = p.kyverno;
  }
  return Object.keys(v).length > 0 ? v : undefined;
}

const capitalised = (l: string): string => l.charAt(0).toUpperCase() + l.slice(1);

/** podSecurityText says in a line what holds a cluster's pods to which level. */
export function podSecurityText(p: PodSecurity): string {
  const parts: string[] = [];
  if (p.admission !== "privileged") {
    parts.push(`${capitalised(p.admission)} by Pod Security Admission`);
  }
  if (p.kyverno !== "off") {
    parts.push(`${capitalised(p.kyverno)} by Kyverno`);
  }
  return parts.length > 0 ? parts.join(", ") : "Not enforced";
}

/** PodSecurityFields chooses the level Pod Security Admission and Kyverno each enforce. */
export function PodSecurityFields({
  value,
  onChange,
}: {
  value: PodSecurity;
  onChange: (p: PodSecurity) => void;
}): JSX.Element {
  // Non-breaking hyphens keep the namespace names whole where the text wraps.
  return (
    <>
      <p className="muted">
        Baseline refuses privileged pods and the host&apos;s namespaces and paths; restricted also has containers run
        without root, privilege escalation or capabilities. The namespaces of the components that need the host
        (kube&#8209;system, the pod network&apos;s, node&#8209;exporter&apos;s and Rancher&apos;s) are exempt from both.
      </p>
      <Field
        label="Pod Security Admission"
        hint="Kubernetes' own check, for every namespace that labels no level of its own. Below restricted, pods short of restricted are warned about. Changing it replaces the control-plane nodes one at a time."
      >
        {(id) => (
          <select
            id={id}
            value={value.admission}
            onChange={(e) => {
              if (oneOf(admissionLevels, e.target.value)) {
                onChange({ ...value, admission: e.target.value });
              }
            }}
          >
            {admissionLevels.map((l) => (
              <option key={l} value={l}>
                {levelNames[l]}
              </option>
            ))}
          </select>
        )}
      </Field>
      <Field
        label="Kyverno"
        hint="Installs Kyverno, whose policies hold every namespace to the level whatever its labels say, and report what falls short."
      >
        {(id) => (
          <select
            id={id}
            value={value.kyverno}
            onChange={(e) => {
              if (oneOf(kyvernoLevels, e.target.value)) {
                onChange({ ...value, kyverno: e.target.value });
              }
            }}
          >
            {kyvernoLevels.map((l) => (
              <option key={l} value={l}>
                {levelNames[l]}
              </option>
            ))}
          </select>
        )}
      </Field>
    </>
  );
}
