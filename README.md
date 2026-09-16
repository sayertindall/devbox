# devbox

A personal clone of the box-style cloud development machine, on Google Cloud.

The CLI runs on your laptop; the box runs in your own GCE project. One command
creates an amd64 Debian instance with a full toolchain already installed, reaches
it over SSH through an IAP tunnel, pauses it without losing the build cache,
snapshots and forks it, moves working trees to it under a positive allowlist, and
runs agent sessions on it that survive the laptop closing.

Everything runs in your own GCE project: there is no service, no devbox account,
and no control plane.

## What it gives you

| Need | Command |
|---|---|
| A ready machine | `devbox machine new dev` |
| A shell on it | `devbox ssh dev` |
| One command without a shell | `devbox exec dev -- systemctl status docker` |
| The build cache after a pause | `devbox machine stop dev` then `devbox machine start dev` |
| A clone of a configured box | `devbox machine fork dev dev-2` |
| A point-in-time copy of the data disk | `devbox machine snapshot dev` |
| A working tree on the box | `devbox push dev` |
| Its changes back | `devbox pull dev` |
| An agent that outlives the laptop | `devbox agent start dev --provider omp --task "..."` |
| An editor pointed at the box | `devbox editors dev` |
| Add or bump a dependency | `devbox tools add ripgrep`, `devbox tools update` |
| Change the toolchain on a running box | `devbox tools apply dev` |
| Remove everything | `devbox machine destroy dev --confirm=dev` |

## Install

Prerequisites on this machine: `gcloud`, authenticated against a project where
you can create instances, disks, and firewall rules; `ssh`, `rsync`, and `git`;
`mise`, which pins Go and resolves tool versions; `npm`, which resolves the harness
version.

Three roles are what it takes to reach a shell: `roles/compute.admin` to create the
instance and the network, `roles/iap.tunnelResourceAccessor` to open the tunnel to a
box that has no external address, and `roles/compute.osLogin` (or
`roles/compute.osAdminLogin` for sudo on the box) for the login itself.
[RUNBOOK.md](RUNBOOK.md) has the full table, including what the box's service
account needs.

The box uses OS Login, so it accepts only a key your OS Login profile has
registered, and devbox pins `~/.ssh/google_compute_engine` (the key
`gcloud compute ssh` registers) unless `ssh_key` names another. If your profile has
no key yet, make and register one:

```sh
ssh-keygen -t ed25519 -f ~/.ssh/google_compute_engine   # if that pair is missing
gcloud compute os-login ssh-keys add --key-file=~/.ssh/google_compute_engine.pub
```

### Build and configure

```sh
mise install            # Go, pinned in mise.toml
mise run install        # builds ~/.local/bin/devbox
devbox config init --project <project-id>
```

`config init` writes `~/.devbox/config.toml` and prints the settings still blank.
On a fresh file that is `service_account` and `bootstrap_url`; everything else has
a default: zone `us-central1-a`, machine `n2-standard-16`, image
`debian-cloud/debian-13`, a 100 GB pd-balanced boot disk, a 1024 GB pd-ssd data
disk named `devbox-data` at `/mnt/data`, `external_ip = false`, `ssh_prefix =
"devbox-"`, and `remote_user` set to your local `$USER`.

`service_account` is the identity the box runs as, and it is the one setting you
must supply. Set it, and any default you want to change, by editing the file
(`devbox tools edit` opens it). Check `remote_user` while you are there: it
defaults to your local `$USER`, and a box's SSH user is your OS Login profile
name, which is not always the same string.

```sh
devbox bootstrap upload --bucket gs://<bucket>   # publish the script, record bootstrap_url
devbox network ensure                            # firewall for IAP SSH, router, NAT
devbox machine new dev                           # the box bootstraps itself at boot
devbox ssh dev                                   # a shell, through the IAP tunnel
```

`bootstrap upload` is what fills `bootstrap_url`, so it is the one blank setting
you never edit by hand. A box created without a startup script starts bare;
`devbox machine new <name> --no-bootstrap` does that on purpose.

Until the configuration names a project, a `service_account`, and a
`bootstrap_url`, the commands that create machines refuse and name the blank
settings. `config`, `tools`, `bootstrap show`, `toolchain`, `help`, `version`, and
`reconcile` keep working, so a fresh machine is not a dead end.

Two traps in that sequence are worth their own lines.

- **The box's service account reads the script, not you.** An instance fetches
  `startup-script-url` with its own account, which holds no project roles. A bucket
  only project members can read is a bucket the box cannot use: it boots, fetches
  nothing, installs nothing, and the only trace is a 403 in the serial console.
  `devbox bootstrap upload` makes the grant; if you create the box another way,
  make it yourself.
