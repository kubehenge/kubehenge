import { Suspense, useCallback, useEffect, useEffectEvent, useLayoutEffect, useRef, useState, type JSX } from "react";

import { ApiError, api, setSignedOutHandler, setUpgradedHandler } from "./api";
import { Console } from "./Console";
import { FleetProvider } from "./fleet";
import { RoutePage, routeAt } from "./Pages";
import { usePath } from "./router";
import { SignIn } from "./SignIn";
import type { Session, SetupStatus } from "./types";
import { ErrorBoundary } from "./ui";

const msPerDay = 86_400_000;

// Administrators are warned this many days before the certificate runs out; it is renewed on the node.
const certificateWarningDays = 30;

// A setup status that could not be read is asked for again after this long.
const setupRetry = 5000;

// Answers from this status on are the server's own failures, which pass.
const firstServerError = 500;

export function App(): JSX.Element {
  // undefined while the session is being looked up, null when signed out.
  const [session, setSession] = useState<Session | null>();
  // Why the sign-in page shows again, when it was not the user's own doing.
  const [notice, setNotice] = useState<string>();

  useEffect(() => {
    setSignedOutHandler(() => {
      setSession((was) => {
        if (was) {
          setNotice("Your session has ended. Sign in again to continue where you were.");
        }
        return null;
      });
    });
    async function lookUp(): Promise<void> {
      try {
        setSession(await api.session());
      } catch {
        // Any failure leads to the sign-in page, which reports the server being unreachable itself.
        setSession(null);
      }
    }
    void lookUp();
  }, []);

  useEffect(() => {
    if (session === null) {
      document.title = "Sign in - Kubehenge";
    }
  }, [session]);

  if (session === undefined) {
    return <p className="loading">Loading…</p>;
  }
  if (session === null) {
    return (
      <SignIn
        notice={notice}
        onSignedIn={(s) => {
          setNotice(undefined);
          setSession(s);
        }}
      />
    );
  }
  return (
    <Shell
      user={session.user}
      headlamp={session.headlamp}
      canCreate={session.canCreate}
      canTrust={session.canTrust}
      namespaces={session.namespaces ?? (session.namespace === undefined ? [] : [session.namespace])}
      onSignOut={() => setSession(null)}
    />
  );
}

