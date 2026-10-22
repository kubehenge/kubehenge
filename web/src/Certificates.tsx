import { useId, useState, type JSX, type ReactNode } from "react";

import { api } from "./api";
import { CredentialField } from "./CredentialField";
import { list } from "./network";
import type { Cluster } from "./types";
import { Dialog, ErrorBox, Field, errorMessage, filled, saveFile, useChange } from "./ui";
import { flag, oneOf, record, text, texts } from "./values";

export const issuers = ["acme-dns01", "acme-http01", "ca"] as const;
export type Issuer = (typeof issuers)[number];

const issuerOptions: { value: Issuer; label: string }[] = [
  { value: "acme-dns01", label: "Let's Encrypt, DNS-01 (recommended)" },
  { value: "acme-http01", label: "Let's Encrypt, HTTP-01" },
  { value: "ca", label: "The cluster's own CA" },
];

const issuerHints: Record<Issuer, string> = {
  "acme-dns01":
    "Proves the domain through a TXT record cert-manager writes with the DNS provider's API. Works for clusters the internet cannot reach and gives a wildcard certificate, so every host under the domain serves HTTPS at once.",
  "acme-http01":
    "Proves each host name over HTTP: Let's Encrypt must reach the cluster's ingress controller on port 80 from the internet. No wildcards, so every Ingress host gets a certificate of its own.",
  ca: "The cluster makes a CA of its own, constrained to the domain so it can sign for nothing else, and its certificates come from it. Browsers and clients trust them once they trust its certificate, which the cluster's page hands out.",
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
  placeholder?: string;
  choices?: string[];
  check?: (v: string) => string | undefined;
}

const maxPort = 65_535;

// hostPort checks a host:port, a bracketed IPv6 address among the hosts.
function hostPort(v: string): string | undefined {
  const m = /^(?:\[[0-9a-f:.]+\]|[a-z0-9](?:[-a-z0-9.]*[a-z0-9])?):(?<port>\d{1,5})$/iu.exec(v);
  const port = Number(m?.groups?.["port"] ?? 0);
  return m && port >= 1 && port <= maxPort ? undefined : `${v} is no host:port, such as 192.0.2.53:53.`;
}

const httpsURL = (v: string): string | undefined =>
  URL.canParse(v) && new URL(v).protocol === "https:" ? undefined : "An https:// URL.";

interface ProviderInfo {
  label: string;
  // The variable's object for the provider's settings, if it takes any.
  group?: string;
  settings: Setting[];
  // What the credential stored through the interface holds; none for a provider that takes only a
  // Secret of the cluster's.
  token?: { label: string; hint: string };
  secretKeys: string[];
}

