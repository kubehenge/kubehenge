import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";

import { Dialog, Field } from "./ui";

function Opener({ onSubmit }: { onSubmit: (v: string) => void }) {
  const [open, setOpen] = useState(false);
  const [value, setValue] = useState("");
  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>
        Open
      </button>
      {open && (
        <Dialog
          title="Rename"
          onClose={() => setOpen(false)}
          onSubmit={() => {
            onSubmit(value);
            setOpen(false);
          }}
          actions={
            <button type="submit" className="primary" disabled={!value}>
              Save
            </button>
          }
        >
          <Field label="Name">
            {(id) => <input id={id} value={value} onChange={(e) => setValue(e.target.value)} />}
          </Field>
        </Dialog>
      )}
    </>
  );
}

describe("dialogs", () => {
  it("submit on Enter, close from the title bar, and give the focus back", async () => {
    const onSubmit = vi.fn<(v: string) => void>();
    const user = userEvent.setup({ delay: null });
    render(<Opener onSubmit={onSubmit} />);
    const open = screen.getByRole("button", { name: "Open" });

    await user.click(open);
    expect(screen.getByRole("dialog", { name: "Rename" })).toBeInTheDocument();
    expect(screen.getByLabelText("Name")).toHaveFocus();
    await user.click(screen.getByRole("button", { name: "Close" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(open).toHaveFocus();

    await user.click(open);
    await user.type(screen.getByLabelText("Name"), "web{Enter}");
    await waitFor(() => expect(onSubmit).toHaveBeenCalledWith("web"));
    expect(open).toHaveFocus();
  });
});
