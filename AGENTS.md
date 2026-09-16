# devbox

A single-operator Go CLI that manages a personal cloud development machine on
Google Cloud. One binary, standard library plus one TOML decoder, no service, no
control plane. Everything runs in the operator's own project.

This file is for an agent that has to use the tool and for one that has to change
it. The first half is usage, the second half is the contracts a change must keep.

## The loop

Create a box, get a shell on it, move a working tree to it, run an agent there
that outlives the laptop, pause it, come back, pull the changes, occasionally
fork or destroy. Every command exists to make one step of that loop shorter.

## What a box has

A box's environment is produced by a rendered startup script
(`internal/bootstrap/render.go`), not by configuration management. Two layers.

**Pinned tools, installed with mise** (`internal/bootstrap/toolchain.go`,
printed by `devbox toolchain`):

| Domain | Pins |
|---|---|
| Development | node 24.18.0, pnpm 12.4.1, bun 1.4.2, go 1.26.5, python 3.12.14, rust 1.98.0, uv 0.8.9, jq 1.8.2 |
| Platform | kubectl 1.34.11, helm 3.16.3, flux2 2.9.5, talosctl 1.14.0, opentofu 1.12.6, cue 0.17.1, kubeconform 0.8.0, conftest 0.70.0, dagger 0.21.9, oras 1.3.4, cosign 3.1.3 |
| Operator | gh 2.100.0, just 1.42.4, ripgrep 15.2.0, fd 10.5.0, ast-grep 0.45.3, hunk 0.19.0, herdr 0.9.0, zellij 0.45.1 |
| Shell and files | starship 1.26.0, atuin 18.22.0, chezmoi 2.72.2, 1password-cli 2.39.0 |
| Harness | `@oh-my-pi/pi-coding-agent` 18.1.22 |

Thirty-one pins plus the harness. `expose_shims` links their shims into
`/usr/local/bin`, sixty-nine binaries in all, including the companions each tool
brings (`npm`, `npx`, `cargo`, `rustc`, `python3`, `uvx`, `tofu`, and so on).

**System packages, installed with apt**: prerequisites (`ca-certificates`,
`curl`, `e2fsprogs`, `gnupg`, `kmod`), the operator packages Debian carries
(`build-essential`, `gh`, `git`, `jq`, `rsync`, `tmux`), the shared libraries a
headless Chromium needs, Docker CE with buildx and compose from Docker's
repository, and `google-cloud-cli` from Google's repository.

Where a package exists in both layers the pin wins, because `/usr/local/bin`
precedes `/usr/bin` on PATH. `gh` is the example: Debian's 2.46 is installed and
the pinned 2.100.0 is what runs.

**Docker and the data disk**

- Docker and containerd run from systemd units, enabled at boot.
- The data disk (`data_disk_name`, default `devbox-data`) is formatted ext4 on
  first boot and mounted at `data_mount` (default `/mnt/data`). It holds Docker's
  data root `/mnt/data/docker`, containerd's content store
  `/mnt/data/containerd`, the Dagger engine cache `/mnt/data/dagger`, and the pnpm
  store `/mnt/data/pnpm-store`, so every cache survives a stop.
- Pushed trees live under the login user's home at `~/devbox/trees/<tree>`, on
  the boot disk, one directory per tree name, owned by that user. That directory
  is the one an editor opens, and the manifest each push sent sits beside the
  trees in `~/devbox/trees/.manifests/<tree>.json` (`internal/tree/paths.go`).
  The bootstrap creates none of it: the first push to a box does.
- The login user runs `docker` without sudo, because the bootstrap puts it in
  the `docker` group. `machine snapshot` and `image bake` stop and start the
  storage services with `sudo systemctl` over SSH, so that user needs sudo
  without a password prompt as well.

**The account and the network**: the instance uses OS Login, so the SSH user is
the operator's OS Login profile name (their `remote_user`). A box has no external
address by default, which makes an IAP tunnel the way in and the NAT from
`devbox network ensure` the way out. That tunnel is
`gcloud compute start-iap-tunnel`, which runs inside gcloud's own interpreter,
and gcloud wants NumPy there: without it the tunnel still connects, but it prints
a warning and carries less throughput. Install it into that interpreter, not into
a virtualenv or the system Python:

```sh
"$(gcloud info --format='value(basic.python_location)')" -m pip install numpy
```

**What is not on a box**: browsers (their libraries are installed, the browser
comes from whatever downloads it), any authentication state (`gh`, `gcloud`,
`op`, the harness, container registries), your dotfiles (chezmoi is installed and
nothing is applied), and your repositories.

