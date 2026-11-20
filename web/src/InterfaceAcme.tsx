import { Fragment, useEffect, useId, useState, type JSX } from "react";

import { api } from "./api";
import { Head } from "./NodeSettings";
import type { ACMEProof, ACMERequest, ACMEView, InterfaceCertificateReplaced as Replaced } from "./types";
import { ErrorBox, Field, Status, errorMessage, filled, type Health } from "./ui";
import { oneOf } from "./values";

const letsEncrypt = "letsencrypt";
const staging = "letsencrypt-staging";
const otherServer = "other";

const proofs: { value: ACMEProof; label: string }[] = [
  { value: "tls-alpn-01", label: "TLS on port 443" },
  { value: "http-01", label: "HTTP on port 80" },
  { value: "dns-01", label: "DNS record" },
];
const proofIds = proofs.map((p) => p.value);

const timeOf = (iso: string): string =>
  new Date(iso).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });

const states: Record<ACMEView["state"], { health: Health; text: string }> = {
  ordering: { health: "progress", text: "Ordering" },
  issued: { health: "ok", text: "Issued" },
  failing: { health: "error", text: "Failing" },
};

/** AcmeState is how the ACME order stands: its state, its error and when it acts next. */
export function AcmeState({ v }: { v: ACMEView }): JSX.Element {
  const state = v.givenUp ? { health: "error" as const, text: "Stopped after a day" } : states[v.state];
  const times: [string, string | undefined][] = [
    ["Next try", v.nextAttempt],
    ["Last issued", v.lastSuccess],
    ["Renews", v.nextRenewal],
  ];
  return (
    <dl className="summary acme-state">
      <dt>ACME</dt>
      <dd>
        <Status state={state.health} text={state.text} />
      </dd>
      {filled(v.error) && (
        <>
          <dt>Error</dt>
          <dd>{v.error}</dd>
        </>
      )}
      {times.map(
        ([label, at]) =>
          filled(at) && (
            // The list is a grid of its terms and details, so a row has no element of its own.
            <Fragment key={label}>
              <dt>{label}</dt>
              <dd>{timeOf(at)}</dd>
            </Fragment>
          ),
      )}
    </dl>
  );
}

interface Credential {
  name: string;
  value: string;
}

/** The form's fields, filled from the saved setting where there is one. */
interface Form {
  server: string;
  url: string;
  ca: string;
  email: string;
  proof: ACMEProof;
  provider: string;
  credentials: Credential[];
  resolvers: string;
  keyID: string;
  hmacKey: string;
  profile: string;
}

const emptyCredential: Credential = { name: "", value: "" };

function serverOf(dir: string): { server: string; url: string } {
  return dir === letsEncrypt || dir === staging ? { server: dir, url: "" } : { server: otherServer, url: dir };
}

function formOf(v: ACMEView | undefined): Form {
  if (v === undefined) {
    return {
      server: letsEncrypt,
      url: "",
      ca: "",
      email: "",
      proof: "tls-alpn-01",
      provider: "",
      credentials: [emptyCredential],
      resolvers: "",
      keyID: "",
      hmacKey: "",
      profile: "",
    };
  }
  const saved = v.dnsCredentialsSet.map((name) => ({ name, value: "" }));
  return {
    ...serverOf(v.directory),
    ca: "",
    email: v.email ?? "",
    proof: v.proof,
    provider: v.dnsProvider ?? "",
    credentials: saved.length > 0 ? saved : [emptyCredential],
    resolvers: v.resolvers.join(", "),
    keyID: v.externalAccountBinding?.keyID ?? "",
    hmacKey: "",
    profile: v.profile ?? "",
  };
}

function requestOf(f: Form): ACMERequest {
  const req: ACMERequest = { directory: f.server === otherServer ? f.url.trim() : f.server, proof: f.proof };
  const set = (key: "email" | "profile" | "directoryCA", value: string): void => {
    if (value.trim() !== "") {
      req[key] = value.trim();
    }
  };
  set("email", f.email);
  set("profile", f.profile);
  if (f.server === otherServer) {
    set("directoryCA", f.ca);
  }
  if (f.keyID.trim() !== "") {
    req.externalAccountBinding = { keyID: f.keyID.trim(), hmacKey: f.hmacKey.trim() };
  }
  if (f.proof === "dns-01") {
    req.dnsProvider = f.provider.trim();
    req.dnsCredentials = Object.fromEntries(
      f.credentials.filter((c) => c.name.trim() !== "").map((c) => [c.name.trim(), c.value]),
    );
    const resolvers = f.resolvers.split(/[\s,]+/u).filter(Boolean);
    if (resolvers.length > 0) {
      req.resolvers = resolvers;
    }
  }
  return req;
}

