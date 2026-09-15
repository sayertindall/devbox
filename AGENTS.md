# Working in this repository

`devbox` is a single-operator Go CLI that manages a personal cloud development
machine on Google Cloud. It is small on purpose: one binary, standard library
plus one TOML decoder, no service, no control plane.

## The loop this code serves

Create a box, get a shell on it, move a working tree to it, run an agent there
that outlives the laptop, pause it, come back, pull the changes, occasionally
fork or destroy. Every command exists to make one step of that loop shorter.

## Contracts a change must respect

- **One command contract.** A command is a `cli.Command` (name, summary, usage,
  optional `ConfigOnly`, optional `Help`, and a `Run`). It receives `cli.Deps`
  and its own arguments, reads no global, and opens no configuration of its own.
  Slices expose `Commands()`; `cmd/devbox` registers them and rejects a duplicate
  name at startup. `internal/cli/cli.go` is the contract.
- **One executor contract.** Every cloud call goes through `gcloud.Executor` as
  an explicit argument vector. `gcloud.Runner` runs it and prints it under
  `--dry-run`; `gcloud.Fake` records it so a test never needs a project.
  Nothing builds a shell string. `internal/gcloud/gcloud.go`.
- **Records before mutations.** An operation that can change or spend anything
  writes a durable, fsynced record of the exact argv *before* the call, and
  records what happened after. An uninterpretable outcome leaves the record
  unresolved, which blocks that box until the operator clears it with
  `devbox reconcile`. `internal/record/record.go`, used by `machine`, `fork`,
  `snapshot`, `destroy`, `image bake`, and `agent start`.
- **The artifact equals the generator.** `deploy/startup-script.sh` is generated
  by `internal/bootstrap.Render` and compared byte for byte in a test. Never
  hand-edit it; change the template and regenerate.
- **Positive allowlist.** What leaves or arrives on this machine is exactly what
  a manifest declares. Excluded paths (`.env*`, `.ssh`, `.aws`, `.config`,
  `.claude`, `.codex`, `.omp`, `.git`, `node_modules`, `dist`, `build`) can never
  enter a projection. `internal/manifest`, `internal/baseline`.
- **Refusals carry the next command.** If a command says no, it says what to run
  next (the record to reconcile, the `push` that is missing, the label to add).
  A refusal without a next action is a bug.
- **Ownership comes from labels.** A box, disk, or image devbox did not label is
  never deleted, started, or snapshotted.

## Conventions

- Name the domain, never the assignment. No ticket numbers, no phase names, no
  agent identities in files, identifiers, test names, or comments.
- Comments explain an invariant or a reason a reader cannot see from the code.
  Do not restate the code, and do not add a comment that will be false after the
  next change.
- Tests assert what a caller observes: the exact argument vector, the state
  transition, the refusal, the bytes written. They use `gcloud.Fake` and
  `access.Recording`; no test makes a network or cloud call.
- A command that cannot act must leave nothing behind: no state directory, no
  record, no file.

## Commands

```sh
mise run check     # the gate: build, vet, format check, tests
mise run test      # tests only
mise run race      # tests under the race detector
mise run install   # build to ~/.local/bin/devbox
```

Run `mise run check` before you claim anything works. A change to the toolchain
list, the startup script, or a command's usage line is only done when that gate
is green.

## Where state lives

`~/.devbox/config.toml` (settings), `~/.devbox/records/` (one record per cloud
mutation), `~/.devbox/trees.json` and `~/.devbox/agents/` (slice state), and one
managed block in `~/.ssh/config` between the devbox markers. Nothing outside
those paths belongs to devbox, and the text outside the markers is the
operator's.

## Not verified by the test suite

No test has ever called the cloud. Flag spellings for instance schedules, NAT,
and machine image properties come from the documented API surface. Treat the
first run against a real project as the test for those, and fix the flag rather
than the doc when they disagree.
