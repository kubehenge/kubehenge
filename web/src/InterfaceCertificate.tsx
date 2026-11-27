import { Fragment, useEffect, useId, useRef, useState, type ChangeEvent, type JSX, type ReactNode } from "react";

import { ApiError, api } from "./api";
import { daysLeft, runsOutSoon } from "./certificate-expiry";
import { Acme } from "./InterfaceAcme";
import type {
  ACMEView,
  ApplianceCA,
  InterfaceCertificate as Certificate,
  InterfaceCertificateReplaced as Replaced,
  InterfaceCertificateSource,
} from "./types";
import {
  Dialog,
  ErrorBox,
  Field,
  Skeleton,
  Status,
  bytes,
  errorMessage,
  filled,
  pollIdle,
  saveFile,
  usePoll,
  ReadError,
} from "./ui";

// The server's limits of an upload.
const maxFiles = 8;
const kibibyte = 1024;
const maxFileBytes = kibibyte * kibibyte;

const sources: Record<InterfaceCertificateSource, string> = {
  installer: "Installer",
  uploaded: "Uploaded",
  "self-signed": "Self-signed",
  "appliance-ca": "Appliance CA",
  acme: "ACME",
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

/** dnsNameOf is the host of a URL when it is a DNS name, and undefined for an IP address. */
function dnsNameOf(url: string | undefined): string | undefined {
  const host = hostOf(url ?? "");
  // An IPv6 host has colons, and an IPv4 one no letters, as no top-level domain is all digits.
  return host.includes(":") || !/[a-z]/iu.test(host) ? undefined : host;
}

/** Warnings are the facts about a certificate that an administrator has to act on, one line each. */
function Warnings({ c }: { c: Certificate }): JSX.Element | null {
  const ends = new Date(c.notAfter);
  const days = daysLeft(ends);
  const lines = [
    runsOutSoon(ends) &&
      (days < 0 ? `Expired on ${dateOf(c.notAfter)}` : `Expires in ${days === 0 ? "under a day" : `${days} days`}`),
    c.incomplete && `Chain incomplete: missing ${c.missingIssuer ?? "an issuer"}`,
    c.selfSigned && "Self-signed, so browsers warn",
    filled(c.trustedBy) && `Browsers warn unless they trust ${c.trustedBy}`,
    c.publicNameResolves === false && `${hostOf(c.publicURL ?? "")} does not resolve to this appliance`,
    c.publicNameCovered === false && `Not valid for ${hostOf(c.publicURL ?? "")}, so browsers warn`,
    filled(c.notServed) && `Stored certificate not served: ${c.notServed}`,
  ].filter((l): l is string => typeof l === "string");
  if (lines.length === 0) {
    return null;
  }
  return (
    <div className="warning warning-lines">
      {lines.map((l) => (
        <p key={l}>{l}</p>
      ))}
    </div>
  );
}

/** Copy puts text on the clipboard. */
function Copy({ text, label }: { text: string; label: string }): JSX.Element {
  const [copied, setCopied] = useState(false);
  const copy = async (): Promise<void> => {
    await navigator.clipboard.writeText(text);
    setCopied(true);
  };
  return (
    <button
      type="button"
      onClick={() => {
        void copy();
      }}
    >
      {copied ? "Copied" : label}
    </button>
  );
}

/** Fingerprint shows a certificate's SHA-256, which a click copies. */
function Fingerprint({ value }: { value: string }): JSX.Element {
  return (
    <>
      <span className="key fingerprint">
        {value.split(":").map((pair, i) => (
          // Pairs repeat, so a pair's place is its only identity.
          // eslint-disable-next-line react/no-array-index-key
          <Fragment key={i}>
            {i > 0 && (
              <>
                :<wbr />
              </>
            )}
            {pair}
          </Fragment>
        ))}
      </span>
      <div>
        <Copy text={value} label="Copy fingerprint" />
      </div>
    </>
  );
}

/** validFor tells whether a certificate is valid for a name or address, a wildcard covering one label. */
function validFor(c: Certificate, name: string): boolean {
  return [...(c.dnsNames ?? []), ...(c.ipAddresses ?? [])].some(
    (n) => n === name || (n.startsWith("*.") && name.slice(name.indexOf(".")) === n.slice(1)),
  );
}

/** permits tells whether the appliance CA may sign for a name or address, as its name constraints read. */
function permits(ca: ApplianceCA, name: string): boolean {
  return ca.permits.some((p) => p === name || name.endsWith(`.${p}`));
}

/** addressEffects are what changing the public address to name does besides. */
function addressEffects(c: Certificate, name: string): string[] {
  const certificate: Partial<Record<InterfaceCertificateSource, string>> = {
    "self-signed": "The self-signed certificate is made again for the new address.",
    "appliance-ca":
      c.applianceCA === undefined || permits(c.applianceCA, name)
        ? "The appliance CA signs a certificate for the new address."
        : "A new appliance CA signs for the new address. Devices warn until they trust the new CA.",
    acme: "ACME orders a certificate for the new address.",
  };
  return [
    certificate[c.source] ??
      (validFor(c, name)
        ? "The certificate stays and covers the new address."
        : "The certificate stays but does not cover the new address. Browsers warn until it is replaced."),
    "Identity providers need the new redirect URL.",
    "The Proxmox VE interface entry keeps the old address.",
  ];
}

/** typedAddress is a typed name or address as the server takes it, also when pasted as a URL. */
const typedAddress = (typed: string): string =>
  typed
    .trim()
    .toLowerCase()
    .replace(/^https?:\/\//u, "")
    .replace(/\/$/u, "")
    .replace(/\.$/u, "")
    .replace(/^\[(?<v6>.*)\]$/u, "$<v6>");

/** ChangeAddress sets the name or address users reach the interface at. */
function ChangeAddress({
  c,
  disabled,
  running,
  run,
}: {
  c: Certificate;
  disabled: boolean;
  running: boolean;
  run: (call: () => Promise<Replaced>) => Promise<unknown>;
}): JSX.Element {
  const current = typedAddress(hostOf(c.publicURL ?? ""));
  const [open, setOpen] = useState(false);
  const [name, setName] = useState(current);
  const [error, setError] = useState<string>();
  const submit = async (): Promise<void> => {
    setError(undefined);
    const failed = await run(() => api.setInterfaceAddress(name.trim()));
    if (failed === undefined) {
      setOpen(false);
    } else {
      setError(errorMessage(failed));
    }
  };
  return (
    <>
      <div>
        <button
          type="button"
          disabled={disabled}
          onClick={() => {
            setName(current);
            setError(undefined);
            setOpen(true);
          }}
        >
          {running ? "Changing…" : "Change address"}
        </button>
      </div>
      {open && (
        <Dialog
          title="Change public address"
          onClose={() => setOpen(false)}
          onSubmit={() => {
            void submit();
          }}
          actions={
            <>
              <button type="button" onClick={() => setOpen(false)}>
                Cancel
              </button>
              <button type="submit" className="primary" disabled={running || name.trim() === ""}>
                {running ? "Changing…" : "Change address"}
              </button>
            </>
          }
        >
          <Field label="Name or address" error={error}>
            {(id) => (
              <input
                id={id}
                value={name}
                autoComplete="off"
                spellCheck={false}
                onChange={(e) => setName(e.target.value)}
              />
            )}
          </Field>
          {typedAddress(name) !== "" && typedAddress(name) !== current && (
            <ul className="effects">
              {addressEffects(c, typedAddress(name)).map((e) => (
                <li key={e}>{e}</li>
              ))}
            </ul>
          )}
        </Dialog>
      )}
    </>
  );
}

/** OwnCA is the appliance's own CA, to download and trust once. */
function OwnCA({ ca }: { ca: ApplianceCA }): JSX.Element {
  const heading = useId();
  return (
    <section className="box" aria-labelledby={heading}>
      <h3 id={heading}>Appliance CA</h3>
      <dl className="summary">
        <dt>Signs for</dt>
        <dd>{ca.permits.join(", ")}</dd>
        <dt>Valid until</dt>
        <dd>{dateOf(ca.notAfter)}</dd>
        <dt>SHA-256 fingerprint</dt>
        <dd>
          <Fingerprint value={ca.fingerprint} />
        </dd>
      </dl>
      <div className="actions">
        <button
          type="button"
          onClick={() => saveFile(new Blob([ca.pem], { type: "application/x-pem-file" }), "kubehenge-appliance-ca.crt")}
        >
          Download CA
        </button>
      </div>
    </section>
  );
}

/** Facts is what the interface serves: where it came from, whom it covers and how long it holds. */
function Facts({ c, changeAddress }: { c: Certificate; changeAddress: ReactNode }): JSX.Element {
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
            {c.publicURL} {window.location.hostname !== hostOf(c.publicURL ?? "") && <a href={c.publicURL}>Open</a>}{" "}
            {c.publicNameResolves !== undefined && (
              <Status
                state={c.publicNameResolves ? "ok" : "warning"}
                text={c.publicNameResolves ? "Resolves" : "Does not resolve"}
              />
            )}
            {changeAddress}
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
  /** The appliance's container ID, for a command run on the Proxmox VE node. */
  ctid: number | undefined;
  /** The saved ACME setting, if any. */
  acme: ACMEView | undefined;
  /** Where the certificate served came from. */
  source: InterfaceCertificateSource;
  /** Whether the appliance has a CA of its own. */
  hasCA: boolean;
  /** The public address's DNS name, undefined for an IP address. */
  name: string | undefined;
}

/**
 * OnNode gives the command that signs a new certificate with the Proxmox VE cluster's CA, whose key
 * never leaves the node.
 */
function OnNode({ way: { ctid } }: { way: WayProps }): JSX.Element {
  const command = `sh install.sh --certificate --ctid ${ctid ?? "<container ID>"}`;
  return (
    <>
      <p>
        Run this on the Proxmox VE node: <code className="said">{command}</code>
      </p>
      <div className="actions">
        <Copy text={command} label="Copy command" />
      </div>
    </>
  );
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
  const picker = useRef<HTMLInputElement>(null);
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
        {(id) => (
          <div>
            <input
              id={id}
              ref={picker}
              className="visually-hidden"
              // The button opens the picker, so the input is no stop of its own.
              tabIndex={-1}
              type="file"
              multiple
              disabled={waiting || locked}
              onChange={choose}
            />
            <button type="button" disabled={waiting || locked} onClick={() => picker.current?.click()}>
              {files.length === 0 ? "Choose files" : "Add files"}
            </button>
          </div>
        )}
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
  way: { waiting, running, locked, run, source },
  of,
  action,
  busyAction,
  title,
  effects,
  call,
}: {
  way: WayProps;
  /** The source the change gives, which needs no change while it is served. */
  of: InterfaceCertificateSource;
  action: string;
  busyAction: string;
  title: string;
  /** What the change does, one sentence each. False leaves a sentence out. */
  effects: (string | false)[];
  call: () => Promise<Replaced>;
}): JSX.Element {
  const inUse = source === of;
  let label = action;
  if (running) {
    label = busyAction;
  } else if (inUse) {
    label = "In use";
  }
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
        <button
          type="button"
          className="primary"
          disabled={waiting || locked || inUse}
          onClick={() => setConfirming(true)}
        >
          {label}
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
          {effects
            .filter((e): e is string => e !== false)
            .map((e) => (
              <p key={e}>{e}</p>
            ))}
        </Dialog>
      )}
    </>
  );
}

