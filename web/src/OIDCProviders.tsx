import { useId, useState, type JSX } from "react";

import type { OIDCProvider } from "./types";
import { Field, filled, sentence } from "./ui";
import { records, text, texts } from "./values";

export const maxProviders = 8;

// The chart's defaults, which an unset field takes.
const defaults = {
  usernameClaim: "email",
  usernamePrefix: "oidc:",
  groupsClaim: "groups",
  groupsPrefix: "oidc:",
  scopes: ["email", "profile"],
};

type ProviderField = "issuerURL" | "clientID" | "usernamePrefix" | "groupsPrefix" | "certificateAuthority";
type ProviderProblems = Partial<Record<ProviderField, string>>;

const fieldOrder: ProviderField[] = ["issuerURL", "clientID", "usernamePrefix", "groupsPrefix", "certificateAuthority"];

/** providerProblems finds, field by field, what Kubehenge would refuse in each provider. */
function providerProblems(providers: OIDCProvider[]): ProviderProblems[] {
  const seen = new Set<string>();
  return providers.map((x) => {
    const p: ProviderProblems = {};
    let url: URL | undefined;
    try {
      url = new URL(x.issuerURL);
    } catch {
      url = undefined;
    }
    const issuer = x.issuerURL.trim();
    if (!url || url.protocol !== "https:" || url.search || url.hash || url.username) {
      p.issuerURL = "the issuer is an https:// URL without query or fragment.";
    } else if (seen.has(issuer)) {
      p.issuerURL = "that issuer is listed already.";
    }
    seen.add(issuer);
    if (!x.clientID.trim()) {
      p.clientID = "a client ID.";
    }
    for (const k of ["usernamePrefix", "groupsPrefix"] as const) {
      if (x[k]?.startsWith("system:") === true) {
        p[k] = "the prefix may not start with system:.";
      }
    }
    if (filled(x.certificateAuthority) && !x.certificateAuthority.includes("-----BEGIN CERTIFICATE-----")) {
      p.certificateAuthority = "the CA is a PEM certificate.";
    }
    return p;
  });
}

/** oidcProblems lists what Kubehenge would refuse in a list of providers. */
export function oidcProblems(providers: OIDCProvider[]): string[] {
  const p: string[] = providers.length > maxProviders ? [`At most ${maxProviders} identity providers.`] : [];
  providerProblems(providers).forEach((found, i) => {
    for (const k of fieldOrder) {
      if (found[k] !== undefined) {
        p.push(`Identity provider ${i + 1}: ${found[k]}`);
      }
    }
  });
  return p;
}

/** cleanProviders drops empty optional fields, so they take the chart's defaults. */
export function cleanProviders(providers: OIDCProvider[]): OIDCProvider[] {
  return providers.map((x) => {
    const out: OIDCProvider = { issuerURL: x.issuerURL.trim(), clientID: x.clientID.trim() };
    if (filled(x.usernameClaim)) {
      out.usernameClaim = x.usernameClaim;
    }
    if (x.usernamePrefix !== undefined) {
      out.usernamePrefix = x.usernamePrefix;
    }
    if (filled(x.groupsClaim)) {
      out.groupsClaim = x.groupsClaim;
    }
    if (x.groupsPrefix !== undefined) {
      out.groupsPrefix = x.groupsPrefix;
    }
    if (x.scopes && x.scopes.length > 0) {
      out.scopes = x.scopes;
    }
    if (filled(x.certificateAuthority)) {
      out.certificateAuthority = x.certificateAuthority;
    }
    return out;
  });
}

const optionalFields = [
  "usernameClaim",
  "usernamePrefix",
  "groupsClaim",
  "groupsPrefix",
  "certificateAuthority",
] as const;

/** oidcProviders reads a cluster's or a class's list of identity providers. */
export function oidcProviders(v: unknown): OIDCProvider[] {
  return records(v).map((r) => {
    const out: OIDCProvider = { issuerURL: text(r["issuerURL"]) ?? "", clientID: text(r["clientID"]) ?? "" };
    for (const k of optionalFields) {
      const s = text(r[k]);
      if (s !== undefined) {
        out[k] = s;
      }
    }
    const scopes = texts(r["scopes"]);
    if (scopes) {
      out.scopes = scopes;
    }
    return out;
  });
}

interface Entry {
  key: number;
  provider: OIDCProvider;
  // scopes as typed, which parsing on every keystroke would rob of its commas and spaces.
  scopesText: string;
}

let nextKey = 0;
const entry = (provider: OIDCProvider): Entry => ({
  key: nextKey++,
  provider,
  scopesText: (provider.scopes ?? []).join(", "),
});

export const parseScopes = (scopes: string): string[] =>
  scopes
    .split(/[\s,]+/u)
    .map((s) => s.trim())
    .filter(Boolean);

/**
 * OIDCProviders edits a list of identity providers, starting from initial. Each entry keeps its
 * key while those before it are removed, so its inputs keep their contents and focus. Fixed, the
 * providers can only be removed: a changed or added one would be one of the user's own, which
 * needs Sys.Modify on the pool. A field shows its problem once it was edited, or every field once
 * the user tried to go on (showAll), as Proxmox VE's forms do.
 */