**The environment carries nothing but PATH.** The rendered script writes
`/etc/environment` with one `PATH` line, and the mise shims directory is on it,
so a non-interactive `ssh <alias> bash -c` command finds every pinned tool
without reading a profile; the links in `/usr/local/bin` are the second guarantee
for a session that never reads that file. Nothing else is exported to a session,
so the only state a command inherits is the one it reads for itself.

## From nothing to a shell

Run in this order. Each step exists because the one after it depends on it.

```sh
devbox config init                      # or: devbox tools edit, devbox config show
devbox network ensure                   # firewall, router, NAT: without these a box has no egress
devbox bootstrap upload --bucket gs://<bucket>
                                        # publishes the startup script and grants the box's
                                        # service account read on that bucket
devbox machine new <name>               # the startup script runs at boot, not at create
devbox ssh <name>                       # waits out the boot, then opens the operator's session
```

Two traps in that sequence are worth their own lines.

- **The box's service account reads the script, not you.** An instance fetches
  `startup-script-url` with its own account, which holds no project roles. A
  bucket only project members can read is a bucket the box cannot use: it boots,
  fetches nothing, installs nothing, and the only trace is a 403 in the serial
  console. `devbox bootstrap upload` makes the grant; if you create the box
  another way, make it yourself.
- **A startup script runs at boot.** Editing the published script does nothing to
  a box that is already up. Stop and start it (`devbox machine stop`,
  `devbox machine start`) to make it run again. A box that finished a bootstrap
  keeps it across a stop, a resume, and a reboot, and skips the long path. To
  rebuild a box that already has its stamp, run the rendered script on it with
  the force flag set (instance metadata is not exported to the script, so the
  variable has to be on the command):

  ```sh
  devbox bootstrap show | devbox ssh <name> -- sudo DEVBOX_BOOTSTRAP_FORCE=1 bash -s
  ```

## Day to day

| Task | Command |
|---|---|
| Shell on a box | `devbox ssh <name>` |
| One command, output captured | `devbox exec <name> -- <command>` |
| Move a file either way | `devbox cp <name> <source> <target> [--down]` |
| Local port to a box | `devbox forward <name> [--port <port>]` |
| Is it ready to work | `devbox doctor <name>` |
| Terminal entry for a Ghostty session | `devbox terminfo <name>` |
| Open the box in an editor | `devbox editors <name> [path]` |
| Send a working tree | `devbox push <name> [path] [--tree <tree>] [--everything]` |
| Take changes back | `devbox pull <name> [path] [--tree <tree>] [--force]` |
| List pushed trees | `devbox trees <name>` |
| Pause and resume | `devbox machine stop <name>` / `devbox machine start <name>` |
| Clone a box | `devbox machine fork <name> <new>` |
| Snapshot without cloning | `devbox machine snapshot <name>` |
| Bake the data disk | `devbox image bake <name> <image>` |
| Converge the pins | `devbox tools apply <name>` |
| Destroy, and only its things | `devbox machine destroy <name> --confirm=<name> [--with-images]` |
| Clear a blocked box | `devbox reconcile` |

`push` and `pull` move only what a manifest declares; excluded paths never leave
the machine. `pull` refuses when the local tree changed since the push.

A push replaces the destination tree wholesale. It stages the projection locally
first, then removes `~/devbox/trees/<tree>` on the box and uploads into the fresh
directory, so an upload that dies halfway is fixed by running the same command
again, and a staging failure never touches the box at all. Nothing else on the
box is removed: the upload is `rsync -a` without `--delete`.

`devbox terminfo <name>` installs the local `xterm-ghostty` entry on the box with
the box's own `tic`. Ghostty's `ssh-terminfo` shim installs the same entry once
per destination and records it in its own cache under the resolved `user@host`,
so it announces itself on the first unrecorded install and is quiet afterwards.
It never runs for `devbox ssh`, which execs the ssh binary directly and so
bypasses the shell wrapper; it covers an interactive `ssh <name>` from a Ghostty
window. `devbox doctor` reports a missing entry as the
`terminfo-xterm-ghostty` check.

## Running an agent on a box

```sh
devbox agent start <box> --provider <provider> --task "<what it should do>" \
  [--tree <name>] [--dir <dir>]
devbox agent list <box>
devbox agent logs <box> <id>
devbox agent attach <box> <id>
devbox agent stop <box> <id> [--yes]
```

The session id is the second positional argument, not a flag. Sessions run in
tmux on the box, so closing the laptop does not stop them. `agent start` writes a
record before it acts, like every other mutation.

`--provider` takes one of the three harnesses devbox starts, and nothing else:
`omp`, `claude`, or `codex`, each run in its own non-interactive mode (`omp -p`,
`claude -p`, `codex exec`). The table is `internal/agent/session.go`, and the
usage line prints it rather than repeating it.