- **A startup script runs at boot, and a box that has finished bootstrapping skips
  it.** Editing the published script does nothing to a box that is already up. A
  stop and a start (`devbox machine stop`, `devbox machine start`) run the script
  again, but a box that finished a bootstrap holds the stamp
  `/var/lib/devbox/bootstrap.ok` and exits at it, which is also why it survives a
  stop, a resume, and a reboot without rebuilding. To rebuild such a box, run the
  rendered script on it with the force flag set (instance metadata is not exported
  to the script, so the variable has to be on the command):

  ```sh
  devbox bootstrap show | devbox ssh <name> -- sudo DEVBOX_BOOTSTRAP_FORCE=1 bash -s
  ```

  Pins are the exception that does not need a rebuild: `devbox tools apply <name>`
  converges them in one SSH call, and re-publishing the script
  (`devbox bootstrap upload --bucket ...`) is what a box created later will run.

### The first connection

A box reports RUNNING before its ssh daemon listens. A session waits up to ninety
seconds for that to clear. If the bootstrap is still running, the session says so on
stderr and names the log to follow; if it failed, the session prints the line the
failure recorded instead of leaving you to read the serial console.

The tunnel gcloud builds prints a warning when NumPy is missing from the
interpreter gcloud runs; without it the tunnel moves bytes more slowly. Install it
into gcloud's own interpreter, not into the system one:

```sh
"$(gcloud info --format='value(basic.python_location)')" -m pip install numpy
```

## The daily loop

```sh
devbox push dev                 # the current directory, as a tree named after it
devbox ssh dev                  # work on it
devbox pull dev                 # the box's copy back over the local one
devbox trees dev                # what the box holds, with the digests recorded here
```

A push removes `~/devbox/trees/<tree>` on the box and writes the staged tree in its
place, so it replaces the destination wholesale. A staging failure on this machine
leaves the box untouched; a transfer that stops partway leaves the tree short, and
running the same command again is the recovery, because the destination is replaced
rather than merged.

What travels is a working copy: `.git` and credential files go, while
`node_modules`, `dist`, `build`, `.venv`, `__pycache__`, `.terraform`, and the other
dependency and build caches stay home. `--everything` sends those too.

`pull` refuses when the local tree changed since the push; `--force` applies the
box's copy anyway and prints what it is writing over. Either way it applies through
a rollback journal and verifies the result before dropping it.

Agent sessions run in tmux on the box, so closing the laptop does not stop them:

```sh
devbox agent start dev --provider omp --task "fix the failing tests" --tree dev
devbox agent list dev
devbox agent logs dev <id>
devbox agent attach dev <id>
devbox agent stop dev <id> [--yes]
```

`--provider` is one of `omp`, `claude`, and `codex`. The session id is the second
positional argument of `logs`, `attach`, and `stop`, not a flag. `start` refuses to
work in a tree that is not on the box, and names the push that would put it there.

```sh
devbox machine stop dev      # the disks stay; nothing else does
devbox machine start dev
devbox machine suspend dev   # the memory is written to disk as well
devbox machine resume dev
```

## Connecting an editor

```sh
devbox editors dev
devbox editors dev /home/<remote_user>/devbox/trees/dev
```

With a path, it prints what to paste. For the pushed tree `v1` on a real box:

```text
zed:        zed ssh://devbox-dev/home/<remote_user>/devbox/trees/v1
zed dialog: devbox-dev      (Remote Projects, Connect New Server: the host alone, never the url)
vscode:     devbox-dev      (Remote-SSH host)
```

Three details decide whether that works.

- The path is absolute and remote. A tilde is not expanded:
  `devbox editors dev '~/devbox/trees/v1'` prints a path holding a literal `~`,
  which is not a directory. A relative path is read from the login user's home,
  which is `/home/<remote_user>`, the OS Login profile name `remote_user` holds.
- Zed's dialog takes the host alone, `devbox-dev`, never the url. That is the
  difference between the dialog working and hanging. VS Code wants the same host as
  a Remote-SSH host.
- Both resolve through the block devbox writes in `~/.ssh/config`, so neither editor
  needs any setup.

`devbox ssh` runs the `ssh` binary directly, so Ghostty's shell wrapper never runs
for it and its terminfo install never happens there. Install the entry on the box
once per box:

```sh
devbox terminfo dev
```