function Shell({
  user,
  headlamp,
  canCreate,
  canTrust,
  namespaces,
  onSignOut,
}: {
  user: string;
  headlamp?: number | undefined;
  canCreate?: boolean | undefined;
  canTrust?: boolean | undefined;
  namespaces: string[];
  onSignOut: () => void;
}): JSX.Element {
  const path = usePath();
  const [setup, setSetup] = useState<SetupStatus>();
  const [certificate, setCertificate] = useState<{ expires: Date; expired: boolean }>();
  const [credentials, setCredentials] = useState<{ expires: Date; expired: boolean }>();
  const [upgradedTo, setUpgradedTo] = useState<string>();
  const retry = useRef<number>(undefined);
  // Counts the reads, so only the newest one's answer shows, and none after sign-out.
  const reading = useRef(0);

  // loadSetup reads the setup status: at the start, on readiness, after a removal, and again after
  // a passing failure.
  const loadSetup = useCallback(async function load(): Promise<void> {
    window.clearTimeout(retry.current);
    reading.current += 1;
    const read = reading.current;
    let s: SetupStatus;
    try {
      s = await api.setupStatus();
    } catch (err) {
      if (read !== reading.current) {
        return;
      }
      // The clusters show meanwhile, and the administrators' link and warnings come back once it
      // answers.
      setSetup((was) => was ?? { appliance: false, ready: true, allowed: false });
      if (!(err instanceof ApiError) || err.status === 0 || err.status >= firstServerError) {
        retry.current = window.setTimeout(() => {
          void load();
        }, setupRetry);
      }
      return;
    }
    if (read === reading.current) {
      setSetup(s);
      setCertificate(certificateWarning(s.certificateExpires));
      setCredentials(certificateWarning(s.credentialsExpire));
    }
  }, []);
  const reloadSetup = useCallback(() => {
    void loadSetup();
  }, [loadSetup]);
  const loadFirst = useEffectEvent(reloadSetup);

  useEffect(() => {
    setUpgradedHandler(setUpgradedTo);
    loadFirst();
    return () => {
      reading.current += 1;
      window.clearTimeout(retry.current);
    };
  }, []);

  // undefined until known; the appliance's first run sets up the management cluster.
  const needsSetup = setup === undefined ? undefined : setup.appliance && !setup.ready;
  // Administrators of an appliance look after it and the management cluster from their own page.
  const canManage = setup?.appliance === true && setup.allowed;
  const settingUp = needsSetup === true;

  const signOut = async (): Promise<void> => {
    try {
      await api.signOut();
    } finally {
      onSignOut();
    }
  };

  const { route, title, key: pageKey } = routeAt(path, needsSetup, canManage);

  useEffect(() => {
    document.title = title === "Kubehenge" ? title : `${title} - Kubehenge`;
  }, [title]);

  // After moving to another page the focus moves to its content, so a screen reader starts there
  // instead of staying on the link that was followed.
  const main = useRef<HTMLElement>(null);
  const shownPath = useRef(path);
  useLayoutEffect(() => {
    if (shownPath.current === path) {
      return;
    }
    shownPath.current = path;
    main.current?.focus();
  }, [path]);

  const page = (
    <>
      {upgradedTo !== undefined && (
        <output className="warning notice">
          Kubehenge was upgraded to {upgradedTo}. Reload the page to use the new version.{" "}
          <button type="button" onClick={() => window.location.reload()}>
            Reload
          </button>
        </output>
      )}
      {setup?.backup === false && !settingUp && (
        <p className="warning" role="alert">
          No Proxmox VE backup job includes this appliance, and it holds the only copy of the management cluster's
          encrypted state. Add it to a backup job under Datacenter → Backup.
        </p>
      )}
      {certificate && (
        <p className="warning" role="alert">
          {certificate.expired
            ? "This appliance's certificate expired on "
            : "This appliance's certificate expires on "}
          {certificate.expires.toLocaleDateString(undefined, { dateStyle: "long" })}. Renew it in the shell of the
          Proxmox VE node with Kubehenge's installer:{" "}
          <code>sh install.sh --certificate --ctid {setup?.applianceVMID ?? "<appliance ID>"}</code>.
        </p>
      )}
      {credentials && (
        <p className="warning" role="alert">
          Management cluster credentials {credentials.expired ? "expired" : "expire"} on{" "}
          {credentials.expires.toLocaleDateString(undefined, { dateStyle: "long" })}. Apply the settings in{" "}
          <a href="/administration">Administration</a> to renew them.
        </p>
      )}
      {/* Keyed by path: a page that failed recovers by navigating away, the top bar stays. */}
      <ErrorBoundary key={pageKey}>
        <Suspense fallback={<p className="loading">Loading…</p>}>
          <RoutePage
            route={route}
            canCreate={canCreate}
            canTrust={canTrust}
            namespaces={namespaces}
            headlamp={headlamp}
            onSetupChanged={reloadSetup}
          />
        </Suspense>
      </ErrorBoundary>
    </>
  );
  const frame = (
    <Console
      user={user}
      headlamp={headlamp}
      canCreate={canCreate}
      canManage={canManage}
      settingUp={needsSetup !== false}
      path={path}
      here={route.page === "cluster" ? route : undefined}
      main={main}
      onSignOut={() => {
        void signOut();
      }}
    >
      {page}
    </Console>
  );
  // The fleet is read once the appliance is set up, for every page alike.
  return <FleetProvider reads={needsSetup === false}>{frame}</FleetProvider>;
}

// certificateWarning is the warning about the appliance's certificate when it runs out within
// certificateWarningDays, or has.
function certificateWarning(expiry: string | undefined): { expires: Date; expired: boolean } | undefined {
  if (expiry === undefined || expiry === "") {
    return undefined;
  }
  const expires = new Date(expiry);
  const left = expires.getTime() - Date.now();
  return left < certificateWarningDays * msPerDay ? { expires, expired: left < 0 } : undefined;
}