`--tree` names a tree that is already on the box and starts the session inside
it; a tree that never arrived is refused with the `push` that would fix it.
`--dir` names the working directory instead, absolute or starting with `~`, and
defaults to the tree, then to the remote home.

## Diagnosing a box that will not come up

Three markers on the box say everything the startup script did:

| Path | Meaning |
|---|---|
| `/var/log/devbox-bootstrap.log` | every phase, and now apt's own output |
| `/var/lib/devbox/bootstrap.ok` | the box finished building; its content is the timestamp |
| `/var/lib/devbox/bootstrap.failed` | a phase failed; the log's last line names the command |

```sh
devbox ssh <name> -- tail -30 /var/log/devbox-bootstrap.log     # the phase and its error
devbox ssh <name> -- test -f /var/lib/devbox/bootstrap.ok && echo built
devbox doctor <name>                                            # one line per check, nonzero on FAIL
gcloud compute instances get-serial-port-output <name> --project=<p> --zone=<z> | tail -40
```

The serial console is where the metadata agent records the startup script's own
stderr, so it is the fallback when the log itself is missing.

Common failures, each seen on a real box:

- `E: Unable to locate package <name>`. The name is not in the target Debian
  release. Debian 13 does not carry zellij, which is why it is a mise pin. Check
  a new package with `apt-get install -s` on a box before adding it.
- `does not have storage.objects.get access`. The bucket grant is missing.
- A connection refused or `failed to connect to backend` within thirty seconds of
  a start is a box that is still booting. A session waits ninety seconds for that
  to clear and reports a refused key immediately.
- `Permission denied (publickey)`. OS Login ignores instance metadata keys. The
  key it accepts is the operator's `~/.ssh/google_compute_engine` unless
  `ssh_key` names another, and the managed SSH entry pins exactly that.

## Working on this repository

`README.md` is the operator's guide to the tool and `RUNBOOK.md` is how to run a
box from a bare project to a working machine. This half is the contract a change
must keep.

This repository is public. Nothing in it names one operator's environment: not a
login user, an email, a project, a bucket, a service account, or a host. Examples
use the placeholders `<remote_user>`, `<project-id>`, `gs://<bucket>`, and a box
named `dev`, whose derived SSH alias `devbox-dev` is the shape any box named
`dev` gets. Real output is welcome; strip the names out of it first. A shipped
example that leaked a login user is what this paragraph exists to prevent.

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
- **Read gcloud's real shapes.** A `describe` answers with one JSON object and a
  `list` answers with an array of them; `gcloud.Resources` normalizes both and
  `gcloud.Object` refuses a list where one object belongs. Every read whose
  output is decoded asks for `--format=json` through one helper. A fixture that
  carries a shape gcloud never produces hides exactly this bug.
- **Records before mutations.** An operation that can change or spend anything
  writes a durable, fsynced record of the exact argv *before* the call, and
  records what happened after. An uninterpretable outcome leaves the record
  unresolved, which blocks that box until the operator clears it with
  `devbox reconcile`. `internal/record/record.go`, used by `machine`, `fork`,
  `snapshot`, `destroy`, `image bake`, and `agent start`.
- **The artifact equals the generator.** `deploy/startup-script.sh` is generated
  by `internal/bootstrap.Render` and compared byte for byte in a test. Never
  hand-edit it; change the template and regenerate:

  ```sh
  DEVBOX_UPDATE_ARTIFACT=1 go test -count=1 -run TestRenderMatchesDeployArtifact ./internal/bootstrap/
  ```
- **The published script comes from the installed binary.** `devbox bootstrap
  upload` renders at publish time, so a renderer change that was not rebuilt
  republishes the old script. Run `mise run install` first, then prove what you
  are about to publish:

  ```sh
  devbox bootstrap show | grep -c '<a line you added>'   # must not be 0
  ```
- **Three projections, one policy type.** `manifest.Mode` says which one:
  `ModeProjection` (the zero value) carries a project's own files and refuses a
  repository found inside the tree, which is what a baseline and an agent packet
  are; `ModeWorkingCopy` carries a working tree, so repository metadata and
  credential files travel and only dependency and build output stays home, which
  is what `push` does; `ModeVerbatim` carries the directory exactly as it is on
  disk, link chains included, which is what `push --everything` does. The
  exclusion list and `isBuildOutput` are the mechanical part.
- **A push sends a working copy.** `isBuildOutput` is the whole of what stays
  behind, because the box can install it again and it is most of the bytes:
  `node_modules`, `dist`, `build`, `.venv`, `venv`, `__pycache__`, `coverage`,
  and the toolchain caches `.terraform`, `.pytest_cache`, `.mypy_cache`,
  `.ruff_cache`, `.turbo`, `.next`, `.parcel-cache`, and `.gradle`. Everything
  else travels, including `.git` and the `.env` files a narrow projection
  refuses. `--everything` sends the build output too.