const providerInfo: Record<Provider, ProviderInfo> = {
  cloudflare: {
    label: "Cloudflare",
    settings: [],
    token: { label: "API token", hint: "A Cloudflare API token with Zone, DNS, Edit on this zone only." },
    secretKeys: ["token"],
  },
  route53: {
    label: "Amazon Route 53",
    group: "route53",
    settings: [
      { key: "region", label: "Region", required: true, placeholder: "us-east-1" },
      { key: "accessKeyID", label: "Access key ID", required: true },
      {
        key: "hostedZoneID",
        label: "Hosted zone ID",
        required: false,
        hint: "Optional: found from the domain when empty.",
      },
    ],
    token: {
      label: "Secret access key",
      hint: "Of an IAM user allowed route53:ChangeResourceRecordSets and route53:GetChange on this hosted zone only, and route53:ListHostedZonesByName.",
    },
    secretKeys: ["token"],
  },
  digitalocean: {
    label: "DigitalOcean",
    settings: [],
    token: { label: "API token", hint: "A custom-scoped DigitalOcean token with domain read and update only." },
    secretKeys: ["token"],
  },
  azuredns: {
    label: "Azure DNS",
    group: "azureDNS",
    settings: [
      { key: "subscriptionID", label: "Subscription ID", required: true },
      { key: "resourceGroupName", label: "Resource group", required: true },
      { key: "tenantID", label: "Tenant ID", required: true },
      { key: "clientID", label: "Client ID", required: true, hint: "Of the service principal." },
      {
        key: "hostedZoneName",
        label: "DNS zone",
        required: false,
        hint: "Optional: found from the domain when empty.",
      },
      {
        key: "environment",
        label: "Cloud",
        required: false,
        choices: ["AzurePublicCloud", "AzureChinaCloud", "AzureUSGovernmentCloud"],
      },
    ],
    token: {
      label: "Client secret",
      hint: "Of the service principal, holding DNS Zone Contributor on this zone only.",
    },
    secretKeys: ["token"],
  },
  clouddns: {
    label: "Google Cloud DNS",
    group: "cloudDNS",
    settings: [
      { key: "project", label: "Project ID", required: true },
      {
        key: "hostedZoneName",
        label: "Managed zone",
        required: false,
        hint: "Optional: found from the domain when empty.",
      },
    ],
    token: {
      label: "Service account key",
      hint: "The JSON key of a service account holding DNS Administrator; Cloud DNS grants no less than a whole project, so keep the zone in a project of its own.",
    },
    secretKeys: ["token"],
  },
  rfc2136: {
    label: "RFC 2136 (BIND, PowerDNS, Technitium, ...)",
    group: "rfc2136",
    settings: [
      {
        key: "nameserver",
        label: "DNS server",
        required: true,
        placeholder: "192.0.2.53:53",
        hint: "host:port of the primary server, which takes the updates.",
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
    token: {
      label: "TSIG secret",
      hint: "Base64, of a key the server lets update only the _acme-challenge records.",
    },
    secretKeys: ["token"],
  },
  acmedns: {
    label: "ACME-DNS",
    group: "acmeDNS",
    settings: [
      {
        key: "host",
        label: "ACME-DNS server",
        required: true,
        placeholder: "https://",
        check: httpsURL,
      },
    ],
    token: {
      label: "Account JSON",
      hint: "What acme-dns register printed, keyed by the domain; _acme-challenge under the domain points at its fulldomain by CNAME.",
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
export const tokenHint = (p: Provider): string => providerInfo[p].token?.hint ?? "";

// secretHint says what a Secret of the cluster's holds for a provider.
function secretHint(p: Provider): string {
  const { label, token, secretKeys } = providerInfo[p];
  if (token === undefined) {
    const keys = `${secretKeys.slice(0, -1).join(", ")} and ${secretKeys.at(-1) ?? ""}`;
    return `In the cluster's cert-manager namespace, holding the keys ${keys}. ${label} needs three secrets, so it takes only a Secret of the cluster's.`;
  }
  return `In the cluster's cert-manager namespace, holding the ${token.label} under the key token. ${token.hint}`;
}

const scopeHint =
  "A credential that can write only this zone limits what a leak costs. Where the provider cannot scope one that far, point _acme-challenge under the domain by CNAME into a zone of its own, served by ACME-DNS or an RFC 2136 server, and give the cluster that one.";

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

/** certificatesVariable is the variable a choice sets; undefined while it is off. */
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
    return `The cluster's own CA, ${names}`;
  }
  let acme = "Let's Encrypt";
  if (filled(f.server)) {
    acme = URL.canParse(f.server) ? new URL(f.server).host : f.server;
  } else if (f.staging) {
    acme = "Let's Encrypt's staging server";
  }
  if (f.issuer === "acme-http01") {
    return `${acme} by HTTP-01, for each Ingress host`;
  }
  return `${acme} by DNS-01 through ${providerInfo[f.provider].label}, ${names}`;
}

/** The fields of the certificates choice a problem belongs to; a provider's settings by "setting.<key>". */
export type CertificatesProblems = Partial<Record<string, string>>;

const dnsName = /^(?=.{1,253}$)(?:[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?\.)+[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?$/u;
const secretName = /^(?=.{1,253}$)[a-z0-9](?:[-a-z0-9.]*[a-z0-9])?$/u;
const emailAddress = /^[^\s@]+@[^\s@]+\.[^\s@]+$/u;

function acmeProblems(f: CertificatesForm): CertificatesProblems {
  const p: CertificatesProblems = {};
  if (filled(f.email.trim()) && !emailAddress.test(f.email.trim())) {
    p["email"] = "An email address.";
  }
  const server = f.server.trim();
  if (filled(server)) {
    p["server"] = httpsURL(server);
  }
  if (filled(f.caBundle.trim()) && !f.caBundle.includes("-----BEGIN CERTIFICATE-----")) {
    p["caBundle"] = "PEM: one or more -----BEGIN CERTIFICATE----- blocks.";
  }
  return p;
}

function dns01Problems(f: CertificatesForm): CertificatesProblems {
  const p: CertificatesProblems = {};
  if (sourceOf(f) === "secret" && !secretName.test(f.existingSecret.trim())) {
    p["existingSecret"] = "A Secret's name: lowercase letters, digits, dashes and dots.";
  }
  p["nameservers"] = list(f.nameservers)
    .map((n) => hostPort(n))
    .find((x) => x !== undefined);
  const typed = f.settings[f.provider] ?? {};
  for (const s of providerInfo[f.provider].settings) {
    const v = typed[s.key]?.trim() ?? "";
    if (v === "") {
      if (s.required) {
        p[`setting.${s.key}`] = `${providerInfo[f.provider].label} needs this.`;
      }
    } else if (s.check !== undefined) {
      p[`setting.${s.key}`] = s.check(v);
    }
  }
  return p;
}

/** certificatesProblems says, by field, what is wrong with a choice; nothing while it is off. */
export function certificatesProblems(f: CertificatesForm): CertificatesProblems {
  if (!f.enabled) {
    return {};
  }
  const p: CertificatesProblems = {
    ...(f.issuer === "ca" ? {} : acmeProblems(f)),
    ...(f.issuer === "acme-dns01" ? dns01Problems(f) : {}),
  };
  if (f.issuer !== "acme-http01" && !dnsName.test(f.domain)) {
    p["domain"] = "A domain name, such as apps.example.com.";
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
  const enabledHint = useId();
  const stagingHint = useId();
  const sourceName = useId();
  const sharedHint = useId();
  const problems = certificatesProblems(f);
  const error = (k: string): string | undefined => (showAll || edited.has(k) ? problems[k] : undefined);
  const update = (k: string, patch: Partial<CertificatesForm>): void => {
    setEdited((s) => (s.has(k) ? s : new Set([...s, k])));
    onChange({ ...f, ...patch });
  };
  const info = providerInfo[f.provider];
  const typed = f.settings[f.provider] ?? {};
  const source = sourceOf(f);
  return (
    <>
      <label className="check">
        <input
          type="checkbox"
          checked={f.enabled}
          aria-describedby={enabledHint}
          onChange={(e) => onChange({ ...f, enabled: e.target.checked })}
        />{" "}
        Issue certificates
      </label>
      <p className="hint" id={enabledHint}>
        cert-manager issues certificates for the cluster&apos;s Ingresses and Gateways, so they serve HTTPS without
        bringing certificates of their own.
      </p>
      {f.enabled && (
        <>
          <Field label="Issuer" hint={issuerHints[f.issuer]}>
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
            <Field
              label="Domain"
              hint="The default certificate covers it and every name directly under it; point those names at the cluster's ingress controller."
              error={error("domain")}
            >
              {(id) => (
                <input
                  id={id}
                  value={f.domain}
                  placeholder="apps.example.com"
                  onChange={(e) => update("domain", { domain: e.target.value.trim().toLowerCase() })}
                />
              )}
            </Field>
          )}
          {f.issuer === "acme-dns01" && (
            <>
              <Field
                label="DNS provider"
                hint="The one serving the domain's zone, or the zone its challenge records are delegated to."
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
                        placeholder={s.placeholder}
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
                        <option value="">{`${s.choices[0] ?? ""} (the default)`}</option>
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
                <legend>The DNS provider&apos;s credential</legend>
                {info.token !== undefined && (
                  <>
                    <label className="check">
                      <input
                        type="radio"
                        name={sourceName}
                        checked={source === "cluster"}
                        onChange={() => onChange({ ...f, source: "cluster" })}
                      />{" "}
                      Stored for this cluster (recommended)
                    </label>
                    <label className="check">
                      <input
                        type="radio"
                        name={sourceName}
                        checked={source === "shared"}
                        disabled={!mayShare && source !== "shared"}
                        aria-describedby={sharedHint}
                        onChange={() => onChange({ ...f, source: "shared" })}
                      />{" "}
                      The one stored for every cluster
                    </label>
                    <p className="hint" id={sharedHint}>
                      An administrator stores it on the Administration page. It can write the records of every cluster
                      that takes it, so choosing it needs Sys.Modify on the cluster&apos;s pool.
                      {!mayShare && source !== "shared" && ` ${shareRefusal}`}
                    </p>
                  </>
                )}
                <label className="check">
                  <input
                    type="radio"
                    name={sourceName}
                    checked={source === "secret"}
                    onChange={() => onChange({ ...f, source: "secret" })}
                  />{" "}
                  A Secret in the cluster
                </label>
              </fieldset>
              {source === "cluster" &&
                renderCredential(tokenLabel(f.provider), `${tokenHint(f.provider)} ${scopeHint}`)}
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
                <summary>DNS servers to check the record with</summary>
                <Field
                  label="DNS servers"
                  hint="host:port of servers that answer for the zone as the internet sees it, when the cluster's own resolvers answer differently, as with split-horizon DNS. Empty: the cluster's resolvers."
                  error={error("nameservers")}
                >
                  {(id) => (
                    <input
                      id={id}
                      value={f.nameservers}
                      placeholder="1.1.1.1:53, 8.8.8.8:53"
                      onChange={(e) => update("nameservers", { nameservers: e.target.value })}
                    />
                  )}
                </Field>
              </details>
            </>
          )}
          {f.issuer !== "ca" && (
            <>
              <Field
                label="Contact email"
                hint="Optional: the ACME server writes here about the account."
                error={error("email")}
              >
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
                Use Let&apos;s Encrypt&apos;s staging server
              </label>
              <p className="hint" id={stagingHint}>
                For trying it out: far higher rate limits, but certificates no browser trusts.
              </p>
              <details className="advanced" open={filled(f.server) || filled(f.caBundle) || undefined}>
                <summary>Another ACME server</summary>
                <Field
                  label="ACME server"
                  hint="The directory URL of another ACME server, such as one of your own, in place of Let's Encrypt."
                  error={error("server")}
                >
                  {(id) => (
                    <input
                      id={id}
                      value={f.server}
                      placeholder="https://"
                      onChange={(e) => update("server", { server: e.target.value.trim() })}
                    />
                  )}
                </Field>
                <Field
                  label="Its CA certificate"
                  hint="PEM, when the server's own certificate is not signed by a public CA."
                  error={error("caBundle")}
                >
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
      title="Certificates"
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
        shareRefusal="You do not hold it."
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
        label="DNS-01 credential for every cluster"
        purpose="dns01"
        keys={[{ key: "token", label: "Credential" }]}
        hint="For the clusters whose certificates choose it, each naming the DNS provider it is for: an API token, a secret access key, a client secret, a key file or a TSIG secret. It can write the records of every zone it reaches, for every cluster that takes it, so only someone holding Sys.Modify on a cluster's pool can give it to that cluster. A credential stored for one cluster alone reaches less."
      />
    </div>
  );
}