It reads the local `xterm-ghostty` entry with `infocmp`, feeds it to the box's
`tic`, and prints the `SetEnv TERM=xterm-256color` fallback for programs that
cannot see the entry. An interactive `ssh devbox-dev` from a Ghostty window
installs the same entry through the shell integration; the two do not conflict, and
`devbox terminfo` is what covers the sessions devbox opens.

## Where things live

On this machine, all under `~/.devbox` (`DEVBOX_HOME` moves that directory):

| Path | Holds |
|---|---|
| `config.toml` | the settings |
| `records/` | one durable record per cloud mutation |
| `trees.json` | the digest and local path of every tree handed over |
| `agents/` | one directory per session, holding the handoff packet |
| `startup-script.sh` | the script `bootstrap upload` wrote |

Plus one managed block in `~/.ssh/config`, between
`# >>> devbox managed block >>>` and `# <<< devbox managed block <<<`. Every
session refreshes that block, and everything outside it is yours.

On the box, `~` is the login user's home, `/home/<remote_user>`:

| Path | Holds |
|---|---|
| `~/devbox/trees/<tree>` | one pushed tree, and the directory an editor opens |
| `~/devbox/trees/.manifests/` | the manifest each pushed tree arrived with |
| `~/devbox/agents/<id>/` | one agent session's packet and status |
| `/mnt/data/docker`, `/mnt/data/containerd` | the engine data roots, so images survive a stop |
| `/mnt/data/dagger`, `/mnt/data/pnpm-store` | the Dagger engine cache and the pnpm store |
| `/var/log/devbox-bootstrap.log` | every bootstrap phase, and apt's own output |
| `/var/lib/devbox/bootstrap.ok` | the box finished building; the file holds the timestamp |
| `/var/lib/devbox/bootstrap.failed` | a phase failed; the log's last line names the command |

## Dependencies

Every pin lives in one table in `~/.devbox/config.toml`, and the CLI edits that
table for you. An empty table means the built-in toolchain, thirty-one mise pins
plus the agent harness, which `devbox toolchain` prints.

```toml
omp_package = "@oh-my-pi/pi-coding-agent"
omp_version = "18.1.22"

[tools]
node = "24.18.0"
python = "3.12.14"
dagger = "0.21.9"
```

`omp_package` and `omp_version` are top-level keys and have to be written above the
`[tools]` header, or they land inside the table.

| Command | Effect |
|---|---|
| `devbox tools add ripgrep` | Resolves the newest version from mise and pins it. Adding the first pin writes the built-in list into the file first, so adding one tool cannot drop the rest. |
| `devbox tools add ripgrep@14.0.0` | Pins an exact version, with no lookup. |
| `devbox tools update` | Refreshes every pin and the harness, printing each transition. |
| `devbox tools outdated` | Read-only drift list. |
| `devbox tools apply dev` | Converges a running box from the pins, in one SSH call. This is the path for a box that already bootstrapped: a reboot re-runs the startup script, which sees the stamp and exits. |
| `devbox tools remove ripgrep` | Drops one pin. |
| `devbox tools list`, `devbox tools edit` | Show the pins, or open the file in `$EDITOR`. |

Edits rewrite one line at a time, keep your comments, and are refused if the result
would not decode.

## Command surface

Every verb the binary prints. `devbox help <command>` gives the usage line and the
flags for any one of them.

| Command | Subcommands | What it does |
|---|---|---|
| `agent` | `start`, `list`, `logs`, `attach`, `stop` | Sessions on a box, in tmux, so they outlive the laptop |
| `bootstrap` | `show`, `upload` | Render the startup script, and publish it |
| `config` | `init` | Show the effective settings, or create the file |
| `cp` | | One path between this machine and a box, `--down` for the other direction |
| `doctor` | | One line per readiness check, nonzero when any fails |
| `editors` | | The Zed command and the VS Code host for a directory on a box |
| `exec` | | One command, combined output, and its exit code |
| `forward` | | An IAP tunnel from a local port to a box |
| `help` | | The command list, or one command's usage and flags |
| `image` | `bake` | An image of a box's data disk |
| `machine` | `new`, `start`, `stop`, `suspend`, `resume`, `list`, `show`, `snapshot`, `fork`, `schedule`, `destroy` | Create, pause, clone, and destroy boxes |
| `network` | `ensure`, `show` | The firewall, router, and NAT a box with no external address needs |
| `pull` | | Apply the box's tree over the local one, under a rollback journal |
| `push` | | Copy a working tree to a box |
| `reconcile` | | List or clear the records that block a box |
| `ssh` | | A shell on a box, or one command through it |
| `ssh-config` | | Write one box's Host entry into the SSH configuration |
| `terminfo` | | Install the Ghostty `xterm-ghostty` entry on a box |
| `toolchain` | | The toolchain a box installs |
| `tools` | `list`, `add`, `remove`, `update`, `outdated`, `apply`, `edit` | The toolchain pins |
| `trees` | | The trees on a box, with the digests recorded locally |
| `version` | | The version |

