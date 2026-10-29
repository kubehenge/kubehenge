import { lazy, type JSX } from "react";

import { adminPath, adminViews, type AdminView } from "./admin-views";
import { ClusterDetail, clusterPath, views, type View } from "./ClusterDetail";
import { ClusterList } from "./ClusterList";
import { Link, match } from "./router";

// Pages only some people open, now and then, load when first opened: whoever only looks at clusters
// never fetches them.
const RunPage = lazy(async () => {
  const m = await import("./RunPage");
  return { default: m.RunPage };
});
const Administration = lazy(async () => {
  const m = await import("./Administration");
  return { default: m.Administration };
});
const Setup = lazy(async () => {
  const m = await import("./Setup");
  return { default: m.Setup };
});
const CreateCluster = lazy(async () => {
  const m = await import("./CreateCluster");
  return { default: m.CreateCluster };
});

export type Route =
  | { page: "loading" | "setup" | "forbidden" | "create" | "list" | "none" }
  | { page: "administration"; view: AdminView }
  | { page: "cluster"; namespace: string; name: string; view: View }
  | { page: "run"; id: string };

export interface At {
  route: Route;
  title: string;
  // A page keeps its state across its own views: only another page starts afresh.
  key: string;
}

/** routeAt is the page at a path, with its title. */
export function routeAt(path: string, needsSetup: boolean | undefined, canManage: boolean): At {
  if (needsSetup === undefined) {
    return { route: { page: "loading" }, title: "Kubehenge", key: path };
  }
  if (needsSetup) {
    return { route: { page: "setup" }, title: "Set up", key: path };
  }
  const admin = adminViews.find((v) => adminPath(v.path) === path);
  if (admin !== undefined) {
    return {
      route: canManage ? { page: "administration", view: admin.path } : { page: "forbidden" },
      title: admin.path === "" ? "Administration" : `Administration: ${admin.label}`,
      key: "/administration",
    };
  }
  if (path === "/clusters/new") {
    return { route: { page: "create" }, title: "Create cluster", key: path };
  }
  const detail = match("/clusters/:namespace/:name", path) ?? match("/clusters/:namespace/:name/:view", path);
  const view = views.find((v) => v.path === (detail?.["view"] ?? ""));
  const namespace = detail?.["namespace"];
  const name = detail?.["name"];
  if (view && namespace !== undefined && name !== undefined) {
    return {
      route: { page: "cluster", namespace, name, view: view.path },
      title: view.path ? `${name}: ${view.label}` : name,
      key: clusterPath(namespace, name),
    };
  }
  // Every cluster's address starts /clusters/, so the bare /clusters is where people look for the list.
  if (path === "/" || path === "/clusters") {
    return { route: { page: "list" }, title: "Clusters", key: "/" };
  }
  if (path.startsWith("/runs/") && path.length > "/runs/".length) {
    return { route: { page: "run", id: path.slice("/runs/".length) }, title: "Run", key: path };
  }
  return { route: { page: "none" }, title: "No such page", key: path };
}

/** RoutePage is the page a route names. */
export function RoutePage({
  route,
  canCreate,
  canTrust,
  namespaces,
  headlamp,
  onSetupChanged,
}: {
  route: Route;
  canCreate: boolean | undefined;
  canTrust: boolean | undefined;
  namespaces: string[];
  headlamp: number | undefined;
  onSetupChanged: () => void;
}): JSX.Element {
  switch (route.page) {
    case "loading": {
      return <p className="loading">Loading…</p>;
    }
    case "setup": {
      return <Setup onReady={onSetupChanged} />;
    }
    case "administration": {
      return <Administration view={route.view} onGone={onSetupChanged} />;
    }
    case "forbidden": {
      return <NoPage title="Administrators only">Needs the Administrator role on / in Proxmox VE.</NoPage>;
    }
    case "create": {
      return <CreateCluster canTrust={canTrust} namespaces={namespaces} />;
    }
    case "cluster": {
      return <ClusterDetail namespace={route.namespace} name={route.name} view={route.view} headlamp={headlamp} />;
    }
    case "list": {
      return <ClusterList canCreate={canCreate} />;
    }
    case "run": {
      return <RunPage id={route.id} />;
    }
    case "none": {
      break;
    }
  }
  return <NoPage />;
}

function NoPage({ title = "No such page", children }: { title?: string; children?: string }): JSX.Element {
  return (
    <section className="panel">
      <div className="toolbar">
        <h2>{title}</h2>
      </div>
      <p className="pad">
        {children !== undefined && `${children} `}
        <Link to="/">Go to clusters</Link>
      </p>
    </section>
  );
}
