import {
  Component,
  cloneElement,
  isValidElement,
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
  type JSX,
  type ReactNode,
} from "react";

import { ApiError, api } from "./api";
import type { ChangeRequest, Cluster } from "./types";

// minPassphrase is the fewest characters the appliance takes for the state passphrase.
export const minPassphrase = 16;

// passphraseShort tells whether a passphrase has fewer than minPassphrase characters, counted as
// code points, as the appliance counts them.
export function passphraseShort(passphrase: string): boolean {
  // oxlint-disable-next-line typescript/no-misused-spread -- code points are what is counted
  return [...passphrase].length < minPassphrase;
}

// errorMessage is what a caught value says, for showing it.
export function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/** filled tells whether an optional text has anything in it. */
export function filled(s: string | null | undefined): s is string {
  return s !== undefined && s !== null && s !== "";
}

const distributionNames: Record<string, string> = { rke2: "RKE2", k3s: "k3s", kubeadm: "kubeadm" };

/** distributionName is how a distribution is written for people: RKE2, k3s, kubeadm. */
export const distributionName = (d: string): string => distributionNames[d] ?? d;

// The controller registers a cluster with the Rancher another of its clusters runs, as soon as
// one does.
export const registerHint = "With the Rancher one of Kubehenge's other clusters runs, now or once one does.";
export const registeredWithRancher = "Registered with Rancher, once one of Kubehenge's clusters runs it";

/** istioHint says what Istio does in a cluster and how workloads join it. */
export const istioHint =
  "Istio in ambient mode, without sidecars: pods in a namespace labelled istio.io/dataplane-mode=ambient talk to each other over mutual TLS, with their workload identities. Switching it off first takes every namespace and pod out of the mesh.";
export const istioText = (on: boolean): string => (on ? "On, ambient mode" : "Off");

/** sole is the one name of a list that has only one: a choice a form can make for the user. */
export const sole = (names: string[]): string | undefined => (names.length === 1 ? names[0] : undefined);

// ErrorBoundary shows a failed page as a message with a way out, instead of leaving the whole
// interface blank.
export class ErrorBoundary extends Component<{ children: ReactNode }, { error?: Error }> {
  override state: { error?: Error } = {};

  static getDerivedStateFromError(error: Error): { error: Error } {
    return { error };
  }

  override render(): ReactNode {
    if (!this.state.error) {
      return this.props.children;
    }
    return (
      <section className="panel">
        <div className="toolbar">
          <h2>Something went wrong on this page</h2>
        </div>
        <div className="pad">
          <ErrorBox message={this.state.error.message} />
          <p className="muted">Reloading usually helps. If it does not, the appliance's log has the details.</p>
          <button type="button" className="primary" onClick={() => window.location.reload()}>
            Reload
          </button>
        </div>
      </section>
    );
  }
}

// How often a page asks again about a job: often while it runs, to follow it, and seldom otherwise,
// only to notice one another administrator starts.
export const pollRunning = 3000;
export const pollIdle = 30_000;

// usePoll loads data now and then every `every` milliseconds, which may follow the data, such as
// often while a job runs and seldom otherwise. It keeps the last good result through errors. A
// hidden tab is not polled and catches up when shown again; an answer older than one already shown
// is dropped, so a slow request cannot bring back what a faster, later one replaced; and an answer
// equal to the one shown changes nothing, so the page is not drawn again.
export interface Poll<T> {
  data: T | undefined;
  error: string | undefined;
  errorStatus: number | undefined;
  reload: () => Promise<void>;
}

export function usePoll<T>(load: () => Promise<T>, every: number | ((data: T | undefined) => number)): Poll<T> {
  const [data, setData] = useState<T>();
  const [error, setError] = useState<string>();
  // The HTTP status of the last failure; 0 when the server did not answer.
  const [errorStatus, setErrorStatus] = useState<number>();
  const loadRef = useRef(load);
  useLayoutEffect(() => {
    loadRef.current = load;
  });
  const asked = useRef(0);
  const shown = useRef(0);
  const pending = useRef(0);
  // No answer's JSON is empty, so the first one is always shown.
  const shownText = useRef("");

  const reload = useCallback(async () => {
    const n = ++asked.current;
    pending.current++;
    try {
      const d = await loadRef.current();
      if (n > shown.current) {
        shown.current = n;
        const text = JSON.stringify(d);
        if (text !== shownText.current) {
          shownText.current = text;
          setData(d);
        }
        setError(undefined);
        setErrorStatus(undefined);
      }
    } catch (err) {
      if (n > shown.current) {
        shown.current = n;
        setError(errorMessage(err));
        setErrorStatus(err instanceof ApiError ? err.status : undefined);
      }
    } finally {
      pending.current--;
    }
  }, []);

  useEffect(() => {
    void reload();
    const shownAgain = () => {
      if (!document.hidden) {
        void reload();
      }
    };
    document.addEventListener("visibilitychange", shownAgain);
    return () => document.removeEventListener("visibilitychange", shownAgain);
  }, [reload]);

  const intervalMs = typeof every === "function" ? every(data) : every;
  useEffect(() => {
    const t = window.setInterval(() => {
      if (!document.hidden && pending.current === 0) {
        void reload();
      }
    }, intervalMs);
    return () => window.clearInterval(t);
  }, [reload, intervalMs]);

  return { data, error, errorStatus, reload };
}