The flags that change what a verb does:

```text
devbox machine new <name> [--no-bootstrap] [--schedule]
devbox machine schedule <name> [--start=HH:MM --stop=HH:MM] [--timezone=ZONE] [--remove]
devbox machine destroy <name> --confirm=<name> [--with-images]
devbox machine snapshot <name>          # stops docker and containerd first, then restarts them
devbox agent start <box> --provider <omp|claude|codex> --task <text> [--tree <name>] [--dir <dir>]
devbox agent stop <box> <id> [--yes]
devbox push <name> [path] [--tree <tree>] [--everything]
devbox pull <name> [path] [--tree <tree>] [--force]
devbox cp <name> <source> <target> [--down]
devbox forward <name> [--port <port>]
devbox bootstrap upload [--bucket gs://bucket]
devbox reconcile [<record-id>] [--box <name>] [--note <text>]
devbox ssh <name> [-- command]          # everything after -- reaches the box
```

## Safety model

Every mutating call is written to a durable record before it leaves this machine, so
a dropped connection cannot leave a billed resource devbox cannot name. When the
outcome is genuinely ambiguous the record becomes unresolved and blocks that box
until you check the cloud and clear it with
`devbox reconcile <id> --note "what you saw"`.

`destroy` prints the full inventory before it checks your confirmation, deletes only
resources whose labels name that box, and never deletes a snapshot. Ownership is
proven from labels, never inferred from a name.

`--dry-run` (or `DEVBOX_DRY_RUN=1`) makes any verb print the calls it would make and
stop. It is accepted before or after the command name, so
`devbox --dry-run machine new dev` and `devbox machine new dev --dry-run` are the
same rehearsal. A rehearsal reaches no box, writes no file, records no mutation, and
claims nothing it did not do. A verb that would have to read a box to know what to
do (`machine list`, `machine show`, `destroy`'s inventory) says so instead of
inventing state, and `destroy` still insists on `--confirm=<name>`.

## Why the shape is what it is

- **A pd-ssd data disk, not Hyperdisk.** A machine image cannot be captured from an
  instance with a Hyperdisk attached, and `fork` needs a machine image.
- **No external address by default.** SSH goes through an IAP tunnel; egress comes
  from the Cloud Router and NAT that `network ensure` creates, because a VM without
  an external address cannot otherwise reach the internet.
- **The caches live on the data disk, not local SSD.** Local SSD is discarded on
  stop and suspend, and the point of pausing is to keep a warm build cache.
- **Trees live under the login user's home, at `~/devbox/trees/<tree>`.** One
  directory per tree name, in the home an editor opens, rather than a root-owned
  path no editor can write.
- **Snapshot quiesces Docker and containerd first.** A disk snapshot is
  crash-consistent, not application-consistent.
- **A run cap with a stop action.** A forgotten box pauses instead of running all
  night.

## What is verified, and what is not

Verified against a real project on 2026-09-15, end to end: `config init`,
`bootstrap upload` including the bucket grant, `machine new` and the box's own
bootstrap, `ssh` through the IAP tunnel (the managed entry, the OS Login key, the
remote command), `push` from a tree root, which reported
`pushed tree v1 to dev: 12443 files, 61123965 bytes`, `terminfo`, which installed
the Ghostty entry on the box, and `editors`, through which Zed connected to the box.

Not verified against a real project: instance schedules, the exact flag spellings
for NAT and machine image properties, `machine snapshot`, `machine fork`,
`image bake`, `pull` end to end, and agent sessions on a box. No test in this
repository makes a cloud call; every command is asserted through a recording
executor and a recording session. Treat the first real run of one of those as its
test, and fix the flag rather than the doc when they disagree.

## Development

```sh
mise run check    # build, vet, format check, tests
mise run test     # tests only
mise run race     # tests under the race detector
mise run build    # build to ./bin
mise run install  # build to ~/.local/bin/devbox
```

Run `mise run check` before you claim anything works.

[RUNBOOK.md](RUNBOOK.md) is the operator's path in more detail: the IAM you grant
once, pausing and what it costs, recovery, and destroying.
[AGENTS.md](AGENTS.md) is the guide for changing this repository: the contracts a
change has to respect, the conventions, and what the test suite does not verify.
