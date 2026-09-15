# Runbook

Setting up and running your own devbox, from a bare project to a working machine.

Everything here is `devbox` doing the work. Where a step needs a permission only
you can grant, or a decision only you can make, it says so.

## 0. What you are building

One Compute Engine instance, Debian 13, amd64, with a data disk that survives
pauses and carries the Docker root, the containerd root, the Dagger cache, and
the pnpm store. It has no external address: you reach it over an IAP tunnel, and
it reaches the internet through the Cloud Router and NAT that `devbox network
ensure` creates. Creating the box runs a startup script that installs the
toolchain, so the first boot takes a few minutes and every later boot is fast.

## 1. Once per project

You need `gcloud` authenticated as an identity that can create compute resources.
Everything in this section is a one-time investment; after it, the loop is two
or three commands.

### IAM, granted by you

| For | Role | Why |
|---|---|---|
| your identity | `roles/compute.admin` | create instances, disks, images, snapshots, firewall rules, routers, NATs |
| your identity | `roles/iam.serviceAccountUser` on the box's service account | attach that service account to the instance |
| your identity | `roles/iap.tunnelResourceAccessor` | open the SSH tunnel, since the box has no external address |
| your identity | `roles/compute.osAdminLogin` | sudo on the box, which `snapshot` needs to stop Docker before capturing the disk |
| the box's service account | `roles/storage.objectViewer` on the bucket holding the startup script | the instance fetches the script at boot |
| the box's service account | `roles/artifactregistry.writer`, if you push images from the box | Dagger pushes what it builds |

```sh
PROJECT=<your-project>
SA=devbox@$PROJECT.iam.gserviceaccount.com

gcloud iam service-accounts create devbox --project=$PROJECT --display-name="devbox instances"
gcloud projects add-iam-policy-binding $PROJECT --member=user:$(gcloud config get-value account) \
  --role=roles/compute.admin
gcloud projects add-iam-policy-binding $PROJECT --member=user:$(gcloud config get-value account) \
  --role=roles/iap.tunnelResourceAccessor
gcloud projects add-iam-policy-binding $PROJECT --member=user:$(gcloud config get-value account) \
  --role=roles/compute.osAdminLogin
gcloud iam service-accounts add-iam-policy-binding $SA --project=$PROJECT \
  --member=user:$(gcloud config get-value account) --role=roles/iam.serviceAccountUser
```

### The configuration

```sh
devbox config init --project $PROJECT
```

It writes `~/.devbox/config.toml` and tells you what is still blank. Open it
(`devbox tools edit`) and set:

- `service_account` to the address above
- `zone` if you do not want `us-central1-a`
- `machine_type`, `data_disk_gb` if the defaults are not what you want

`devbox config show` prints the effective settings and the file they came from.
Until `service_account` is set, every command that would create a machine refuses
and says so; `config`, `tools`, `help`, `version`, and `reconcile` keep working.

### The startup script and the network

```sh
gcloud storage buckets create gs://$PROJECT-devbox      # once
devbox bootstrap upload --bucket gs://$PROJECT-devbox  # publishes the script, records the url
devbox network ensure                                  # firewall for IAP SSH, router, NAT
```

`bootstrap upload` prints the script it wrote and the url it recorded. Re-run it
after any change to `[tools]` or to `bootstrap_url`, and a box you create later
gets the new script. An existing box converges with `devbox tools apply <box>`
or on its next boot.

Before spending anything, read the exact call:

```sh
devbox --dry-run machine new dev
```

## 2. The first box

```sh
devbox machine new dev
```

That makes one instance and its data disk, both labeled for the box. It then
prints the two commands that matter: `devbox network ensure` if you have not run
it, and `devbox ssh dev`.

The first boot is where the toolchain installs. Watch it, and check the box:

```sh
devbox ssh dev
journalctl -u google-startup-scripts -f     # or: tail -f /var/log/devbox-bootstrap.log
devbox doctor dev                            # 13 checks, one line each
```

`doctor` is the honest answer to "is it ready". It checks the data mount, that
both the Docker root and the containerd root are on the data disk, `br_netfilter`,
cgroup v2, mise shims on the path for a non-interactive shell, the harness, the
Chromium libraries, the Ghostty terminfo entry, and egress.

## 3. The daily loop

```sh
devbox ssh dev                      # a shell, through the tunnel
devbox push dev                     # this working tree onto the box, under the allowlist
devbox agent start dev --provider omp --task "fix the failing test in src/queue"
devbox agent list dev               # running, and anything unresolved
devbox agent attach dev <id>        # watch it, detach with Ctrl-b d
devbox pull dev                     # bring its changes back, with a rollback journal
devbox machine stop dev             # pause; the data disk keeps the caches
devbox machine start dev            # come back warm
```

`exec` is the scriptable form: `devbox exec dev -- systemctl status docker`. It
exits nonzero when the remote command does. Everything after `--` is the remote
command, so its own flags survive.

Editor and terminal setup, once per box:

