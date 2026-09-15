package access

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"devbox/internal/box"
	"devbox/internal/cli"
	"devbox/internal/config"
)

// containerdConfig is where the system containerd records its data root. It is
// the only place that root can be read from, and the file the bootstrap points at
// the data disk so the content store survives a stop.
const containerdConfig = "/etc/containerd/config.toml"

// The two answers a check can report.
const (
	statusPass = "pass"
	statusFail = "fail"
)

// reportTab separates the fields of a reported check.
const reportTab = "\t"

// doctorCheck is one remote probe: the report lines it emits and the shell that
// emits them.
type doctorCheck struct {
	names []string
	note  string
	body  string
}

// doctorChecks are the probes in report order. Each body reports every one of its
// names exactly once, and this list is the only source of the report's shape, so
// a run that stops early cannot look like a run that passed.
var doctorChecks = []doctorCheck{
	{
		names: []string{"cgroup-v2"},
		note:  "cgroup v2: the unified hierarchy Docker and systemd both need",
		body: `kind=$(stat -fc %T /sys/fs/cgroup/ 2>/dev/null || true)
if [ "$kind" = cgroup2fs ]; then
	report cgroup-v2 pass "$kind"
else
	report cgroup-v2 fail "stat -fc %T /sys/fs/cgroup/ printed '${kind}'"
fi
`,
	},
	{
		names: []string{"br_netfilter"},
		note:  "br_netfilter: required before the Docker bridge can route",
		body: `if grep -q '^br_netfilter' /proc/modules 2>/dev/null || [ -e /proc/sys/net/bridge/bridge-nf-call-iptables ]; then
	report br_netfilter pass loaded
else
	report br_netfilter fail "not loaded; the Docker bridge needs it"
fi
`,
	},
	{
		names: []string{"data-mount"},
		note:  "the data disk is mounted where the caches expect it",
		body: `mount=$(awk -v point="$data_mount" '$2 == point { print $1 " as " $3; exit }' /proc/mounts 2>/dev/null || true)
if [ -n "$mount" ]; then
	report data-mount pass "$data_mount ($mount)"
else
	report data-mount fail "$data_mount is not a mount point"
fi
`,
	},
	{
		names: []string{"docker-root"},
		note:  "the Docker data root is on the data disk, so images survive a stop",
		body: `if root=$(docker info --format '{{.DockerRootDir}}' 2>&1); then
	case "$root" in
		"$docker_root" | "$data_mount"/*) report docker-root pass "$root" ;;
		*) report docker-root fail "docker reports $root, which is not under $data_mount" ;;
	esac
else
	report docker-root fail "$(squash "$root")"
fi
`,
	},
	{
		names: []string{"containerd-root"},
		note:  "the containerd content store is on the data disk as well",
		body: `root=
if [ -f "$containerd_config" ]; then
	root=$(awk -F= '/^[[:space:]]*root[[:space:]]*=/ { value=$2; gsub(/[[:space:]"]/, "", value); print value; exit }' "$containerd_config")
fi
case "$root" in
	"$containerd_root" | "$data_mount"/*) report containerd-root pass "$root" ;;
	"") report containerd-root fail "$containerd_config declares no root, so the content store is on the boot disk" ;;
	*) report containerd-root fail "the containerd root is $root, which is not under $data_mount" ;;
esac
`,
	},
	{
		names: []string{"mise-shims"},
		note:  "a non-interactive shell sees the mise shims, which is how devbox runs commands",
		body: `shims="$HOME/.local/share/mise/shims"
on_path=no
case ":${PATH}:" in
	*":$shims:"*) on_path=yes ;;
esac
if [ "$on_path" = yes ] && [ -d "$shims" ]; then
	report mise-shims pass "$shims is on PATH"
else
	report mise-shims fail "a non-interactive shell cannot see $shims"
fi
`,
	},
	{
		names: []string{"docker-without-sudo"},
		note:  "Docker answers the login user, so no step needs sudo",
		body: `if out=$(docker info 2>&1); then
	report docker-without-sudo pass "docker info succeeded as $(id -un)"
else
	report docker-without-sudo fail "$(squash "$out")"
fi
`,
	},
	{
		names: []string{"gh", "gcloud"},
		note:  "the command line tools the box's own workflow uses",
		body: `for binary in gh gcloud; do
	if found=$(command -v "$binary" 2>/dev/null); then
		report "$binary" pass "$found"
	else
		report "$binary" fail "$binary is not on PATH"
	fi
done
`,
	},
	{
		names: []string{"agent-harness"},
		note:  "the agent harness, at the version the configuration pins",
		body: `if out=$(omp --version 2>&1); then
	if [ -z "$expected_harness" ] || printf '%s' "$out" | grep -qF "$expected_harness"; then
		report agent-harness pass "$(squash "$out")"
	else
		report agent-harness fail "installed $(squash "$out"), expected $expected_harness"
	fi
else
	report agent-harness fail "$(squash "$out")"
fi
`,
	},
	{
		names: []string{"chromium-libs"},
		note:  "the shared libraries a headless browser needs",
		body: `found=0
missing=
for library in libnss3.so libnspr4.so libatk-1.0.so.0 libatk-bridge-2.0.so.0 libcups.so.2 libdrm.so.2 libxkbcommon.so.0 libXcomposite.so.1 libXdamage.so.1 libXfixes.so.3 libXrandr.so.2 libgbm.so.1 libpango-1.0.so.0 libcairo.so.2 libasound.so.2 libatspi.so.0; do
	if ldconfig -p 2>/dev/null | grep -q " $library "; then
		found=$((found + 1))
	else
		missing="$missing $library"
	fi
done
if [ -z "$missing" ]; then
	report chromium-libs pass "$found shared libraries present"
else
	report chromium-libs fail "missing:$missing"
fi
`,
	},
	{
		names: []string{"terminfo-xterm-ghostty"},
		note:  "the terminal entry a Ghostty session needs on the box",
		body: `if infocmp -x xterm-ghostty >/dev/null 2>&1; then
	report terminfo-xterm-ghostty pass "installed for the box's login user"
else
	report terminfo-xterm-ghostty fail "missing; run devbox terminfo @box_name@ to install it"
fi
`,
	},
	{
		names: []string{"egress"},
		note:  "outbound reachability, which a box with no external address gets from Cloud NAT",
		body: `if code=$(curl -sS -m 10 -o /dev/null -w '%{http_code}' https://storage.googleapis.com 2>&1); then
	report egress pass "https://storage.googleapis.com answered HTTP $code"
else
	report egress fail "$(squash "$code")"
fi
`,
	},
}

