import { useId, useState, type JSX, type ReactNode } from "react";

import { api } from "./api";
import { CredentialField } from "./CredentialField";
import { list } from "./network";
import type { Cluster } from "./types";
import { Dialog, ErrorBox, Field, Info, errorMessage, filled, saveFile, useChange } from "./ui";
import { flag, oneOf, record, text, texts } from "./values";

export const issuers = ["acme-dns01", "acme-http01", "ca"] as const;
export type Issuer = (typeof issuers)[number];

const issuerOptions: { value: Issuer; label: string }[] = [
  { value: "acme-dns01", label: "Let's Encrypt, DNS-01 (recommended)" },
  { value: "acme-http01", label: "Let's Encrypt, HTTP-01" },
  { value: "ca", label: "Cluster's own CA" },
];

const issuerInfo: Record<Issuer, string> = {
  "acme-dns01": "Gives a wildcard certificate, even for clusters the internet can't reach.",
  "acme-http01": "One certificate per Ingress host, no wildcard.",
  ca: "Clients must trust the CA certificate from the cluster page.",
};

export const providers = [
  "cloudflare",
  "route53",
  "digitalocean",
  "azuredns",
  "clouddns",
  "rfc2136",
  "acmedns",
  "akamai",
] as const;
export type Provider = (typeof providers)[number];

/** One setting a DNS provider takes besides its credential. */
interface Setting {
  key: string;
  label: string;
  required: boolean;
  hint?: string;
  choices?: string[];
  check?: (v: string) => string | undefined;
}

const maxPort = 65_535;

// hostPort checks a host:port, a bracketed IPv6 address among the hosts.
function hostPort(v: string): string | undefined {
  const m = /^(?:\[[0-9a-f:.]+\]|[a-z0-9](?:[-a-z0-9.]*[a-z0-9])?):(?<port>\d{1,5})$/iu.exec(v);
  const port = Number(m?.groups?.["port"] ?? 0);
  return m && port >= 1 && port <= maxPort ? undefined : `Enter ${v} as host:port, such as 192.0.2.53:53`;
}

const httpsURL = (v: string): string | undefined =>
  URL.canParse(v) && new URL(v).protocol === "https:" ? undefined : "Enter an https:// URL";

const optional = "Optional, looked up from the domain";

interface ProviderInfo {
  label: string;
  // The variable's object for the provider's settings, if it takes any.
  group?: string;
  settings: Setting[];
  // What the credential stored through the interface holds, the least it needs, and rarely a caution
  // too long for the hint. No token for a provider that takes only a Secret of the cluster's.
  token?: { label: string; hint: string; info?: string };
  secretKeys: string[];
}

