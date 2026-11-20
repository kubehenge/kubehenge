import { useEffect, useId, useRef, useState, type ChangeEvent, type JSX } from "react";

import { ApiError, api } from "./api";
import { daysLeft, runsOutSoon } from "./certificate-expiry";
import type {
  InterfaceCertificate as Certificate,
  InterfaceCertificateReplaced as Replaced,
  InterfaceCertificateSource,
} from "./types";
import { Dialog, ErrorBox, Field, Skeleton, Status, bytes, errorMessage, filled, pollIdle, usePoll } from "./ui";

// The server's limits of an upload.
const maxFiles = 8;
const kibibyte = 1024;
const maxFileBytes = kibibyte * kibibyte;

const sources: Record<InterfaceCertificateSource, string> = {
  installer: "Installer",
  uploaded: "Uploaded",
  "self-signed": "Self-signed",
};

const dateOf = (iso: string): string => new Date(iso).toLocaleDateString(undefined, { dateStyle: "long" });

/** daysText says how long a certificate has, from its last valid day. */
function daysText(notAfter: string): string {
  const days = daysLeft(new Date(notAfter));
  if (days < 0) {
    return "Expired";
  }
  if (days === 0) {
    return "Under a day left";
  }
  return `${days} ${days === 1 ? "day" : "days"} left`;
}

/** hostOf is the name or address of a URL, or the text when it is none. */
function hostOf(url: string): string {
  try {
    return new URL(url).hostname;
  } catch {
    return url;
  }
}

/** Warnings are the facts about a certificate that an administrator has to act on, one line each. */
function Warnings({ c }: { c: Certificate }): JSX.Element {
  const ends = new Date(c.notAfter);
  const days = daysLeft(ends);
  return (
    <>
      {runsOutSoon(ends) && (
        <p className="warning">
          {days < 0 ? `Expired on ${dateOf(c.notAfter)}` : `Expires in ${days === 0 ? "under a day" : `${days} days`}`}
        </p>
      )}
      {c.incomplete && <p className="warning">Chain incomplete, missing {c.missingIssuer ?? "an issuer"}</p>}
      {c.selfSigned && <p className="warning">Self-signed, so browsers warn</p>}
      {c.publicNameResolves === false && (
        <p className="warning">{hostOf(c.publicURL ?? "")} does not resolve to this appliance</p>
      )}
      {filled(c.notServed) && <p className="warning">Stored certificate not served, {c.notServed}</p>}
    </>
  );
}

/** Fingerprint shows a certificate's SHA-256, which a click copies. */
function Fingerprint({ value }: { value: string }): JSX.Element {
  const [copied, setCopied] = useState(false);
  const copy = async (): Promise<void> => {
    await navigator.clipboard.writeText(value);
    setCopied(true);
  };
  return (
    <>
      <span className="key">{value}</span>
      <div>
        <button
          type="button"
          onClick={() => {
            void copy();
          }}
        >
          {copied ? "Copied" : "Copy fingerprint"}
        </button>
      </div>
    </>
  );
}

/** Facts is what the interface serves: where it came from, whom it covers and how long it holds. */
function Facts({ c }: { c: Certificate }): JSX.Element {
  const covers = [...(c.dnsNames ?? []), ...(c.ipAddresses ?? [])];
  return (
    <dl className="summary">
      <dt>Source</dt>
      <dd>{sources[c.source]}</dd>
      <dt>Covers</dt>
      <dd>{covers.join(", ")}</dd>
      <dt>Issuer</dt>
      <dd>{c.issuer}</dd>
      <dt>Valid until</dt>
      <dd>
        {dateOf(c.notAfter)} <span className="muted">{daysText(c.notAfter)}</span>
      </dd>
      <dt>SHA-256 fingerprint</dt>
      <dd>
        <Fingerprint value={c.fingerprint} />
      </dd>
      <dt>Chain</dt>
      <dd>
        <ol className="chain">
          {c.chain.map((subject, i) => (
            // A subject can repeat, as when a root is cross-signed, so its turn is part of the key.
            <li key={`${subject}#${c.chain.slice(0, i).filter((s) => s === subject).length}`}>{subject}</li>
          ))}
        </ol>
      </dd>
      {filled(c.publicURL) && (
        <>
          <dt>Public address</dt>
          <dd>
            {c.publicURL}{" "}
            {c.publicNameResolves !== undefined && (
              <Status
                state={c.publicNameResolves ? "ok" : "warning"}
                text={c.publicNameResolves ? "Resolves" : "Does not resolve"}
              />
            )}
          </dd>
        </>
      )}
    </dl>
  );
}

