import type { JSX, ReactNode } from "react";

import type { CredentialKey } from "./CredentialField";
import { list } from "./network";
import { Field, Info } from "./ui";
import { flag, record, records, text, texts } from "./values";

/** How a remote write target or an external Alertmanager is signed in to. */
export type Auth = "none" | "basic" | "bearer";

/** The monitoring add-on's alert receivers, remote write target and external Alertmanagers. */
export interface AlertingForm {
  email: { on: boolean; smarthost: string; from: string; to: string; username: string; requireTLS: boolean };
  webhook: boolean;
  slack: { on: boolean; channel: string };
  teams: boolean;
  remoteWrite: { on: boolean; url: string; auth: Auth; username: string };
  external: { on: boolean; urls: string; auth: Auth; username: string };
}

/** Values typed for credentials the form stores once the cluster exists, by purpose. */
export type AlertingSecrets = Record<string, Record<string, string>>;

export const newAlerting = (): AlertingForm => ({
  email: { on: false, smarthost: "", from: "", to: "", username: "", requireTLS: true },
  webhook: false,
  slack: { on: false, channel: "" },
  teams: false,
  remoteWrite: { on: false, url: "", auth: "none", username: "" },
  external: { on: false, urls: "", auth: "none", username: "" },
});

const authOf = (v: unknown): Auth => (v === "basic" || v === "bearer" ? v : "none");

/** alertingOf reads the settings out of a cluster's monitoring variable. */
export function alertingOf(monitoring: unknown): AlertingForm {
  const m = record(monitoring);
  const a = newAlerting();
  for (const r of records(m["alertReceivers"])) {
    switch (text(r["type"]) ?? "") {
      case "email": {
        a.email = {
          on: true,
          smarthost: text(r["smarthost"]) ?? "",
          from: text(r["from"]) ?? "",
          to: (texts(r["to"]) ?? []).join(", "),
          username: text(r["username"]) ?? "",
          requireTLS: flag(r["requireTLS"]) !== false,
        };
        break;
      }
      case "webhook": {
        a.webhook = true;
        break;
      }
      case "slack": {
        a.slack = { on: true, channel: text(r["channel"]) ?? "" };
        break;
      }
      case "teams": {
        a.teams = true;
        break;
      }
      default: {
        break;
      }
    }
  }
  const write = record(m["remoteWrite"]);
  if (text(write["url"]) !== undefined) {
    a.remoteWrite = {
      on: true,
      url: text(write["url"]) ?? "",
      auth: authOf(write["auth"]),
      username: text(write["username"]) ?? "",
    };
  }
  const external = record(m["externalAlertmanagers"]);
  if (texts(external["urls"]) !== undefined) {
    a.external = {
      on: true,
      urls: (texts(external["urls"]) ?? []).join("\n"),
      auth: authOf(external["auth"]),
      username: text(external["username"]) ?? "",
    };
  }
  return a;
}

function receivers(a: AlertingForm): Record<string, unknown>[] {
  const out: Record<string, unknown>[] = [];
  if (a.email.on) {
    const e: Record<string, unknown> = {
      type: "email",
      smarthost: a.email.smarthost,
      from: a.email.from,
      to: list(a.email.to),
    };
    if (a.email.username !== "") {
      e["username"] = a.email.username;
    }
    if (!a.email.requireTLS) {
      e["requireTLS"] = false;
    }
    out.push(e);
  }
  if (a.webhook) {
    out.push({ type: "webhook" });
  }
  if (a.slack.on) {
    out.push(a.slack.channel === "" ? { type: "slack" } : { type: "slack", channel: a.slack.channel });
  }
  if (a.teams) {
    out.push({ type: "teams" });
  }
  return out;
}

const signIn = (auth: Auth, username: string): Record<string, unknown> =>
  auth === "none" ? {} : { auth, ...(auth === "basic" && { username }) };

/** alertingVariable are the monitoring variable's keys the settings make, none for what is off. */
export function alertingVariable(a: AlertingForm): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  const to = receivers(a);
  if (to.length > 0) {
    out["alertReceivers"] = to;
  }
  if (a.remoteWrite.on) {
    out["remoteWrite"] = { url: a.remoteWrite.url, ...signIn(a.remoteWrite.auth, a.remoteWrite.username) };
  }
  if (a.external.on) {
    out["externalAlertmanagers"] = { urls: list(a.external.urls), ...signIn(a.external.auth, a.external.username) };
  }
  return out;
}