const providerInfo: Record<Provider, ProviderInfo> = {
  cloudflare: {
    label: "Cloudflare",
    settings: [],
    token: { label: "API token", hint: "Needs Zone, DNS, Edit on this zone only" },
    secretKeys: ["token"],
  },
  route53: {
    label: "Amazon Route 53",
    group: "route53",
    settings: [
      { key: "region", label: "Region", required: true },
      { key: "accessKeyID", label: "Access key ID", required: true },
      { key: "hostedZoneID", label: "Hosted zone ID", required: false, hint: optional },
    ],
    token: {
      label: "Secret access key",
      hint: "Needs route53:ChangeResourceRecordSets, GetChange and ListHostedZonesByName",
      info: "Limit the first two to this hosted zone.",
    },
    secretKeys: ["token"],
  },
  digitalocean: {
    label: "DigitalOcean",
    settings: [],
    token: { label: "API token", hint: "Custom scopes: domain read and update" },
    secretKeys: ["token"],
  },
  azuredns: {
    label: "Azure DNS",
    group: "azureDNS",
    settings: [
      { key: "subscriptionID", label: "Subscription ID", required: true },
      { key: "resourceGroupName", label: "Resource group", required: true },
      { key: "tenantID", label: "Tenant ID", required: true },
      { key: "clientID", label: "Client ID", required: true },
      { key: "hostedZoneName", label: "DNS zone", required: false, hint: optional },
      {
        key: "environment",
        label: "Cloud",
        required: false,
        choices: ["AzurePublicCloud", "AzureChinaCloud", "AzureUSGovernmentCloud"],
      },
    ],
    token: { label: "Client secret", hint: "Needs DNS Zone Contributor on this zone only" },
    secretKeys: ["token"],
  },
  clouddns: {
    label: "Google Cloud DNS",
    group: "cloudDNS",
    settings: [
      { key: "project", label: "Project ID", required: true },
      { key: "hostedZoneName", label: "Managed zone", required: false, hint: optional },
    ],
    token: {
      label: "Service account key",
      hint: "JSON key of a service account with DNS Administrator",
      info: "Cloud DNS grants it per project, so give the zone its own project.",
    },
    secretKeys: ["token"],
  },
  rfc2136: {
    label: "RFC 2136 (BIND, PowerDNS)",
    group: "rfc2136",
    settings: [
      {
        key: "nameserver",
        label: "DNS server",
        required: true,
        hint: "The primary, as host:port, such as 192.0.2.53:53",
        check: hostPort,
      },
      { key: "tsigKeyName", label: "TSIG key name", required: true },
      {
        key: "tsigAlgorithm",
        label: "TSIG algorithm",
        required: false,
        choices: ["HMACSHA256", "HMACSHA512", "HMACSHA1", "HMACMD5"],
      },
    ],
    token: { label: "TSIG secret", hint: "Base64, of a key limited to _acme-challenge records" },
    secretKeys: ["token"],
  },
  acmedns: {
    label: "ACME-DNS",
    group: "acmeDNS",
    settings: [{ key: "host", label: "ACME-DNS server", required: true, check: httpsURL }],
    token: {
      label: "Account JSON",
      hint: "The output of acme-dns register, keyed by the domain",
      info: "Point _acme-challenge under the domain at its fulldomain by CNAME.",
    },
    secretKeys: ["token"],
  },
  akamai: {
    label: "Akamai Edge DNS",
    group: "akamai",
    settings: [{ key: "serviceConsumerDomain", label: "Service consumer domain", required: true }],
    secretKeys: ["clientToken", "clientSecret", "accessToken"],
  },
};

/** Where the DNS provider's credential comes from. */
export type Source = "cluster" | "shared" | "secret";

/** A cluster's certificates variable as the fields edit it. */
export interface CertificatesForm {
  enabled: boolean;
  issuer: Issuer;
  domain: string;
  email: string;
  staging: boolean;
  server: string;
  caBundle: string;
  provider: Provider;
  source: Source;
  existingSecret: string;
  nameservers: string;
  // Each provider's settings, kept while another is chosen.
  settings: Partial<Record<Provider, Record<string, string>>>;
}

const textsOf = (v: unknown): Record<string, string> =>
  Object.fromEntries(Object.entries(record(v)).filter((e): e is [string, string] => typeof e[1] === "string"));

export function certificatesOf(v: unknown): CertificatesForm {
  const r = record(v);
  const acme = record(r["acme"]);
  const dns01 = record(r["dns01"]);
  const issuer = text(r["issuer"]);
  const provider = text(dns01["provider"]);
  const existingSecret = text(dns01["existingSecret"]) ?? "";
  let source: Source = "cluster";
  if (filled(existingSecret)) {
    source = "secret";
  } else if (flag(dns01["sharedCredential"]) === true) {
    source = "shared";
  }
  const settings: CertificatesForm["settings"] = {};
  for (const p of providers) {
    const { group } = providerInfo[p];
    if (group !== undefined && group in dns01) {
      settings[p] = textsOf(dns01[group]);
    }
  }
  return {
    enabled: flag(r["enabled"]) === true,
    issuer: oneOf(issuers, issuer) ? issuer : "acme-dns01",
    domain: text(r["domain"]) ?? "",
    email: text(acme["email"]) ?? "",
    staging: flag(acme["staging"]) === true,
    server: text(acme["server"]) ?? "",
    caBundle: text(acme["caBundle"]) ?? "",
    provider: oneOf(providers, provider) ? provider : "cloudflare",
    source,
    existingSecret,
    nameservers: (texts(dns01["nameservers"]) ?? []).join(", "),
    settings,
  };
}

