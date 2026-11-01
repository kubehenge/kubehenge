# Writing for the interface

People scan the interface to get something done, not to read it. Each string answers a question someone has at that moment, in as few words as work. If deleting it would leave no one stuck or mistaken, it stays deleted.

## What gets text

- **Labels and controls say it first.** A field's label and its options name what it is. Add nothing that repeats them and no placeholder text.
- **A hint only where people would otherwise get it wrong:** a format (`192.0.2.11/24`), a unit, a limit, or a consequence they cannot undo. One short sentence, on a third of the fields at most.
- **Info for the rare longer explanation.** A field's `info`, or `Info` beside a checkbox or radio, puts it behind a small "?". Two sentences at most.
  - Anything longer belongs in the docs. A screen that needs many is a screen to redesign.
- **No intros.** No paragraph under a page title, section heading or form. The heading names it and the content shows the rest.
- **No reasons, no internals.** Why Kubehenge works as it does, and what Cluster API, the controller or a provider does underneath, go in the docs. The interface says what people get.
- **Say what changes.** Say what stays the same only before a risky action, where people would fear otherwise.
- **Statuses are states:** Available, Upgrading, Failed. A failure's message goes behind `Detail`.
- **Success shows in the result.** A notice, when one is needed, is the object and what happened, such as "Saved" or "Token copied".

## Lengths

| Kind                                        | At most               | Also                                                           |
| ------------------------------------------- | --------------------- | -------------------------------------------------------------- |
| Label: field, heading, tab, column, option  | 4 words               | Sentence case, no colon, no full stop                          |
| Button                                      | 3 words               | A verb first ("Back up now"), never Submit or OK. No full stop |
| Hint                                        | 10 words, 1 sentence  |                                                                |
| Info                                        | 24 words, 2 sentences |                                                                |
| Status                                      | 6 words, 1 sentence   | Details behind `Detail`                                        |
| Error                                       | 16 words, 2 sentences | What happened, then what to do                                 |
| Any other text: dialog, banner, empty state | 20 words, 2 sentences |                                                                |

A value written into a string (`${name}`) counts as a word.

## Style

- **Sentence case.** Capitals for the first word and for names only, such as Proxmox VE, Talos and Kubernetes.
- **Plain words.** The words the check refuses are `banned` in `src/writing.ts`.
- **Punctuation.** No em or en dashes and no spaced hyphen as a dash. No semicolons, no exclamation marks, no "&", no "e.g." or "etc.".
  - A colon introduces a value (`Failed: checksum mismatch`), never an explanation. Five words after it at most.
- **No padding patterns.** `not only … but`, lists of three where one item says it, a closing sentence that sums up, hedges, parenthetical asides.
- **Numbers and units over prose:** "7 days", "every 6 h", "2 of 3 ready", "412 MiB".
- **Present tense, active voice.** "Deletes the VMs", not "the VMs will be deleted".

## Errors, dialogs and empty states

- **Errors** name what went wrong and the fix, without blame or apology. The server's message is shown as it is, so it follows these rules too.
  - kubectl users read the same words.
- **Dialogs** are titled with the action and its object ("Delete cluster web"). Their confirm button repeats the verb.
  - The body says in one sentence what is lost.
  - Destroying a cluster or data asks for the typed name. A privilege grant names the privilege and what it allows.
- **"…" on a button** only when it opens a dialog that asks for more.
- **Empty states** say what is missing in a few words, next to the button that adds one, such as "No backups yet".

## Terms

One name for each thing, everywhere:

| Use                                | Not                                           |
| ---------------------------------- | --------------------------------------------- |
| cluster, management cluster        | workload cluster, unless both are on the page |
| control plane, control-plane nodes | masters                                       |
| worker, worker pool                | machine deployment, node group                |
| Proxmox VE node                    | host, server, PVE                             |
| VM, VM template                    | virtual machine                               |
| host name                          | hostname                                      |
| add-on                             | addon, plugin                                 |
| storage                            | datastore                                     |
| back up (verb), backup (noun)      |                                               |
| sign in, sign out                  | log in, login, log out                        |
| delete: destroys it and its data   | remove, for the same                          |

## The check

`src/writing.ts` gathers every string the interface shows and `src/writing.test.ts` checks it in `make unit`. Each finding names the file, line, rule and text. A string that has to break a rule goes into `exceptions` in `src/writing.test.ts` with its reason. An exception that no longer matches anything fails the test.
