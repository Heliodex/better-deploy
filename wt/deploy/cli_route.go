package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultRoutePath = "/"

type routeAppInfo struct {
	Name          string `json:"name"`
	Host          string `json:"host"`
	Status        string `json:"status"`
	Ready         bool   `json:"ready"`
	CurrentDigest string `json:"currentDigest"`
}

type routeReport struct {
	App        string   `json:"app"`
	Host       string   `json:"host"`
	URL        string   `json:"url"`
	Addresses  []string `json:"addresses,omitempty"`
	DNSError   string   `json:"dnsError,omitempty"`
	HTTPStatus int      `json:"httpStatus,omitempty"`
	HTTPError  string   `json:"httpError,omitempty"`
	Routed     bool     `json:"routed"`
	Ready      bool     `json:"ready"`
	AppStatus  string   `json:"appStatus,omitempty"`
	HealthPath string   `json:"healthPath"`
}

// routeApp fetches one app's public host from the supervisor.
func routeApp(cfg clientOptions, name string) (routeAppInfo, error) {
	status, _, body, err := apiDo(cfg, http.MethodGet, "/apps/"+name, nil, "")
	if err != nil {
		return routeAppInfo{}, err
	}

	if err := checkAPI(status, body); err != nil {
		return routeAppInfo{}, err
	}

	var info routeAppInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return routeAppInfo{}, err
	}

	return info, nil
}

