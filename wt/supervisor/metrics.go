// Lightweight Prometheus metrics for the supervisor.

package main

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type requestMetricKey struct {
	app, method, status string
}

type durationMetricKey struct {
	app, method string
}

type storageMetricKey struct {
	app, pool, result string
}

type storageDurationKey struct {
	app, pool string
}

type durationStat struct {
	count uint64
	nanos uint64
}

type outcomeKey struct {
	app, action, result string
}

type metrics struct {
	mu sync.Mutex

	requests   map[requestMetricKey]uint64
	durations  map[durationMetricKey]durationStat
	inflight   map[string]int64
	storage    map[storageMetricKey]uint64
	storageDur map[storageDurationKey]durationStat
	deploys    map[outcomeKey]uint64
	backups    map[outcomeKey]uint64
	jobs       map[outcomeKey]uint64
}

func newMetrics() *metrics {
	return &metrics{
		requests:   map[requestMetricKey]uint64{},
		durations:  map[durationMetricKey]durationStat{},
		inflight:   map[string]int64{},
		storage:    map[storageMetricKey]uint64{},
		storageDur: map[storageDurationKey]durationStat{},
		deploys:    map[outcomeKey]uint64{},
		backups:    map[outcomeKey]uint64{},
		jobs:       map[outcomeKey]uint64{},
	}
}

func (m *metrics) beginRequest(app string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.inflight[app]++
	m.mu.Unlock()
}

func (m *metrics) endRequest(app, method string, status int, duration time.Duration) {
	if m == nil {
		return
	}
	statusText := strconv.Itoa(status)
	m.mu.Lock()
	m.inflight[app]--
	if m.inflight[app] < 0 {
		m.inflight[app] = 0
	}
	m.requests[requestMetricKey{app: app, method: method, status: statusText}]++
	stat := m.durations[durationMetricKey{app: app, method: method}]
	stat.count++
	stat.nanos += uint64(duration)
	m.durations[durationMetricKey{app: app, method: method}] = stat
	m.mu.Unlock()
}

func (m *metrics) observeStorage(app, pool string, err error, duration time.Duration) {
	if m == nil {
		return
	}
	result := "ok"
	if err != nil {
		result = "error"
	}
	m.mu.Lock()
	m.storage[storageMetricKey{app: app, pool: pool, result: result}]++
	stat := m.storageDur[storageDurationKey{app: app, pool: pool}]
	stat.count++
	stat.nanos += uint64(duration)
	m.storageDur[storageDurationKey{app: app, pool: pool}] = stat
	m.mu.Unlock()
}

func (m *metrics) observeOutcome(target map[outcomeKey]uint64, app, action, result string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	target[outcomeKey{app: app, action: action, result: result}]++
	m.mu.Unlock()
}

func (m *metrics) observeDeploy(app, result string) {
	if m == nil {
		return
	}
	m.observeOutcome(m.deploys, app, "deploy", result)
}

func (m *metrics) observeBackup(app, action, result string) {
	if m == nil {
		return
	}
	m.observeOutcome(m.backups, app, action, result)
}

func (m *metrics) observeJob(app, result string) {
	if m == nil {
		return
	}
	m.observeOutcome(m.jobs, app, "job", result)
}

func prometheusLabel(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return value
}

func writeMetric(builder *strings.Builder, name, labels string, value string) {
	builder.WriteString(name)
	if labels != "" {
		builder.WriteByte('{')
		builder.WriteString(labels)
		builder.WriteByte('}')
	}
	builder.WriteByte(' ')
	builder.WriteString(value)
	builder.WriteByte('\n')
}

// render writes a Prometheus exposition snapshot. It is intentionally a small hand-written registry so the supervisor keeps one dependency set and no background goroutines.
func writeOutcomes(builder *strings.Builder, name, help string, values map[outcomeKey]uint64) {
	fmt.Fprintf(builder, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	keys := make([]outcomeKey, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].app != keys[j].app {
			return keys[i].app < keys[j].app
		}
		if keys[i].action != keys[j].action {
			return keys[i].action < keys[j].action
		}
		return keys[i].result < keys[j].result
	})
	for _, key := range keys {
		writeMetric(builder, name, fmt.Sprintf(`app="%s",action="%s",result="%s"`, prometheusLabel(key.app), prometheusLabel(key.action), prometheusLabel(key.result)), strconv.FormatUint(values[key], 10))
	}
}