/** newCertificates is the choice before anything is chosen: off, the recommended issuer and provider. */
export const newCertificates = (): CertificatesForm => certificatesOf(undefined);

// takesSecretOnly tells whether a provider's credential can only be a Secret of the cluster's.
const takesSecretOnly = (p: Provider): boolean => providerInfo[p].token === undefined;

/** sourceOf is where a choice takes the credential from, a provider that takes only a Secret held to it. */
export const sourceOf = (f: CertificatesForm): Source => (takesSecretOnly(f.provider) ? "secret" : f.source);

/** needsClusterToken tells whether a choice takes the DNS-01 credential stored for this cluster. */
export const needsClusterToken = (f: CertificatesForm): boolean =>
  f.enabled && f.issuer === "acme-dns01" && sourceOf(f) === "cluster";

/** sharesCredential tells whether a choice takes the DNS-01 credential stored for every cluster. */
export const sharesCredential = (f: CertificatesForm): boolean =>
  f.enabled && f.issuer === "acme-dns01" && sourceOf(f) === "shared";

/** tokenLabel names what a provider's credential holds, such as its API token. */
export const tokenLabel = (p: Provider): string => providerInfo[p].token?.label ?? "Credential";

/** tokenHint says what a provider's credential has to be, as little as it can reach. */
export function tokenHint(p: Provider): ReactNode {
  const { token } = providerInfo[p];
  if (token?.info === undefined) {
    return token?.hint;
  }
  return (
    <>
      {token.hint} <Info about={token.label}>{token.info}</Info>
    </>
  );
}

// secretHint says what a Secret of the cluster's holds for a provider.
function secretHint(p: Provider): string {
  const { token, secretKeys } = providerInfo[p];
  if (token === undefined) {
    return `In cert-manager, with keys ${secretKeys.slice(0, -1).join(", ")} and ${secretKeys.at(-1) ?? ""}`;
  }
  return `In cert-manager, with the ${token.label} under key token`;
}

// settingsOf is the chosen provider's settings as the variable holds them: the filled ones only.
function settingsOf(f: CertificatesForm): Record<string, string> {
  const typed = f.settings[f.provider] ?? {};
  return Object.fromEntries(
    providerInfo[f.provider].settings.flatMap(({ key }) => {
      const v = typed[key]?.trim() ?? "";
      return v === "" ? [] : [[key, v]];
    }),
  );
}

/** certificatesVariable is the variable a choice sets. Undefined while it is off. */
export function certificatesVariable(f: CertificatesForm): Record<string, unknown> | undefined {
  if (!f.enabled) {
    return undefined;
  }
  const v: Record<string, unknown> = { enabled: true, issuer: f.issuer };
  if (f.issuer !== "acme-http01") {
    v["domain"] = f.domain;
  }
  if (f.issuer !== "ca") {
    const acme: Record<string, unknown> = {};
    if (filled(f.email.trim())) {
      acme["email"] = f.email.trim();
    }
    if (filled(f.server.trim())) {
      acme["server"] = f.server.trim();
    } else if (f.staging) {
      acme["staging"] = true;
    }
    if (filled(f.caBundle.trim())) {
      acme["caBundle"] = f.caBundle.trim();
    }
    if (Object.keys(acme).length > 0) {
      v["acme"] = acme;
    }
  }
  if (f.issuer === "acme-dns01") {
    const dns01: Record<string, unknown> = { provider: f.provider };
    const source = sourceOf(f);
    if (source === "secret") {
      dns01["existingSecret"] = f.existingSecret.trim();
    } else if (source === "shared") {
      dns01["sharedCredential"] = true;
    }
    const nameservers = list(f.nameservers);
    if (nameservers.length > 0) {
      dns01["nameservers"] = nameservers;
    }
    const { group } = providerInfo[f.provider];
    const settings = settingsOf(f);
    if (group !== undefined && Object.keys(settings).length > 0) {
      dns01[group] = settings;
    }
    v["dns01"] = dns01;
  }
  return v;
}