/** The credential each setting stores, by purpose, and what it holds. */
const credentials = {
  "alerts-email": { label: "Mail password", keys: [{ key: "password", label: "Password" }] },
  "alerts-webhook": {
    label: "Webhook secrets",
    keys: [
      { key: "url", label: "URL" },
      { key: "token", label: "Bearer token", optional: true },
    ],
  },
  "alerts-slack": { label: "Slack webhook", keys: [{ key: "url", label: "Webhook URL" }] },
  "alerts-teams": { label: "Teams webhook", keys: [{ key: "url", label: "Webhook URL" }] },
  "remote-write": { label: "Remote write secret", keys: [{ key: "secret", label: "Password or token" }] },
  "external-alertmanager": { label: "Alertmanager secret", keys: [{ key: "secret", label: "Password or token" }] },
} as const satisfies Record<string, { label: string; keys: CredentialKey[] }>;

/** AlertingPurpose is a purpose of a credential the settings store. */
export type AlertingPurpose = keyof typeof credentials;

/** alertingPurposes are the credentials the settings in use need. */
export function alertingPurposes(a: AlertingForm): AlertingPurpose[] {
  const needed: AlertingPurpose[] = [];
  if (a.email.on && a.email.username !== "") {
    needed.push("alerts-email");
  }
  if (a.webhook) {
    needed.push("alerts-webhook");
  }
  if (a.slack.on) {
    needed.push("alerts-slack");
  }
  if (a.teams) {
    needed.push("alerts-teams");
  }
  if (a.remoteWrite.on && a.remoteWrite.auth !== "none") {
    needed.push("remote-write");
  }
  if (a.external.on && a.external.auth !== "none") {
    needed.push("external-alertmanager");
  }
  return needed;
}

/** alertingText says in a line what the settings send where. */
export function alertingText(a: AlertingForm): string {
  const parts = [
    a.email.on && "email",
    a.webhook && "webhook",
    a.slack.on && "Slack",
    a.teams && "Teams",
    a.remoteWrite.on && "remote write",
    a.external.on && "external Alertmanagers",
  ].filter(Boolean);
  return parts.length === 0 ? "" : parts.join(", ");
}

/** pendingAlerting are the credentials the form holds, to store once the cluster exists. */
export function pendingAlerting(
  a: AlertingForm,
  secrets: AlertingSecrets,
): { purpose: AlertingPurpose; values: Record<string, string>; label: string }[] {
  return alertingPurposes(a).flatMap((purpose) => {
    const values = secrets[purpose] ?? {};
    const complete = credentials[purpose].keys.every((k) => "optional" in k || (values[k.key] ?? "") !== "");
    return complete ? [{ purpose, values, label: credentials[purpose].label }] : [];
  });
}

const authLabels: Record<Auth, string> = { none: "None", basic: "User name and password", bearer: "Bearer token" };

function AuthFields({
  value,
  onChange,
}: {
  value: { auth: Auth; username: string };
  onChange: (v: { auth: Auth; username: string }) => void;
}): JSX.Element {
  return (
    <>
      <Field label="Sign in">
        {(id) => (
          <select id={id} value={value.auth} onChange={(e) => onChange({ ...value, auth: authOf(e.target.value) })}>
            {(["none", "basic", "bearer"] as const).map((a) => (
              <option key={a} value={a}>
                {authLabels[a]}
              </option>
            ))}
          </select>
        )}
      </Field>
      {value.auth === "basic" && (
        <Field label="User name">
          {(id) => (
            <input
              id={id}
              value={value.username}
              autoComplete="off"
              onChange={(e) => onChange({ ...value, username: e.target.value.trim() })}
            />
          )}
        </Field>
      )}
    </>
  );
}

/** What a setting's credential is called when it is rendered: its purpose, label and values. */
export type RenderCredential = (purpose: AlertingPurpose, label: string, keys: CredentialKey[]) => ReactNode;

/**
 * AlertingFields chooses where the monitoring add-on sends alerts and samples. renderCredential is
 * how a credential is entered: in the wizard a field of its own, on a running cluster the stored one.
 */
