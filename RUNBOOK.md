# Runbook

Setting up and running your own devbox, from a bare project to a working machine.

Everything here is `devbox` doing the work. Where a step needs a permission only
you can grant, or a decision only you can make, it says so. Run each command from
the directory you are working on unless the step says otherwise. `--dry-run` goes
anywhere on the line and makes a command print the cloud calls it would run.

## 0. What you are building

One Compute Engine instance, Debian 13, amd64, with a data disk that survives
pauses and carries the Docker root, the containerd root, the Dagger cache, and
the pnpm store. It has no external address: you reach it over an IAP tunnel, and
it reaches the internet through the Cloud Router and NAT that `devbox network
ensure` creates. Creating the box runs a startup script that installs the
toolchain, so the first boot takes a few minutes and every later boot is fast.

The login user on the box is your OS Login profile name (`remote_user`). It has
passwordless sudo, it runs `docker` without it, and its home directory is where
everything devbox puts on the box lives.

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

The bucket grant is the one you do not have to make by hand: `devbox bootstrap
upload --bucket ...` makes it. The IAP and OS Login bindings are the ones an
operator most often finds missing, and section 9 says what each failure looks
like.

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
and says so; `config`, `tools`, `bootstrap show`, `toolchain`, `help`, `version`,
and `reconcile` keep working.

### The startup script and the network

```sh
gcloud storage buckets create gs://$PROJECT-devbox      # once
devbox bootstrap upload --bucket gs://$PROJECT-devbox  # publishes the script, records the url
devbox network ensure                                  # firewall for IAP SSH, router, NAT
```

`bootstrap upload` prints the script it wrote and the url it recorded, and it
grants the box's service account read on that bucket, which is what lets an
instance fetch the script at boot. Re-run it after any change to `[tools]` or to
`bootstrap_url`, and a box you create later runs the new script. An existing box
does not: it carries the stamp `/var/lib/devbox/bootstrap.ok` and skips the whole
script on later boots. Its pins converge with `devbox tools apply <box>`, and
anything else the script installs needs the forced rebuild in section 9.

`network ensure` creates only what is missing and says what it found: a firewall
rule allowing port 22 from `35.235.240.0/20` to machines tagged `devbox`, a
router, and a NAT that gives an address-less box its egress. `devbox network show`
prints all of it, plus the subnet's Private Google Access state.

Before spending anything, read the exact call:

```sh
devbox --dry-run machine new dev
```

## 2. The first box

```sh
devbox machine new dev
```

That makes one instance and its data disk, both labeled for the box, and refuses
if a box of that name already exists or if no `bootstrap_url` is configured. It
then prints the two commands that matter: `devbox network ensure` if you have not
run it, and `devbox ssh dev`.

The first boot is where the toolchain installs. The startup script runs at boot,
not at create, so a box reports RUNNING while Debian and then the toolchain are
still coming up. Section 3 covers what a session tells you while that is true.

## 3. The first connection

```sh
devbox ssh dev                       # a shell, through the IAP tunnel
devbox ssh dev -- tail -f /var/log/devbox-bootstrap.log
```

The first command reaches a box that is still booting by waiting: a session
retries a refused tunnel for up to ninety seconds, because a box reports RUNNING
before its ssh daemon listens. A refusal that is not transient, such as a key the
box will not accept, is reported immediately.

Every connection resolves through one managed block in `~/.ssh/config`, written
between `# >>> devbox managed block >>>` and `# <<< devbox managed block <<<`.
For a box with no external address the entry carries a `ProxyCommand` that runs
`gcloud compute start-iap-tunnel`, so ssh, scp, rsync, Zed, and VS Code all reach
the box through the tunnel without knowing about it. `devbox ssh-config dev`
writes the block on demand; every session refreshes it anyway. Text outside the
markers is yours and is never rewritten.

Then check the box and set up the terminal:

