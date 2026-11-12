// Recording and history accounting for retained deployment bundles.

package main

import (
	"encoding/json/v2"
	"log"
	"os"
	"path/filepath"
	"time"
)

// deploymentMetadata fills the history entry for a successful bundle.
func deploymentMetadata(previous deploymentInfo, id string, now time.Time, record componentRecord, static map[string]string, total int64, artifact artifactInfo) deploymentInfo {
	digest, size, _ := record.digest()

	if previous.QueuedAt.IsZero() {
		previous.QueuedAt = now
	}

	if previous.StartedAt.IsZero() {
		previous.StartedAt = now
	}

	previous.ID = id
	previous.DeployedAt = now
	previous.FinishedAt = now
	previous.Digest = digest
	previous.Size = size
	previous.WasmDigest = record.wasmDigest
	previous.WasmBytes = record.wasmSize
	previous.Runtime = artifact.Wasmtime
	previous.Target = artifact.Target
	previous.Rebuildable = record.files["main.wasm"] != ""
	previous.Bytes = total
	previous.Blobs = record.files
	previous.Static = static
	previous.Status = deploymentStatusOK
	previous.Error = ""

	return previous
}

// recordAt materialises a successful bundle into an existing deployment id, keeping its log and queue timestamps.
func (m *deploymentManager) recordAt(app, id string, previous deploymentInfo, source string) (deploymentInfo, error) {
	if err := validDeploymentID(id); err != nil {
		return deploymentInfo{}, err
	}

	temp, err := os.MkdirTemp(m.appDir(app), ".record-")
	if err != nil {
		return deploymentInfo{}, err
	}
	defer os.RemoveAll(temp)

	artifact, _ := readArtifact(source)

	record, static, total, err := m.stageDeployment(app, id, source, temp)
	if err != nil {
		return deploymentInfo{}, err
	}

	now := time.Now().UTC()

	recorded := deploymentMetadata(previous, id, now, record, static, total, artifact)
	if err := writeDeploymentMeta(temp, recorded); err != nil {
		return deploymentInfo{}, err
	}

	if err := replaceDir(temp, filepath.Join(m.appDir(app), id)); err != nil {
		return deploymentInfo{}, err
	}

	if err := m.prune(app); err != nil {
		log.Printf("deploy history %s: prune: %v", app, err)
	}

	return recorded, nil
}

// record copies a bundle into history under a new deployment id.
func (m *deploymentManager) record(app, source string) (deploymentInfo, error) {
	id, now, err := m.newID(app)
	if err != nil {
		return deploymentInfo{}, err
	}

	return m.recordAt(app, id, deploymentInfo{QueuedAt: now}, source)
}

// recordFailure keeps a failed deploy attempt in history as metadata only. It has no bundle, so it can never be rolled back to, but the panel can show why the deploy did not replace the running version.
func (m *deploymentManager) recordFailure(app, digest, message string) (deploymentInfo, error) {
	if len(message) > deploymentErrorBytes {
		message = message[:deploymentErrorBytes] + "…"
	}

	id, now, err := m.newID(app)
	if err != nil {
		return deploymentInfo{}, err
	}

	temp, err := os.MkdirTemp(m.appDir(app), ".failure-")
	if err != nil {
		return deploymentInfo{}, err
	}
	defer os.RemoveAll(temp)

	recorded := deploymentInfo{
		ID:         id,
		DeployedAt: now,
		Digest:     digest,
		Status:     deploymentStatusFail,
		Error:      message,
	}

	meta, err := json.Marshal(recorded)
	if err != nil {
		return deploymentInfo{}, err
	}

	if err := os.WriteFile(filepath.Join(temp, deploymentMetaName), append(meta, '\n'), 0o644); err != nil {
		return deploymentInfo{}, err
	}

	if err := os.Rename(temp, filepath.Join(m.appDir(app), id)); err != nil {
		return deploymentInfo{}, err
	}

	if err := m.prune(app); err != nil {
		log.Printf("deploy history %s: prune: %v", app, err)
	}

	return recorded, nil
}

// historyBytes returns cached on-disk deployment history size, counting shared artifacts once.
func (s *supervisor) historyBytes(a *app) int64 {
	if s.deployments == nil {
		return 0
	}

	now := time.Now()

	a.history.mu.Lock()
	if !a.history.at.IsZero() && now.Sub(a.history.at) < diskUsageInterval {
		bytes := a.history.bytes
		a.history.mu.Unlock()

		return bytes
	}
	a.history.mu.Unlock()

	bytes, err := s.deployments.usage(a.name)
	if err != nil {
		log.Printf("app %s: deployment history usage: %v", a.name, err)
	}

	a.history.mu.Lock()
	a.history.bytes = bytes
	a.history.at = now
	a.history.mu.Unlock()

	return bytes
}

// resetHistoryUsage invalidates the cached deployment history size for an app.
func (s *supervisor) resetHistoryUsage(name string) {
	if a := s.lookup(name); a != nil {
		a.history.mu.Lock()
		a.history.at = time.Time{}
		a.history.mu.Unlock()
	}
}

// recordDeployment stores a deployed bundle in history and invalidates its size cache.
func (s *supervisor) recordDeployment(name, dir string) {
	if s.deployments == nil {
		return
	}

	if _, err := s.deployments.record(name, dir); err != nil {
		log.Printf("deploy history %s: %v", name, err)
	}

	s.resetHistoryUsage(name)
}

// recordDeploymentFailure adds a failed deploy attempt to history, with secrets redacted like the app's own errors.
func (s *supervisor) recordDeploymentFailure(name, digest string, cause error) {
	if s.deployments == nil || cause == nil {
		return
	}

	message := s.redactAppText(name, cause.Error())
	if _, err := s.deployments.recordFailure(name, digest, message); err != nil {
		log.Printf("deploy history %s: %v", name, err)
	}

	s.resetHistoryUsage(name)
}