/** certificatesText says in a line where a cluster's certificates come from, and for what. */
export function certificatesText(f: CertificatesForm): string {
  if (!f.enabled) {
    return "Off";
  }
  const names = `for ${f.domain} and *.${f.domain}`;
  if (f.issuer === "ca") {
    return `Cluster's own CA, ${names}`;
  }
  let acme = "Let's Encrypt";
  if (filled(f.server)) {
    acme = URL.canParse(f.server) ? new URL(f.server).host : f.server;
  } else if (f.staging) {
    acme = "Let's Encrypt staging";
  }
  if (f.issuer === "acme-http01") {
    return `${acme} by HTTP-01, for each Ingress host`;
  }
  return `${acme} by DNS-01 through ${providerInfo[f.provider].label}, ${names}`;
}

/** The fields of the certificates choice a problem belongs to. A provider's settings by "setting.<key>". */
export type CertificatesProblems = Partial<Record<string, string>>;

const dnsName = /^(?=.{1,253}$)(?:[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?\.)+[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?$/u;
const secretName = /^(?=.{1,253}$)[a-z0-9](?:[-a-z0-9.]*[a-z0-9])?$/u;
const emailAddress = /^[^\s@]+@[^\s@]+\.[^\s@]+$/u;

function acmeProblems(f: CertificatesForm): CertificatesProblems {
  const p: CertificatesProblems = {};
  if (filled(f.email.trim()) && !emailAddress.test(f.email.trim())) {
    p["email"] = "Enter an email address, such as ops@example.com";
  }
  const server = f.server.trim();
  if (filled(server)) {
    p["server"] = httpsURL(server);
  }
  if (filled(f.caBundle.trim()) && !f.caBundle.includes("-----BEGIN CERTIFICATE-----")) {
    p["caBundle"] = "Paste one or more PEM certificates";
  }
  return p;
}

// lowerFirst lowers a label's first letter for a sentence, unless it starts an acronym.
const lowerFirst = (s: string): string => (/^[A-Z][a-z]/u.test(s) ? s.charAt(0).toLowerCase() + s.slice(1) : s);

function dns01Problems(f: CertificatesForm): CertificatesProblems {
  const p: CertificatesProblems = {};
  if (sourceOf(f) === "secret" && !secretName.test(f.existingSecret.trim())) {
    p["existingSecret"] = "Use lowercase letters, digits, dashes and dots";
  }
  p["nameservers"] = list(f.nameservers)
    .map((n) => hostPort(n))
    .find((x) => x !== undefined);
  const typed = f.settings[f.provider] ?? {};
  for (const s of providerInfo[f.provider].settings) {
    const v = typed[s.key]?.trim() ?? "";
    if (v === "") {
      if (s.required) {
        p[`setting.${s.key}`] = `Enter the ${lowerFirst(s.label)}`;
      }
    } else if (s.check !== undefined) {
      p[`setting.${s.key}`] = s.check(v);
    }
  }
  return p;
}

/** certificatesProblems says, by field, what is wrong with a choice. Nothing while it is off. */
export function certificatesProblems(f: CertificatesForm): CertificatesProblems {
  if (!f.enabled) {
    return {};
  }
  const p: CertificatesProblems = {
    ...(f.issuer === "ca" ? {} : acmeProblems(f)),
    ...(f.issuer === "acme-dns01" ? dns01Problems(f) : {}),
  };
  if (f.issuer !== "acme-http01" && !dnsName.test(f.domain)) {
    p["domain"] = "Enter a domain name, such as apps.example.com";
  }
  return Object.fromEntries(Object.entries(p).filter(([, v]) => v !== undefined));
}

/**
 * CertificatesFields chooses where a cluster's certificates come from. renderCredential is how the
 * credential stored for this cluster is entered, given its label and hint: in the wizard a field of
 * its own, on a running cluster the stored credential. A field shows its problem once it was
 * edited, or every field once the user tried to go on (showAll).
 */
export function CertificatesFields({
  value: f,
  onChange,
  showAll,
  mayShare,
  shareRefusal,
  renderCredential,
}: {
  value: CertificatesForm;
  onChange: (f: CertificatesForm) => void;
  showAll: boolean;
  mayShare: boolean;
  // Why the credential for every cluster cannot be chosen, when it cannot.
  shareRefusal: string;
  renderCredential: (label: string, hint: ReactNode) => ReactNode;
}): JSX.Element {
  const [edited, setEdited] = useState<ReadonlySet<string>>(new Set());
  const stagingHint = useId();
  const sourceName = useId();
  const sharedHint = useId();
  const refusalHint = useId();
  const problems = certificatesProblems(f);
  const error = (k: string): string | undefined => (showAll || edited.has(k) ? problems[k] : undefined);
  const update = (k: string, patch: Partial<CertificatesForm>): void => {
    setEdited((s) => (s.has(k) ? s : new Set([...s, k])));
    onChange({ ...f, ...patch });
  };
  const info = providerInfo[f.provider];
  const typed = f.settings[f.provider] ?? {};
  const source = sourceOf(f);
  const refused = !mayShare && source !== "shared";
  return (
    <>
      <label className="check">
        <input type="checkbox" checked={f.enabled} onChange={(e) => onChange({ ...f, enabled: e.target.checked })} />{" "}
        Issue certificates
      </label>
      {f.enabled && (
        <>
          <Field
            label="Issuer"
            hint={f.issuer === "acme-http01" ? "Needs port 80 reachable from the internet" : undefined}
            info={issuerInfo[f.issuer]}
          >
            {(id) => (
              <select
                id={id}
                value={f.issuer}
                onChange={(e) => {
                  const v = e.target.value;
                  if (oneOf(issuers, v)) {
                    onChange({ ...f, issuer: v });
                  }
                }}
              >
                {issuerOptions.map((o) => (
                  <option key={o.value} value={o.value}>
                    {o.label}
                  </option>
                ))}
              </select>
            )}
          </Field>
          {f.issuer !== "acme-http01" && (
            <Field label="Domain" hint="Such as apps.example.com, with its subdomains" error={error("domain")}>
              {(id) => (
                <input
                  id={id}
                  value={f.domain}
                  onChange={(e) => update("domain", { domain: e.target.value.trim().toLowerCase() })}
                />
              )}
            </Field>
          )}
          {f.issuer === "acme-dns01" && (
            <>
              <Field
                label="DNS provider"
                info="A CNAME from _acme-challenge to an ACME-DNS or RFC 2136 zone limits what a leaked key reaches."
              >
                {(id) => (
                  <select
                    id={id}
                    value={f.provider}
                    onChange={(e) => {
                      const v = e.target.value;
                      if (oneOf(providers, v)) {
                        onChange({ ...f, provider: v });
                      }
                    }}
                  >
                    {providers.map((p) => (
                      <option key={p} value={p}>
                        {providerInfo[p].label}
                      </option>
                    ))}
                  </select>
                )}
              </Field>
              {info.settings.map((s) => (
                <Field key={`${f.provider}.${s.key}`} label={s.label} hint={s.hint} error={error(`setting.${s.key}`)}>
                  {(id) =>
                    s.choices === undefined ? (
                      <input
                        id={id}
                        value={typed[s.key] ?? ""}
                        spellCheck={false}
                        onChange={(e) =>
                          update(`setting.${s.key}`, {
                            settings: { ...f.settings, [f.provider]: { ...typed, [s.key]: e.target.value } },
                          })
                        }
                      />
                    ) : (
                      <select
                        id={id}
                        value={typed[s.key] ?? ""}
                        onChange={(e) =>
                          onChange({
                            ...f,
                            settings: { ...f.settings, [f.provider]: { ...typed, [s.key]: e.target.value } },
                          })
                        }
                      >
                        <option value="">{`${s.choices[0] ?? ""} (default)`}</option>
                        {s.choices.slice(1).map((c) => (
                          <option key={c} value={c}>
                            {c}
                          </option>
                        ))}
                      </select>
                    )
                  }
                </Field>
              ))}
              <fieldset className="choices">
                <legend>DNS credential</legend>
                {info.token !== undefined && (
                  <>
                    <label className="check">
                      <input
                        type="radio"
                        name={sourceName}
                        checked={source === "cluster"}
                        onChange={() => onChange({ ...f, source: "cluster" })}
                      />{" "}
                      For this cluster (recommended)
                    </label>
                    <label className="check">
                      <input
                        type="radio"
                        name={sourceName}
                        checked={source === "shared"}
                        disabled={refused}
                        aria-describedby={refused ? `${sharedHint} ${refusalHint}` : sharedHint}
                        onChange={() => onChange({ ...f, source: "shared" })}
                      />{" "}
                      Shared by all clusters
                    </label>
                    <Info about="the shared credential">
                      Set under Administration. It writes records for every cluster using it.
                    </Info>
                    <p className="hint" id={sharedHint}>
                      Needs Sys.Modify on the cluster&apos;s pool
                    </p>
                    {refused && (
                      <p className="hint" id={refusalHint}>
                        {shareRefusal}
                      </p>
                    )}
                  </>
                )}
                <label className="check">
                  <input
                    type="radio"
                    name={sourceName}
                    checked={source === "secret"}
                    onChange={() => onChange({ ...f, source: "secret" })}
                  />{" "}
                  Secret in the cluster
                </label>
              </fieldset>
              {source === "cluster" && renderCredential(tokenLabel(f.provider), tokenHint(f.provider))}
              {source === "secret" && (
                <Field label="Secret name" hint={secretHint(f.provider)} error={error("existingSecret")}>
                  {(id) => (
                    <input
                      id={id}
                      value={f.existingSecret}
                      spellCheck={false}
                      onChange={(e) => update("existingSecret", { existingSecret: e.target.value.trim() })}
                    />
                  )}
                </Field>
              )}
              <details className="advanced" open={filled(f.nameservers) || undefined}>
                <summary>Record check servers</summary>
                <Field
                  label="DNS servers"
                  hint="host:port list, such as 1.1.1.1:53, 8.8.8.8:53"
                  info="For split-horizon DNS. Empty uses the cluster's resolvers."
                  error={error("nameservers")}
                >
                  {(id) => (
                    <input
                      id={id}
                      value={f.nameservers}
                      onChange={(e) => update("nameservers", { nameservers: e.target.value })}
                    />
                  )}
                </Field>
              </details>
            </>
          )}
          {f.issuer !== "ca" && (
            <>
              <Field label="Contact email" hint="Optional, for account notices" error={error("email")}>
                {(id) => (
                  <input
                    id={id}
                    type="email"
                    value={f.email}
                    onChange={(e) => update("email", { email: e.target.value.trim() })}
                  />
                )}
              </Field>
              <label className="check">
                <input
                  type="checkbox"
                  checked={f.staging && !filled(f.server)}
                  disabled={filled(f.server)}
                  aria-describedby={stagingHint}
                  onChange={(e) => onChange({ ...f, staging: e.target.checked })}
                />{" "}
                Let&apos;s Encrypt staging
              </label>
              <p className="hint" id={stagingHint}>
                Higher rate limits, but browsers don&apos;t trust its certificates
              </p>
              <details className="advanced" open={filled(f.server) || filled(f.caBundle) || undefined}>
                <summary>Another ACME server</summary>
                <Field label="ACME server" hint="Directory URL, used instead of Let's Encrypt" error={error("server")}>
                  {(id) => (
                    <input
                      id={id}
                      value={f.server}
                      onChange={(e) => update("server", { server: e.target.value.trim() })}
                    />
                  )}
                </Field>
                <Field label="CA certificate" hint="PEM, for a server with a private CA" error={error("caBundle")}>
                  {(id) => (
                    <textarea
                      id={id}
                      rows={3}
                      value={f.caBundle}
                      onChange={(e) => update("caBundle", { caBundle: e.target.value })}
                    />
                  )}
                </Field>
              </details>
            </>
          )}
        </>
      )}
    </>
  );
}

/** issuesFromCA tells whether a cluster's certificates come from its own CA, which it hands out. */
export function issuesFromCA(c: Cluster): boolean {
  const f = certificatesOf(c.variables["certificates"]);
  return f.enabled && f.issuer === "ca";
}

/** saveCA fetches a cluster's CA certificate and saves it. */
export async function saveCA(c: Cluster): Promise<void> {
  saveFile(await api.ingressCA(c.namespace, c.name), `${c.name}-ca.crt`);
}

export function CertificatesDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  // Switched off, the variable keeps what else it holds, for switching on again.
  const held = record(c.variables["certificates"]);
  const current = certificatesOf(held);
  const [value, setValue] = useState(current);
  const [attempted, setAttempted] = useState(false);
  const { busy, error, send } = useChange(c, onClose);
  const [caError, setCAError] = useState<string>();
  // Fetched rather than linked, so that a refusal shows here instead of as a broken download.
  const downloadCA = async (): Promise<void> => {
    setCAError(undefined);
    try {
      await saveCA(c);
    } catch (err) {
      setCAError(errorMessage(err));
    }
  };
  const open = Object.keys(certificatesProblems(value)).length > 0;
  const next = certificatesVariable(value);
  const changed = JSON.stringify(next) !== JSON.stringify(certificatesVariable(current));
  return (
    <Dialog
      title="Edit certificates"
      onClose={onClose}
      onSubmit={() => {
        if (open) {
          setAttempted(true);
          return;
        }
        void send({ variables: { certificates: next ?? { ...held, enabled: false } } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || !changed}>
            Save
          </button>
        </>
      }
    >
      <CertificatesFields
        value={value}
        onChange={setValue}
        showAll={attempted}
        mayShare={c.allowed.kubeconfig || sharesCredential(current)}
        shareRefusal="You don't hold it"
        renderCredential={(label, hint) => (
          <CredentialField
            label={label}
            purpose="dns01"
            cluster={{ namespace: c.namespace, name: c.name }}
            keys={[{ key: "token", label }]}
            hint={hint}
          />
        )}
      />
      {issuesFromCA(c) && (
        <div className="actions">
          <button
            type="button"
            onClick={() => {
              void downloadCA();
            }}
          >
            Download CA certificate
          </button>
        </div>
      )}
      <ErrorBox message={caError} />
      <ErrorBox message={error} />
    </Dialog>
  );
}

/** SharedDNS01 stores the DNS-01 credential for every cluster, on the Administration page. */
export function SharedDNS01(): JSX.Element {
  return (
    <div className="box">
      <h3>Certificates</h3>
      <CredentialField
        label="Shared DNS-01 credential"
        purpose="dns01"
        keys={[{ key: "token", label: "Credential" }]}
        hint={
          <>
            Writes records for every cluster that chooses it{" "}
            <Info about="the shared credential">
              Only someone with Sys.Modify on a cluster&apos;s pool can give it to that cluster.
            </Info>
          </>
        }
      />
    </div>
  );
}
