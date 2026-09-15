# devbox

A personal clone of the box-style cloud development machine, on Google Cloud.

One CLI creates an amd64 Debian box with a full toolchain already installed, reaches it over SSH through an IAP tunnel, pauses it without losing the build cache, snapshots and forks it, moves working trees to it under a positive allowlist, and runs agent sessions on it that survive the laptop closing.

Everything runs on your own GCE project. There is no service, no account, and no control plane.

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
| Add or bump a dependency | `devbox tools add ripgrep`, `devbox tools update` |
| Change the toolchain on a running box | `devbox tools apply dev` |
| Remove everything | `devbox machine destroy dev --confirm=dev` |

## Install

Needs `gcloud` authenticated against your project, plus `ssh`, `rsync`, `git`,
and `mise` on this machine. `mise` is only used to resolve tool versions and to
build devbox itself; the box installs its own copy.

```sh
mise install            # Go, pinned in mise.toml
mise run install        # builds ~/.local/bin/devbox
devbox config init --project <project-id>
devbox help <command>   # a command's usage and flags
```

`devbox config init` writes `~/.devbox/config.toml`. Fill in `service_account`, then:

```sh
devbox network ensure   # firewall rule for IAP SSH, cloud router, NAT for egress
devbox machine new dev  # creates the box, which runs the startup script on boot
devbox ssh dev          # the SSH entry is written automatically on first use
```

Creating a machine costs money and is your decision; run `devbox --dry-run machine new dev`
first to read the exact `gcloud` call it would make. `--dry-run` belongs before the
command name, and every command honors it: a rehearsal prints what it would run and
touches neither your machine nor the box.

## Dependencies

Dependencies live in one table in `~/.devbox/config.toml`, and the CLI edits that table for you.

```toml
[tools]
node = "24.18.0"
python = "3.12.14"
dagger = "0.21.9"

omp_package = "@oh-my-pi/pi-coding-agent"
omp_version = "18.1.22"
```

| Command | Effect |
|---|---|
| `devbox tools add ripgrep` | Resolves the newest version from mise and pins it. Adding the first pin writes the built-in list into the file first, so adding one tool cannot drop the rest. |
| `devbox tools add ripgrep@14.0.0` | Pins an exact version with no lookup. |
| `devbox tools update` | Refreshes every pin and the harness, printing each transition. |
| `devbox tools outdated` | Read-only drift list. |
| `devbox tools apply dev` | Converges a running box from the pins, in one SSH call. A reboot converges too, because the startup script is idempotent. |
| `devbox tools list`, `devbox tools edit` | Show the pins, or open the file in `$EDITOR`. |

An empty table means the built-in toolchain. There is one rule for that, so an empty table cannot mean two different things to two commands. Edits rewrite one line at a time, keep your comments, and are refused if the result would not decode.

## Command surface

- `devbox machine` new, start, stop, suspend, resume, list, show, snapshot, fork, schedule, destroy
- `devbox network` ensure, show
- `devbox ssh-config`, `ssh`, `exec`, `cp`, `forward`, `terminfo`, `editors`, `doctor`
- `devbox push`, `pull`, `trees`
- `devbox agent` start, list, logs, attach, stop
- `devbox tools` list, add, remove, update, outdated, apply, edit
- `devbox bootstrap` show, upload; `devbox image bake`; `devbox toolchain`
- `devbox reconcile`, `devbox config`, `devbox version`

## Where things live

On your machine: `~/.devbox/config.toml` (settings), `~/.devbox/records/` (one durable record per cloud mutation), `~/.devbox/trees.json` and `~/.devbox/agents/` (state), and one managed block in `~/.ssh/config` that every tool reads.

On the box: the data disk at `/mnt/data` carries the Docker root, the containerd root, the Dagger cache, and the pnpm store; trees land in `~/devbox/trees`; agent sessions in `~/devbox/agents`.

## Why the shape is what it is

- **A pd-ssd data disk, not Hyperdisk.** A machine image cannot be captured from an instance with a Hyperdisk attached, and `fork` needs a machine image.
- **No external address by default.** SSH goes through an IAP tunnel; egress comes from the Cloud Router and NAT that `network ensure` creates, because a VM without an external address cannot otherwise reach the internet.
- **The caches live on the data disk, not local SSD.** Local SSD is discarded on stop and suspend, and the point of pausing is to keep a warm build cache.
- **Snapshot quiesces Docker and containerd first.** A disk snapshot is crash-consistent, not application-consistent.
- **A run cap with a stop action.** A forgotten box pauses instead of running all night.

## Safety model

Every mutating call is written to a durable record before it leaves the machine, so a dropped connection cannot leave a billed resource devbox cannot name. When the outcome is genuinely ambiguous the record becomes unresolved and blocks that box until you check the cloud and clear it with `devbox reconcile <id> --note "what you saw"`.

`destroy` prints the full inventory before it checks your confirmation, deletes only resources whose labels name that box, and never deletes a snapshot. Ownership is proven from labels, never inferred from a name.

## What is not verified yet

No test in this repository makes a cloud call or touches a real box; every command is asserted through a recording executor and a recording session. The exact `gcloud` flag spellings for instance schedules, NAT, and machine image properties come from the documented API surface, not from a live call. The first run against a real project is where those get confirmed.

## Development

```sh
mise run check    # build, vet, format check, tests
```
