import { useState, type JSX, type ReactNode, type RefObject } from "react";

import { clusterPath, headlampURL, views, type View } from "./ClusterDetail";
import { FleetRail, ranked, useFleet } from "./fleet";
import logo from "./logo.svg";
import { Palette, ShortcutsDialog, useShortcuts, type Command, type Shortcut } from "./Palette";
import { Link, navigate } from "./router";
import { RunsPanel, useRuns } from "./Runs";
import type { Cluster } from "./types";

/** The cluster whose page is open, and which of its views. */
export interface Here {
  namespace: string;
  name: string;
  view: View;
}

// The views of a cluster's page, by the digit that opens each.
const viewKeys = views.map((v, i) => ({ view: v, key: String(i + 1) }));

// From this window width on the running work shows beside the page, unless folded away.
const wide = "(min-width: 1280px)";

const shown = (c: Cluster): string => (c.namespace === "default" ? c.name : `${c.namespace}/${c.name}`);

// nextCluster is the cluster `by` places on from the open one in the rail's order, going round.
function nextCluster(order: Cluster[], here: Here | undefined, by: number): Cluster | undefined {
  const at = order.findIndex((c) => c.namespace === here?.namespace && c.name === here.name);
  if (at === -1) {
    return by > 0 ? order[0] : order.at(-1);
  }
  return order[(at + by + order.length) % order.length];
}

interface Opens {
  palette: () => void;
  keys: () => void;
  runs: () => void;
  step: (by: number) => void;
}

function shortcutsFor(
  here: Here | undefined,
  may: { canCreate: boolean | undefined; canManage: boolean },
  open: Opens,
): Shortcut[] {
  const { canCreate, canManage } = may;
  return [
    { keys: "/", what: "Go to or run, as Ctrl K does", run: open.palette },
    { keys: "c", what: "The clusters", run: () => navigate("/") },
    { keys: "n", what: "New cluster", on: canCreate !== false, run: () => navigate("/clusters/new") },
    { keys: "a", what: "Administration", on: canManage, run: () => navigate("/administration") },
    { keys: "[", what: "The cluster before in the fleet", run: () => open.step(-1) },
    { keys: "]", what: "The next cluster in the fleet", run: () => open.step(1) },
    ...viewKeys.map(({ view, key }) => ({
      keys: key,
      what: `The cluster's ${view.label}`,
      on: here !== undefined,
      run: () => {
        if (here) {
          navigate(clusterPath(here.namespace, here.name, view.path));
        }
      },
    })),
    { keys: "r", what: "Show or fold away the running work", run: open.runs },
    { keys: "?", what: "These keys", run: open.keys },
  ];
}

function pageCommands(canCreate: boolean | undefined, canManage: boolean): Command[] {
  const commands: Command[] = [
    { id: "clusters", label: "Clusters", kind: "Page", keys: "C", run: () => navigate("/") },
  ];
  if (canCreate !== false) {
    commands.push({
      id: "new",
      label: "New cluster",
      kind: "Action",
      keys: "N",
      words: "create",
      run: () => navigate("/clusters/new"),
    });
  }
  if (canManage) {
    commands.push({
      id: "administration",
      label: "Administration",
      kind: "Page",
      keys: "A",
      words: "appliance templates management credentials",
      run: () => navigate("/administration"),
    });
  }
  return commands;
}

// clusterCommands are the open cluster's views first, then every cluster, then every cluster's views.
function clusterCommands(order: Cluster[], here: Here | undefined): Command[] {
  const hereViews: Command[] = here
    ? viewKeys.map(({ view, key }) => ({
        id: `here-${view.path}`,
        label: `${here.name}: ${view.label}`,
        kind: "View",
        keys: key,
        run: () => navigate(clusterPath(here.namespace, here.name, view.path)),
      }))
    : [];
  const clusters = order.map((c) => ({
    id: `cluster-${c.namespace}/${c.name}`,
    label: shown(c),
    kind: "Cluster",
    words: `${c.distribution} ${c.version}`,
    run: () => navigate(clusterPath(c.namespace, c.name)),
  }));
  const everyView = order.flatMap((c) =>
    views
      .filter((v) => v.path !== "")
      .map((v) => ({
        id: `view-${c.namespace}/${c.name}/${v.path}`,
        label: `${shown(c)}: ${v.label}`,
        kind: "View",
        run: () => navigate(clusterPath(c.namespace, c.name, v.path)),
      })),
  );
  return [...hereViews, ...clusters, ...everyView];
}

/**
 * Console is the frame round every page: the top bar, the fleet rail on the left, the running work
 * on the right, the palette and the keys.
 */