- **The allowlist is decided per mode.** Every walk entry goes through
  `admit`, and exclusion is by base name, at any depth, case-insensitively, so a
  nested `node_modules` or a `sub/.env` is decided exactly like a top-level one.
  `ModeProjection` (the zero value, what a baseline and an agent packet use)
  skips every name in `mandatoryExcludes`: `.env`, the `.env.*` family, `.ssh`,
  `.aws`, `.config`, `.claude`, `.codex`, `.omp`, `.agentbox`, `.git`, and the
  build output below, and it refuses a `.git` deeper in the tree outright
  because that is a nested repository it cannot carry. `ModeWorkingCopy` skips
  only build output, so repository metadata, `.env` files, and `.ssh` all travel
  with the tree. `ModeVerbatim` skips nothing and resolves no links. The
  projection a push selected is recorded in the handoff, so `pull` rebuilds the
  projection the push produced rather than refusing it.
- **Refusals carry the next command.** If a command says no, it says what to run
  next (the record to reconcile, the `push` that is missing, the label to add).
  A refusal without a next action is a bug.
- **A rehearsal never acts.** Every verb honors `--dry-run` (and
  `DEVBOX_DRY_RUN=1`): it prints the calls it would make and stops. It must not
  reach a box, write a file, record a mutation, or claim an action it did not
  take. `cli.Deps.Outcome` is the one place that phrases "did" against "would".
- **Ownership comes from labels.** A box, disk, or image devbox did not label is
  never deleted, started, or snapshotted.

## Conventions

- Name the domain, never the assignment. No ticket numbers, no phase names, no
  agent identities in files, identifiers, test names, or comments.
- Comments explain an invariant or a reason a reader cannot see from the code.
  Do not restate the code, and do not add a comment that will be false after the
  next change.
- Tests assert what a caller observes: the exact argument vector, the state
  transition, the refusal, the bytes written. They use `gcloud.Fake`, a
  recording executor, and `access.Recording`, a recording session: no test makes
  a cloud call, and none needs a box.

## Commands

```sh
mise run check     # the gate: build, vet, format check, tests
mise run test      # tests only
mise run race      # tests under the race detector
mise run build     # build to ./bin
mise run install   # build to ~/.local/bin/devbox
```

Run `mise run check` before you claim anything works. A change to the toolchain
list, the startup script, or a command's usage line is only done when that gate
is green.

## Where state lives

`~/.devbox/config.toml` (settings; `DEVBOX_HOME` moves the directory),
`~/.devbox/records/` (one record per cloud mutation), `~/.devbox/startup-script.sh`
(the script the last `bootstrap upload` wrote), `~/.devbox/trees.json` (the last
handoff of each tree on each box), `~/.devbox/agents/` (one directory per agent
session), `~/.devbox/journal/` (the rollback journal a `pull` keeps until the
apply verifies), and one managed block in `~/.ssh/config` between
`# >>> devbox managed block >>>` and `# <<< devbox managed block <<<`. Nothing
outside those paths belongs to devbox, and the text outside the markers is the
operator's.

## What the test suite does not cover

No test calls the cloud, and none needs a box: the cloud is a `gcloud.Fake` that
records every argument vector, and a box is an `access.Recording` session that
records every command and every file move. Flag spellings come from the
documented API surface unless a run proved them.

Verified against a real project: a box created and bootstrapped by itself,
`devbox ssh` reaching it through the IAP tunnel, `devbox push` replacing the tree
on it (`pushed tree v1 to dev: 12443 files, 61123965 bytes`), `devbox terminfo`
installing the Ghostty entry, and Zed connecting to the box through the SSH entry
devbox writes. Earlier runs also cover `network show`, `machine new`, `list`,
`show`, `start`, and `stop`, `bootstrap upload` including the bucket grant, and
the rest of the SSH path (managed entry, OS Login key, remote command).

Not yet exercised against a real project: `network ensure` creating its firewall
rule, router, and NAT, and the exact flag spellings of those NAT calls; instance
schedules; `machine snapshot`; `machine fork` and the machine-image properties it
sets; `image bake`; `pull` end to end; `agent start`; and `destroy`. Treat the
first run of each as its test, and fix the flag rather than the doc when they
disagree.

## Boundaries

Creating, starting, stopping, and deleting instances, writing IAM bindings,
publishing objects, and deleting images or disks are the operator's authority,
not an agent's. An agent may read the project freely, and may write only in this
repository, in its own state directory, and on a box the operator has given it.
