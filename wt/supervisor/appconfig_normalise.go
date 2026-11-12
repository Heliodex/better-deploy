package main

import (
	"errors"
	"fmt"
	"net"
	"path"
	"slices"
	"strings"
)

// normaliseAllowCallers validates the target app's inbound app-to-app allowlist. Empty entries are ignored so a panel comma-separated field stays convenient.
func normaliseAllowCallers(names []string) ([]string, error) {
	if len(names) > maxAllowCallers {
		return nil, fmt.Errorf("allow_callers must contain at most %d apps", maxAllowCallers)
	}

	out := make([]string, 0, len(names))
	seen := map[string]struct{}{}

	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}

		if !validName(name) {
			return nil, fmt.Errorf("allow_callers: %q is not a valid app name", name)
		}

		if _, ok := seen[name]; ok {
			continue
		}

		seen[name] = struct{}{}
		out = append(out, name)
	}

	slices.Sort(out)

	return out, nil
}

// normalisePathPrefixes validates a sorted, de-duplicated list of absolute path prefixes.
func normalisePathPrefixes(field string, paths []string) ([]string, error) {
	if len(paths) > maxServerPaths {
		return nil, fmt.Errorf("%s must contain at most %d prefixes", field, maxServerPaths)
	}

	out := make([]string, 0, len(paths))
	seen := map[string]struct{}{}

	for _, raw := range paths {
		value := strings.TrimSpace(raw)
		if value == "" {
			return nil, fmt.Errorf("%s must not contain an empty prefix", field)
		}

		if !strings.HasPrefix(value, "/") {
			return nil, fmt.Errorf("%s: %q must start with /", field, value)
		}

		if strings.ContainsAny(value, "?#\\\x00\t\r\n") {
			return nil, fmt.Errorf("%s: %q contains a query, fragment, backslash or whitespace", field, value)
		}

		clean := path.Clean(value)
		if clean != value && clean != strings.TrimSuffix(value, "/") {
			return nil, fmt.Errorf("%s: %q must be a clean absolute path", field, value)
		}

		if _, ok := seen[clean]; ok {
			continue
		}

		seen[clean] = struct{}{}
		out = append(out, clean)
	}

	slices.Sort(out)

	return out, nil
}

// normaliseServerPaths validates the path prefixes where static HTML fallback must not intercept a browser navigation. Real static files still win.
func normaliseServerPaths(paths []string) ([]string, error) {
	return normalisePathPrefixes("server_paths", paths)
}

// normaliseServerPages validates the path prefixes where a page navigation reaches the component before a matching static file, so the component can choose the page or redirect.
func normaliseServerPages(paths []string) ([]string, error) {
	return normalisePathPrefixes("server_pages", paths)
}

// normaliseDependsOn validates the apps this app waits for before its host routes traffic. Forward references are allowed because a dependency may be deployed later.
func normaliseDependsOn(names []string, self string) ([]string, error) {
	if len(names) > maxDependsOn {
		return nil, fmt.Errorf("depends_on must contain at most %d apps", maxDependsOn)
	}

	out := make([]string, 0, len(names))
	seen := map[string]struct{}{}

	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}

		if name == self {
			return nil, errors.New("depends_on must not include the app itself")
		}

		if !validName(name) {
			return nil, fmt.Errorf("depends_on: %q is not a valid app name", name)
		}

		if _, ok := seen[name]; ok {
			continue
		}

		seen[name] = struct{}{}
		out = append(out, name)
	}

	slices.Sort(out)

	return out, nil
}

// normaliseEgress validates an explicit untrusted-mode outbound allowlist. IPv4 and IPv6 CIDRs are kept and applied to their own iptables chain.
func normaliseEgress(rules []egressRule) ([]egressRule, error) {
	if len(rules) > maxEgressRules {
		return nil, fmt.Errorf("egress must contain at most %d rules", maxEgressRules)
	}

	out := make([]egressRule, 0, len(rules))
	seen := map[egressRule]struct{}{}

	for _, rule := range rules {
		_, network, err := net.ParseCIDR(strings.TrimSpace(rule.CIDR))
		if err != nil {
			return nil, fmt.Errorf("egress %q: use an IPv4 or IPv6 CIDR such as 203.0.113.0/24 or 2001:db8::/32", rule.CIDR)
		}

		if rule.Port < 1 || rule.Port > 65535 {
			return nil, fmt.Errorf("egress %q: port must be between 1 and 65535", network)
		}

		next := egressRule{CIDR: network.String(), Port: rule.Port}
		if _, ok := seen[next]; ok {
			continue
		}

		seen[next] = struct{}{}
		out = append(out, next)
	}

	slices.SortFunc(out, func(a, b egressRule) int {
		if a.CIDR != b.CIDR {
			return strings.Compare(a.CIDR, b.CIDR)
		}

		return a.Port - b.Port
	})

	return out, nil
}

// normaliseAppAddress fills in and validates the app name, host and host aliases.
func normaliseAppAddress(cfg appConfig, name, domain string) (appConfig, error) {
	if strings.TrimSpace(cfg.Name) == "" {
		cfg.Name = name
	}

	if strings.TrimSpace(cfg.Host) == "" {
		cfg.Host = defaultHost(name, domain)
	}

	cfg.Host = strings.ToLower(strings.TrimSpace(cfg.Host))
	if err := validateAppHost(cfg.Host, domain); err != nil {
		return cfg, err
	}

	hosts, err := normaliseAppHosts(cfg.Hosts, cfg.Host, domain)
	if err != nil {
		return cfg, err
	}

	cfg.Hosts = hosts

	return cfg, nil
}

// normaliseAppRelations validates the caller allowlist, dependencies and egress rules.
func normaliseAppRelations(cfg appConfig, name string) (appConfig, error) {
	callers, err := normaliseAllowCallers(cfg.AllowCallers)
	if err != nil {
		return cfg, err
	}

	cfg.AllowCallers = callers

	dependsOn, err := normaliseDependsOn(cfg.DependsOn, name)
	if err != nil {
		return cfg, err
	}

	cfg.DependsOn = dependsOn

	egress, err := normaliseEgress(cfg.Egress)
	if err != nil {
		return cfg, err
	}

	cfg.Egress = egress

	return cfg, nil
}
