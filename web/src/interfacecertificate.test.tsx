import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { api } from "./api";
import { App } from "./App";
import { expectNoViolations } from "./axe";
import { fakeServer, held, type Answer, type Route } from "./fake-server";
import { InterfaceCertificate } from "./InterfaceCertificate";
import type { ACMEView, InterfaceCertificate as Certificate } from "./types";

const msPerDay = 86_400_000;
const inDays = (days: number): string => new Date(Date.now() + days * msPerDay + 3_600_000).toISOString();
const dateOf = (iso: string): string => new Date(iso).toLocaleDateString(undefined, { dateStyle: "long" });

const fingerprint = (seed: string): string => Array.from({ length: 32 }, () => seed).join(":");

const certificate = (over: Partial<Certificate> = {}): Certificate => ({
  source: "uploaded",
  subject: "CN=kubehenge.example.org",
  dnsNames: ["kubehenge.example.org"],
  ipAddresses: ["192.0.2.40"],
  issuer: "CN=Example Org CA",
  notBefore: inDays(-100),
  notAfter: inDays(120),
  fingerprint: fingerprint("AB"),
  chain: ["CN=kubehenge.example.org", "CN=Example Org CA"],
  incomplete: false,
  selfSigned: false,
  publicURL: "https://kubehenge.example.org",
  publicNameResolves: true,
  changeable: true,
  ...over,
});

const path = "/api/appliance/certificate";
const read = (over: Partial<Certificate> = {}): Record<string, Route> => ({
  [`GET ${path}`]: { body: certificate(over) },
});

// sequence answers each request with the next of its answers, and the last again after them.
function sequence(answers: Answer[]): () => Answer {
  let n = 0;
  return () => {
    const answer = answers[Math.min(n, answers.length - 1)] ?? {};
    n += 1;
    return answer;
  };
}

const pem = (name: string, size = 20): File => new File([new Uint8Array(size)], name);
const variable = (n: number): HTMLElement => screen.getByRole("group", { name: `Variable ${n}` });
const signedIn = { "GET /api/session": { body: { user: "root@pam", csrf: "c1" } } };

