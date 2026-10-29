import { useEffect, useState, type JSX } from "react";

import { api } from "./api";
import { addressRoom } from "./Autoscaling";
import { Link } from "./router";
import type { Cluster, ClusterHealth, Effect, Plan } from "./types";
import { Status, errorMessage } from "./ui";

// What the machines go through, by the most a change does.
const replaces: Record<Effect, string> = {
  none: "Replaces no node",
  scales: "Adds or removes nodes",
  "control-plane": "Replaces the control-plane nodes, one at a time",
  workers: "Replaces the workers, one at a time",
  all: "Replaces every node, one at a time",
};
const effectOrder: Effect[] = ["none", "scales", "workers", "control-plane", "all"];

/** PlanRead is what the API server makes of a version, checked without being made. */
export interface PlanRead {
  version: string;
  plan?: Plan;
  error?: string;
}

/** usePlan has the API server check an upgrade to version, unless it is the version running. */
export function usePlan(c: Cluster, version: string): PlanRead | undefined {
  const [read, setRead] = useState<PlanRead>();
  const { namespace, name, generation } = c;
  const running = c.version;
  useEffect(() => {
    let current = true;
    async function check(): Promise<void> {
      try {
        const plan = await api.plan({ namespace, name, generation }, { version });
        if (current) {
          setRead({ version, plan });
        }
      } catch (err) {
        if (current) {
          setRead({ version, error: errorMessage(err) });
        }
      }
    }
    if (version !== running) {
      void check();
    }
    return () => {
      current = false;
    };
  }, [namespace, name, generation, running, version]);
  return read?.version === version ? read : undefined;
}

type CheckState = "ok" | "warning" | "error" | "progress";

interface Check {
  what: string;
  state: CheckState;
  text: string;
}

function changeCheck(read: PlanRead | undefined): Check {
  if (read === undefined) {
    return { what: "Change", state: "progress", text: "Checking" };
  }
  if (read.plan === undefined) {
    return { what: "Change", state: "error", text: read.error ?? "Refused" };
  }
  const effects = read.plan.changes.map((ch) => effectOrder.indexOf(ch.effect));
  return { what: "Change", state: "ok", text: replaces[effectOrder[Math.max(0, ...effects)] ?? "none"] };
}

// A rollout makes each new node before it removes the old one, so it needs one address spare.
function addressCheck(c: Cluster): Check | undefined {
  const room = addressRoom(c);
  if (room === undefined) {
    return undefined;
  }
  const spare = room - c.controlPlane.desired - c.workers.desired;
  return spare > 0
    ? { what: "Addresses", state: "ok", text: `${spare} spare` }
    : { what: "Addresses", state: "error", text: "None spare. Add one under Options." };
}

function healthCheck(health: ClusterHealth | string | undefined): Check {
  if (health === undefined) {
    return { what: "Health", state: "progress", text: "Checking" };
  }
  if (typeof health === "string") {
    return { what: "Health", state: "warning", text: health };
  }
  if (health.verdict === "unreachable") {
    return { what: "Health", state: "error", text: "Unreachable" };
  }
  const errors = health.attention.filter((a) => a.severity === "error").length;
  const warnings = health.attention.filter((a) => a.severity === "warning").length;
  if (errors + warnings === 0) {
    return { what: "Health", state: "ok", text: "No problems" };
  }
  const count = errors + warnings;
  return {
    what: "Health",
    state: errors > 0 ? "error" : "warning",
    text: `${count} ${count === 1 ? "problem" : "problems"}`,
  };
}

/**
 * UpgradeChecks is what to know before an upgrade: what the API server makes of it and what it
 * replaces, whether the rollout has the address it needs, and what is wrong with the cluster already.
 */
export function UpgradeChecks({
  cluster: c,
  read,
  healthPath,
}: {
  cluster: Cluster;
  read: PlanRead | undefined;
  healthPath: string;
}): JSX.Element {
  const [health, setHealth] = useState<ClusterHealth | string>();
  const { namespace, name } = c;
  useEffect(() => {
    let current = true;
    async function load(): Promise<void> {
      try {
        const h = await api.health(namespace, name);
        if (current) {
          setHealth(h);
        }
      } catch (err) {
        if (current) {
          setHealth(errorMessage(err));
        }
      }
    }
    void load();
    return () => {
      current = false;
    };
  }, [namespace, name]);
  const address = addressCheck(c);
  const checks = [changeCheck(read), ...(address === undefined ? [] : [address]), healthCheck(health)];
  return (
    <dl className="preflight" aria-label="Before upgrading">
      {checks.map((ch) => (
        <div key={ch.what}>
          <dt>{ch.what === "Health" ? <Link to={healthPath}>Health</Link> : ch.what}</dt>
          <dd>
            <Status state={ch.state} text={ch.text} />
          </dd>
        </div>
      ))}
    </dl>
  );
}
