import { useId, useState, type JSX, type ReactNode } from "react";

import {
  argListNames,
  argLists,
  argProblems,
  carries,
  commandProblem,
  fileProblem,
  filePermissions,
  maxArgs,
  maxCommands,
  maxFiles,
  newFile,
  nodeCustomisationOf,
  nodeCustomisationProblem,
  nodeCustomisationVariable,
  nodeRoles,
  objectNameProblem,
  roleNames,
  sameNodeCustomisation,
  secretKeyProblem,
  type ArgList,
  type Carries,
  type NodeArg,
  type NodeCommand,
  type NodeCustomisation,
  type NodeFile,
  type NodeRoles,
} from "./node-customisation";
import { Field, Head } from "./NodeSettings";
import type { Cluster } from "./types";
import { Dialog, ErrorBox, useChange } from "./ui";
import { oneOf } from "./values";

/** Whether a row's problem shows: once its field was touched, or for every row with showAll. */
function useShown(showAll: boolean): {
  touch: (key: string) => void;
  reset: () => void;
  shown: (key: string, problem: string | undefined) => string | undefined;
} {
  const [touched, setTouched] = useState<ReadonlySet<string>>(new Set());
  return {
    touch: (key) => setTouched((s) => new Set([...s, key])),
    // Rows are edited in place and have no identity, so removing one clears what was touched.
    reset: () => setTouched(new Set()),
    shown: (key, problem) => (showAll || touched.has(key) ? problem : undefined),
  };
}

type Shown = ReturnType<typeof useShown>;

interface Editor<T> {
  value: T;
  onChange: (v: T) => void;
  rows: Shown;
  base: string;
}

function TextBox({
  label,
  value,
  invalid,
  describedBy,
  onChange,
  onBlur,
}: {
  label: string;
  value: string;
  invalid: boolean;
  describedBy: string;
  onChange: (v: string) => void;
  onBlur: () => void;
}): JSX.Element {
  return (
    <Field label={label}>
      <input
        aria-label={label}
        aria-invalid={invalid ? true : undefined}
        aria-describedby={invalid ? describedBy : undefined}
        value={value}
        autoComplete="off"
        spellCheck={false}
        onChange={(e) => onChange(e.target.value)}
        onBlur={onBlur}
      />
    </Field>
  );
}

function RolesSelect({ value, onChange }: { value: NodeRoles; onChange: (r: NodeRoles) => void }): JSX.Element {
  return (
    <Field label="Nodes">
      <select
        aria-label="Nodes"
        value={value}
        onChange={(e) => onChange(oneOf(nodeRoles, e.target.value) ? e.target.value : "both")}
      >
        {nodeRoles.map((r) => (
          <option key={r} value={r}>
            {roleNames[r]}
          </option>
        ))}
      </select>
    </Field>
  );
}

function Problem({ id, message }: { id: string; message: string | undefined }): JSX.Element | null {
  return message === undefined ? null : (
    <div className="field-error" id={id}>
      {message}
    </div>
  );
}

const replaceAt = <T,>(list: T[], i: number, patch: Partial<T>): T[] =>
  list.map((item, j) => (j === i ? { ...item, ...patch } : item));

function FileSource({
  file,
  can,
  label,
  update,
  invalid,
  describedBy,
  touch,
}: {
  file: NodeFile;
  can: Carries;
  label: string;
  update: (patch: Partial<NodeFile>) => void;
  invalid: boolean;
  describedBy: string;
  touch: () => void;
}): JSX.Element {
  return (
    <div className={`node-row source-row ${file.source === "content" ? "text" : ""}`}>
      <Field label="Source">
        <select
          aria-label={`${label} source`}
          value={file.source}
          disabled={!can.secrets}
          onChange={(e) => {
            touch();
            update({ source: e.target.value === "secret" ? "secret" : "content" });
          }}
        >
          <option value="content">Text</option>
          {can.secrets && <option value="secret">Secret</option>}
        </select>
      </Field>
      {file.source === "content" ? (
        <Field label="Text">
          <textarea
            aria-label={`${label} text`}
            aria-invalid={invalid ? true : undefined}
            aria-describedby={invalid ? describedBy : undefined}
            rows={4}
            spellCheck={false}
            value={file.content}
            onChange={(e) => {
              touch();
              update({ content: e.target.value });
            }}
            onBlur={touch}
          />
        </Field>
      ) : (
        <>
          <TextBox
            label="Secret name"
            value={file.secretName}
            invalid={invalid}
            describedBy={describedBy}
            onChange={(secretName) => update({ secretName })}
            onBlur={touch}
          />
          <TextBox
            label="Secret key"
            value={file.secretKey}
            invalid={invalid}
            describedBy={describedBy}
            onChange={(secretKey) => update({ secretKey })}
            onBlur={touch}
          />
        </>
      )}
    </div>
  );
}

