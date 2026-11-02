package main

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
)

// validDNSName checks DNS label syntax: at most 253 bytes, labels of 1–63 characters, and no leading or trailing hyphen. Callers lower-case names first.
func validDNSName(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}

	for label := range strings.SplitSeq(host, ".") {
		if len(label) == 0 || len(label) > 63 {
			return false
		}

		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}

		for _, r := range label {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				continue
			}

			return false
		}
	}

	return true
}

// isReservedHostName reports names that belong to the supervisor or the local machine rather than an app.
func isReservedHostName(host string) bool {
	return host == "localhost" || net.ParseIP(host) != nil
}

// validHost validates an exact app host. Reserved loopback and IP names are rejected so an app cannot shadow the admin listener.
func validHost(host string) bool {
	return validDNSName(host) && !isReservedHostName(host)
}

// validHostPattern validates an exact host or a single-label wildcard such as *.apps.example.com. A wildcard needs a dotted suffix, so *.com and bare *.localhost are not accepted.
func validHostPattern(host string) bool {
	if suffix, ok := strings.CutPrefix(host, "*."); ok {
		return strings.Contains(suffix, ".") && validDNSName(suffix) && net.ParseIP(suffix) == nil
	}

	return validHost(host)
}

// validateAppHost rejects malformed hosts and exact matches for the supervisor domain root. Wildcards may cover that root's children.
func validateAppHost(host, domain string) error {
	if suffix, ok := strings.CutPrefix(host, "*."); ok {
		if !strings.Contains(suffix, ".") || !validDNSName(suffix) || net.ParseIP(suffix) != nil {
			return fmt.Errorf("invalid host %q", host)
		}

		return nil
	}

	if isReservedHostName(host) {
		return fmt.Errorf("host %q is reserved for the supervisor", host)
	}

	if !validDNSName(host) {
		return fmt.Errorf("invalid host %q", host)
	}

	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if domain != "" && host == domain {
		return fmt.Errorf("host %q is reserved for the supervisor", host)
	}

	return nil
}

// normalizeAppHosts lower-cases, validates, de-duplicates and sorts aliases, dropping the primary host when it also appears in the list.
func normalizeAppHosts(hosts []string, primary, domain string) ([]string, error) {
	normalized := []string{}
	seen := map[string]bool{primary: true}

	for _, raw := range hosts {
		host := strings.ToLower(strings.TrimSpace(raw))
		if host == "" {
			return nil, errors.New("hosts must not contain an empty host")
		}

		if err := validateAppHost(host, domain); err != nil {
			return nil, err
		}

		if seen[host] {
			continue
		}

		seen[host] = true
		normalized = append(normalized, host)
	}

	slices.Sort(normalized)

	return normalized, nil
}

// appHosts returns the primary host followed by its aliases.
func appHosts(cfg appConfig) []string {
	hosts := make([]string, 0, len(cfg.Hosts)+1)
	if cfg.Host != "" {
		hosts = append(hosts, cfg.Host)
	}

	for _, host := range cfg.Hosts {
		if host != "" && host != cfg.Host {
			hosts = append(hosts, host)
		}
	}

	return hosts
}

// hostMatchesPattern reports whether an exact host is selected by a host or wildcard pattern. A wildcard covers exactly one leading label.
func hostMatchesPattern(host, pattern string) bool {
	if suffix, ok := strings.CutPrefix(pattern, "*."); ok {
		if !strings.HasSuffix(host, "."+suffix) {
			return false
		}

		label := strings.TrimSuffix(host, "."+suffix)

		return label != "" && !strings.Contains(label, ".")
	}

	return host == pattern
}

// hostPatternsOverlap reports whether two configured host patterns can select the same request. Single-label wildcards for different suffixes do not overlap.
func hostPatternsOverlap(a, b string) bool {
	if a == b {
		return true
	}

	aWildcard := strings.HasPrefix(a, "*.")

	bWildcard := strings.HasPrefix(b, "*.")
	switch {
	case aWildcard && bWildcard:
		return a == b
	case aWildcard:
		return hostMatchesPattern(b, a)
	case bWildcard:
		return hostMatchesPattern(a, b)
	default:
		return false
	}
}

// listenHost extracts a hostname from a TCP listen address. Unix sockets and port-only addresses have no host.
func listenHost(address string) string {
	address = strings.TrimSpace(address)
	if address == "" || strings.HasPrefix(strings.ToLower(address), "unix:") {
		return ""
	}

	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}

	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")

	return strings.Trim(host, "[]")
}

// reservedHost reports whether a request host belongs to the supervisor: loopback, an IP literal, the configured app domain root, or either listener's hostname.
func (s *supervisor) reservedHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" {
		return false
	}

	if isReservedHostName(host) {
		return true
	}

	domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s.cfg.domain)), ".")
	if domain != "" && host == domain {
		return true
	}

	for _, address := range []string{s.cfg.listen, s.cfg.adminListen} {
		if listenHost(address) == host {
			return true
		}
	}

	return false
}

// hostConflictError reports the first reserved or overlapping host in cfg. except lets an app keep its own hosts while it is updated.
func (s *supervisor) hostConflictError(cfg appConfig, except string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, host := range appHosts(cfg) {
		if s.reservedHost(host) {
			return fmt.Errorf("host %q is reserved for the supervisor", host)
		}

		for existing, owner := range s.hosts {
			if owner == nil || owner.name == except {
				continue
			}

			if hostPatternsOverlap(host, existing) {
				return fmt.Errorf("host %q is already used by another app", host)
			}
		}
	}

	return nil
}

// setAppHostsLocked replaces an app's routes. The caller holds s.mu.
func (s *supervisor) setAppHostsLocked(a *app, old, next appConfig) {
	for _, host := range appHosts(old) {
		if s.hosts[host] == a {
			delete(s.hosts, host)
		}
	}

	for _, host := range appHosts(next) {
		s.hosts[host] = a
	}
}

// removeAppHostsLocked drops an app's routes. The caller holds s.mu.
func (s *supervisor) removeAppHostsLocked(a *app, cfg appConfig) {
	for _, host := range appHosts(cfg) {
		if s.hosts[host] == a {
			delete(s.hosts, host)
		}
	}
}