// doctorPrelude is the part of the script every check shares: the configured
// values are baked in as shell words, and one helper writes a report line.
const doctorPrelude = `# devbox doctor. Each check prints one line: name, a tab, pass or fail, a tab,
# and a detail. The script reports every check and always exits zero, so one
# failure cannot hide the checks after it; devbox reads the report and decides
# the exit status.
set -u

data_mount=@data_mount@
docker_root=@docker_root@
containerd_root=@containerd_root@
containerd_config=@containerd_config@
expected_harness=@harness_version@

report() {
	printf '%s\t%s\t%s\n' "$1" "$2" "$3"
}

# squash folds tool output onto one line, so a detail cannot break the report.
squash() {
	printf '%s' "$1" | tr '\n\t' '  ' | cut -c1-200
}
`

// checkNames is every report line a run must contain.
func checkNames() []string {
	var names []string
	for _, check := range doctorChecks {
		names = append(names, check.names...)
	}
	return names
}

// doctorScript renders the remote checks for one box. The configured values are
// single-quoted shell words, so a path from the configuration can never be read
// as shell syntax on the box.
func doctorScript(cfg config.Config, name box.Name) string {
	values := [][2]string{
		{"@data_mount@", shQuote(cfg.DataMount)},
		{"@docker_root@", shQuote(box.DockerRoot)},
		{"@containerd_root@", shQuote(box.ContainerdRoot)},
		{"@containerd_config@", shQuote(containerdConfig)},
		{"@harness_version@", shQuote(cfg.OmpVersion)},
		{"@box_name@", name.String()},
	}
	var script strings.Builder
	script.WriteString(fill(doctorPrelude, values))
	for _, check := range doctorChecks {
		script.WriteString("\n# " + check.note + "\n")
		script.WriteString(fill(check.body, values))
	}
	script.WriteString("\nexit 0\n")
	return script.String()
}

