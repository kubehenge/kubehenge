import type { JSX } from "react";

import { Field } from "./ui";
import { oneOf, record, text } from "./values";

/** The Pod Security Standard levels Pod Security Admission enforces; baseline is the classes' default. */
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
  privileged: "Privileged",
  baseline: "Baseline",
  restricted: "Restricted",
};

/** podSecurityOf reads a cluster's podSecurity variable, with the class's defaults for what it leaves out. */
export function podSecurityOf(v: unknown): PodSecurity {
  const r = record(v);
  const admission = text(r["admission"]);
  const kyverno = text(r["kyverno"]);
  return {
    admission: oneOf(admissionLevels, admission) ? admission : "baseline",
    kyverno: oneOf(kyvernoLevels, kyverno) ? kyverno : "off",
  };
}

export const cisText = (on: boolean): string => (on ? "On" : "Off");

/** cisHint is what a user must know before choosing CIS hardening: it cannot be undone. */
export const cisHint = "Fixed once the cluster exists";

/** cisInfo says what CIS hardening costs workloads, beyond the baseline every cluster gets. */
export const cisInfo =
  "Restricted pods, default seccomp, idle exec sessions end after 5 minutes. RKE2 isolates kube-system, kube-public and default.";

/** enforced is what holds a cluster's pods: CIS hardening has Pod Security Admission enforce restricted, whatever was chosen. */
export function enforced(p: PodSecurity, cis: boolean): PodSecurity {
  return cis ? { ...p, admission: "restricted" } : p;
}

/** podSecurityVariable is the variable for a choice, or undefined for the defaults, which need none. */
export function podSecurityVariable(p: PodSecurity): Partial<PodSecurity> | undefined {
  const v: Partial<PodSecurity> = {};
  if (p.admission !== "baseline") {
    v.admission = p.admission;
  }
  if (p.kyverno !== "off") {
    v.kyverno = p.kyverno;
  }
  return Object.keys(v).length > 0 ? v : undefined;
}

const capitalised = (l: string): string => l.charAt(0).toUpperCase() + l.slice(1);

/**
 * podSecurityText says in a line what holds a cluster's pods to which level; a bare level is Pod
 * Security Admission's.
 */
export function podSecurityText(p: PodSecurity): string {
  const parts: string[] = [];
  if (p.admission !== "privileged") {
    parts.push(capitalised(p.admission));
  }
  if (p.kyverno !== "off") {
    parts.push(`Kyverno ${p.kyverno}`);
  }
  return parts.length > 0 ? parts.join(", ") : "Not enforced";
}

/** PodSecurityFields chooses the level Pod Security Admission and Kyverno each enforce; under CIS hardening, Kyverno's alone. */
export function PodSecurityFields({
  value,
  onChange,
  cis = false,
}: {
  value: PodSecurity;
  onChange: (p: PodSecurity) => void;
  cis?: boolean;
}): JSX.Element {
  return (
    <>
      <Field
        label="Pod Security Admission"
        hint={cis ? "Restricted under CIS hardening" : "Changing it replaces the control-plane nodes"}
        info="Baseline blocks privileged pods and host access. Restricted also needs containers without root or capabilities."
      >
        {(id) => (
          <select
            id={id}
            value={enforced(value, cis).admission}
            disabled={cis}
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
      <Field label="Kyverno" info="Ignores namespace labels and reports what falls short.">
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
