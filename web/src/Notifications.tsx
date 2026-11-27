import { useMemo, useState, type JSX } from "react";

import { api } from "./api";
import { CredentialField } from "./CredentialField";
import type { NotificationSent } from "./types";
import { ErrorBox, ReadError, Status, age, errorMessage, filled, pollIdle, usePoll } from "./ui";

/** SentLine is a target's last sending: how it went, when, and what it was. */
function SentLine({ sent }: { sent: NotificationSent | undefined }): JSX.Element | null {
  if (sent?.at === undefined) {
    return null;
  }
  const failed = filled(sent.error);
  return (
    <div className="sent">
      <Status state={failed ? "error" : "ok"} text={`${failed ? "Failed" : "Sent"} ${age(sent.at)} ago`} />{" "}
      <span className="muted">{sent.title}</span>
      {failed && <div className="field-error">{sent.error}</div>}
    </div>
  );
}

// The credentials that hold the notification targets.
const targets = ["notify-webhook", "notify-ntfy", "notify-email"];

/**
 * Notifications sets where the appliance sends cluster problems and failed runs, shows each
 * target's last sending and sends a test.
 */
export function Notifications(): JSX.Element {
  const read = usePoll(api.notifications, pollIdle);
  const { data, put } = read;
  const [testing, setTesting] = useState(false);
  const [error, setError] = useState<string>();
  const sent = Object.fromEntries((data ?? []).map((s) => [s.kind, s]));
  // The targets whose stored settings could not be read. A test needs at least one target read.
  const [unread, setUnread] = useState<ReadonlySet<string>>(new Set());
  const [handleWebhookRead, handleNtfyRead, handleEmailRead] = useMemo(
    () =>
      targets.map((purpose) => (ok: boolean) => {
        setUnread((was) => {
          const now = new Set(was);
          if (ok) {
            now.delete(purpose);
          } else {
            now.add(purpose);
          }
          return now;
        });
      }),
    [],
  );
  const noTest = unread.size === targets.length ? "Targets not read." : undefined;

  const test = async (): Promise<void> => {
    setTesting(true);
    setError(undefined);
    try {
      const tested = await api.testNotifications();
      put((shown) => [...(shown ?? []).filter((s) => !tested.some((t) => t.kind === s.kind)), ...tested]);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setTesting(false);
    }
  };
  const startTest = (): void => {
    void test();
  };

  return (
    <div className="box fields">
      <h3>Notifications</h3>
      <CredentialField
        label="Webhook"
        purpose="notify-webhook"
        onRead={handleWebhookRead}
        keys={[{ key: "url", label: "URL" }]}
        status={<SentLine sent={sent["notify-webhook"]} />}
      />
      <CredentialField
        label="ntfy"
        purpose="notify-ntfy"
        onRead={handleNtfyRead}
        keys={[
          { key: "url", label: "Topic URL" },
          { key: "token", label: "Access token", optional: true },
        ]}
        status={<SentLine sent={sent["notify-ntfy"]} />}
      />
      <CredentialField
        label="Email"
        purpose="notify-email"
        onRead={handleEmailRead}
        keys={[
          { key: "server", label: "SMTP server (host:port)", plain: true },
          { key: "from", label: "From", plain: true },
          { key: "to", label: "To", plain: true },
          { key: "username", label: "Username", plain: true, optional: true },
          { key: "password", label: "Password", optional: true },
        ]}
        hint="Port 465 uses TLS, any other STARTTLS."
        status={<SentLine sent={sent["notify-email"]} />}
      />
      <div className="actions">
        <button type="button" disabled={testing || noTest !== undefined} title={noTest} onClick={startTest}>
          Send test
        </button>
      </div>
      {noTest !== undefined && <p className="muted">{noTest}</p>}
      <ErrorBox message={error} />
      <ReadError read={read} />
    </div>
  );
}
