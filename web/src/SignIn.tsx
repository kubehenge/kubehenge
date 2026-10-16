import { useEffect, useLayoutEffect, useRef, useState, type JSX, type SubmitEvent } from "react";

import { api } from "./api";
import type { Realm, SecondFactor, Session } from "./types";
import { ErrorBox, Field, errorMessage, filled } from "./ui";

// The realm last signed in with is the next sign-in's choice, as in Proxmox VE's own dialog: on a
// host whose people sign in with pve, the realm marked default (or else pam) would otherwise have to
// be changed every time, and a password manager fills in only the user name and password.
const realmKey = "kubehenge-realm";

function rememberRealm(realm: string): void {
  try {
    localStorage.setItem(realmKey, realm);
  } catch {
    // Storage the browser refuses only costs the choice next time.
  }
}

function rememberedRealm(): string | null {
  try {
    return localStorage.getItem(realmKey);
  } catch {
    return null;
  }
}

// The fields follow Proxmox VE's own sign-in dialog: user name, password, then realm.
export function SignIn({
  onSignedIn,
  notice,
}: {
  onSignedIn: (s: Session) => void;
  notice?: string | undefined;
}): JSX.Element {
  const [realms, setRealms] = useState<Realm[]>([]);
  const [username, setUsername] = useState("");
  const [realm, setRealm] = useState("");
  const [password, setPassword] = useState("");
  const [factor, setFactor] = useState<SecondFactor>();
  const [busy, setBusy] = useState(false);
  // An OpenID sign-in that failed comes back with its reason.
  const [error, setError] = useState<string | undefined>(
    () => new URLSearchParams(window.location.search).get("signin-error") ?? undefined,
  );
  const chosen = realms.find((r) => r.realm === realm);
  const openID = chosen?.type === "openid";
  const first = useRef<HTMLInputElement>(null);

  // Before the browser paints, so the form never shows without its focus.
  useLayoutEffect(() => {
    first.current?.focus();
  }, []);

  useEffect(() => {
    if (window.location.search.includes("signin-error")) {
      window.history.replaceState(null, "", "/");
    }
    async function loadRealms(): Promise<void> {
      try {
        const r = await api.realms();
        setRealms(r);
        const last = rememberedRealm();
        const initial = r.find((x) => x.realm === last) ?? r.find((x) => x.default === true) ?? r[0];
        setRealm((current) => (current === "" ? (initial?.realm ?? "") : current));
      } catch (err) {
        setError(errorMessage(err));
      }
    }
    void loadRealms();
  }, []);

  const submit = async (): Promise<void> => {
    setBusy(true);
    setError(undefined);
    try {
      const s = await api.signIn(username, username.includes("@") ? "" : realm, password);
      setPassword("");
      rememberRealm(s.user.slice(s.user.lastIndexOf("@") + 1));
      if (s.secondFactor) {
        setFactor(s.secondFactor);
      } else {
        onSignedIn(s);
      }
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="signin">
      <div className="signin-panel">
        <h1 className="brand">Kubehenge</h1>
        {filled(notice) && <output className="warning notice">{notice}</output>}
        {factor ? (
          <SecondFactorForm factor={factor} onSignedIn={onSignedIn} onCancel={() => setFactor(undefined)} />
        ) : (
          <form
            onSubmit={(ev: SubmitEvent<HTMLFormElement>) => {
              ev.preventDefault();
              void submit();
            }}
          >
            <p className="muted">Sign in with your Proxmox VE account.</p>
            {!openID && (
              <>
                <Field label="User name" hint="or user@realm">
                  {(id) => (
                    <input
                      id={id}
                      ref={first}
                      autoComplete="username"
                      value={username}
                      onChange={(e) => setUsername(e.target.value)}
                      required
                    />
                  )}
                </Field>
                <Field label="Password">
                  {(id) => (
                    <input
                      id={id}
                      type="password"
                      autoComplete="current-password"
                      value={password}
                      onChange={(e) => setPassword(e.target.value)}
                      required
                    />
                  )}
                </Field>
              </>
            )}
            <Field label="Realm" hint={username.includes("@") ? "Taken from the user name." : undefined}>
              {(id) => (
                <select
                  id={id}
                  value={realm}
                  onChange={(e) => setRealm(e.target.value)}
                  disabled={username.includes("@")}
                >
                  {realms.map((r) => (
                    <option key={r.realm} value={r.realm}>
                      {filled(r.comment) ? `${r.comment} (${r.realm})` : r.realm}
                    </option>
                  ))}
                </select>
              )}
            </Field>
            <ErrorBox message={error} />
            <div className="actions">
              {openID ? (
                <a
                  className="button primary"
                  href={`/api/openid/start?realm=${encodeURIComponent(realm)}`}
                  onClick={() => rememberRealm(realm)}
                >
                  Continue with {filled(chosen.comment) ? chosen.comment : realm}
                </a>
              ) : (
                <button type="submit" className="primary" disabled={busy}>
                  {busy ? "Signing in…" : "Sign in"}
                </button>
              )}
            </div>
          </form>
        )}
      </div>
    </div>
  );
}

const factorKinds = [
  { kind: "totp", choice: "Authenticator app (TOTP)", label: "TOTP code" },
  { kind: "yubico", choice: "YubiKey (Yubico OTP)", label: "Yubico OTP" },
  { kind: "recovery", choice: "A recovery key", label: "Recovery key" },
] as const;

function SecondFactorForm({
  factor,
  onSignedIn,
  onCancel,
}: {
  factor: SecondFactor;
  onSignedIn: (s: Session) => void;
  onCancel: () => void;
}): JSX.Element {
  const kinds = factorKinds.filter(
    (k) =>
      (k.kind === "totp" && factor.totp) ||
      (k.kind === "yubico" && factor.yubico) ||
      (k.kind === "recovery" && (factor.recovery?.length ?? 0) > 0),
  );
  const [kind, setKind] = useState<string>(kinds[0]?.kind ?? "totp");
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const input = useRef<HTMLInputElement>(null);

  useLayoutEffect(() => {
    input.current?.focus();
  }, []);

  const submit = async (): Promise<void> => {
    setBusy(true);
    setError(undefined);
    try {
      onSignedIn(await api.secondFactor(kind, code));
    } catch (err) {
      setError(errorMessage(err));
      setCode("");
      input.current?.focus();
    } finally {
      setBusy(false);
    }
  };

  return (
    <form
      onSubmit={(ev: SubmitEvent<HTMLFormElement>) => {
        ev.preventDefault();
        void submit();
      }}
    >
      <p className="muted">Your account has a second factor.</p>
      {kinds.length > 1 && (
        <fieldset className="choices">
          <legend>Answer with</legend>
          {kinds.map((k) => (
            <label key={k.kind} className="check">
              <input
                type="radio"
                name="factor"
                value={k.kind}
                checked={k.kind === kind}
                onChange={() => setKind(k.kind)}
              />
              {k.choice}
            </label>
          ))}
        </fieldset>
      )}
      <Field
        label={kinds.find((k) => k.kind === kind)?.label ?? "Code"}
        hint={
          kind === "recovery" && factor.recovery
            ? `Unused keys: ${factor.recovery.map((i) => i + 1).join(", ")}`
            : undefined
        }
      >
        {(id) => (
          <input
            id={id}
            ref={input}
            autoComplete="one-time-code"
            inputMode={kind === "totp" ? "numeric" : "text"}
            value={code}
            onChange={(e) => setCode(e.target.value)}
            required
          />
        )}
      </Field>
      <ErrorBox message={error} />
      <div className="actions">
        <button type="button" onClick={onCancel}>
          Back
        </button>
        <button type="submit" className="primary" disabled={busy}>
          {busy ? "Checking…" : "Confirm"}
        </button>
      </div>
    </form>
  );
}