describe("interface certificate", () => {
  it("1: sits under Administration for administrators and is refused to others", async () => {
    const answers = {
      "GET /api/clusters": { body: [] },
      "GET /api/fleet/health": { body: [] },
      "GET /api/runs": { body: [] },
      ...read(),
    };
    window.history.replaceState(null, "", "/administration/certificates");
    fakeServer({
      ...signedIn,
      ...answers,
      "GET /api/setup": { body: { appliance: true, ready: true, allowed: true } },
    });
    const { unmount } = render(<App />);
    const views = await screen.findByRole("navigation", { name: "Administration pages" });
    expect(within(views).getByRole("link", { name: "Certificates" })).toHaveAttribute("aria-current", "page");
    await expect(screen.findByRole("heading", { name: "Interface certificate" })).resolves.toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Cluster certificates" })).toBeInTheDocument();
    unmount();

    fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c1" } },
      "GET /api/setup": { body: { appliance: true, ready: true, allowed: false } },
      ...answers,
    });
    render(<App />);
    await expect(screen.findByRole("heading", { name: "Administrators only" })).resolves.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Administration" })).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Interface certificate" })).not.toBeInTheDocument();
  });

  it("2: shows where the certificate comes from, what it covers, its end, fingerprint, chain and address", async () => {
    fakeServer(read());
    const user = userEvent.setup({ delay: null });
    const c = certificate();
    render(<InterfaceCertificate />);
    await screen.findByText("Source");
    expect(screen.getByText("Uploaded")).toBeInTheDocument();
    expect(screen.getByText("kubehenge.example.org, 192.0.2.40")).toBeInTheDocument();
    expect(screen.getByText("CN=Example Org CA", { selector: "dd" })).toBeInTheDocument();
    expect(screen.getByText(dateOf(c.notAfter))).toBeInTheDocument();
    expect(screen.getByText("120 days left")).toBeInTheDocument();
    expect(screen.getByText(c.fingerprint).querySelectorAll("wbr")).toHaveLength(31);
    const chain = within(screen.getByRole("list"));
    expect(chain.getAllByRole("listitem").map((li) => li.textContent)).toStrictEqual(c.chain);
    expect(screen.getByText("https://kubehenge.example.org")).toBeInTheDocument();
    expect(screen.getByText("Resolves")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Copy fingerprint" }));
    await expect(navigator.clipboard.readText()).resolves.toBe(c.fingerprint);
    expect(screen.getByRole("button", { name: "Copied" })).toBeInTheDocument();
  });

  it("3: warns in one line each of the facts that need action", async () => {
    fakeServer(
      read({
        source: "installer",
        notAfter: inDays(10),
        incomplete: true,
        missingIssuer: "CN=Example Root CA",
        selfSigned: true,
        publicNameResolves: false,
        notServed: "the file holds no key",
        trustedBy: "CN=Proxmox Virtual Environment",
      }),
    );
    const { unmount } = render(<InterfaceCertificate />);
    await expect(screen.findByText("Expires in 10 days")).resolves.toBeInTheDocument();
    expect(screen.getByText("Chain incomplete, missing CN=Example Root CA")).toBeInTheDocument();
    expect(screen.getByText("Self-signed, so browsers warn")).toBeInTheDocument();
    expect(screen.getByText("Browsers warn unless they trust CN=Proxmox Virtual Environment")).toBeInTheDocument();
    expect(screen.getByText("kubehenge.example.org does not resolve to this appliance")).toBeInTheDocument();
    expect(screen.getByText("Stored certificate not served, the file holds no key")).toBeInTheDocument();
    expect(document.querySelectorAll(".warning")).toHaveLength(1);
    expect(document.querySelectorAll(".warning p")).toHaveLength(6);
    unmount();

    const over = inDays(-3);
    fakeServer(read({ notAfter: over }));
    render(<InterfaceCertificate />);
    await expect(screen.findByText(`Expired on ${dateOf(over)}`)).resolves.toBeInTheDocument();
    expect(screen.queryByText(/Chain incomplete/u)).not.toBeInTheDocument();
    expect(screen.queryByText(/Self-signed, so/u)).not.toBeInTheDocument();
    expect(screen.queryByText(/does not resolve/u)).not.toBeInTheDocument();
    expect(screen.queryByText(/not served/u)).not.toBeInTheDocument();
  });

  it("4: takes several files, lists them with sizes, removes one at a time and refuses what the server would", async () => {
    const calls = fakeServer({ ...signedIn, ...read(), [`PUT ${path}`]: { body: certificate() } });
    await api.session();
    const user = userEvent.setup({ delay: null });
    render(<InterfaceCertificate />);
    const picker = await screen.findByLabelText("Certificate files");
    expect(picker).toHaveAttribute("multiple");
    expect(picker).not.toHaveAttribute("accept");
    await user.upload(picker, [pem("site.crt", 1500), pem("site.key"), pem("chain.p7b", 2048)]);
    const list = within(screen.getByRole("list", { name: "Chosen files" }));
    expect(list.getAllByRole("listitem").map((li) => li.textContent)).toStrictEqual([
      "site.crt 1.5 KiBRemove",
      "site.key 20 BRemove",
      "chain.p7b 2.0 KiBRemove",
    ]);
    await user.click(list.getByRole("button", { name: "Remove site.key" }));
    expect(list.queryByText("site.key")).not.toBeInTheDocument();
    expect(list.getAllByRole("listitem")).toHaveLength(2);

    await user.upload(
      picker,
      Array.from({ length: 7 }, (_, i) => pem(`more-${i}.pem`)),
    );
    expect(screen.getByRole("alert")).toHaveTextContent("At most 8 files. Remove some");
    expect(screen.getByRole("button", { name: "Replace" })).toBeDisabled();
    await user.click(list.getByRole("button", { name: "Remove more-6.pem" }));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    await user.click(list.getByRole("button", { name: "Remove more-5.pem" }));

    await user.upload(picker, pem("big.pfx", 1024 * 1024 + 1));
    expect(screen.getByRole("alert")).toHaveTextContent("File big.pfx is over 1 MiB. Remove it.");
    expect(screen.getByRole("button", { name: "Replace" })).toBeDisabled();
    await user.click(list.getByRole("button", { name: "Remove big.pfx" }));
    await user.upload(picker, pem("edge.pfx", 1024 * 1024));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(calls.filter((c) => c.key === `PUT ${path}`)).toHaveLength(0);

    await user.click(screen.getByRole("button", { name: "Replace" }));
    await waitFor(() => expect(calls.filter((c) => c.key === `PUT ${path}`)).toHaveLength(1));
    const put = calls.find((c) => c.key === `PUT ${path}`);
    expect(put?.body).toStrictEqual({
      file: ["site.crt", "chain.p7b", "more-0.pem", "more-1.pem", "more-2.pem", "more-3.pem", "more-4.pem", "edge.pfx"],
    });
    expect(put?.headers.get("X-Kubehenge-CSRF")).toBe("c1");
    expect(put?.headers.get("Content-Type")).toBeNull();
  });

  it("5: asks for a passphrase on a refusal, keeps the files and shows a wrong one at that field", async () => {
    const renewed = certificate({ fingerprint: fingerprint("CD") });
    const calls = fakeServer({
      ...read(),
      [`PUT ${path}`]: sequence([
        {
          status: 422,
          body: { error: "host.pfx is encrypted. Send its passphrase", code: "passphrase", requestId: "r1" },
        },
        {
          status: 422,
          body: { error: "The passphrase does not open host.pfx. Send the right one", code: "passphrase" },
        },
        { body: renewed },
      ]),
    });
    const user = userEvent.setup({ delay: null });
    render(<InterfaceCertificate />);
    await user.upload(await screen.findByLabelText("Certificate files"), pem("host.pfx"));
    expect(screen.queryByLabelText("Passphrase")).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Replace" }));

    const field = await screen.findByLabelText("Passphrase");
    expect(field).toHaveAttribute("type", "password");
    expect(field).toHaveFocus();
    expect(field).toHaveAccessibleDescription("host.pfx is encrypted. Send its passphrase");
    expect(screen.getByRole("list", { name: "Chosen files" })).toHaveTextContent("host.pfx");

    await user.type(field, "wrong");
    await user.click(screen.getByRole("button", { name: "Replace" }));
    await waitFor(() =>
      expect(screen.getByLabelText("Passphrase")).toHaveAccessibleDescription(
        "The passphrase does not open host.pfx. Send the right one",
      ),
    );
    expect(screen.getByLabelText("Passphrase")).toBeInvalid();

    await user.clear(screen.getByLabelText("Passphrase"));
    await user.type(screen.getByLabelText("Passphrase"), "right");
    await user.click(screen.getByRole("button", { name: "Replace" }));
    await waitFor(() => expect(screen.queryByLabelText("Passphrase")).not.toBeInTheDocument());
    const puts = calls.filter((c) => c.key === `PUT ${path}`);
    expect(puts.map((c) => c.body)).toStrictEqual([
      { file: ["host.pfx"] },
      { file: ["host.pfx"], passphrase: ["wrong"] },
      { file: ["host.pfx"], passphrase: ["right"] },
    ]);
  });

  it("6: shows every other refusal's sentence as it is, with the names a wrong name lists", async () => {
    fakeServer({
      ...read(),
      [`PUT ${path}`]: sequence([
        {
          status: 422,
          body: { error: "No private key was found in site.crt. Send the certificate and its key", code: "unreadable" },
        },
        {
          status: 422,
          body: {
            error: "The certificate is not valid for kubehenge.example.org. Send one that covers it",
            code: "wrong-name",
            names: ["other.example.org", "192.0.2.41"],
          },
        },
      ]),
    });
    const user = userEvent.setup({ delay: null });
    render(<InterfaceCertificate />);
    await user.upload(await screen.findByLabelText("Certificate files"), pem("site.crt"));
    await user.click(screen.getByRole("button", { name: "Replace" }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("No private key was found in site.crt. Send the certificate and its key.");
    expect(screen.queryByLabelText("Passphrase")).not.toBeInTheDocument();
    expect(screen.queryByText(/^Covers:/u)).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Replace" }));
    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent(/^The certificate is not valid for/u));
    expect(screen.getByText("Covers: other.example.org, 192.0.2.41")).toBeInTheDocument();
  });

  it("7: shows the new certificate at once and the certificates left out", async () => {
    const replaced = certificate({ fingerprint: fingerprint("CD"), issuer: "CN=Other CA" });
    fakeServer({
      ...read(),
      [`PUT ${path}`]: { body: { ...replaced, leftOut: ["CN=Old CA", "CN=Spare CA"] } },
    });
    const user = userEvent.setup({ delay: null });
    render(<InterfaceCertificate />);
    await user.upload(await screen.findByLabelText("Certificate files"), [pem("a.pem"), pem("b.pem")]);
    await user.click(screen.getByRole("button", { name: "Replace" }));
    await expect(screen.findByText(replaced.fingerprint)).resolves.toBeInTheDocument();
    expect(screen.getByText("CN=Other CA", { selector: "dd" })).toBeInTheDocument();
    expect(screen.getByText("Left out: CN=Old CA, CN=Spare CA")).toBeInTheDocument();
    expect(screen.queryByRole("list", { name: "Chosen files" })).not.toBeInTheDocument();
  });

  it("8: makes a self-signed certificate and goes back to the installer's, each after a confirmation", async () => {
    const calls = fakeServer({
      ...read(),
      [`POST ${path}/self-signed`]: {
        body: certificate({ source: "self-signed", selfSigned: true, fingerprint: fingerprint("EF") }),
      },
      [`DELETE ${path}`]: { body: certificate({ source: "installer", fingerprint: fingerprint("12") }) },
    });
    const user = userEvent.setup({ delay: null });
    render(<InterfaceCertificate />);
    await user.click(await screen.findByRole("radio", { name: "Self-signed" }));
    await user.click(screen.getByRole("button", { name: "Make self-signed" }));
    const dialog = within(screen.getByRole("dialog", { name: "Make self-signed certificate" }));
    expect(dialog.getByText("Browsers warn about a self-signed certificate.")).toBeInTheDocument();
    await user.click(dialog.getByRole("button", { name: "Cancel" }));
    expect(calls.filter((c) => c.key.startsWith("POST "))).toHaveLength(0);

    await user.click(screen.getByRole("button", { name: "Make self-signed" }));
    await user.click(
      within(screen.getByRole("dialog", { name: "Make self-signed certificate" })).getByRole("button", {
        name: "Make self-signed",
      }),
    );
    await expect(screen.findByText(fingerprint("EF"))).resolves.toBeInTheDocument();
    expect(screen.getAllByText("Self-signed", { selector: "dd" })).toHaveLength(1);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();

    await user.click(screen.getByRole("radio", { name: "Installer's" }));
    await user.click(screen.getByRole("button", { name: "Use installer's" }));
    const back = within(screen.getByRole("dialog", { name: "Use installer's certificate" }));
    expect(back.getByText("The certificate made at install replaces the current one.")).toBeInTheDocument();
    await user.click(back.getByRole("button", { name: "Use installer's" }));
    await expect(screen.findByText(fingerprint("12"))).resolves.toBeInTheDocument();
    expect(screen.getByText("Installer", { selector: "dd" })).toBeInTheDocument();
    expect(calls.map((c) => c.key).filter((k) => k !== `GET ${path}`)).toStrictEqual([
      `POST ${path}/self-signed`,
      `DELETE ${path}`,
    ]);
  });

  it("14: signs with the appliance's CA after a confirmation and offers the CA to download", async () => {
    const ca = {
      subject: "O=Kubehenge,CN=Kubehenge appliance CA",
      fingerprint: fingerprint("CA"),
      notAfter: inDays(3650),
      permits: ["kubehenge.example.org", "192.0.2.40"],
      pem: "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----\n",
    };
    const calls = fakeServer({
      ...read(),
      [`POST ${path}/appliance-ca`]: {
        body: certificate({ source: "appliance-ca", fingerprint: fingerprint("EF"), applianceCA: ca }),
      },
    });
    const blobs: Blob[] = [];
    const created = vi.fn<(b: Blob) => string>((b) => {
      blobs.push(b);
      return "blob:ca";
    });
    vi.stubGlobal("URL", Object.assign(URL, { createObjectURL: created, revokeObjectURL: vi.fn<() => void>() }));
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockReturnValue();
    const user = userEvent.setup({ delay: null });
    render(<InterfaceCertificate />);
    await user.click(await screen.findByRole("radio", { name: "Appliance CA" }));
    expect(screen.queryByRole("heading", { name: "Appliance CA" })).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Use appliance CA" }));
    const dialog = within(screen.getByRole("dialog", { name: "Use appliance CA" }));
    expect(
      dialog.getByText(
        "Browsers accept the certificate once they trust the appliance CA. It signs for this appliance's names only.",
      ),
    ).toBeInTheDocument();
    await user.click(dialog.getByRole("button", { name: "Use appliance CA" }));
    await expect(screen.findByText(fingerprint("EF"))).resolves.toBeInTheDocument();
    expect(screen.getByText("Appliance CA", { selector: "dd" })).toBeInTheDocument();
    const box = screen.getByRole("region", { name: "Appliance CA" });
    expect(within(box).getByText("kubehenge.example.org, 192.0.2.40")).toBeInTheDocument();
    expect(within(box).getByText(fingerprint("CA"))).toBeInTheDocument();
    await user.click(within(box).getByRole("button", { name: "Download CA" }));
    expect(click).toHaveBeenCalledOnce();
    await expect(blobs[0]?.text()).resolves.toBe(ca.pem);
    expect(calls.map((c) => c.key).filter((k) => k !== `GET ${path}`)).toStrictEqual([`POST ${path}/appliance-ca`]);
    click.mockRestore();
    vi.unstubAllGlobals();
  });

  it("15: changes the public address in a dialog, shows a refusal at its field and warns when the certificate misses it", async () => {
    const moved = "https://moved.example.org";
    const calls = fakeServer({
      ...read(),
      [`PUT ${path}/address`]: sequence([
        { status: 422, body: { error: '"bank example" is not a DNS name or an IP address', code: "bad-address" } },
        { body: certificate({ publicURL: moved, publicNameCovered: false }) },
      ]),
    });
    const user = userEvent.setup({ delay: null });
    const { container } = render(<InterfaceCertificate />);
    await user.click(await screen.findByRole("button", { name: "Change address" }));
    const dialog = within(screen.getByRole("dialog", { name: "Change public address" }));
    const field = dialog.getByRole("textbox", { name: "Name or address" });
    expect(field).toHaveValue("kubehenge.example.org");
    await user.clear(field);
    expect(dialog.getByRole("button", { name: "Change address" })).toBeDisabled();
    await user.type(field, "bank example");
    await user.click(dialog.getByRole("button", { name: "Change address" }));
    await expect(dialog.findByText('"bank example" is not a DNS name or an IP address')).resolves.toBeInTheDocument();
    expect(field).toHaveAccessibleDescription('"bank example" is not a DNS name or an IP address');
    await expectNoViolations(container, "interface certificate, address refused");

    await user.clear(field);
    await user.type(field, " moved.example.org ");
    await user.click(dialog.getByRole("button", { name: "Change address" }));
    await expect(screen.findByText(moved)).resolves.toBeInTheDocument();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.getByText("Not valid for moved.example.org, so browsers warn")).toBeInTheDocument();
    expect(calls.filter((c) => c.key === `PUT ${path}/address`).map((c) => c.body)).toStrictEqual([
      { name: "bank example" },
      { name: "moved.example.org" },
    ]);
  });

  it("18: says what a change of address or certificate does besides, and which source is in use", async () => {
    fakeServer(
      read({
        source: "appliance-ca",
        applianceCA: {
          subject: "CN=Kubehenge appliance CA",
          fingerprint: fingerprint("CD"),
          notAfter: inDays(3000),
          permits: ["kubehenge.example.org", "192.0.2.40"],
          pem: "-----BEGIN CERTIFICATE-----\n",
        },
        acme: {
          directory: "https://acme-v02.api.letsencrypt.org/directory",
          proof: "http-01",
          directoryCASet: false,
          dnsCredentialsSet: [],
          resolvers: [],
          state: "failing",
          givenUp: false,
        },
      }),
    );
    const user = userEvent.setup({ delay: null });
    render(<InterfaceCertificate ctid={105} />);
    await user.click(await screen.findByRole("button", { name: "Change address" }));
    const dialog = within(screen.getByRole("dialog", { name: "Change public address" }));
    const field = dialog.getByRole("textbox", { name: "Name or address" });
    expect(dialog.queryByRole("list")).not.toBeInTheDocument();
    await user.clear(field);
    await user.type(field, "https://a.kubehenge.example.org/");
    expect(dialog.getByText("The appliance CA signs a certificate for the new address.")).toBeInTheDocument();
    await user.clear(field);
    await user.type(field, "moved.example.org");
    expect(
      dialog.getByText(
        "A new appliance CA signs for the new address. Devices that trusted the old one warn until they trust it.",
      ),
    ).toBeInTheDocument();
    expect(dialog.getByText("The Proxmox VE entry keeps the old address.")).toBeInTheDocument();
    await user.click(dialog.getByRole("button", { name: "Cancel" }));

    await user.click(screen.getByRole("radio", { name: "Appliance CA" }));
    expect(screen.getByRole("button", { name: "In use" })).toBeDisabled();
    await user.click(screen.getByRole("radio", { name: "Installer's" }));
    await user.click(screen.getByRole("button", { name: "Use installer's" }));
    const confirm = within(screen.getByRole("dialog", { name: "Use installer's certificate" }));
    expect(
      confirm.getByText("The appliance CA and its key are deleted, so devices that trust it trust nothing here."),
    ).toBeInTheDocument();
    expect(confirm.getByText("The ACME setting and its DNS credentials are removed.")).toBeInTheDocument();
    await user.click(confirm.getByRole("button", { name: "Cancel" }));

    await user.click(screen.getByRole("radio", { name: "Proxmox VE CA" }));
    expect(screen.getByRole("button", { name: "Copy command" })).toBeInTheDocument();
  });

  it("9: disables the actions with a reason when nothing can change, and keeps the form on busy", async () => {
    fakeServer(read({ changeable: false }));
    const { unmount } = render(<InterfaceCertificate />);
    await expect(screen.findByText("No appliance directory, so changes cannot be kept")).resolves.toBeInTheDocument();
    expect(screen.getByLabelText("Certificate files")).toBeDisabled();
    expect(screen.getByRole("button", { name: "Replace" })).toBeDisabled();
    for (const radio of screen.getAllByRole("radio")) {
      expect(radio).toBeDisabled();
    }
    expect(screen.getByText("Uploaded", { selector: "dd" })).toBeInTheDocument();
    unmount();

    fakeServer({
      ...read(),
      [`PUT ${path}`]: {
        status: 409,
        body: { error: "Another change of the certificate is under way. Try again in a moment", code: "busy" },
      },
    });
    const user = userEvent.setup({ delay: null });
    render(<InterfaceCertificate />);
    await user.upload(await screen.findByLabelText("Certificate files"), pem("site.pem"));
    await user.click(screen.getByRole("button", { name: "Replace" }));
    await expect(screen.findByRole("alert")).resolves.toHaveTextContent(
      "Another change of the certificate is under way. Try again in a moment",
    );
    expect(screen.getByRole("list", { name: "Chosen files" })).toHaveTextContent("site.pem");
    expect(screen.getByRole("button", { name: "Replace" })).toBeEnabled();
  });

  it("10: shows a running request on its button and makes the other actions wait", async () => {
    const answer = held();
    fakeServer({ ...read(), [`PUT ${path}`]: answer.route });
    const user = userEvent.setup({ delay: null });
    render(<InterfaceCertificate />);
    await user.upload(await screen.findByLabelText("Certificate files"), pem("site.pem"));
    await user.click(screen.getByRole("button", { name: "Replace" }));
    await expect(screen.findByRole("button", { name: "Replacing…" })).resolves.toBeDisabled();
    for (const radio of screen.getAllByRole("radio")) {
      expect(radio).toBeDisabled();
    }
    expect(screen.getByLabelText("Certificate files")).toBeDisabled();
    expect(screen.getByRole("button", { name: "Remove site.pem" })).toBeDisabled();
    answer.answer({ body: certificate({ fingerprint: fingerprint("CD") }) });
    await expect(screen.findByRole("button", { name: "Replace" })).resolves.toBeDisabled();
    for (const radio of screen.getAllByRole("radio")) {
      expect(radio).toBeEnabled();
    }
  });

  it("11: links the setup page's expiry warning to the page", async () => {
    window.history.replaceState(null, "", "/");
    fakeServer({
      ...signedIn,
      "GET /api/setup": {
        body: {
          appliance: true,
          ready: true,
          allowed: true,
          applianceVMID: 105,
          certificateExpires: inDays(10),
        },
      },
      "GET /api/clusters": { body: [] },
      "GET /api/fleet/health": { body: [] },
      "GET /api/runs": { body: [] },
    });
    render(<App />);
    await screen.findByText(/certificate expires on/u);
    const link = screen.getByRole("link", { name: "Certificates" });
    expect(link).toHaveAttribute("href", "/administration/certificates");
    expect(link.closest("p")).toHaveTextContent(/certificate expires on/u);
  });

  it("13: shows what the demo answers as a believable certificate with nothing to warn of", async () => {
    // The answer of `kubehenge serve --demo --demo-time 2026-10-13T09:00:00Z`.
    const demo: Certificate = {
      source: "installer",
      subject: "CN=kubehenge.example.org",
      dnsNames: ["kubehenge.example.org", "kubehenge"],
      ipAddresses: ["192.0.2.10"],
      issuer: "CN=Example Org Internal CA,O=Example Org",
      notBefore: "2026-09-13T00:00:00Z",
      notAfter: "2027-09-13T00:00:00Z",
      fingerprint: "07:9E:79:11:74:D0:7B:8C:62:13:3C:C1:03:0A:AE:6C:75:8C:79:97:26:90:F1:57:58:D6:E4:2A:53:ED:CC:1A",
      chain: ["CN=kubehenge.example.org", "CN=Example Org Internal CA,O=Example Org"],
      incomplete: false,
      selfSigned: false,
      publicURL: "https://kubehenge.example.org",
      publicNameResolves: true,
      changeable: true,
    };
    vi.useFakeTimers({ toFake: ["Date"], now: new Date("2026-10-13T09:00:00Z") });
    fakeServer({ [`GET ${path}`]: { body: demo } });
    render(<InterfaceCertificate />);
    await screen.findByText("Source");
    expect(screen.getByText("Installer", { selector: "dd" })).toBeInTheDocument();
    expect(screen.getByText("kubehenge.example.org, kubehenge, 192.0.2.10")).toBeInTheDocument();
    expect(screen.getByText("334 days left")).toBeInTheDocument();
    expect(screen.getByText(demo.fingerprint)).toBeInTheDocument();
    expect(screen.getByText("Resolves")).toBeInTheDocument();
    expect(document.querySelector(".warning")).toBeNull();
    expect(screen.getByLabelText("Certificate files")).toBeEnabled();
    expect(screen.getAllByRole("radio")).toHaveLength(6);
  });

  it("12: passes the accessibility checks in each state and is reached by keyboard alone", async () => {
    const answer = held();
    fakeServer({
      ...read({ notAfter: inDays(5), incomplete: true, missingIssuer: "CN=Example Root CA", notServed: "no key" }),
      [`PUT ${path}`]: sequence([
        { status: 422, body: { error: "host.pfx is encrypted. Send its passphrase", code: "passphrase" } },
        {
          status: 422,
          body: { error: "The certificate is not valid. Send another", code: "wrong-name", names: ["a.example.org"] },
        },
      ]),
      [`POST ${path}/self-signed`]: answer.route,
    });
    const user = userEvent.setup({ delay: null });
    const { container } = render(<InterfaceCertificate />);
    await screen.findByText("Source");
    await expectNoViolations(container, "interface certificate, warnings");

    // Tab order: the copy button, the link to the public address, the address change, the radios
    // (one stop), the file picker's button.
    await user.tab();
    expect(screen.getByRole("button", { name: "Copy fingerprint" })).toHaveFocus();
    await user.tab();
    expect(screen.getByRole("link", { name: "Open" })).toHaveFocus();
    await user.tab();
    expect(screen.getByRole("button", { name: "Change address" })).toHaveFocus();
    await user.tab();
    expect(screen.getByRole("radio", { name: "Upload files" })).toHaveFocus();
    await user.tab();
    expect(screen.getByRole("button", { name: "Choose files" })).toHaveFocus();
    await user.upload(screen.getByLabelText("Certificate files"), pem("host.pfx"));
    await user.tab();
    expect(screen.getByRole("button", { name: "Add files" })).toHaveFocus();
    await user.tab();
    expect(screen.getByRole("button", { name: "Remove host.pfx" })).toHaveFocus();
    await user.tab();
    expect(screen.getByRole("button", { name: "Replace" })).toHaveFocus();
    await user.keyboard("{Enter}");
    await screen.findByLabelText("Passphrase");
    await expectNoViolations(container, "interface certificate, passphrase");

    await user.keyboard("{Enter}");
    await screen.findByText("Covers: a.example.org");
    await expectNoViolations(container, "interface certificate, refusal");

    screen.getByRole("radio", { name: "Upload files" }).focus();
    await user.keyboard("{ArrowDown}");
    expect(screen.getByRole("radio", { name: "ACME" })).toBeChecked();
    await user.selectOptions(screen.getByLabelText("Proof"), "dns-01");
    await screen.findByRole("group", { name: "Provider variables" });
    await expectNoViolations(container, "interface certificate, ACME");
    screen.getByRole("radio", { name: "ACME" }).focus();
    await user.keyboard("{ArrowDown}");
    expect(screen.getByRole("radio", { name: "Proxmox VE CA" })).toBeChecked();
    expect(screen.getByText(/sh install\.sh --certificate/u, { selector: "code" })).toBeInTheDocument();
    await expectNoViolations(container, "interface certificate, cluster CA");
    await user.keyboard("{ArrowDown}");
    expect(screen.getByRole("radio", { name: "Appliance CA" })).toBeChecked();
    await user.keyboard("{ArrowDown}");
    expect(screen.getByRole("radio", { name: "Self-signed" })).toBeChecked();
    await user.tab();
    expect(screen.getByRole("button", { name: "Make self-signed" })).toHaveFocus();
    await user.keyboard("{Enter}");
    const dialog = screen.getByRole("dialog", { name: "Make self-signed certificate" });
    await expectNoViolations(container, "interface certificate, confirmation");
    within(dialog).getByRole("button", { name: "Cancel" }).focus();
    await user.keyboard("{Enter}");
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(screen.getByRole("button", { name: "Make self-signed" })).toHaveFocus();

    await user.keyboard("{Enter}");
    within(screen.getByRole("dialog", { name: "Make self-signed certificate" }))
      .getByRole("button", { name: "Make self-signed" })
      .focus();
    await user.keyboard("{Enter}");
    await screen.findByRole("button", { name: "Making…" });
    await expectNoViolations(container, "interface certificate, running");
    answer.answer({ body: certificate({ source: "self-signed", selfSigned: true }) });
    await screen.findByText("Self-signed, so browsers warn");
  });

  it("14: drops the setup page's expiry warning once the certificate is replaced", async () => {
    window.history.replaceState(null, "", "/administration/certificates");
    const fresh = { appliance: true, ready: true, allowed: true, applianceVMID: 105 };
    let setup: object = { ...fresh, certificateExpires: inDays(10) };
    fakeServer({
      ...signedIn,
      ...read({ notAfter: inDays(10) }),
      "GET /api/setup": () => ({ body: setup }),
      [`PUT ${path}`]: () => {
        setup = fresh;
        return { body: certificate() };
      },
      "GET /api/clusters": { body: [] },
      "GET /api/fleet/health": { body: [] },
      "GET /api/runs": { body: [] },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await screen.findByText(/certificate expires on/u);
    await user.upload(await screen.findByLabelText("Certificate files"), pem("site.pem"));
    await user.click(screen.getByRole("button", { name: "Replace" }));
    await waitFor(() => expect(screen.queryByText(/certificate expires on/u)).not.toBeInTheDocument());
  });

  it("15: gives the node's command for a certificate the cluster's CA signs, naming this appliance", async () => {
    window.history.replaceState(null, "", "/administration/certificates");
    const calls = fakeServer({
      ...signedIn,
      ...read(),
      "GET /api/setup": { body: { appliance: true, ready: true, allowed: true, applianceVMID: 105 } },
      "GET /api/clusters": { body: [] },
      "GET /api/fleet/health": { body: [] },
      "GET /api/runs": { body: [] },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.click(await screen.findByRole("radio", { name: "Proxmox VE CA" }));
    expect(screen.getByText("sh install.sh --certificate --ctid 105", { selector: "code" })).toBeInTheDocument();
    expect(calls.filter((c) => !c.key.startsWith("GET "))).toHaveLength(0);
  });

  it("16: sets up ACME from a private server with DNS proof, then shows its state and orders again", async () => {
    const view: ACMEView = {
      directory: "https://ca.example.org/acme/directory",
      email: "admin@example.org",
      proof: "dns-01",
      externalAccountBinding: { keyID: "kid-1", hmacKeySet: true },
      directoryCASet: true,
      dnsProvider: "rfc2136",
      dnsCredentialsSet: ["RFC2136_NAMESERVER", "RFC2136_TSIG_SECRET"],
      resolvers: ["192.0.2.53:53"],
      state: "ordering",
      givenUp: false,
    };
    const issued = certificate({ source: "acme", acme: { ...view, state: "issued", lastSuccess: inDays(0) } });
    const calls = fakeServer({
      ...read(),
      [`GET ${path}/acme/providers`]: { body: { providers: ["cloudflare", "rfc2136"], variablesListed: false } },
      [`PUT ${path}/acme`]: { status: 202, body: certificate({ acme: view }) },
      [`POST ${path}/acme/order`]: { status: 202, body: issued },
    });
    const user = userEvent.setup({ delay: null });
    render(<InterfaceCertificate />);
    await user.click(await screen.findByRole("radio", { name: "ACME" }));
    await user.selectOptions(screen.getByLabelText("ACME server"), "other");
    await user.type(screen.getByLabelText("Directory URL"), view.directory);
    await user.type(screen.getByLabelText("Server CA"), "-----BEGIN CERTIFICATE-----");
    await user.type(screen.getByLabelText("Contact e-mail"), "admin@example.org");
    await user.selectOptions(screen.getByLabelText("Proof"), "dns-01");
    await user.type(screen.getByLabelText("DNS provider"), "rfc2136");
    expect(screen.getByRole("link", { name: "Its variables" })).toHaveAttribute(
      "href",
      "https://go-acme.github.io/lego/dns/rfc2136/",
    );
    await user.type(within(variable(1)).getByLabelText("Name"), "RFC2136_NAMESERVER");
    await user.type(within(variable(1)).getByLabelText("Value"), "192.0.2.53");
    await user.click(screen.getByRole("button", { name: "Add variable" }));
    await user.type(within(variable(2)).getByLabelText("Name"), "RFC2136_TSIG_SECRET");
    await user.type(within(variable(2)).getByLabelText("Value"), "c2VjcmV0");
    await user.type(screen.getByLabelText("DNS resolvers"), "192.0.2.53:53, 198.51.100.53");
    expect(screen.getByLabelText("Proof")).toHaveAccessibleDescription(
      "The ACME server checks a TXT record at _acme-challenge.kubehenge.example.org.",
    );
    await user.type(screen.getByLabelText("Key ID"), "kid-1");
    await user.type(screen.getByLabelText("HMAC key"), "aG1hYw");
    await user.click(screen.getByRole("button", { name: "Use ACME" }));

    await screen.findByText("Ordering");
    expect(calls.find((c) => c.key === `PUT ${path}/acme`)?.body).toStrictEqual({
      directory: view.directory,
      directoryCA: "-----BEGIN CERTIFICATE-----",
      email: "admin@example.org",
      proof: "dns-01",
      dnsProvider: "rfc2136",
      dnsCredentials: { RFC2136_NAMESERVER: "192.0.2.53", RFC2136_TSIG_SECRET: "c2VjcmV0" },
      resolvers: ["192.0.2.53:53", "198.51.100.53"],
      externalAccountBinding: { keyID: "kid-1", hmacKey: "aG1hYw" },
    });
    // The secrets sent are cleared from the form, and the saved ones say so.
    expect(screen.getByLabelText("HMAC key")).toHaveValue("");
    expect(screen.getByLabelText("Server CA")).toHaveValue("");
    expect(within(variable(2)).getByLabelText("Value")).toHaveValue("");
    expect(within(variable(2)).getByLabelText("Value")).toHaveAttribute("placeholder", "Saved");

    await user.click(screen.getByRole("button", { name: "Order now" }));
    await screen.findByText("Issued");
    expect(screen.getByText("Last issued")).toBeInTheDocument();
    expect(calls.filter((c) => c.key === `POST ${path}/acme/order`)).toHaveLength(1);
  });

  it("17: opens on a saved ACME setting, keeps secrets left empty and shows a refusal or a stop", async () => {
    const view: ACMEView = {
      // The server keeps Let's Encrypt's address for the name it was given.
      directory: "https://acme-v02.api.letsencrypt.org/directory",
      proof: "dns-01",
      directoryCASet: false,
      dnsProvider: "cloudflare",
      dnsCredentialsSet: ["CF_DNS_API_TOKEN"],
      resolvers: [],
      state: "failing",
      givenUp: true,
      error: "The DNS record was not found",
    };
    const calls = fakeServer({
      ...read({ source: "acme", acme: view }),
      [`GET ${path}/acme/providers`]: { body: { providers: ["cloudflare"], variablesListed: false } },
      [`PUT ${path}/acme`]: {
        status: 422,
        body: { error: "Cloudflare refused the token. Give another", code: "acme-dns-credentials" },
      },
    });
    const user = userEvent.setup({ delay: null });
    const { container } = render(<InterfaceCertificate />);
    await expect(screen.findByRole("radio", { name: "ACME" })).resolves.toBeChecked();
    expect(screen.getByText("ACME", { selector: "dd" })).toBeInTheDocument();
    expect(screen.getByText("Stopped after a day")).toBeInTheDocument();
    expect(screen.getByText("The DNS record was not found")).toBeInTheDocument();
    expect(screen.getByLabelText("ACME server")).toHaveValue("letsencrypt");
    expect(screen.getByLabelText("DNS provider")).toHaveValue("cloudflare");
    expect(screen.getByLabelText("Name")).toHaveValue("CF_DNS_API_TOKEN");
    expect(screen.getByLabelText("Value")).toHaveAttribute("placeholder", "Saved");
    await expectNoViolations(container, "interface certificate, ACME stopped");

    await user.click(screen.getByRole("button", { name: "Save" }));
    await expect(screen.findByRole("alert")).resolves.toHaveTextContent("Cloudflare refused the token. Give another.");
    expect(calls.find((c) => c.key === `PUT ${path}/acme`)?.body).toStrictEqual({
      directory: "letsencrypt",
      proof: "dns-01",
      dnsProvider: "cloudflare",
      dnsCredentials: { CF_DNS_API_TOKEN: "" },
    });
    expect(screen.getByLabelText("DNS provider")).toHaveValue("cloudflare");
  });

  it("19: says which name ACME proves and where, offers account binding for other servers only", async () => {
    fakeServer({
      ...read(),
      [`GET ${path}/acme/providers`]: { body: { providers: [], variablesListed: false } },
    });
    const user = userEvent.setup({ delay: null });
    const { unmount } = render(<InterfaceCertificate />);
    await user.click(await screen.findByRole("radio", { name: "ACME" }));
    const proof = screen.getByLabelText("Proof");
    expect(proof).toHaveAccessibleDescription(
      "Let's Encrypt checks kubehenge.example.org from the internet on port 443.",
    );
    await user.selectOptions(proof, "http-01");
    expect(proof).toHaveAccessibleDescription(
      "Let's Encrypt checks kubehenge.example.org from the internet on port 80.",
    );
    expect(screen.queryByLabelText("Key ID")).not.toBeInTheDocument();
    await user.selectOptions(screen.getByLabelText("ACME server"), "other");
    expect(proof).toHaveAccessibleDescription("The ACME server checks kubehenge.example.org on port 80.");
    expect(screen.getByLabelText("Key ID")).toBeInTheDocument();
    unmount();

    // An IPv4 and an IPv6 address are not names ACME can prove.
    fakeServer(read({ publicURL: "https://192.0.2.10" }));
    const { unmount: v4done } = render(<InterfaceCertificate />);
    await user.click(await screen.findByRole("radio", { name: "ACME" }));
    expect(screen.getByLabelText("Proof")).toHaveAccessibleDescription("ACME needs a DNS name as the public address.");
    v4done();
    fakeServer(read({ publicURL: "https://[2001:db8::10]:8443" }));
    render(<InterfaceCertificate />);
    await user.click(await screen.findByRole("radio", { name: "ACME" }));
    expect(screen.getByLabelText("Proof")).toHaveAccessibleDescription("ACME needs a DNS name as the public address.");
  });
});