export function AlertingFields({
  value,
  onChange,
  renderCredential,
}: {
  value: AlertingForm;
  onChange: (v: AlertingForm) => void;
  renderCredential: RenderCredential;
}): JSX.Element {
  const credential = (purpose: AlertingPurpose): ReactNode =>
    renderCredential(purpose, credentials[purpose].label, [...credentials[purpose].keys]);
  const { email } = value;
  return (
    <>
      <fieldset className="group">
        <legend>Alert receivers</legend>
        <label className="check">
          <input
            type="checkbox"
            checked={email.on}
            onChange={(e) => onChange({ ...value, email: { ...email, on: e.target.checked } })}
          />{" "}
          Email
        </label>
        {email.on && (
          <>
            <Field label="Mail server" hint="host:port">
              {(id) => (
                <input
                  id={id}
                  value={email.smarthost}
                  onChange={(e) => onChange({ ...value, email: { ...email, smarthost: e.target.value.trim() } })}
                />
              )}
            </Field>
            <Field label="From">
              {(id) => (
                <input
                  id={id}
                  value={email.from}
                  onChange={(e) => onChange({ ...value, email: { ...email, from: e.target.value.trim() } })}
                />
              )}
            </Field>
            <Field label="To" hint="Addresses, separated by commas">
              {(id) => (
                <input
                  id={id}
                  value={email.to}
                  onChange={(e) => onChange({ ...value, email: { ...email, to: e.target.value } })}
                />
              )}
            </Field>
            <Field label="User name" hint="Optional">
              {(id) => (
                <input
                  id={id}
                  value={email.username}
                  autoComplete="off"
                  onChange={(e) => onChange({ ...value, email: { ...email, username: e.target.value.trim() } })}
                />
              )}
            </Field>
            <label className="check">
              <input
                type="checkbox"
                checked={email.requireTLS}
                onChange={(e) => onChange({ ...value, email: { ...email, requireTLS: e.target.checked } })}
              />{" "}
              Require TLS
            </label>
            {email.username !== "" && credential("alerts-email")}
          </>
        )}
        <label className="check">
          <input
            type="checkbox"
            checked={value.webhook}
            onChange={(e) => onChange({ ...value, webhook: e.target.checked })}
          />{" "}
          Webhook
        </label>
        {value.webhook && credential("alerts-webhook")}
        <label className="check">
          <input
            type="checkbox"
            checked={value.slack.on}
            onChange={(e) => onChange({ ...value, slack: { ...value.slack, on: e.target.checked } })}
          />{" "}
          Slack
        </label>
        <Info about="Slack">Also Mattermost and other services that take Slack&apos;s webhook format.</Info>
        {value.slack.on && (
          <>
            <Field label="Channel" hint="Optional">
              {(id) => (
                <input
                  id={id}
                  value={value.slack.channel}
                  onChange={(e) => onChange({ ...value, slack: { ...value.slack, channel: e.target.value.trim() } })}
                />
              )}
            </Field>
            {credential("alerts-slack")}
          </>
        )}
        <label className="check">
          <input
            type="checkbox"
            checked={value.teams}
            onChange={(e) => onChange({ ...value, teams: e.target.checked })}
          />{" "}
          Teams
        </label>
        {value.teams && credential("alerts-teams")}
      </fieldset>
      <fieldset className="group">
        <legend>Remote write</legend>
        <label className="check">
          <input
            type="checkbox"
            checked={value.remoteWrite.on}
            onChange={(e) => onChange({ ...value, remoteWrite: { ...value.remoteWrite, on: e.target.checked } })}
          />{" "}
          Send samples elsewhere
        </label>
        {value.remoteWrite.on && (
          <>
            <Field label="Write URL">
              {(id) => (
                <input
                  id={id}
                  value={value.remoteWrite.url}
                  onChange={(e) =>
                    onChange({ ...value, remoteWrite: { ...value.remoteWrite, url: e.target.value.trim() } })
                  }
                />
              )}
            </Field>
            <AuthFields
              value={value.remoteWrite}
              onChange={(v) => onChange({ ...value, remoteWrite: { ...value.remoteWrite, ...v } })}
            />
            {value.remoteWrite.auth !== "none" && credential("remote-write")}
          </>
        )}
      </fieldset>
      <fieldset className="group">
        <legend>Alert forwarding</legend>
        <label className="check">
          <input
            type="checkbox"
            checked={value.external.on}
            onChange={(e) => onChange({ ...value, external: { ...value.external, on: e.target.checked } })}
          />{" "}
          Forward alerts
        </label>
        {value.external.on && (
          <>
            <Field label="Addresses" hint="One per line">
              {(id) => (
                <textarea
                  id={id}
                  rows={2}
                  value={value.external.urls}
                  onChange={(e) => onChange({ ...value, external: { ...value.external, urls: e.target.value } })}
                />
              )}
            </Field>
            <AuthFields
              value={value.external}
              onChange={(v) => onChange({ ...value, external: { ...value.external, ...v } })}
            />
            {value.external.auth !== "none" && credential("external-alertmanager")}
          </>
        )}
      </fieldset>
    </>
  );
}