export function OIDCProviders({
  initial,
  fixed = false,
  showAll = false,
  onChange,
}: {
  initial: OIDCProvider[];
  fixed?: boolean;
  showAll?: boolean;
  onChange: (v: OIDCProvider[]) => void;
}): JSX.Element {
  const [entries, setEntries] = useState<Entry[]>(() => initial.map((p) => entry(p)));
  const [edited, setEdited] = useState<ReadonlySet<string>>(new Set());
  // The provider just added, whose first field takes the focus.
  const [added, setAdded] = useState<number>();
  const fixedHint = useId();
  const change = (next: Entry[]) => {
    setEntries(next);
    onChange(next.map((e) => e.provider));
  };
  const update = (i: number, patch: Partial<OIDCProvider>) => {
    const key = entries[i]?.key;
    setEdited((s) => new Set([...s, ...Object.keys(patch).map((k) => `${key}.${k}`)]));
    change(entries.map((e, j) => (j === i ? { ...e, provider: { ...e.provider, ...patch } } : e)));
  };
  const updateScopes = (i: number, typed: string) =>
    change(
      entries.map((e, j) =>
        j === i ? { ...e, scopesText: typed, provider: { ...e.provider, scopes: parseScopes(typed) } } : e,
      ),
    );
  const remove = (i: number) => change(entries.filter((_, j) => j !== i));
  const value = entries.map((e) => e.provider);
  const problems = providerProblems(value);
  return (
    <div className="providers">
      {value.length === 0 && <p className="muted">None: people sign in with tokens and certificates only.</p>}
      {entries.map(({ key, provider: x, scopesText }, i) => {
        const error = (k: ProviderField): string | undefined => {
          const found = problems[i]?.[k];
          return found !== undefined && (showAll || edited.has(`${key}.${k}`)) ? sentence(found) : undefined;
        };
        return (
          <fieldset key={key} className="group">
            <legend>
              Identity provider {i + 1}
              {i === 0 && value.length > 1 ? " (Headlamp and kubectl sign in with this one)" : ""}
            </legend>
            <fieldset className="bare" disabled={fixed}>
              <Field label="Issuer URL" hint="Exactly as the provider's tokens name it." error={error("issuerURL")}>
                {(id) => (
                  <input
                    id={id}
                    value={x.issuerURL}
                    placeholder="https://"
                    ref={(el) => {
                      if (el && key === added) {
                        el.focus();
                        setAdded(undefined);
                      }
                    }}
                    onChange={(e) => update(i, { issuerURL: e.target.value })}
                  />
                )}
              </Field>
              <Field
                label="Client ID"
                hint="Tokens must be issued to this client. Register it as a public client with PKCE."
                error={error("clientID")}
              >
                {(id) => <input id={id} value={x.clientID} onChange={(e) => update(i, { clientID: e.target.value })} />}
              </Field>
              <Field label="User name claim" hint="Empty: the one shown.">
                {(id) => (
                  <input
                    id={id}
                    value={x.usernameClaim ?? ""}
                    placeholder={defaults.usernameClaim}
                    onChange={(e) => update(i, { usernameClaim: e.target.value })}
                  />
                )}
              </Field>
              <Field
                label="User name prefix"
                hint="Put before every user name, so none passes for a user of another kind. Empty: no prefix."
                error={error("usernamePrefix")}
              >
                {(id) => (
                  <input
                    id={id}
                    value={x.usernamePrefix ?? defaults.usernamePrefix}
                    onChange={(e) => update(i, { usernamePrefix: e.target.value })}
                  />
                )}
              </Field>
              <Field label="Groups claim" hint="Empty: the one shown.">
                {(id) => (
                  <input
                    id={id}
                    value={x.groupsClaim ?? ""}
                    placeholder={defaults.groupsClaim}
                    onChange={(e) => update(i, { groupsClaim: e.target.value })}
                  />
                )}
              </Field>
              <Field label="Groups prefix" hint="Empty: no prefix." error={error("groupsPrefix")}>
                {(id) => (
                  <input
                    id={id}
                    value={x.groupsPrefix ?? defaults.groupsPrefix}
                    onChange={(e) => update(i, { groupsPrefix: e.target.value })}
                  />
                )}
              </Field>
              <Field
                label="Scopes"
                hint="Asked for besides openid when signing in, separated by commas or spaces. Empty: the ones shown."
              >
                {(id) => (
                  <input
                    id={id}
                    value={scopesText}
                    placeholder={defaults.scopes.join(", ")}
                    onChange={(e) => updateScopes(i, e.target.value)}
                  />
                )}
              </Field>
              <Field
                label="CA certificate"
                hint="PEM, when the provider's certificate is not signed by a public CA."
                error={error("certificateAuthority")}
              >
                {(id) => (
                  <textarea
                    id={id}
                    rows={3}
                    value={x.certificateAuthority ?? ""}
                    onChange={(e) => update(i, { certificateAuthority: e.target.value })}
                  />
                )}
              </Field>
            </fieldset>
            <button type="button" onClick={() => remove(i)}>
              Remove identity provider {i + 1}
            </button>
          </fieldset>
        );
      })}
      {fixed && (
        <p className="hint" id={fixedHint}>
          These can be kept or removed. Adding or changing one needs Sys.Modify on the pool, as whoever runs a provider
          can sign in as anyone.
        </p>
      )}
      {value.length < maxProviders && (
        <button
          type="button"
          disabled={fixed}
          aria-describedby={fixed ? fixedHint : undefined}
          onClick={() => {
            const fresh = entry({ issuerURL: "", clientID: "" });
            setAdded(fresh.key);
            change([...entries, fresh]);
          }}
        >
          Add identity provider
        </button>
      )}
    </div>
  );
}