```sh
devbox ssh-config dev   # writes the managed block; sessions refresh it anyway
devbox terminfo dev     # installs the Ghostty entry so TERM=xterm-ghostty works
devbox editors dev      # prints a zed ssh:// url and the VS Code host
devbox forward dev      # tunnels the configured port (default 9224) for web UIs
```

## 4. Pausing, and what it costs

| Command | Keeps | Costs while paused |
|---|---|---|
| `machine stop dev` | disks, caches, everything on the data disk | disks only |
| `machine suspend dev` | the running memory image too, for up to 60 days | disks |
| `machine start dev` / `resume dev` | resumes warm | compute again |

Two limits worth knowing:

- **The run cap.** `max_run_duration` defaults to `12h` with a stop action, so a
  box that starts stops itself twelve hours later. That protects you from a
  forgotten box; it will also cut off an agent session that runs past it. Raise
  `max_run_duration` in the configuration (or `machine schedule`) if you want a
  longer day, and re-create the box for the change to apply.
- **Neither stop nor suspend keeps local SSD**, which is why the caches live on
  the data disk. There is no local SSD in the default shape at all.

Cost, list prices, `us-central1`, from the figures recorded during the build:

| Item | Rate | Per month if left running |
|---|---|---|
| `n2-standard-16` compute, on demand | about $0.78/hour | about $565 |
| the same on spot | about $0.47/hour | about $340 |
| 1 TiB pd-ssd data disk | about $0.17/GiB-month | about $174 |
| 100 GiB pd-balanced boot disk | about $0.10/GiB-month | about $10 |
| NAT | about $0.045/GiB processed, plus a small hourly gateway charge | usage |

Paused, a box costs the two disks. Run the numbers for your own pattern before
you decide the machine type; `devbox machine show dev` prints what exists, and
`devbox machine list` prints every box you have.

## 5. Recovering

**A command refuses because of an unresolved record.** A cloud call left devbox
unable to tell what happened. Look at the cloud, then clear it:

```sh
devbox reconcile                      # every unresolved record, with the exact call it recorded
devbox reconcile <id> --note "the instance exists and is running"
```

**The box is wedged.** `devbox machine stop dev && devbox machine start dev`.
The data disk and its caches are untouched.

**You need yesterday's data disk.**

```sh
devbox machine snapshot dev          # quiesces Docker and containerd, then captures the disk
```

Restoring is deliberately manual, because it replaces a disk:

```sh
gcloud compute disks create devbox-data-restored --project=$PROJECT --zone=<zone> \
  --source-snapshot=<snapshot name from the command above>
```

Then stop the box, detach the current disk, attach the restored one, and start it.
`devbox machine show dev` prints the disk the box is using.

**You want a copy of a configured box.** `devbox machine fork dev dev2` takes a
machine image of the whole box and creates the new one from it, labels and all.
This is the reason the data disk is `pd-ssd`: a machine image cannot be captured
from an instance with a Hyperdisk attached.

**You want a prepared data disk as an image.** `devbox image bake dev work-image`
captures the data disk. It is not a bootable image; use `fork` for a clone.

**Doctor says a check fails.** It names the check and prints one line per check.
The common ones: the data mount missing means the disk was not attached at boot;
a Docker root on the boot disk means the containerd config was overwritten; mise
shims missing means a non-interactive shell cannot see the toolchain.

## 6. Destroying

```sh
devbox machine destroy dev              # prints the full inventory, then refuses
devbox machine destroy dev --confirm=dev
```

It deletes the instance and the data disk only when the disk carries the box's
labels, and machine images only with `--with-images`. It never deletes a
snapshot. Ownership is proven from labels, so a machine you made by hand in the
same project is never touched; if you want devbox to adopt one, label it
`devbox-name=<name>,devbox-managed=true`.

## 7. Cheat sheet

| | |
|---|---|
| Create, inspect | `machine new`, `machine list`, `machine show` |
| Pause, resume | `machine stop`, `machine start`, `machine suspend`, `machine resume` |
| Copy, clone | `machine snapshot`, `machine fork`, `image bake` |
| Remove | `machine destroy --confirm=<name>` |
| Network | `network ensure`, `network show` |
| Reach it | `ssh`, `exec`, `cp`, `forward`, `terminfo`, `editors`, `doctor`, `ssh-config` |
| Move trees | `push`, `pull`, `trees` |
| Agents | `agent start`, `agent list`, `agent logs`, `agent attach`, `agent stop` |
| Toolchain | `tools list`, `tools add`, `tools remove`, `tools update`, `tools outdated`, `tools apply`, `tools edit` |
| Startup script | `bootstrap show`, `bootstrap upload`, `toolchain` |
| Recover | `reconcile`, `config`, `help`, `version` |

Every command answers `devbox help <command>`, and every command honors
`devbox --dry-run`. A rehearsal prints the calls it would make and stops: it
reaches no box, writes nothing, and claims nothing it did not do.

## 8. What has not been proven yet

No test in this repository has ever called the cloud. The flag spellings for
instance schedules, NAT, and machine image properties come from the documented
API surface. Treat your first `machine new` and your first `machine schedule` as
the test for those, and if a flag is rejected, fix the flag rather than the doc.