function FileRow({
  file,
  index,
  editor,
  can,
  onRemove,
}: {
  file: NodeFile;
  index: number;
  editor: Editor<NodeFile[]>;
  can: Carries;
  onRemove: () => void;
}): JSX.Element {
  const { value, onChange, rows, base } = editor;
  const key = `files.${index}`;
  const label = `File ${index + 1}`;
  const id = `${base}-${key}`;
  const failed = rows.shown(key, fileProblem(file));
  const update = (patch: Partial<NodeFile>): void => {
    rows.touch(key);
    onChange(replaceAt(value, index, patch));
  };
  return (
    <fieldset aria-label={label} className="bare custom-file">
      <div className="node-row file-row">
        <TextBox
          label="Path"
          value={file.path}
          invalid={failed !== undefined}
          describedBy={id}
          onChange={(path) => update({ path })}
          onBlur={() => rows.touch(key)}
        />
        <Field label="Mode">
          <select
            aria-label={`${label} mode`}
            value={file.permissions}
            onChange={(e) => update({ permissions: e.target.value })}
          >
            {filePermissions.map((p) => (
              <option key={p} value={p}>
                {p}
              </option>
            ))}
          </select>
        </Field>
        <RolesSelect value={file.roles} onChange={(roles) => update({ roles })} />
        <button type="button" aria-label={`Remove file ${index + 1}`} onClick={onRemove}>
          Remove
        </button>
      </div>
      <FileSource
        file={file}
        can={can}
        label={label}
        update={update}
        invalid={failed !== undefined}
        describedBy={id}
        touch={() => rows.touch(key)}
      />
      <Problem id={id} message={failed} />
    </fieldset>
  );
}

function Files({ editor, can }: { editor: Editor<NodeFile[]>; can: Carries }): JSX.Element {
  const { value, onChange, rows } = editor;
  return (
    <fieldset className="group node-settings">
      <legend>Files</legend>
      <div className="node-rows labelled">
        {value.map((f, i) => (
          <FileRow
            // oxlint-disable-next-line react/no-array-index-key -- rows are edited in place
            key={i}
            file={f}
            index={i}
            editor={editor}
            can={can}
            onRemove={() => {
              rows.reset();
              onChange(value.filter((_, j) => j !== i));
            }}
          />
        ))}
      </div>
      {value.length === 0 && <p className="muted">No files</p>}
      <button type="button" disabled={value.length >= maxFiles} onClick={() => onChange([...value, newFile()])}>
        Add file
      </button>
    </fieldset>
  );
}

function CommandRow({
  command,
  index,
  editor,
}: {
  command: NodeCommand;
  index: number;
  editor: Editor<NodeCommand[]>;
}): JSX.Element {
  const { value, onChange, rows, base } = editor;
  const key = `commands.${index}`;
  const id = `${base}-${key}`;
  const failed = rows.shown(key, commandProblem(command));
  const update = (patch: Partial<NodeCommand>): void => {
    rows.touch(key);
    onChange(replaceAt(value, index, patch));
  };
  return (
    <fieldset aria-label={`Command ${index + 1}`} className="bare node-row command-row">
      <TextBox
        label="Command"
        value={command.run}
        invalid={failed !== undefined}
        describedBy={id}
        onChange={(run) => update({ run })}
        onBlur={() => rows.touch(key)}
      />
      <RolesSelect value={command.roles} onChange={(roles) => update({ roles })} />
      <button
        type="button"
        aria-label={`Remove command ${index + 1}`}
        onClick={() => {
          rows.reset();
          onChange(value.filter((_, j) => j !== index));
        }}
      >
        Remove
      </button>
      <Problem id={id} message={failed} />
    </fieldset>
  );
}