```sh
devbox doctor dev
devbox terminfo dev
devbox editors dev
devbox forward dev
```

`doctor` is the honest answer to "is it ready". It prints one line per check,
`PASS` or `FAIL` and then the check name and a detail, thirteen of them, and
exits nonzero when any fails: `cgroup-v2`, `br_netfilter`, `data-mount`,
`docker-root`, `containerd-root`, `mise-shims`, `docker-without-sudo`, `gh`,
`gcloud`, `agent-harness`, `chromium-libs`, `terminfo-xterm-ghostty`, and
`egress`. It ends with `all 13 checks passed on dev`, or with the names of the
checks that did not pass, and a FAIL line's detail is usually the whole
diagnosis.

`terminfo` installs the `xterm-ghostty` entry on the box once, reading the local
entry with `infocmp` and feeding it to the box's `tic`. It prints the `SetEnv
TERM=xterm-256color` line you can put in the Host entry if a program still cannot
read the entry.

`editors` prints what to paste into an editor. The box needs no editor-side
setup, because both editors resolve the same alias. For the tree a push wrote,
the directory to open is `~/devbox/trees/<tree>` under the login user's home:

```sh
devbox editors dev /home/<remote_user>/devbox/trees/v1
zed:        zed ssh://devbox-dev/home/<remote_user>/devbox/trees/v1
zed dialog: devbox-dev      (Remote Projects, Connect New Server: the host alone, never the url)
vscode:     devbox-dev      (Remote-SSH host)
```

Three things about that path. It is remote and absolute: a tilde is not expanded,
so `devbox editors dev '~/devbox/trees/v1'` prints a path ending in
`/~/devbox/trees/v1`, which is not a directory. A path with no leading slash is
read from the login user's home, so `devbox editors dev devbox/trees/v1` means
the same directory. And it is the tree root the push wrote, not the directory
`push` was run in: the box keeps one directory per tree name under
`~/devbox/trees`, whatever the local checkout is called.

`forward` opens a tunnel from a local port to the box, default 9224, for anything
that speaks to localhost: a browser, a notebook server, a web UI. It prints the
`gcloud` command it runs, so you can open the tunnel again without devbox.

## 4. The daily loop

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

## 5. Trees: push and pull

A push hands one directory to the box; a pull brings the box's copy of it back.
Both work in a named tree on the box, `~/devbox/trees/<tree>` under the login
user's home, one directory per tree name.

### What a push sends

```sh
devbox push dev                     # the current directory, as tree dev
devbox push dev ~/code/api          # another directory, as tree api
devbox push dev ~/code/api --tree app
devbox push dev --everything        # the directory exactly as it is on disk
```

The tree name defaults to the base name of the local path and must be 1 to 64
characters of letters, digits, dots, hyphens, and underscores, and may not start
with a dot.

A push is a working copy, not a mirror of your disk. It builds the manifest of
the local tree under the allowlist, stages exactly the declared entries into a
staging directory, and uploads that staging directory. Repository metadata
travels, `.git` included, and so does any file whose name starts with `.env`.
Dependency and build output stays home, because the box can install it again and
it is most of the bytes: `node_modules`, `dist`, `build`, `.venv`, `venv`,
`__pycache__`, `.terraform`, and the other toolchain caches. `--everything` sends
those too, which is what you want for a directory you intend to reproduce
exactly.

The push prints what it is about to do and what it did:

```
replacing ~/devbox/trees/v1 on dev
pushed tree v1 to dev: 12443 files, 61123965 bytes
```

The count and the byte total describe what landed, not what was read.

### What a push does to the box

A push **replaces** the destination tree. It removes `~/devbox/trees/<tree>` on
the box and writes the staged tree in its place, then uploads the manifest to
`~/devbox/trees/.manifests/<tree>.json`. It never merges, so a file that exists
on the box and not in the local tree is gone after the push, and an upload that
was interrupted leaves a partial tree rather than a mixed one.

That is also the recovery: **re-running the same push is how a partial upload is
fixed.** The destination is replaced again from the local tree, and the manifest
the push stages describes what actually landed, so the box and the recorded
digest agree afterwards. There is nothing to clean up first.

A push that fails while staging never reaches the box. The staging directory is
built first, and a file the push cannot read (a socket, a path replaced while it
was being read, a name the allowlist refuses) fails the command with the path
named, for example `stage the tree: materialize src/queue.sock: ...`, and the
tree on the box is untouched. Fix the file and run the same command.

### What is on the box

```sh
devbox trees dev
```

prints every tree on the box with the digest and local path devbox recorded for
it, so you can see at a glance which trees the box holds and what the next pull
of each one compares against:

```
v1   <sha256 of the pushed tree>   /Users/you/code/v1
api  not recorded  -
```

`not recorded` means the tree is on the box but devbox has no handoff for it on
this machine, which is what a tree pushed from another laptop looks like. `no
trees on dev` means the box has none.

### What a pull does

```sh
devbox pull dev                     # apply the box's tree dev over the current directory
devbox pull dev ~/code/api --tree api
devbox pull dev --force             # apply even though the local tree has changed
```

A pull downloads `~/devbox/trees/<tree>`, builds the manifest of what arrived,
and applies it over the local tree. It refuses while the local tree is not the
tree devbox last handed over for this box and name, which is what keeps a pull
from quietly overwriting local work that has not been sent anywhere. The three
refusals name the next command:

- no handoff is recorded: `push it first, or pass --force`
- the handoff was recorded for a different local path: `push from <that path> first`
- the local tree changed since the handoff: `push the local changes first, or pass --force`

`--force` applies anyway and prints what it is writing over. The apply runs under
a rollback journal under `~/.devbox/journal/<box>/<tree>/<timestamp>/`, which
holds the bytes of every local path the apply can destroy before the first byte
is written. If the apply fails, the local tree is put back from the journal and
the command says so; if it succeeds, the journal is discarded and the new digest
is recorded, so the next pull compares against what you now have.

The pull rebuilds the projection the push recorded, so a tree sent with
`--everything` comes back whole and a tree sent as a working copy comes back
without the dependency directories. It reports the same shape of line as a push:
`pulled tree v1 from dev: 12443 files, 61123965 bytes`.

## 6. Pausing, and what it costs

| Command | Keeps | Costs while paused |
|---|---|---|
| `machine stop dev` | disks, caches, everything on the data disk | disks only |
| `machine suspend dev` | the running memory image too, for up to 60 days | disks |
| `machine start dev` / `resume dev` | resumes warm | compute again |

Two limits worth knowing:

- **The run cap.** `max_run_duration` defaults to `12h` with a stop action, so a
  box that starts stops itself twelve hours later. That protects you from a
  forgotten box; it will also cut off an agent session that runs past it. Raise
  `max_run_duration` in the configuration and re-create the box for the change to
  apply, or attach a start and stop window with
  `devbox machine schedule dev --start=07:00 --stop=21:00 --timezone=UTC` (see
  section 11: no one has run that against a real project yet).
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

## 7. Recovering

**A command refuses because of an unresolved record.** A cloud call left devbox
unable to tell what happened, so every mutation for that box is blocked until you
look at the cloud and say what you found. The refusal names the record and the
command to clear it:

```sh
devbox reconcile                      # every unresolved record, with the exact call it recorded
devbox reconcile <id> --note "the instance exists and is running"
devbox reconcile --box dev            # only the records blocking one box
```

Clearing a record needs `--note`, so an unblock always says who checked what.

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
captures the data disk, with Docker and containerd stopped for the capture. It is
not a bootable image; use `fork` for a clone.

**Doctor says a check fails.** It names the check and prints one line per check.
The common ones: the data mount missing means the disk was not attached at boot;
a Docker root on the boot disk means the containerd config was overwritten; mise
shims missing means a non-interactive shell cannot see the toolchain.

## 8. Destroying

```sh
devbox machine destroy dev              # prints the full inventory, then refuses
devbox machine destroy dev --confirm=dev
```

The inventory is printed before the confirmation is read, so you can see exactly
what is at stake: the instance and its status, its data disk or the fact that it
will be kept, every machine image of the box, and the line that snapshots are
never deleted by destroy. Nothing is deleted until `--confirm` repeats the box
name.

It deletes the instance and the data disk only when the disk carries the box's
labels, and machine images only with `--with-images`. It never deletes a
snapshot. Ownership is proven from labels, so a machine you made by hand in the
same project is never touched; if you want devbox to adopt one, label it
`devbox-name=<name>,devbox-managed=true`.

## 9. Troubleshooting

Each entry is what you saw, why, and what to run.

### The IAP tunnel warns about NumPy

**Symptom.** Every connection prints a warning from `gcloud` that NumPy is not
installed in the interpreter gcloud runs, and the tunnel is slow.

**Cause.** `gcloud compute start-iap-tunnel` uses NumPy for the code path that
moves tunnel bytes quickly. It is not installed in gcloud's own Python
environment, and installing it into the interpreter a box's toolchain uses does
nothing for gcloud.

**Action.**

```sh
"$(gcloud info --format='value(basic.python_location)')" -m pip install numpy
```

That asks gcloud which interpreter is its own and installs into it. The warning
disappears and the tunnel speeds up. It costs nothing else and needs no restart
of the box.

### The box is missing the `xterm-ghostty` entry

**Symptom.** A session from Ghostty opens, but inside it tmux refuses with
`missing or unsuitable terminal: xterm-ghostty`, vim draws the wrong colours or
loses its function keys, and modified keys such as Shift+Enter and Ctrl+Enter
never reach the remote program. `devbox doctor dev` reports a FAIL for
`terminfo-xterm-ghostty`.

**Cause.** `TERM=xterm-ghostty` travels to the box with the connection, and the
box's terminfo database has no such entry, so every full-screen program on the
box fails capability lookups. The local entry is not the box's.

**Action.**

```sh
devbox terminfo dev
```

It reads the local entry with `infocmp` and installs it on the box with `tic`. It
is once per box, and `devbox doctor dev` should then report
`terminfo-xterm-ghostty PASS`. If a program still cannot read it, the command
prints the fallback to put in the box's Host entry:

```
SetEnv TERM=xterm-256color
```

### Ghostty announces `Setting up xterm-ghostty terminfo` when you connect

**Symptom.** Connecting to the box from a Ghostty window prints `Setting up
xterm-ghostty terminfo`.

**Cause.** That is Ghostty's own ssh integration installing its terminal entry on
the box, once per destination, recorded in its own cache under the resolved
`user@host`. It is not devbox's doing: `devbox ssh` execs the ssh binary
directly, so Ghostty's wrapper never runs for it.

**Action.** None. If the line appears on every interactive `ssh <name>` from a
Ghostty window, its cache has no record for that destination:

```sh
ghostty +ssh-cache
```

`devbox terminfo <name>` installs the entry regardless, which is what the box
needs; the announce line is Ghostty's bookkeeping beside it.

### The box is still bootstrapping

**Symptom.** The shell opens, but half the toolchain is missing and `devbox
doctor dev` fails several checks. A session says so directly:

```
devbox-dev is still bootstrapping; follow it with: devbox ssh dev -- tail -f /var/log/devbox-bootstrap.log
```

**Cause.** The startup script runs at boot. A box reports RUNNING, and accepts
ssh, well before the toolchain install is finished.

**Action.** Watch it, then check again:

```sh
devbox ssh dev -- tail -f /var/log/devbox-bootstrap.log
devbox doctor dev
```

Three markers say where it got to: `/var/lib/devbox/bootstrap.ok` means it
finished and is a timestamp, `/var/lib/devbox/bootstrap.failed` means a phase
failed and the log's last line names the command, and neither means it is still
running. A session reports a failure with the bootstrap's own last line. A box
that already has its stamp skips the whole script on the next boot; to rebuild
one on purpose:

```sh
devbox bootstrap show | devbox ssh dev -- sudo DEVBOX_BOOTSTRAP_FORCE=1 bash -s
```

### A record is unresolved and the box is blocked

**Symptom.** Every command that would change the box refuses:

```
unresolved records block this box; reconcile them first:
  <id> create unknown: gcloud compute instances create dev ...
    clear it with: devbox reconcile <id> --note "what the cloud holds"