type WayId = "upload" | "acme" | "cluster-ca" | "appliance-ca" | "self-signed" | "installer";

// What choosing another certificate does to a saved ACME setting.
const acmeRemoved = "The ACME setting and its DNS credentials are removed.";

// The ways to change the certificate. A further way is one more entry here.
const ways: { id: WayId; label: string; body: (p: WayProps) => JSX.Element }[] = [
  { id: "upload", label: "Upload files", body: (p) => <Upload way={p} /> },
  {
    id: "acme",
    label: "ACME",
    body: (p) => (
      <Acme view={p.acme} name={p.name} waiting={p.waiting} running={p.running} locked={p.locked} run={p.run} />
    ),
  },
  { id: "cluster-ca", label: "Proxmox VE CA", body: (p) => <OnNode way={p} /> },
  {
    id: "appliance-ca",
    label: "Appliance CA",
    body: (p) => (
      <Confirmed
        way={p}
        of="appliance-ca"
        action="Use appliance CA"
        busyAction="Signing…"
        title="Use appliance CA"
        effects={[
          "Browsers accept the certificate once they trust the appliance CA, which signs only for this appliance's names and addresses.",
          p.acme !== undefined && acmeRemoved,
        ]}
        call={api.signInterfaceCertificateByApplianceCA}
      />
    ),
  },
  {
    id: "self-signed",
    label: "Self-signed",
    body: (p) => (
      <Confirmed
        way={p}
        of="self-signed"
        action="Make self-signed"
        busyAction="Making…"
        title="Make self-signed certificate"
        effects={["Browsers warn about a self-signed certificate.", p.acme !== undefined && acmeRemoved]}
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
        of="installer"
        action="Use installer's"
        busyAction="Switching…"
        title="Use installer's certificate"
        effects={[
          "The installer's certificate replaces the current one.",
          p.hasCA && "The appliance CA and its key are deleted.",
          p.acme !== undefined && acmeRemoved,
        ]}
        call={api.resetInterfaceCertificate}
      />
    ),
  },
];

