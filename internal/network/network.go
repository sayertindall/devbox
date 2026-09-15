package network

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"devbox/internal/cli"
	"devbox/internal/config"
	"devbox/internal/gcloud"
)

// The pieces devbox owns in the default network. A box is created with no
// external address, so these three objects are the whole path in and out: the
// firewall opens port 22 to the IAP forwarding range, and the router with NAT
// gives a box that has no public address its egress.
const (
	vpcNetwork = "default"
	firewallID = "devbox-ssh"
	routerID   = "devbox-router"
	natID      = "devbox-nat"
	// iapRange is the source range Google's Identity-Aware Proxy tunnels from.
	iapRange = "35.235.240.0/20"
	sshPort  = "tcp:22"
)

// firewallFacts is the part of a firewall rule show reports.
type firewallFacts struct {
	Name         string   `json:"name"`
	Disabled     bool     `json:"disabled"`
	SourceRanges []string `json:"sourceRanges"`
	TargetTags   []string `json:"targetTags"`
	Allowed      []struct {
		IPProtocol string   `json:"IPProtocol"`
		Ports      []string `json:"ports"`
	} `json:"allowed"`
}

// natFacts is the part of a NAT configuration show reports.
type natFacts struct {
	Name                  string `json:"name"`
	NatIPAllocateOption   string `json:"natIpAllocateOption"`
	SubnetworkIPRangesToN string `json:"sourceSubnetworkIpRangesToNat"`
}

// subnetFacts is the part of a subnet show reports. Private Google Access
// matters because the box reaches Google APIs either over the NAT or over the
// private path, and an operator reading an egress failure needs to know which.
type subnetFacts struct {
	Name                  string `json:"name"`
	PrivateIPGoogleAccess bool   `json:"privateIpGoogleAccess"`
}

// region is the region the network objects live in, derived from the zone so a
// --zone override cannot leave the router in the wrong region.
func region(cfg config.Config) string {
	if derived := config.RegionFromZone(cfg.Zone); derived != cfg.Zone {
		return derived
	}
	return cfg.Region
}

// tags are the network tags a box carries. They come from the configuration so
// the firewall rule below and the instance created by the machine slice always
// agree about which machines may be reached.
func tags(cfg config.Config) []string {
	if len(cfg.Tags) == 0 {
		return []string{"devbox"}
	}
	return cfg.Tags
}

func firewallDescribeArgs(cfg config.Config) []string {
	return []string{"compute", "firewall-rules", "describe", firewallID, cfg.ProjectFlag()}
}

// firewallCreateArgs allows SSH from the IAP range only. A box has no external
// address, so an IAP tunnel is the only way in and a wider source range would
// open nothing an attacker could use in a useful way.
func firewallCreateArgs(cfg config.Config) []string {
	return []string{
		"compute", "firewall-rules", "create", firewallID,
		cfg.ProjectFlag(),
		"--network=" + vpcNetwork,
		"--allow=" + sshPort,
		"--source-ranges=" + iapRange,
		"--target-tags=" + strings.Join(tags(cfg), ","),
		"--description=SSH through IAP for devbox boxes",
	}
}

func routerArgs(verb string, cfg config.Config) []string {
	return []string{"compute", "routers", verb, routerID, cfg.ProjectFlag(), "--region=" + region(cfg)}
}

func routerCreateArgs(cfg config.Config) []string {
	return append(routerArgs("create", cfg), "--network="+vpcNetwork, "--description=egress for devbox boxes without an external address")
}

func natDescribeArgs(cfg config.Config) []string {
	return []string{"compute", "routers", "nats", "describe", natID, cfg.ProjectFlag(), "--region=" + region(cfg), "--router=" + routerID}
}

// natCreateArgs gives every subnet range in the region automatic external IPs for
// translation, which is what makes an address-less box able to reach the network.
func natCreateArgs(cfg config.Config) []string {
	return []string{
		"compute", "routers", "nats", "create", natID,
		cfg.ProjectFlag(),
		"--region=" + region(cfg),
		"--router=" + routerID,
		"--auto-allocate-nat-external-ips",
		"--nat-all-subnet-ip-ranges",
	}
}

func subnetDescribeArgs(cfg config.Config) []string {
	return []string{"compute", "networks", "subnets", "describe", vpcNetwork, cfg.ProjectFlag(), "--region=" + region(cfg)}
}

// withJSON asks gcloud for the JSON a read decodes. A describe that forgets the
// flag answers with a human table, which fails at the decode far from the
// mistake, so the request for JSON sits next to the decode that needs it.
func withJSON(argv []string) []string {
	return append(append([]string{}, argv...), "--format=json")
}

