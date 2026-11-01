import { useState, type JSX } from "react";

import { api } from "./api";
import { CredentialField } from "./CredentialField";
import type { NotificationSent } from "./types";
import { ErrorBox, Status, age, errorMessage, filled, pollIdle, usePoll } from "./ui";

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

/**
 * Notifications sets where the appliance sends cluster problems and failed runs, shows each
 * target's last sending and sends a test.
 */
export function Notifications(): JSX.Element {
  const { data, error: readError, put } = usePoll(api.notifications, pollIdle);
  const [testing, setTesting] = useState(false);
  const [error, setError] = useState<string>();
  const sent = Object.fromEntries((data ?? []).map((s) => [s.kind, s]));

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
        keys={[{ key: "url", label: "URL" }]}
        status={<SentLine sent={sent["notify-webhook"]} />}
      />
      <CredentialField
        label="ntfy"
        purpose="notify-ntfy"
        keys={[
          { key: "url", label: "Topic URL" },
          { key: "token", label: "Access token", optional: true },
        ]}
        status={<SentLine sent={sent["notify-ntfy"]} />}
      />
      <CredentialField
        label="Email"
        purpose="notify-email"
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
        <button type="button" disabled={testing} onClick={startTest}>
          Send test
        </button>
      </div>
      <ErrorBox message={error ?? readError} />
    </div>
  );
}