// renderRequests writes request counters; the caller holds m.mu.
func (m *metrics) renderRequests(builder *strings.Builder) {
	builder.WriteString("# HELP wt_requests_total Proxied requests by app, method and status.\n# TYPE wt_requests_total counter\n")
	requestKeys := make([]requestMetricKey, 0, len(m.requests))
	for key := range m.requests {
		requestKeys = append(requestKeys, key)
	}
	sort.Slice(requestKeys, func(i, j int) bool {
		if requestKeys[i].app != requestKeys[j].app {
			return requestKeys[i].app < requestKeys[j].app
		}
		if requestKeys[i].method != requestKeys[j].method {
			return requestKeys[i].method < requestKeys[j].method
		}
		return requestKeys[i].status < requestKeys[j].status
	})
	for _, key := range requestKeys {
		writeMetric(builder, "wt_requests_total", fmt.Sprintf(`app="%s",method="%s",status="%s"`, prometheusLabel(key.app), prometheusLabel(key.method), prometheusLabel(key.status)), strconv.FormatUint(m.requests[key], 10))
	}
}

// renderDurations writes request duration summaries; the caller holds m.mu.
func (m *metrics) renderDurations(builder *strings.Builder) {
	builder.WriteString("# HELP wt_request_duration_seconds Proxied request duration.\n# TYPE wt_request_duration_seconds summary\n")
	durationKeys := make([]durationMetricKey, 0, len(m.durations))
	for key := range m.durations {
		durationKeys = append(durationKeys, key)
	}
	sort.Slice(durationKeys, func(i, j int) bool {
		if durationKeys[i].app != durationKeys[j].app {
			return durationKeys[i].app < durationKeys[j].app
		}
		return durationKeys[i].method < durationKeys[j].method
	})
	for _, key := range durationKeys {
		stat := m.durations[key]
		labels := fmt.Sprintf(`app="%s",method="%s"`, prometheusLabel(key.app), prometheusLabel(key.method))
		writeMetric(builder, "wt_request_duration_seconds_sum", labels, fmt.Sprintf("%.6f", float64(stat.nanos)/float64(time.Second)))
		writeMetric(builder, "wt_request_duration_seconds_count", labels, strconv.FormatUint(stat.count, 10))
	}
}

// renderInflight writes the current in-flight gauge; the caller holds m.mu.
func (m *metrics) renderInflight(builder *strings.Builder) {
	builder.WriteString("# HELP wt_inflight_requests Requests currently being proxied.\n# TYPE wt_inflight_requests gauge\n")
	inflightKeys := make([]string, 0, len(m.inflight))
	for app := range m.inflight {
		inflightKeys = append(inflightKeys, app)
	}
	sort.Strings(inflightKeys)
	for _, app := range inflightKeys {
		writeMetric(builder, "wt_inflight_requests", fmt.Sprintf(`app="%s"`, prometheusLabel(app)), strconv.FormatInt(m.inflight[app], 10))
	}
}

// renderStorage writes storage operation counters; the caller holds m.mu.
func (m *metrics) renderStorage(builder *strings.Builder) {
	builder.WriteString("# HELP wt_storage_operations_total Storage operations by app, pool and result.\n# TYPE wt_storage_operations_total counter\n")
	storageKeys := make([]storageMetricKey, 0, len(m.storage))
	for key := range m.storage {
		storageKeys = append(storageKeys, key)
	}
	sort.Slice(storageKeys, func(i, j int) bool {
		if storageKeys[i].app != storageKeys[j].app {
			return storageKeys[i].app < storageKeys[j].app
		}
		if storageKeys[i].pool != storageKeys[j].pool {
			return storageKeys[i].pool < storageKeys[j].pool
		}
		return storageKeys[i].result < storageKeys[j].result
	})
	for _, key := range storageKeys {
		writeMetric(builder, "wt_storage_operations_total", fmt.Sprintf(`app="%s",pool="%s",result="%s"`, prometheusLabel(key.app), prometheusLabel(key.pool), prometheusLabel(key.result)), strconv.FormatUint(m.storage[key], 10))
	}
}

