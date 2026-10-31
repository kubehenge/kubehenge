import { Fragment, useRef, useState, type JSX } from "react";

import { api } from "./api";
import type { Room } from "./create-form";
import type { CreateCheck, CreateRequest } from "./types";
import { Status, bytes, errorMessage } from "./ui";

/** CheckRead is what the API server made of a new cluster, checked without creating it. */
export interface CheckRead {
  check?: CreateCheck;
  error?: string;
}

/**
 * useCreateCheck has the API server check a new cluster when run is called. An answer to an earlier
 * run that arrives late is dropped. A cluster is created only once the API server accepts it.
 */
export function useCreateCheck(): {
  read: CheckRead | undefined;
  accepted: boolean;
  run: (req: CreateRequest) => void;
} {
  const [read, setRead] = useState<CheckRead>();
  const runs = useRef(0);
  const run = (req: CreateRequest): void => {
    runs.current += 1;
    const mine = runs.current;
    setRead(undefined);
    async function check(): Promise<void> {
      try {
        const answer = await api.checkCreate(req);
        if (mine === runs.current) {
          setRead({ check: answer });
        }
      } catch (err) {
        if (mine === runs.current) {
          setRead({ error: errorMessage(err) });
        }
      }
    }
    void check();
  };
  return { read, accepted: read?.check !== undefined, run };
}

type CheckState = "ok" | "warning" | "error" | "progress";

interface Check {
  what: string;
  state: CheckState;
  text: string;
}

function serverCheck(read: CheckRead | undefined): Check {
  if (read === undefined) {
    return { what: "API server", state: "progress", text: "Checking" };
  }
  return read.check === undefined
    ? { what: "API server", state: "error", text: read.error ?? "Refused" }
    : { what: "API server", state: "ok", text: "Accepts it" };
}

// roomCheck weighs what the VMs take against what is free: advice, since memory and disk free and
// fill as other VMs come and go.
function roomCheck(what: string, need: number, free: number | undefined): Check[] {
  if (free === undefined) {
    return [];
  }
  return need > free
    ? [{ what, state: "warning", text: `Needs ${bytes(need)}, ${bytes(free)} free` }]
    : [{ what, state: "ok", text: `${bytes(need)} of ${bytes(free)} free` }];
}

/** CreateChecks is what to know before creating: what the API server makes of it, and whether it fits. */
export function CreateChecks({ read, room }: { read: CheckRead | undefined; room: Room }): JSX.Element {
  const checks = [
    serverCheck(read),
    ...roomCheck("Memory", room.memory, room.freeMemory),
    ...roomCheck("Disk", room.disk, room.freeDisk),
  ];
  return (
    <dl className="preflight" aria-label="Before creating">
      {checks.map((ch) => (
        <div key={ch.what}>
          <dt>{ch.what}</dt>
          <dd>
            <Status state={ch.state} text={ch.text} />
          </dd>
        </div>
      ))}
    </dl>
  );
}

/** ConfirmStep is the wizard's last step: the checks, what the cluster will be, and the cluster as YAML. */
export function ConfirmStep({
  read,
  room,
  summary,
}: {
  read: CheckRead | undefined;
  room: Room;
  summary: [string, string][];
}): JSX.Element {
  const manifest = read?.check?.manifest;
  return (
    <>
      <CreateChecks read={read} room={room} />
      <dl className="summary" aria-label="Summary">
        {summary.map(([k, v]) => (
          <Fragment key={k}>
            <dt>{k}</dt>
            <dd>{v}</dd>
          </Fragment>
        ))}
      </dl>
      <div className="actions">
        <CopyYAML manifest={manifest} />
      </div>
      {manifest !== undefined && (
        <details className="advanced">
          <summary>YAML</summary>
          <pre className="confirm" aria-label="YAML">
            {manifest}
          </pre>
        </details>
      )}
    </>
  );
}

/** CopyYAML copies the cluster, as the API server would store it, for kubectl apply or a Git repository. */
export function CopyYAML({ manifest }: { manifest: string | undefined }): JSX.Element {
  const [copied, setCopied] = useState(false);
  async function copy(text: string): Promise<void> {
    await navigator.clipboard.writeText(text);
    setCopied(true);
  }
  return (
    <button
      type="button"
      disabled={manifest === undefined}
      onClick={() => {
        if (manifest !== undefined) {
          void copy(manifest);
        }
      }}
    >
      {copied ? "Copied" : "Copy as YAML"}
    </button>
  );
}
