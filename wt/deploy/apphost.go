package main

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// isLocalSupervisorHost reports whether a supervisor URL points at this machine.
func isLocalSupervisorHost(host string) bool {
	parsed, err := url.Parse(baseURL(strings.TrimSpace(host)))
	if err != nil {
		return false
	}

	name := strings.ToLower(parsed.Hostname())
	if name == "localhost" {
		return true
	}

	ip := net.ParseIP(name)

	return ip != nil && ip.IsLoopback()
}

// isLocalAppHost reports whether an app host is a local-only name.
func isLocalAppHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")

	return host == "localhost" || strings.HasSuffix(host, ".localhost")
}

// applyAppDomain rewrites a manifest's local default host into the profile's public app domain.
func applyAppDomain(m *manifest, domain string) {
	domain = strings.Trim(strings.TrimSpace(domain), ".")
	if domain == "" {
		return
	}

	host := strings.TrimSpace(m.Host)
	lower := strings.ToLower(host)

	switch {
	case host == "":
		m.Host = m.Name + "." + domain
	case lower == "localhost":
		m.Host = m.Name + "." + domain
	case strings.HasSuffix(lower, ".localhost"):
		prefix := strings.TrimSuffix(host[:len(host)-len(".localhost")], ".")
		if prefix == "" {
			prefix = m.Name
		}

		m.Host = prefix + "." + domain
	}
}

// checkAppHostTarget refuses a local app host on a remote supervisor before anything is built or uploaded.
func checkAppHostTarget(supervisorHost, appHost, appName, profile string) error {
	if isLocalSupervisorHost(supervisorHost) {
		return nil
	}

	host := strings.TrimSpace(appHost)
	if host == "" {
		host = appName + ".localhost"
	} else if !isLocalAppHost(host) {
		return nil
	}

	hint := "pass --app-host or set app_domain in the config file"
	if profile != "" {
		hint = fmt.Sprintf("set app_domain in profile %q or pass --app-host", profile)
	}

	return fmt.Errorf("app host %q is local but supervisor %q is remote; %s", host, supervisorHost, hint)
}

// appDomainForTarget drops a profile's app domain when an explicit host switches between local and remote.
func appDomainForTarget(profileHost, supervisorHost, appDomain string) string {
	if appDomain == "" || isLocalSupervisorHost(profileHost) == isLocalSupervisorHost(supervisorHost) {
		return appDomain
	}

	return ""
}