// renderStorageDurations writes storage duration summaries; the caller holds m.mu.
func (m *metrics) renderStorageDurations(builder *strings.Builder) {
	builder.WriteString("# HELP wt_storage_operation_duration_seconds Storage operation duration.\n# TYPE wt_storage_operation_duration_seconds summary\n")
	storageDurationKeys := make([]storageDurationKey, 0, len(m.storageDur))
	for key := range m.storageDur {
		storageDurationKeys = append(storageDurationKeys, key)
	}
	sort.Slice(storageDurationKeys, func(i, j int) bool {
		if storageDurationKeys[i].app != storageDurationKeys[j].app {
			return storageDurationKeys[i].app < storageDurationKeys[j].app
		}
		return storageDurationKeys[i].pool < storageDurationKeys[j].pool
	})
	for _, key := range storageDurationKeys {
		stat := m.storageDur[key]
		labels := fmt.Sprintf(`app="%s",pool="%s"`, prometheusLabel(key.app), prometheusLabel(key.pool))
		writeMetric(builder, "wt_storage_operation_duration_seconds_sum", labels, fmt.Sprintf("%.6f", float64(stat.nanos)/float64(time.Second)))
		writeMetric(builder, "wt_storage_operation_duration_seconds_count", labels, strconv.FormatUint(stat.count, 10))
	}
}

// renderApps writes the per-app gauges outside the metrics lock.
func (m *metrics) renderApps(builder *strings.Builder, s *supervisor) {
	builder.WriteString("# HELP wt_app_cpu_percent CPU usage of one app process.\n# TYPE wt_app_cpu_percent gauge\n")
	builder.WriteString("# HELP wt_app_memory_bytes Resident memory of one app process.\n# TYPE wt_app_memory_bytes gauge\n")
	builder.WriteString("# HELP wt_app_ready Whether the app is ready for traffic.\n# TYPE wt_app_ready gauge\n")
	builder.WriteString("# HELP wt_app_disk_bytes Measured /data and database bytes.\n# TYPE wt_app_disk_bytes gauge\n")
	for _, a := range s.allApps() {
		cpu, memory, _ := a.resourceStats()
		_, ready := a.servingState()
		labels := fmt.Sprintf(`app="%s"`, prometheusLabel(a.name))
		writeMetric(builder, "wt_app_cpu_percent", labels, fmt.Sprintf("%.3f", cpu))
		writeMetric(builder, "wt_app_memory_bytes", labels, strconv.FormatInt(memory, 10))
		readyValue := 0
		if ready {
			readyValue = 1
		}
		writeMetric(builder, "wt_app_ready", labels, strconv.Itoa(readyValue))
		writeMetric(builder, "wt_app_disk_bytes", labels, strconv.FormatInt(s.diskBytes(a), 10))
	}
}

func (m *metrics) render(s *supervisor) string {
	var builder strings.Builder
	m.mu.Lock()
	m.renderRequests(&builder)
	m.renderDurations(&builder)
	m.renderInflight(&builder)
	m.renderStorage(&builder)
	m.renderStorageDurations(&builder)
	writeOutcomes(&builder, "wt_deploy_total", "Deploy and rollback outcomes.", m.deploys)
	writeOutcomes(&builder, "wt_backup_total", "Backup actions by outcome.", m.backups)
	writeOutcomes(&builder, "wt_jobs_total", "Job outcomes.", m.jobs)
	m.mu.Unlock()
	m.renderApps(&builder, s)
	return builder.String()
}

func (s *supervisor) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if s.metrics == nil {
		writeError(w, http.StatusServiceUnavailable, "metrics unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(s.metrics.render(s)))
}