// probeRoute checks DNS and one HTTP path on the app's public URL. A 404 means the host is not routed.
func probeRoute(supervisorHost, appHost, healthPath string, timeout time.Duration) routeReport {
	if healthPath == "" {
		healthPath = defaultRoutePath
	}

	target := appURLForHost(supervisorHost, appHost)
	report := routeReport{Host: appHost, URL: target, HealthPath: healthPath}

	parsed, err := url.Parse(target)
	if err != nil || parsed.Hostname() == "" {
		report.HTTPError = "invalid app URL"

		return report
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	addresses, err := net.DefaultResolver.LookupHost(ctx, parsed.Hostname())
	if err != nil {
		report.DNSError = err.Error()
	} else {
		report.Addresses = addresses
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(target, "/")+healthPath, nil)
	if err != nil {
		report.HTTPError = err.Error()

		return report
	}

	request.Header.Set("Accept", "text/html")

	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Do(request)
	if err != nil {
		report.HTTPError = err.Error()

		return report
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	text := strings.TrimSpace(string(body))

	report.HTTPStatus = resp.StatusCode
	report.Routed = resp.Header.Get("X-Request-ID") != ""

	if !report.Routed && resp.StatusCode != http.StatusNotFound {
		report.Routed = true
	}

	if !report.Routed && text != "" && text != "Not Found" && !strings.Contains(text, "no app for this host") {
		report.Routed = true
	}

	return report
}

// printRouteReport prints one route probe in a compact human-readable form.
func printRouteReport(report routeReport) {
	fmt.Printf("app: %s\n", report.App)
	fmt.Printf("host: %s (%s)\n", report.Host, report.HealthPath)

	if len(report.Addresses) > 0 {
		fmt.Printf("dns: %s\n", strings.Join(report.Addresses, ", "))
	} else if report.DNSError != "" {
		fmt.Printf("dns: %s\n", report.DNSError)
	}

	if report.HTTPStatus != 0 {
		fmt.Printf("http: %d\n", report.HTTPStatus)
	}

	if report.HTTPError != "" {
		fmt.Printf("http error: %s\n", report.HTTPError)
	}

	fmt.Printf("routed: %v\n", report.Routed)

	if report.AppStatus != "" {
		fmt.Printf("app: %s (ready %v)\n", report.AppStatus, report.Ready)
	}
}

// routeFailure turns an unrouted probe into an actionable error.
func routeFailure(report routeReport) error {
	detail := report.HTTPError
	if detail == "" {
		detail = fmt.Sprintf("HTTP %d", report.HTTPStatus)
	}

	return fmt.Errorf("app host %s is not routed (%s); create the ingress for %s or pass --skip-route-check", report.Host, detail, report.URL)
}

// runRoute dispatches wt route check <app>.
func runRoute(args []string) error {
	cfg, rest, err := clientFlags(args, true)
	if err != nil {
		return err
	}

	if len(rest) == 0 || rest[0] != "check" {
		return errors.New("usage: wt route check <app>")
	}

	return routeCheck(cfg, rest[1:])
}

// routeCheck checks one app's public host.
func routeCheck(cfg clientOptions, args []string) error {
	flags := newFlagSet("route check")
	healthPath := flags.String("path", defaultRoutePath, "path to request")
	timeout := flags.Duration("timeout", 8*time.Second, "probe timeout")

	if err := flags.Parse(args); err != nil {
		return err
	}

	if flags.NArg() != 1 {
		return errors.New("usage: wt route check <app>")
	}

	info, err := routeApp(cfg, flags.Arg(0))
	if err != nil {
		return err
	}

	report := probeRoute(cfg.host, info.Host, *healthPath, *timeout)
	report.App = info.Name

	if cfg.jsonOut {
		printRawOrJSONJSON(report)
	} else {
		printRouteReport(report)
	}

	if !report.Routed {
		return routeFailure(report)
	}

	return nil
}

// printRawOrJSONJSON marshals a typed report for --json without re-encoding an API body.
func printRawOrJSONJSON(value any) {
	data, err := json.Marshal(value)
	if err != nil {
		fmt.Println(err)
		return
	}

	fmt.Println(string(data))
}

// runDoctor reports supervisor runtime and optionally one app's route.
func runDoctor(args []string) error {
	cfg, rest, err := clientFlags(args, true)
	if err != nil {
		return err
	}

	flags := newFlagSet("doctor")
	healthPath := flags.String("path", defaultRoutePath, "path to request")
	timeout := flags.Duration("timeout", 8*time.Second, "probe timeout")

	if err := flags.Parse(rest); err != nil {
		return err
	}

	if flags.NArg() > 1 {
		return errors.New("usage: wt doctor [app]")
	}

	caps, err := fetchCapabilities(clientOptions{host: cfg.host, token: cfg.token, timeout: 10 * time.Second})
	if err != nil {
		return err
	}

	fmt.Printf("supervisor: %s\n", caps.displayVersion())

	if caps.Wasmtime != "" {
		fmt.Printf("wasmtime: %s\n", caps.Wasmtime)
	}

	if caps.WasmtimeTarget != "" {
		fmt.Printf("target: %s\n", caps.WasmtimeTarget)
	}

	fmt.Printf("compiler: %v\n", caps.Compiler)

	if flags.NArg() == 0 {
		return nil
	}

	info, err := routeApp(cfg, flags.Arg(0))
	if err != nil {
		return err
	}

	report := probeRoute(cfg.host, info.Host, *healthPath, *timeout)
	report.App = info.Name
	report.AppStatus = info.Status
	report.Ready = info.Ready

	if cfg.jsonOut {
		printRawOrJSONJSON(report)
	} else {
		printRouteReport(report)
	}

	if !report.Routed {
		return routeFailure(report)
	}

	return nil
}

// localHost reports whether a supervisor URL or app host is a loopback development name.
func localHost(value string) bool {
	host := value
	if parsed, err := url.Parse(baseURL(value)); err == nil && parsed.Hostname() != "" {
		host = parsed.Hostname()
	}

	host = strings.Trim(strings.ToLower(host), "[]")

	return host == "localhost" || strings.HasSuffix(host, ".localhost") || host == "127.0.0.1" || host == "::1"
}

// checkDeployRoute refuses to build when a non-local app host has no route yet.
func checkDeployRoute(cfg *deployConfig, m manifest) error {
	if cfg.skipRouteCheck || m.Host == "" || strings.Contains(m.Host, "*") || localHost(cfg.host) || localHost(m.Host) {
		return nil
	}

	report := probeRoute(cfg.host, m.Host, defaultRoutePath, 5*time.Second)
	if report.Routed {
		return nil
	}

	return routeFailure(report)
}