// filesProblem is what is wrong with the chosen files before any is sent, by the server's own rule.
function filesProblem(files: File[]): string | undefined {
  if (files.length > maxFiles) {
    return `At most ${maxFiles} files. Remove some`;
  }
  const large = files.find((f) => f.size > maxFileBytes);
  return large === undefined ? undefined : `File ${large.name} is over 1 MiB. Remove it`;
}

const sameFile = (a: File, b: File): boolean =>
  a.name === b.name && a.size === b.size && a.lastModified === b.lastModified;

/** What one way of changing the certificate is given by the page. */
interface WayProps {
  /** Whether a change is under way, which every action waits for. */
  waiting: boolean;
  /** Whether the way is the one running. */
  running: boolean;
  /** Whether the interface cannot keep a change. */
  locked: boolean;
  /** run makes the change and returns what went wrong with it, if anything. */
  run: (call: () => Promise<Replaced>) => Promise<unknown>;
}

/** Upload takes the certificate, its key and its chain as files, in any format. */
function Upload({ way: { waiting, running, locked, run } }: { way: WayProps }): JSX.Element {
  const [files, setFiles] = useState<File[]>([]);
  const [passphrase, setPassphrase] = useState("");
  // The passphrase field shows once a refusal asks for one, and stays for the next send.
  const [asked, setAsked] = useState(false);
  const [passphraseError, setPassphraseError] = useState<string>();
  const [error, setError] = useState<string>();
  const [names, setNames] = useState<string[]>([]);
  const problem = filesProblem(files);
  const passphraseInput = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (asked) {
      passphraseInput.current?.focus();
    }
  }, [asked]);

  const choose = (e: ChangeEvent<HTMLInputElement>): void => {
    const chosen = [...(e.target.files ?? [])];
    // The same file may be chosen again after it was removed.
    e.target.value = "";
    setFiles((was) => [...was, ...chosen.filter((f) => !was.some((w) => sameFile(w, f)))]);
  };
  const send = async (): Promise<void> => {
    setError(undefined);
    setNames([]);
    setPassphraseError(undefined);
    const failed = await run(async () => api.replaceInterfaceCertificate(files, passphrase));
    if (failed === undefined) {
      setFiles([]);
      setPassphrase("");
      setAsked(false);
    } else if (failed instanceof ApiError && failed.code === "passphrase") {
      setAsked(true);
      setPassphraseError(failed.message);
    } else {
      setError(errorMessage(failed));
      setNames(failed instanceof ApiError ? failed.names : []);
    }
  };
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        void send();
      }}
    >
      <Field label="Certificate files" hint="Certificate, key and chain, in any format">
        {(id) => <input id={id} type="file" multiple disabled={waiting || locked} onChange={choose} />}
      </Field>
      {files.length > 0 && (
        <ul className="files" aria-label="Chosen files">
          {files.map((f) => (
            <li key={`${f.name}:${f.size}:${f.lastModified}`}>
              <span className="file-name">{f.name}</span> <span className="muted">{bytes(f.size)}</span>
              <button
                type="button"
                className="file-remove"
                aria-label={`Remove ${f.name}`}
                disabled={waiting}
                onClick={() => setFiles((was) => was.filter((w) => w !== f))}
              >
                Remove
              </button>
            </li>
          ))}
        </ul>
      )}
      {asked && (
        <Field label="Passphrase" error={passphraseError}>
          {(id) => (
            <input
              id={id}
              ref={passphraseInput}
              type="password"
              autoComplete="off"
              value={passphrase}
              disabled={waiting}
              onChange={(e) => setPassphrase(e.target.value)}
            />
          )}
        </Field>
      )}
      <ErrorBox message={problem ?? error} />
      {names.length > 0 && <p className="muted">Covers: {names.join(", ")}</p>}
      <div className="actions">
        <button
          type="submit"
          className="primary"
          disabled={waiting || locked || files.length === 0 || problem !== undefined}
        >
          {running ? "Replacing…" : "Replace"}
        </button>
      </div>
    </form>
  );
}