// fill substitutes the configured values into a script fragment.
func fill(fragment string, values [][2]string) string {
	for _, pair := range values {
		fragment = strings.ReplaceAll(fragment, pair[0], pair[1])
	}
	return fragment
}

// shQuote renders a value as one single-quoted shell word.
func shQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// doctorResult is one reported check.
type doctorResult struct {
	name   string
	status string
	detail string
}

// parseDoctor reads the reported checks out of a run's output. Anything else is
// ignored, because a warning on the way in is not a check; the checks the box
// never answered are found by comparing the report against the expected names.
func parseDoctor(out string) []doctorResult {
	known := checkNames()
	var results []doctorResult
	for _, line := range strings.Split(out, "\n") {
		fields := strings.SplitN(strings.TrimSuffix(line, "\r"), reportTab, 3)
		if len(fields) != 3 {
			continue
		}
		if fields[1] != statusPass && fields[1] != statusFail {
			continue
		}
		if !slices.Contains(known, fields[0]) {
			continue
		}
		results = append(results, doctorResult{name: fields[0], status: fields[1], detail: fields[2]})
	}
	return results
}

// reportDoctor prints one line per check and returns the names that did not pass,
// including the checks the box never answered.
func reportDoctor(deps cli.Deps, out string) []string {
	reported := make(map[string]doctorResult, len(doctorChecks))
	for _, result := range parseDoctor(out) {
		reported[result.name] = result
	}
	var failed []string
	for _, name := range checkNames() {
		result, ok := reported[name]
		if !ok {
			deps.Printf("%-4s %-24s %s", "FAIL", name, "not reported")
			failed = append(failed, name)
			continue
		}
		if result.status != statusPass {
			deps.Printf("%-4s %-24s %s", "FAIL", name, result.detail)
			failed = append(failed, name)
			continue
		}
		deps.Printf("%-4s %-24s %s", "PASS", name, result.detail)
	}
	return failed
}

// opDoctor runs the checks through the session and fails when any check does not
// pass, naming the failing checks instead of printing their output again.
func opDoctor(o ops) cli.Command {
	const usage = "devbox doctor <name>"
	return cli.Command{
		Name:    "doctor",
		Summary: "Check that a box is ready to run work",
		Usage:   usage,
		Run: func(ctx context.Context, deps cli.Deps, args []string) error {
			positional, err := cli.Parse(deps.FlagSet("doctor"), args)
			if err != nil {
				return parseError(err, usage)
			}
			name, err := onlyBox(positional, usage)
			if err != nil {
				return err
			}
			if dryRun(deps) {
				deps.Printf("would run %d checks on %s:", len(checkNames()), name)
				deps.Printf("%s", strings.TrimRight(doctorScript(deps.Config, name), "\n"))
				return nil
			}
			session, err := o.open(ctx, deps, name)
			if err != nil {
				return err
			}
			out, runErr := session.Run(ctx, doctorScript(deps.Config, name))
			failed := reportDoctor(deps, out)
			if runErr != nil {
				return fmt.Errorf("run the checks on %s: %w", name, runErr)
			}
			if len(failed) > 0 {
				return fmt.Errorf("%d of %d checks failed on %s: %s",
					len(failed), len(checkNames()), name, strings.Join(failed, ", "))
			}
			deps.Printf("all %d checks passed on %s", len(checkNames()), name)
			return nil
		},
	}
}