export type Health = "ok" | "progress" | "error" | "deleting";

function steady(r: Cluster["workers"]): boolean {
  return r.ready === r.desired && r.available === r.desired && r.upToDate === r.desired;
}

// settled tells whether nothing is changing in a cluster: available, every machine ready and up to
// date. Its page then asks again seldom; while anything moves, often.
export function settled(c: Cluster): boolean {
  return (
    health(c).state === "ok" && steady(c.controlPlane) && steady(c.workers) && (c.machines ?? []).every((m) => m.ready)
  );
}

export function health(c: Cluster): { state: Health; text: string } {
  if (c.deleting) {
    return { state: "deleting", text: "Deleting" };
  }
  if (c.available) {
    const upgrading = c.conditions.find((x) => x.type === "TopologyReconciled" && x.status !== "True");
    if (upgrading) {
      return { state: "progress", text: upgrading.reason === "ClusterUpgrading" ? "Upgrading" : "Reconciling" };
    }
    return { state: "ok", text: "Available" };
  }
  if (c.phase === "Failed") {
    return { state: "error", text: "Failed" };
  }
  // A distribution's controllers start with its first cluster, which Cluster API holds until they
  // serve and the class is ready again.
  if (c.conditions.some((x) => x.type === "TopologyReconciled" && x.reason === "ClusterClassNotReconciled")) {
    return { state: "progress", text: "Waiting for its ClusterClass" };
  }
  // Cluster API's phase reads Provisioned once the infrastructure is up, long before the cluster is.
  return { state: "progress", text: "Provisioning" };
}

// Cluster API conditions that are normal when False.
const negativePolarity = new Set(["Paused", "Deleting", "RollingOut", "Remediating", "ScalingUp", "ScalingDown"]);

// abnormal tells whether a condition is worth a look.
export function abnormal(type: string, status: string): boolean {
  return negativePolarity.has(type) ? status === "True" : status !== "True";
}

/** Status shows a state as Proxmox VE does: an icon, then the text. */
export function Status({ state, text }: { state: Health | "idle"; text: string }): JSX.Element {
  return <span className={`status status-${state}`}>{text}</span>;
}

export function StatusBadge({ cluster }: { cluster: Cluster }): JSX.Element {
  const h = health(cluster);
  return <Status state={h.state} text={h.text} />;
}

const msPerSecond = 1000;
const secondsPerMinute = 60;
const secondsPerHour = 3600;
const secondsPerDay = 86_400;

export function age(iso: string, now = Date.now()): string {
  const s = Math.max(0, Math.floor((now - new Date(iso).getTime()) / msPerSecond));
  if (s < secondsPerMinute) {
    return `${s}s`;
  }
  if (s < secondsPerHour) {
    return `${Math.floor(s / secondsPerMinute)}m`;
  }
  if (s < secondsPerDay) {
    return `${Math.floor(s / secondsPerHour)}h`;
  }
  return `${Math.floor(s / secondsPerDay)}d`;
}

const byteUnits = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
const bytesPerUnit = 1024;

export function bytes(n: number): string {
  let value = n;
  let i = 0;
  while (value >= bytesPerUnit && i < byteUnits.length - 1) {
    value /= bytesPerUnit;
    i++;
  }
  return `${value.toFixed(i === 0 ? 0 : 1)} ${byteUnits[i] ?? ""}`;
}

/** sentence turns an API message, written lowercase and unpunctuated like a Go error, into one. */
export function sentence(message: string): string {
  const s = message.trim();
  if (!s) {
    return s;
  }
  const capital = s.charAt(0).toUpperCase() + s.slice(1);
  return /[.!?…:]$/u.test(capital) ? capital : `${capital}.`;
}

export function ErrorBox({ message }: { message?: string | undefined }): JSX.Element | null {
  if (message === undefined || message === "") {
    return null;
  }
  return (
    <div className="error" role="alert">
      {sentence(message)}
    </div>
  );
}

// Some browsers read a download's URL only after click returns; revoking it at once can cancel it.
const revokeDownloadAfterMs = 60_000;

/** saveFile hands a downloaded file to the browser to save under name. */
export function saveFile(blob: Blob, name: string): void {
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  document.body.append(a);
  a.click();
  a.remove();
  window.setTimeout(() => URL.revokeObjectURL(url), revokeDownloadAfterMs);
}

/** browserZone is the browser's IANA time zone, UTC when it names none. */
export const browserZone = (): string => new Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";

/** timeZones lists the IANA time zones, with UTC and the current one, which the browser may not list. */
export function timeZones(current: string): string[] {
  return [...new Set(["UTC", current, ...Intl.supportedValuesOf("timeZone")])];
}

