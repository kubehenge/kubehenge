import { useEffect, useState, type JSX } from "react";

import { adminPath } from "./admin-views";
import { api } from "./api";
import { Link } from "./router";
import type { ClusterClass } from "./types";
import { Status, distributionName } from "./ui";

// hasTemplate is whether a class has a VM template to build clusters from, on at least one node OS.
const hasTemplate = (c: ClusterClass): boolean =>
  (c.nodeOSes ?? []).length > 0
    ? (c.nodeOSes ?? []).some((o) => o.noTemplate === undefined)
    : c.noTemplate === undefined;

/** withTemplate are the distributions with a VM template to build clusters from. */
export const withTemplate = (classes: ClusterClass[]): string[] =>
  classes.filter((c) => hasTemplate(c)).map((c) => distributionName(c.distribution));

// The checks read the classes once; undefined until then, and on failure, where the row stays out.
function useClasses(): ClusterClass[] | undefined {
  const [classes, setClasses] = useState<ClusterClass[]>();
  useEffect(() => {
    let current = true;
    async function load(): Promise<void> {
      try {
        const listed = await api.classes();
        if (current) {
          setClasses(listed);
        }
      } catch {
        // Creating says what is wrong with the classes in any case.
      }
    }
    void load();
    return () => {
      current = false;
    };
  }, []);
  return classes;
}

/** FirstCluster is the fleet list while there is no cluster: what a first one needs, each ready or not. */
export function FirstCluster({
  canCreate,
  canManage,
}: {
  canCreate: boolean | undefined;
  canManage: boolean;
}): JSX.Element {
  const classes = useClasses();
  const built = classes === undefined ? undefined : withTemplate(classes);
  const refused = canCreate === false;
  return (
    <div className="pad first-cluster">
      <dl className="preflight" aria-label="First cluster">
        {built !== undefined && (
          <div>
            <dt>VM template</dt>
            <dd>
              {built.length > 0 ? (
                <Status state="ok" text={built.join(", ")} />
              ) : (
                <>
                  <Status state="warning" text="None yet" />
                  {canManage && <Link to={adminPath("templates")}>Build one</Link>}
                </>
              )}
            </dd>
          </div>
        )}
        <div>
          <dt>Rights</dt>
          <dd>
            <Status state={refused ? "warning" : "ok"} text={refused ? "Needs VM.Allocate" : "May create clusters"} />
          </dd>
        </div>
        <div>
          <dt>Clusters</dt>
          <dd>
            <Status state="idle" text="None yet" />
          </dd>
        </div>
      </dl>
      <p className="muted">Seeing a cluster needs VM.Audit on its pool.</p>
    </div>
  );
}