function Commands({ editor }: { editor: Editor<NodeCommand[]> }): JSX.Element {
  const { value, onChange } = editor;
  return (
    <fieldset className="group node-settings">
      <legend>Commands</legend>
      <div className="node-rows">
        {value.length > 0 && <Head parts={["Command", "Nodes"]} shape="command-row" />}
        {value.map((c, i) => (
          // oxlint-disable-next-line react/no-array-index-key -- rows are edited in place
          <CommandRow key={i} command={c} index={i} editor={editor} />
        ))}
      </div>
      {value.length === 0 && <p className="muted">No commands</p>}
      <button
        type="button"
        disabled={value.length >= maxCommands}
        onClick={() => onChange([...value, { run: "", roles: "both" }])}
      >
        Add command
      </button>
    </fieldset>
  );
}

function Args({ list, editor }: { list: ArgList; editor: Editor<NodeArg[]> }): JSX.Element {
  const { value, onChange, rows, base } = editor;
  const title = argListNames[list];
  const problems = argProblems(value);
  return (
    <fieldset className="group node-settings">
      <legend>{title}</legend>
      <div className="node-rows">
        {value.length > 0 && <Head parts={["Name", "Value"]} />}
        {value.map((a, i) => {
          const key = `${list}.${i}`;
          const id = `${base}-${key}`;
          const failed = rows.shown(key, problems[i]);
          const update = (patch: Partial<NodeArg>): void => {
            rows.touch(key);
            onChange(replaceAt(value, i, patch));
          };
          return (
            // oxlint-disable-next-line react/no-array-index-key -- rows are edited in place
            <fieldset key={i} aria-label={`${title} ${i + 1}`} className="bare node-row">
              <TextBox
                label="Name"
                value={a.name}
                invalid={failed !== undefined}
                describedBy={id}
                onChange={(name) => update({ name })}
                onBlur={() => rows.touch(key)}
              />
              <TextBox
                label="Value"
                value={a.value}
                invalid={false}
                describedBy={id}
                onChange={(v) => update({ value: v })}
                onBlur={() => rows.touch(key)}
              />
              <button
                type="button"
                aria-label={`Remove ${title.toLowerCase()} ${i + 1}`}
                onClick={() => {
                  rows.reset();
                  onChange(value.filter((_, j) => j !== i));
                }}
              >
                Remove
              </button>
              <Problem id={id} message={failed} />
            </fieldset>
          );
        })}
      </div>
      {value.length === 0 && <p className="muted">No flags</p>}
      <button
        type="button"
        disabled={value.length >= maxArgs}
        onClick={() => onChange([...value, { name: "", value: "" }])}
      >
        Add flag
      </button>
    </fieldset>
  );
}