// FieldAria ties a control to its field's message.
export interface FieldAria {
  "aria-invalid"?: boolean;
  "aria-describedby"?: string;
}

// withAria gives a native control the attributes that tie it to its hint and message; the render
// prop may pass them on itself for anything else.
function withAria(control: ReactNode, aria: FieldAria): ReactNode {
  if (isValidElement<FieldAria>(control) && typeof control.type === "string") {
    return cloneElement(control, aria);
  }
  return control;
}

// shows tells whether React renders anything for a node.
function shows(node: ReactNode): boolean {
  return node !== undefined && node !== null && typeof node !== "boolean" && node !== "";
}

// Field labels one control; the control is the single child and gets the generated id, and with an
// error the attributes that tie it to the message shown under it.
export function Field({
  label,
  hint,
  error,
  children,
}: {
  label: string;
  hint?: ReactNode;
  error?: string | undefined;
  children: (id: string, aria: FieldAria) => ReactNode;
}): JSX.Element {
  const id = useId();
  const message = error ?? "";
  const hinted = shows(hint);
  const describedBy = [message === "" ? "" : `${id}-error`, hinted ? `${id}-hint` : ""].filter(Boolean).join(" ");
  const aria: FieldAria = {};
  if (message !== "") {
    aria["aria-invalid"] = true;
  }
  if (describedBy !== "") {
    aria["aria-describedby"] = describedBy;
  }
  return (
    <div className={message === "" ? "field" : "field invalid"}>
      <label htmlFor={id}>{label}</label>
      <div className="field-control">
        {withAria(children(id, aria), aria)}
        {message !== "" && (
          <div className="field-error" id={`${id}-error`}>
            {message}
          </div>
        )}
        {hinted && (
          <div className="hint" id={`${id}-hint`}>
            {hint}
          </div>
        )}
      </div>
    </div>
  );
}

/** Choice picks one of a list, starting at none; an optional one can be left to the default. */
export function Choice({
  id,
  value,
  options,
  optional,
  onChoose,
}: {
  id: string;
  value: string;
  options: string[];
  // What leaving it unchosen means, for a choice that may be left.
  optional?: string;
  onChoose: (v: string) => void;
}): JSX.Element {
  return (
    <select id={id} value={value} onChange={(e) => onChoose(e.target.value)}>
      <option value="" disabled={optional === undefined}>
        {optional ?? "Choose…"}
      </option>
      {options.map((o) => (
        <option key={o} value={o}>
          {o}
        </option>
      ))}
    </select>
  );
}

/** How many of a log's last lines show: about a screen of them. */
export const logLines = 60;

// firstField is the dialog body's first control that takes the focus; of a radio group, the chosen
// radio, where the arrow keys move from.
function firstField(d: HTMLDialogElement): HTMLElement | null {
  const first = d.querySelector<HTMLElement>(
    ".dialog-body :is(input, select, textarea, button):not(:disabled, [type='hidden'])",
  );
  if (first instanceof HTMLInputElement && first.type === "radio") {
    const group = [...d.querySelectorAll<HTMLInputElement>("input[type='radio']")].filter((r) => r.name === first.name);
    return group.find((r) => r.checked) ?? first;
  }
  return first;
}

// Dialog is a modal window. Its body and actions are a form: Enter submits it through onSubmit,
// which the primary action triggers as the form's submit button. Closing it, by Cancel, Escape or
// the close button, gives the focus back to what had it before it opened.
export function Dialog({
  title,
  onClose,
  onSubmit,
  children,
  actions,
}: {
  title: string;
  onClose: () => void;
  onSubmit?: () => void;
  children: ReactNode;
  actions: ReactNode;
}): JSX.Element {
  const ref = useRef<HTMLDialogElement>(null);
  const titleId = useId();
  useLayoutEffect(() => {
    const opener = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const d = ref.current;
    if (d && !d.open) {
      d.showModal();
      // showModal focuses the header's close button, the first control; the work starts at the body's.
      firstField(d)?.focus();
    }
    return () => opener?.focus();
  }, []);
  return (
    <dialog
      ref={ref}
      className="dialog"
      aria-labelledby={titleId}
      onCancel={(e) => {
        e.preventDefault();
        onClose();
      }}
    >
      <header className="dialog-title">
        <h2 id={titleId}>{title}</h2>
        <button type="button" className="dialog-close" aria-label="Close" onClick={onClose}>
          ×
        </button>
      </header>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          onSubmit?.();
        }}
      >
        <div className="dialog-body">{children}</div>
        <footer className="dialog-actions">{actions}</footer>
      </form>
    </dialog>
  );
}

// useChange sends one change and closes the dialog when it is accepted.
export function useChange(
  c: Cluster,
  onClose: () => void,
): { busy: boolean; error: string | undefined; send: (req: ChangeRequest) => Promise<void> } {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const send = async (req: ChangeRequest): Promise<void> => {
    setBusy(true);
    setError(undefined);
    try {
      await api.change(c.namespace, c.name, req);
      onClose();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return { busy, error, send };
}