```

**Cause.** A cloud call was interrupted or answered in a way devbox could not
interpret, so devbox does not know whether the change happened. It refuses to
make a second change on top of an unknown first one.

**Action.** Look at the cloud, then clear it:

```sh
devbox reconcile
devbox reconcile <id> --note "the instance exists and is running"
```

The listing prints the exact recorded call, so you know what to check. An agent
session that was interrupted leaves a record too, and the refusal names it; the
same command clears it.

### The tunnel cannot be opened

**Symptom.** `devbox ssh dev` fails with a connection refused or `failed to
connect to backend` that does not clear, or gcloud reports the caller does not
have permission to use the tunnel.

**Cause.** Three different things, in order of likelihood: a box that is still
booting (section above), a missing firewall rule, or a missing IAM role.

**Action.**

```sh
devbox network show                    # the firewall rule, router, NAT, and subnet state
devbox network ensure                  # create only what is missing
gcloud projects get-iam-policy $PROJECT --format=json | grep iap.tunnelResourceAccessor
```

A refused connection that clears within ninety seconds is a box still booting.
A missing rule for port 22 from `35.235.240.0/20` to machines tagged `devbox` is
what `network ensure` creates. A caller without
`roles/iap.tunnelResourceAccessor`, or a user without an OS Login role, is not
fixed by anything devbox can run: that is yours to grant.

## 10. Cheat sheet

| | |
|---|---|
| Create, inspect | `machine new`, `machine list`, `machine show` |
| Pause, resume | `machine stop`, `machine start`, `machine suspend`, `machine resume` |
| Copy, clone | `machine snapshot`, `machine fork`, `image bake` |
| Remove | `machine destroy --confirm=<name>` |
| Schedule | `machine schedule --start=HH:MM --stop=HH:MM [--timezone=ZONE]`, `--remove` |
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

## 11. What has not been proven yet

No test in this repository has ever called the cloud; every command is asserted
through a recording executor and a recording session. Against a real project, a
box has been created and has bootstrapped itself, `devbox ssh` has reached it
through the IAP tunnel, `devbox push` has replaced a tree on it and reported
`pushed tree v1 to dev: 12443 files, 61123965 bytes`, `devbox terminfo` has
installed the Ghostty entry on it, and Zed has connected through the SSH entry
the tool writes.

Everything below is written from the documented API surface and has not been
exercised against a real project. Treat the first run of each as its test, and
fix the flag rather than this document when they disagree.

| Area | What is not proven |
|---|---|
| Instance schedules | `machine schedule`, `machine new --schedule`: no start and stop window has ever been created |
| NAT | the exact flag spellings behind the NAT that `network ensure` creates |
| Machine images | the flag spellings for the machine image properties that `machine fork`, `machine snapshot`, and `image bake` use |
| `machine snapshot` | nothing has captured a data disk with the services quiesced |
| `machine fork` | nothing has created a box from a machine image |
| `image bake` | nothing has captured a data disk as an image |
| `pull` | no tree has been pulled back from a box end to end, so the rollback journal has never run outside a test |
| Agent sessions | no `agent start` has run on a box |