export function Console({
  user,
  headlamp,
  canCreate,
  canManage,
  settingUp,
  path,
  here,
  main,
  onSignOut,
  children,
}: {
  user: string;
  headlamp: number | undefined;
  canCreate: boolean | undefined;
  canManage: boolean;
  settingUp: boolean;
  path: string;
  here: Here | undefined;
  main: RefObject<HTMLElement | null>;
  onSignOut: () => void;
  children: ReactNode;
}): JSX.Element {
  const fleet = useFleet();
  const runs = useRuns();
  const [palette, setPalette] = useState(false);
  const [keys, setKeys] = useState(false);
  // undefined follows the window's width; true or false is the user's own choice.
  const [runsShown, setRunsShown] = useState<boolean>();
  const order = fleet?.clusters === undefined ? [] : ranked(fleet.clusters, fleet.health).map((r) => r.cluster);
  const port = headlamp !== undefined && headlamp > 0 && !settingUp ? headlamp : undefined;

  const open: Opens = {
    palette: () => setPalette(true),
    keys: () => setKeys(true),
    runs: () => setRunsShown((was) => !(was ?? window.matchMedia(wide).matches)),
    step: (by) => {
      const next = nextCluster(order, here, by);
      if (next) {
        navigate(clusterPath(next.namespace, next.name, here?.view ?? ""));
      }
    },
  };
  const shortcuts = shortcutsFor(here, { canCreate, canManage }, open);
  useShortcuts(open.palette, settingUp ? [] : shortcuts);

  const commands: Command[] = [
    ...pageCommands(canCreate, canManage),
    ...clusterCommands(order, here),
    ...(port === undefined
      ? []
      : [
          {
            id: "headlamp",
            label: "Open Headlamp",
            kind: "Link",
            run: () => window.open(headlampURL(port), "_blank", "noopener"),
          },
        ]),
    { id: "keys", label: "Keyboard shortcuts", kind: "Help", keys: "?", run: open.keys },
    { id: "sign-out", label: "Sign out", kind: "Action", run: onSignOut },
  ];

  const classes = ["shell"];
  if (settingUp) {
    classes.push("shell-setup");
  }
  if (runsShown !== undefined) {
    classes.push(runsShown ? "runs-shown" : "runs-folded");
  }
  return (
    <div className={classes.join(" ")}>
      <TopBar
        user={user}
        headlamp={port}
        canManage={canManage}
        settingUp={settingUp}
        path={path}
        running={runs.length}
        runsShown={runsShown}
        open={open}
        onSignOut={onSignOut}
      />
      {!settingUp && <FleetRail current={here} canCreate={canCreate} onCreate={() => navigate("/clusters/new")} />}
      <main className="content" ref={main} tabIndex={-1}>
        {children}
      </main>
      {!settingUp && runs.length > 0 && <RunsPanel runs={runs} onFold={() => setRunsShown(false)} />}
      {palette && <Palette commands={commands} onClose={() => setPalette(false)} />}
      {keys && <ShortcutsDialog shortcuts={shortcuts.filter((s) => s.on !== false)} onClose={() => setKeys(false)} />}
    </div>
  );
}

function TopBar({
  user,
  headlamp,
  canManage,
  settingUp,
  path,
  running,
  runsShown,
  open,
  onSignOut,
}: {
  user: string;
  headlamp: number | undefined;
  canManage: boolean;
  settingUp: boolean;
  path: string;
  running: number;
  runsShown: boolean | undefined;
  open: Opens;
  onSignOut: () => void;
}): JSX.Element {
  return (
    <header className="topbar">
      <Link to="/" className="brand">
        <img src={logo} alt="" />
        Kubehenge
      </Link>
      {!settingUp && (
        <>
          <nav className="nav" aria-label="Pages">
            <Link to="/" current={path === "/" || path.startsWith("/clusters/")}>
              Clusters
            </Link>
            {canManage && (
              <Link to="/administration" current={path === "/administration"}>
                Administration
              </Link>
            )}
          </nav>
          <button type="button" className="go" onClick={() => open.palette()}>
            <span>Go to or run…</span>
            <kbd aria-hidden="true">Ctrl K</kbd>
          </button>
        </>
      )}
      <span className="spacer" />
      {!settingUp && running > 0 && (
        <button type="button" className="runs-toggle" aria-pressed={runsShown} onClick={() => open.runs()}>
          <span className="dot dot-progress" aria-hidden="true" /> Running {running}
        </button>
      )}
      {headlamp !== undefined && (
        <a href={headlampURL(headlamp)} target="_blank" rel="noopener noreferrer">
          Headlamp
        </a>
      )}
      <span className="user">{user}</span>
      <button type="button" onClick={onSignOut}>
        Sign out
      </button>
    </header>
  );
}
