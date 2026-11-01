// Trusted-proxy handling for forwarded headers.

package main

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// forwardedInfo is the effective public view of a request after applying only trusted forwarding headers.
type forwardedInfo struct {
	Host      string
	Authority string
	Proto     string
	ClientIP  string
	Trusted   bool
}

// parseTrustedProxies reads a comma- or space-separated list of IPs and CIDRs. An entry without a prefix is one host.
func parseTrustedProxies(value string) ([]netip.Prefix, error) {
	var prefixes []netip.Prefix
	seen := map[netip.Prefix]struct{}{}
	for _, entry := range strings.FieldsFunc(value, func(char rune) bool {
		return char == ',' || char == ' ' || char == '\t' || char == '\n'
	}) {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		var prefix netip.Prefix
		if strings.Contains(entry, "/") {
			parsed, err := netip.ParsePrefix(entry)
			if err != nil {
				return nil, fmt.Errorf("trusted proxy %q: %w", entry, err)
			}
			prefix = parsed.Masked()
		} else {
			address, err := netip.ParseAddr(entry)
			if err != nil {
				return nil, fmt.Errorf("trusted proxy %q: %w", entry, err)
			}
			prefix = netip.PrefixFrom(address, address.BitLen())
		}
		if _, ok := seen[prefix]; ok {
			continue
		}
		seen[prefix] = struct{}{}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, nil
}

func (s *supervisor) isTrustedProxy(address string) bool {
	if address == "" {
		return false
	}
	parsed, err := netip.ParseAddr(strings.Trim(address, "[]"))
	if err != nil {
		return false
	}
	parsed = parsed.Unmap()
	for _, prefix := range s.cfg.trustedProxies {
		if prefix.Contains(parsed) {
			return true
		}
	}
	return false
}

// splitForwardedHost takes the first X-Forwarded-Host value and returns its host without a port plus the authority to forward.
func splitForwardedHost(value string) (host, authority string, ok bool) {
	value = strings.TrimSpace(strings.Split(value, ",")[0])
	if value == "" || strings.ContainsAny(value, "/\\ \t\r\n?#@") {
		return "", "", false
	}
	authority = strings.ToLower(strings.TrimSuffix(value, "."))
	host = authority
	if parsed, _, err := net.SplitHostPort(authority); err == nil {
		host = parsed
	} else if strings.HasPrefix(authority, "[") && strings.HasSuffix(authority, "]") {
		host = strings.Trim(authority, "[]")
	}
	return host, authority, host != ""
}

// firstForwardedProto accepts only the two schemes the supervisor can serve.
func firstForwardedProto(value string) string {
	value = strings.ToLower(strings.TrimSpace(strings.Split(value, ",")[0]))
	if value == "http" || value == "https" {
		return value
	}
	return ""
}

// forwardedClientIP walks the X-Forwarded-For chain from the nearest hop and returns the first address that is not a trusted proxy. If every entry is trusted, the leftmost address is the origin.
func (s *supervisor) forwardedClientIP(values []string, remote string) string {
	var addresses []string
	for _, value := range values {
		for _, entry := range strings.Split(value, ",") {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			if host, _, err := net.SplitHostPort(entry); err == nil {
				entry = host
			}
			address, err := netip.ParseAddr(strings.Trim(entry, "[]"))
			if err != nil {
				continue
			}
			addresses = append(addresses, address.Unmap().String())
		}
	}
	if len(addresses) == 0 {
		return remote
	}
	for index := len(addresses) - 1; index >= 0; index-- {
		if !s.isTrustedProxy(addresses[index]) {
			return addresses[index]
		}
	}
	return addresses[0]
}

// remoteIP returns the immediate peer address without its port.
func remoteIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(remoteAddr, "[]")
}

// forwardedInfo resolves the host, scheme and client address, trusting X-Forwarded-* only when the immediate peer is configured as a trusted proxy.
func (s *supervisor) forwardedInfo(r *http.Request) forwardedInfo {
	remote := remoteIP(r.RemoteAddr)
	authority := strings.ToLower(strings.TrimSpace(r.Host))
	host := requestHost(r)
	if authority == "" {
		authority = host
	}
	proto := "http"
	if r.TLS != nil {
		proto = "https"
	}
	info := forwardedInfo{Host: host, Authority: authority, Proto: proto, ClientIP: remote}
	if !s.isTrustedProxy(remote) {
		return info
	}
	info.Trusted = true
	if forwardedHost, forwardedAuthority, ok := splitForwardedHost(r.Header.Get("X-Forwarded-Host")); ok {
		info.Host, info.Authority = forwardedHost, forwardedAuthority
	}
	if forwardedProto := firstForwardedProto(r.Header.Get("X-Forwarded-Proto")); forwardedProto != "" {
		info.Proto = forwardedProto
	}
	info.ClientIP = s.forwardedClientIP(r.Header.Values("X-Forwarded-For"), remote)
	return info
}

// effectiveHost is the host used for routing, including a trusted X-Forwarded-Host when an ingress rewrites Host.
func (s *supervisor) effectiveHost(r *http.Request) string {
	return s.forwardedInfo(r).Host
}