/**
 * InterfaceCertificate shows the certificate the interface serves, and changes it: files an
 * administrator gives, one the cluster's CA signs on the node, a new self-signed certificate, or the
 * one the installer made. onChanged lets the rest of the interface drop its warning about the
 * certificate it replaced.
 */
export function InterfaceCertificate({
  onChanged,
  ctid,
}: {
  onChanged?: () => void;
  ctid?: number | undefined;
}): JSX.Element {
  const read = usePoll(api.interfaceCertificate, pollIdle);
  const { data, error, put } = read;
  const [chosenWay, setChosenWay] = useState<WayId>();
  const [running, setRunning] = useState<WayId | "address">();
  const [leftOut, setLeftOut] = useState<string[]>([]);
  const name = useId();

  if (!data) {
    return (
      <div className="box">
        <h3>Interface certificate</h3>
        <ReadError read={read} />
        {!filled(error) && <Skeleton rows={4} />}
      </div>
    );
  }
  const locked = !data.changeable;
  const waiting = running !== undefined;
  // A certificate from ACME opens on its setting, any other on an upload.
  const way = chosenWay ?? (data.acme === undefined ? "upload" : "acme");
  const runFor =
    (id: WayId | "address") =>
    async (call: () => Promise<Replaced>): Promise<unknown> => {
      setRunning(id);
      try {
        const { leftOut: left = [], ...view } = await call();
        setLeftOut(left);
        put(() => view);
        onChanged?.();
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
        <Facts
          c={data}
          changeAddress={
            <ChangeAddress
              c={data}
              disabled={waiting || locked}
              running={running === "address"}
              run={runFor("address")}
            />
          }
        />
        {leftOut.length > 0 && <p className="muted">Left out: {leftOut.join(", ")}</p>}
        <ReadError read={read} />
      </div>
      {data.applianceCA !== undefined && <OwnCA ca={data.applianceCA} />}
      <div className="box fields">
        <h3>Change certificate</h3>
        {locked && <p className="muted">This interface cannot keep changes</p>}
        <fieldset className="choices" disabled={waiting || locked}>
          <legend>New source</legend>
          {ways.map((w) => (
            <label key={w.id} className="check">
              <input type="radio" name={name} value={w.id} checked={way === w.id} onChange={() => setChosenWay(w.id)} />{" "}
              {w.label}
            </label>
          ))}
        </fieldset>
        {current?.body({
          waiting,
          running: running === current.id,
          locked,
          run: runFor(current.id),
          ctid,
          acme: data.acme,
          source: data.source,
          hasCA: data.applianceCA !== undefined,
          name: dnsNameOf(data.publicURL),
        })}
      </div>
    </>
  );
}