/** Credentials are the DNS provider's variables, a name and a value each. */
function Credentials({
  rows,
  saved,
  disabled,
  onChange,
}: {
  rows: Credential[];
  saved: string[];
  disabled: boolean;
  onChange: (rows: Credential[]) => void;
}): JSX.Element {
  const change = (i: number, row: Credential): void => onChange(rows.map((r, j) => (j === i ? row : r)));
  return (
    <fieldset className="group">
      <legend>Provider variables</legend>
      <div className="node-rows">
        <Head parts={["Name", "Value"]} />
        {rows.map((row, i) => (
          // oxlint-disable-next-line react/no-array-index-key -- rows are edited in place
          <fieldset key={i} aria-label={`Variable ${i + 1}`} className="bare node-row">
            <input
              aria-label="Name"
              value={row.name}
              disabled={disabled}
              autoComplete="off"
              spellCheck={false}
              onChange={(e) => change(i, { ...row, name: e.target.value })}
            />
            <input
              aria-label="Value"
              type="password"
              value={row.value}
              placeholder={saved.includes(row.name) ? "Saved" : undefined}
              disabled={disabled}
              autoComplete="off"
              onChange={(e) => change(i, { ...row, value: e.target.value })}
            />
            <button
              type="button"
              aria-label={`Remove variable ${i + 1}`}
              disabled={disabled || rows.length === 1}
              onClick={() => onChange(rows.filter((_, j) => j !== i))}
            >
              Remove
            </button>
          </fieldset>
        ))}
      </div>
      <button type="button" disabled={disabled} onClick={() => onChange([...rows, emptyCredential])}>
        Add variable
      </button>
    </fieldset>
  );
}

/** useProviders lists lego's DNS providers once the DNS proof is chosen. */
function useProviders(wanted: boolean): string[] {
  const [providers, setProviders] = useState<string[]>();
  useEffect(() => {
    let live = true;
    const load = async (): Promise<void> => {
      let names: string[] = [];
      try {
        const listed = await api.acmeProviders();
        names = listed.providers;
      } catch {
        // Without the list the field still takes any name, and the server checks it.
      }
      if (live) {
        setProviders(names);
      }
    };
    if (wanted && providers === undefined) {
      void load();
    }
    return () => {
      live = false;
    };
  }, [wanted, providers]);
  return providers ?? [];
}

/** What the page gives the ACME way. */
interface AcmeProps {
  view: ACMEView | undefined;
  waiting: boolean;
  running: boolean;
  locked: boolean;
  run: (call: () => Promise<Replaced>) => Promise<unknown>;
}

/**
 * Acme gets the certificate from Let's Encrypt or another ACME server, proved on port 443, port 80 or
 * by a DNS record, and orders it again on request.
 */