function Audit({ editor, can }: { editor: Editor<NodeCustomisation>; can: Carries }): JSX.Element | null {
  const { value, onChange, rows, base } = editor;
  const inline = value.inlinePolicy !== undefined;
  if (!can.auditConfigMap && !can.webhook && !inline) {
    return null;
  }
  const policy = rows.shown(
    "auditConfigMap",
    value.auditConfigMap === "" ? undefined : objectNameProblem(value.auditConfigMap),
  );
  const hook = rows.shown("webhook", value.webhookSecret === "" ? undefined : objectNameProblem(value.webhookSecret));
  const hookKey = rows.shown("webhookKey", value.webhookKey === "" ? undefined : secretKeyProblem(value.webhookKey));
  return (
    <fieldset className="group node-settings">
      <legend>Audit</legend>
      {inline && <p className="muted">Inline policy set with kubectl</p>}
      <div className="node-rows labelled">
        {can.auditConfigMap && !inline && (
          <div className="node-row wide">
            <TextBox
              label="Policy ConfigMap"
              value={value.auditConfigMap}
              invalid={policy !== undefined}
              describedBy={`${base}-policy`}
              onChange={(auditConfigMap) => {
                rows.touch("auditConfigMap");
                onChange({ ...value, auditConfigMap });
              }}
              onBlur={() => rows.touch("auditConfigMap")}
            />
            <Problem id={`${base}-policy`} message={policy} />
          </div>
        )}
        {can.webhook && (
          <div className="node-row pair">
            <TextBox
              label="Webhook Secret"
              value={value.webhookSecret}
              invalid={hook !== undefined}
              describedBy={`${base}-hook`}
              onChange={(webhookSecret) => {
                rows.touch("webhook");
                onChange({ ...value, webhookSecret });
              }}
              onBlur={() => rows.touch("webhook")}
            />
            <TextBox
              label="Webhook key"
              value={value.webhookKey}
              invalid={hookKey !== undefined}
              describedBy={`${base}-hook`}
              onChange={(webhookKey) => {
                rows.touch("webhookKey");
                onChange({ ...value, webhookKey });
              }}
              onBlur={() => rows.touch("webhookKey")}
            />
            <Problem id={`${base}-hook`} message={hook ?? hookKey} />
          </div>
        )}
      </div>
    </fieldset>
  );
}

/**
 * NodeCustomisationFields edits the files, commands, component flags and audit settings that every
 * node of a cluster gets. A row's problem shows once the row was touched, or all of them with
 * showAll. The server stays the authority: its refusal shows under the fields.
 */
export function NodeCustomisationFields({
  value,
  onChange,
  talos,
  showAll = false,
  refusal,
}: {
  value: NodeCustomisation;
  onChange: (v: NodeCustomisation) => void;
  talos: boolean;
  showAll?: boolean;
  /** The server's refusal of the settings, shown with them. */
  refusal?: string | undefined;
}): JSX.Element {
  const base = useId();
  const rows = useShown(showAll);
  const can = carries(talos);
  const on = <K extends keyof NodeCustomisation>(field: K) => ({
    value: value[field],
    onChange: (v: NodeCustomisation[K]) => onChange({ ...value, [field]: v }),
    rows,
    base,
  });
  let talosNote: ReactNode = null;
  if (talos) {
    talosNote = <p className="muted">Talos takes inline files and flags only.</p>;
  }
  return (
    <>
      {talosNote}
      <Files editor={on("files")} can={can} />
      {can.commands && <Commands editor={on("commands")} />}
      {argLists.map((list) => (
        <Args key={list} list={list} editor={on(list)} />
      ))}
      <Audit editor={{ value, onChange, rows, base }} can={can} />
      <ErrorBox message={refusal} />
    </>
  );
}

/** NodeCustomisationDialog changes the settings of a cluster that exists. */
export function NodeCustomisationDialog({
  cluster: c,
  onClose,
}: {
  cluster: Cluster;
  onClose: () => void;
}): JSX.Element {
  const talos = c.distribution === "talos";
  const was = nodeCustomisationOf(c.variables["nodeCustomisation"]);
  const [value, setValue] = useState(was);
  const { busy, error, send } = useChange(c, onClose);
  const problem = nodeCustomisationProblem(value, talos);
  const [attempted, setAttempted] = useState(false);
  const unchanged = sameNodeCustomisation(value, was, talos);
  return (
    <Dialog
      title="Edit node customisation"
      onClose={onClose}
      onSubmit={() => {
        if (problem !== undefined) {
          setAttempted(true);
          return;
        }
        void send({ variables: { nodeCustomisation: nodeCustomisationVariable(value, talos) } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || unchanged}>
            Save
          </button>
        </>
      }
    >
      <p className="muted">Saving replaces every node, one at a time.</p>
      <NodeCustomisationFields value={value} onChange={setValue} talos={talos} showAll={attempted} refusal={error} />
    </Dialog>
  );
}