/**
 * Confirmed is a way that changes the certificate with one click, once a dialog has said what
 * the change does.
 */
function Confirmed({
  way: { waiting, running, locked, run },
  action,
  busyAction,
  title,
  effect,
  call,
}: {
  way: WayProps;
  action: string;
  busyAction: string;
  title: string;
  effect: string;
  call: () => Promise<Replaced>;
}): JSX.Element {
  const [confirming, setConfirming] = useState(false);
  const [error, setError] = useState<string>();
  const go = async (): Promise<void> => {
    setError(undefined);
    const failed = await run(call);
    if (failed !== undefined) {
      setError(errorMessage(failed));
    }
  };
  return (
    <>
      <ErrorBox message={error} />
      <div className="actions">
        <button type="button" className="primary" disabled={waiting || locked} onClick={() => setConfirming(true)}>
          {running ? busyAction : action}
        </button>
      </div>
      {confirming && (
        <Dialog
          title={title}
          onClose={() => setConfirming(false)}
          onSubmit={() => {
            setConfirming(false);
            void go();
          }}
          actions={
            <>
              <button type="button" onClick={() => setConfirming(false)}>
                Cancel
              </button>
              <button type="submit" className="primary">
                {action}
              </button>
            </>
          }
        >
          <p>{effect}</p>
        </Dialog>
      )}
    </>
  );
}

type WayId = "upload" | "self-signed" | "installer";

// The ways to change the certificate. A further way is one more entry here.
const ways: { id: WayId; label: string; body: (p: WayProps) => JSX.Element }[] = [
  { id: "upload", label: "Upload files", body: (p) => <Upload way={p} /> },
  {
    id: "self-signed",
    label: "Self-signed",
    body: (p) => (
      <Confirmed
        way={p}
        action="Make self-signed"
        busyAction="Making…"
        title="Make self-signed certificate"
        effect="Browsers warn about a self-signed certificate."
        call={api.makeInterfaceCertificateSelfSigned}
      />
    ),
  },
  {
    id: "installer",
    label: "Installer's",
    body: (p) => (
      <Confirmed
        way={p}
        action="Use installer's"
        busyAction="Switching…"
        title="Use installer's certificate"
        effect="The certificate made at install replaces the current one."
        call={api.resetInterfaceCertificate}
      />
    ),
  },
];

/**
 * InterfaceCertificate shows the certificate the interface serves, and changes it: files an
 * administrator gives, a new self-signed certificate, or the one the installer made.
 */
export function InterfaceCertificate(): JSX.Element {
  const { data, error, put } = usePoll(api.interfaceCertificate, pollIdle);
  const [way, setWay] = useState<WayId>("upload");
  const [running, setRunning] = useState<WayId>();
  const [leftOut, setLeftOut] = useState<string[]>([]);
  const name = useId();

  if (!data) {
    return (
      <div className="box">
        <h3>Interface certificate</h3>
        {filled(error) ? <ErrorBox message={error} /> : <Skeleton rows={4} />}
      </div>
    );
  }
  const locked = !data.changeable;
  const waiting = running !== undefined;
  const runFor =
    (id: WayId) =>
    async (call: () => Promise<Replaced>): Promise<unknown> => {
      setRunning(id);
      try {
        const { leftOut: left = [], ...view } = await call();
        setLeftOut(left);
        put(() => view);
        return undefined;
      } catch (err) {
        return err;
      } finally {
        setRunning(undefined);
      }
    };
  const current = ways.find((w) => w.id === way);
  return (
    <>
      <div className="box">
        <h3>Interface certificate</h3>
        <Warnings c={data} />
        <Facts c={data} />
        {leftOut.length > 0 && <p className="muted">Left out: {leftOut.join(", ")}</p>}
        <ErrorBox message={error} />
      </div>
      <div className="box fields">
        <h3>Change certificate</h3>
        {locked && <p className="muted">No key directory, so changes cannot be kept</p>}
        <fieldset className="choices" disabled={waiting || locked}>
          <legend>New source</legend>
          {ways.map((w) => (
            <label key={w.id} className="check">
              <input type="radio" name={name} value={w.id} checked={way === w.id} onChange={() => setWay(w.id)} />{" "}
              {w.label}
            </label>
          ))}
        </fieldset>
        {current?.body({ waiting, running: running === current.id, locked, run: runFor(current.id) })}
      </div>
    </>
  );
}