export function Acme({ view, waiting, running, locked, run }: AcmeProps): JSX.Element {
  const [f, setF] = useState<Form>(() => formOf(view));
  const [error, setError] = useState<string>();
  const [ordering, setOrdering] = useState(false);
  const listId = useId();
  const providers = useProviders(f.proof === "dns-01");
  const edit = (over: Partial<Form>): void => setF((was) => ({ ...was, ...over }));
  const off = waiting || locked;

  const save = async (): Promise<void> => {
    setError(undefined);
    const failed = await run(async () => api.setInterfaceACME(requestOf(f)));
    if (failed === undefined) {
      // The secrets sent are kept by the server now, and the fields say so.
      setF((was) => ({
        ...was,
        hmacKey: "",
        ca: "",
        credentials: was.credentials.map((c) => ({ ...c, value: "" })),
      }));
    } else {
      setError(errorMessage(failed));
    }
  };
  const orderNow = async (): Promise<void> => {
    setError(undefined);
    setOrdering(true);
    const failed = await run(api.orderInterfaceACME);
    setOrdering(false);
    if (failed !== undefined) {
      setError(errorMessage(failed));
    }
  };
  let saveLabel = view === undefined ? "Use ACME" : "Save";
  if (running && !ordering) {
    saveLabel = "Saving…";
  }
  const provider = f.provider.trim();
  const caSaved = view?.directoryCASet === true && f.ca === "";
  const hmacSaved = view?.externalAccountBinding?.hmacKeySet === true && f.hmacKey === "";

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        void save();
      }}
    >
      {view !== undefined && <AcmeState v={view} />}
      <Field label="ACME server">
        {(id) => (
          <select id={id} value={f.server} disabled={off} onChange={(e) => edit({ server: e.target.value })}>
            <option value={letsEncrypt}>Let&apos;s Encrypt</option>
            <option value={staging}>Let&apos;s Encrypt staging</option>
            <option value={otherServer}>Other</option>
          </select>
        )}
      </Field>
      {f.server === otherServer && (
        <>
          <Field label="Directory URL" hint="https://ca.example.org/acme/directory">
            {(id) => (
              <input
                id={id}
                type="url"
                value={f.url}
                disabled={off}
                spellCheck={false}
                onChange={(e) => edit({ url: e.target.value })}
              />
            )}
          </Field>
          <Field label="Server CA" hint={caSaved ? "Saved, empty keeps it" : "PEM, when browsers distrust the server"}>
            {(id) => (
              <textarea
                id={id}
                rows={4}
                value={f.ca}
                disabled={off}
                spellCheck={false}
                onChange={(e) => edit({ ca: e.target.value })}
              />
            )}
          </Field>
        </>
      )}
      <Field label="Contact e-mail">
        {(id) => (
          <input
            id={id}
            type="email"
            value={f.email}
            disabled={off}
            autoComplete="email"
            onChange={(e) => edit({ email: e.target.value })}
          />
        )}
      </Field>
      <Field label="Proof">
        {(id) => (
          <select
            id={id}
            value={f.proof}
            disabled={off}
            onChange={(e) => {
              if (oneOf(proofIds, e.target.value)) {
                edit({ proof: e.target.value });
              }
            }}
          >
            {proofs.map((p) => (
              <option key={p.value} value={p.value}>
                {p.label}
              </option>
            ))}
          </select>
        )}
      </Field>
      {f.proof === "dns-01" && (
        <>
          <Field
            label="DNS provider"
            hint={
              provider === "" ? undefined : (
                <a
                  href={`https://go-acme.github.io/lego/dns/${encodeURIComponent(provider)}/`}
                  target="_blank"
                  rel="noreferrer"
                >
                  Its variables
                </a>
              )
            }
          >
            {(id) => (
              <input
                id={id}
                list={listId}
                value={f.provider}
                disabled={off}
                autoComplete="off"
                spellCheck={false}
                onChange={(e) => edit({ provider: e.target.value })}
              />
            )}
          </Field>
          <datalist id={listId}>
            {providers.map((p) => (
              <option key={p} value={p}>
                {p}
              </option>
            ))}
          </datalist>
          <Credentials
            rows={f.credentials}
            saved={view?.dnsProvider === provider ? view.dnsCredentialsSet : []}
            disabled={off}
            onChange={(credentials) => edit({ credentials })}
          />
          <Field label="DNS resolvers" hint="192.0.2.53:53, comma-separated">
            {(id) => (
              <input
                id={id}
                value={f.resolvers}
                disabled={off}
                spellCheck={false}
                onChange={(e) => edit({ resolvers: e.target.value })}
              />
            )}
          </Field>
        </>
      )}
      <details className="detail">
        <summary>Account binding and profile</summary>
        <Field label="Key ID">
          {(id) => (
            <input
              id={id}
              value={f.keyID}
              disabled={off}
              autoComplete="off"
              spellCheck={false}
              onChange={(e) => edit({ keyID: e.target.value })}
            />
          )}
        </Field>
        <Field label="HMAC key" hint={hmacSaved ? "Saved, empty keeps it" : undefined}>
          {(id) => (
            <input
              id={id}
              type="password"
              value={f.hmacKey}
              disabled={off}
              autoComplete="off"
              onChange={(e) => edit({ hmacKey: e.target.value })}
            />
          )}
        </Field>
        <Field label="Profile" hint="Offered by the server, such as shortlived">
          {(id) => (
            <input
              id={id}
              value={f.profile}
              disabled={off}
              spellCheck={false}
              onChange={(e) => edit({ profile: e.target.value })}
            />
          )}
        </Field>
      </details>
      <ErrorBox message={error} />
      <div className="actions">
        <button type="submit" className="primary" disabled={off}>
          {saveLabel}
        </button>
        {view !== undefined && (
          <button
            type="button"
            disabled={off}
            onClick={() => {
              void orderNow();
            }}
          >
            {ordering ? "Ordering…" : "Order now"}
          </button>
        )}
      </div>
    </form>
  );
}
