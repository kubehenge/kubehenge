import { useEffect, useRef, useState, type JSX, type ReactNode, type RefObject } from "react";

import { adminPath, adminViews } from "./admin-views";
import { clusterActions, clusterPath, headlampURL, intentPath, views, type View } from "./cluster-paths";
import { FleetRail, ranked, useFleet } from "./fleet";
import logo from "./logo.svg";
import { Palette, ShortcutsDialog, useShortcuts, type Command, type Shortcut } from "./Palette";
import { Link, navigate } from "./router";
import { RunsPanel, useRuns } from "./Runs";
import { setTheme, themes, useTheme, type Theme } from "./theme";
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
// Below this width the account controls move into a menu (styles.css, the 900px media query).
const narrowBar = 900;

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
    { keys: "/", what: "Go to or run", run: open.palette },
    { keys: "g c", what: "Clusters", run: () => navigate("/") },
    { keys: "g n", what: "New cluster", on: canCreate !== false, run: () => navigate("/clusters/new") },
    { keys: "g a", what: "Administration", on: canManage, run: () => navigate("/administration") },
    { keys: "[", what: "Previous cluster", run: () => open.step(-1) },
    { keys: "]", what: "Next cluster", run: () => open.step(1) },
    ...viewKeys.map(({ view, key }) => ({
      keys: key,
      what: view.label,
      on: here !== undefined,
      run: () => {
        if (here) {
          navigate(clusterPath(here.namespace, here.name, view.path));
        }
      },
    })),
    { keys: "t r", what: "Show or hide runs", run: open.runs },
    { keys: "?", what: "Keyboard shortcuts", run: open.keys },
  ];
}

function pageCommands(canCreate: boolean | undefined, canManage: boolean): Command[] {
  const commands: Command[] = [
    { id: "clusters", label: "Clusters", kind: "Page", keys: "G C", run: () => navigate("/") },
  ];
  if (canCreate !== false) {
    commands.push({
      id: "new",
      label: "New cluster",
      kind: "Action",
      keys: "G N",
      words: "create",
      run: () => navigate("/clusters/new"),
    });
  }
  if (canManage) {
    commands.push({
      id: "administration",
      label: "Administration",
      kind: "Page",
      keys: "G A",
      run: () => navigate("/administration"),
    });
    for (const v of adminViews) {
      commands.push({
        id: `administration/${v.path}`,
        label: `Administration: ${v.label}`,
        kind: "Page",
        words: v.words,
        run: () => navigate(adminPath(v.path)),
      });
    }
  }
  return commands;
}

// actionCommands are what the user may start on a cluster, on its page.
const actionCommands = (c: Cluster, id: string): Command[] =>
  clusterActions(c).map((a) => ({
    id: `${id}-${a.intent}`,
    label: `${shown(c)}: ${a.label}`,
    kind: "Action",
    run: () => navigate(intentPath(c, a.intent)),
  }));

// clusterCommands are the open cluster's views and actions first, then every cluster, then every
// cluster's views and actions.
function clusterCommands(order: Cluster[], here: Here | undefined): Command[] {
  const open = order.find((c) => c.namespace === here?.namespace && c.name === here.name);
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
  const hereActions = open === undefined ? [] : actionCommands(open, "here-do");
  const everyAction = order.flatMap((c) => actionCommands(c, `do-${c.namespace}/${c.name}`));
  return [...hereViews, ...hereActions, ...clusters, ...everyView, ...everyAction];
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
    ...themes.map((t): Command => ({
      id: `theme-${t.theme}`,
      label: `${t.label} theme`,
      kind: "Action",
      run: () => setTheme(t.theme),
    })),
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
        running={runs.filter((r) => r.group === "running").length}
        anyRuns={runs.length > 0}
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
  anyRuns,
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
  anyRuns: boolean;
  runsShown: boolean | undefined;
  open: Opens;
  onSignOut: () => void;
}): JSX.Element {
  const account = useRef<HTMLDivElement>(null);
  // An account menu left open while the window widens would float over the bar it now sits in.
  useEffect(() => {
    const close = (): void => {
      const menu = account.current;
      if (window.innerWidth >= narrowBar && menu !== null && "hidePopover" in menu) {
        menu.hidePopover();
      }
    };
    window.addEventListener("resize", close);
    return () => {
      window.removeEventListener("resize", close);
    };
  }, []);
  return (
    <header className="topbar">
      <Link to="/" className="brand">
        <img src={logo} alt="" />
        <span className="brand-name">Kubehenge</span>
      </Link>
      {!settingUp && (
        <>
          <nav className="nav" aria-label="Pages">
            <Link to="/" current={path === "/" || path === "/clusters" || path.startsWith("/clusters/")}>
              Clusters
            </Link>
            {canManage && (
              <Link to="/administration" current={path === "/administration" || path.startsWith("/administration/")}>
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
      {!settingUp && anyRuns && (
        <button type="button" className="runs-toggle" aria-pressed={runsShown} onClick={() => open.runs()}>
          <span className={`dot dot-${running > 0 ? "progress" : "idle"}`} aria-hidden="true" />{" "}
          <span className="runs-word">Running</span> {running}
        </button>
      )}
      <button type="button" className="account-toggle icon" popoverTarget="account" aria-label="Account">
        <svg viewBox="0 0 16 16" aria-hidden="true">
          <path d="M2 4h12M2 8h12M2 12h12" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
        </svg>
      </button>
      {/* One set of controls: in the bar on wide screens, in a popover behind the button above on narrow ones. */}
      <div id="account" ref={account} className="account" popover="auto">
        {headlamp !== undefined && (
          <a href={headlampURL(headlamp)} target="_blank" rel="noopener noreferrer">
            Headlamp
          </a>
        )}
        <ThemeButton />
        <span className="user">{user}</span>
        <button type="button" onClick={onSignOut}>
          Sign out
        </button>
      </div>
    </header>
  );
}

const themeIcons: Record<Theme, JSX.Element> = {
  system: (
    <svg viewBox="0 0 16 16" aria-hidden="true">
      <circle cx="8" cy="8" r="6" fill="none" stroke="currentColor" strokeWidth="1.5" />
      <path d="M8 2a6 6 0 0 1 0 12z" fill="currentColor" />
    </svg>
  ),
  light: (
    <svg viewBox="0 0 16 16" aria-hidden="true">
      <circle cx="8" cy="8" r="3" fill="currentColor" />
      <path
        d="M8 1v2M8 13v2M1 8h2M13 8h2M3 3l1.4 1.4M11.6 11.6 13 13M3 13l1.4-1.4M11.6 4.4 13 3"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinecap="round"
      />
    </svg>
  ),
  dark: (
    <svg viewBox="0 0 16 16" aria-hidden="true">
      <path d="M13.5 10.2A6 6 0 0 1 5.8 2.5a6 6 0 1 0 7.7 7.7z" fill="currentColor" />
    </svg>
  ),
};

// ThemeButton goes round the themes: the system's, light, dark.
function ThemeButton(): JSX.Element {
  const theme = useTheme();
  const at = themes.findIndex((t) => t.theme === theme);
  const next = themes[(at + 1) % themes.length] ?? themes[0];
  const label = `Theme: ${themes[at]?.label ?? "System"}`;
  return (
    <button
      type="button"
      className="theme-toggle"
      aria-label={label}
      title={label}
      onClick={() => next && setTheme(next.theme)}
    >
      {themeIcons[theme]}
      <span className="theme-label" aria-hidden="true">
        {label}
      </span>
    </button>
  );
}