// exists reports whether a describe found its resource. A missing resource is a
// fact rather than an error, which is what makes ensure idempotent.
func exists(ctx context.Context, deps cli.Deps, argv []string) (bool, error) {
	if deps.DryRun {
		// A rehearsal gets an empty answer from every read, and reporting "already
		// exists" from that would hide the very calls the operator asked to see.
		return false, nil
	}
	if _, err := deps.Cloud.Run(ctx, argv...); err != nil {
		if gcloud.Missing(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ensure describes each object first and creates only what is missing, so the
// command can be run against a fresh project and against a configured one with
// the same result, and always says what it found or did.
func ensure(ctx context.Context, deps cli.Deps) error {
	cfg := deps.Config
	steps := []struct {
		what     string
		describe []string
		create   []string
	}{
		{"firewall rule " + firewallID, firewallDescribeArgs(cfg), firewallCreateArgs(cfg)},
		{"router " + routerID, routerArgs("describe", cfg), routerCreateArgs(cfg)},
		{"nat " + natID, natDescribeArgs(cfg), natCreateArgs(cfg)},
	}
	for _, step := range steps {
		present, err := exists(ctx, deps, step.describe)
		if err != nil {
			return err
		}
		if present {
			deps.Printf("%s already exists", step.what)
			continue
		}
		if _, err := deps.Cloud.Run(ctx, step.create...); err != nil {
			return err
		}
		deps.Printf("%s", deps.Outcome("created "+step.what, "would create "+step.what))
	}
	return nil
}

// show reports the network state a box depends on. It never creates anything: an
// operator diagnosing a box that cannot be reached needs to see what is missing
// rather than have it silently provisioned.
func show(ctx context.Context, deps cli.Deps) error {
	cfg := deps.Config

	out, err := deps.Cloud.Run(ctx, withJSON(firewallDescribeArgs(cfg))...)
	if err != nil {
		if !gcloud.Missing(err) {
			return err
		}
		deps.Printf("firewall %s missing", firewallID)
	} else {
		var rule firewallFacts
		if err := json.Unmarshal([]byte(out), &rule); err != nil {
			return fmt.Errorf("decode firewall rule %s: %w", firewallID, err)
		}
		state := "enabled"
		if rule.Disabled {
			state = "disabled"
		}
		deps.Printf("firewall %s %s: allow %s from %s to tag %s",
			fallback(rule.Name, firewallID), state, allow(rule), list(rule.SourceRanges), list(rule.TargetTags))
	}

	out, err = deps.Cloud.Run(ctx, withJSON(routerArgs("describe", cfg))...)
	if err != nil {
		if !gcloud.Missing(err) {
			return err
		}
		deps.Printf("router %s missing", routerID)
	} else {
		deps.Printf("router %s present in %s", routerID, region(cfg))
	}

	out, err = deps.Cloud.Run(ctx, withJSON(natDescribeArgs(cfg))...)
	if err != nil {
		if !gcloud.Missing(err) {
			return err
		}
		deps.Printf("nat %s missing on router %s", natID, routerID)
	} else {
		var nat natFacts
		if err := json.Unmarshal([]byte(out), &nat); err != nil {
			return fmt.Errorf("decode nat %s: %w", natID, err)
		}
		deps.Printf("nat %s on router %s: %s, %s",
			fallback(nat.Name, natID), routerID, fallback(nat.NatIPAllocateOption, "unknown allocation"), fallback(nat.SubnetworkIPRangesToN, "unknown ranges"))
	}

	out, err = deps.Cloud.Run(ctx, withJSON(subnetDescribeArgs(cfg))...)
	if err != nil {
		if !gcloud.Missing(err) {
			return err
		}
		deps.Printf("subnet %s missing in %s", vpcNetwork, region(cfg))
		return nil
	}
	var subnet subnetFacts
	if err := json.Unmarshal([]byte(out), &subnet); err != nil {
		return fmt.Errorf("decode subnet %s: %w", vpcNetwork, err)
	}
	deps.Printf("subnet %s private google access %t", fallback(subnet.Name, vpcNetwork), subnet.PrivateIPGoogleAccess)
	return nil
}

func allow(rule firewallFacts) string {
	parts := make([]string, 0, len(rule.Allowed))
	for _, entry := range rule.Allowed {
		value := entry.IPProtocol
		if len(entry.Ports) > 0 {
			value += ":" + strings.Join(entry.Ports, ",")
		}
		parts = append(parts, value)
	}
	return list(parts)
}

func list(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ",")
}

func fallback(value, when string) string {
	if value == "" {
		return when
	}
	return value
}
