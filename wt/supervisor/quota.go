// Optional per-uid filesystem quotas for untrusted app /data.

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

const (
	dataQuotaMonitor  = "monitor"
	dataQuotaSetquota = "setquota"
)

// quotaRunner executes one command and returns its combined output.
// Tests inject a fake so they never need root, setquota or a mounted quota filesystem.
type quotaRunner func(bin string, args ...string) (string, error)

// dataQuota applies standard user quotas to each app uid. The default monitor mode keeps the
// measured byte and entry-count monitor as the only enforcement and makes every method a no-op.
type dataQuota struct {
	mode       string
	filesystem string
	stateDir   string
	setquota   string
	findmnt    string
	run        quotaRunner

	mu       sync.Mutex
	resolved string
}

func newDataQuota(mode, filesystem, stateDir, setquota, findmnt string, runner quotaRunner) *dataQuota {
	if runner == nil {
		runner = execQuotaCommand
	}

	return &dataQuota{
		mode:       mode,
		filesystem: filesystem,
		stateDir:   stateDir,
		setquota:   setquota,
		findmnt:    findmnt,
		run:        runner,
	}
}

// enabled reports whether a real filesystem quota should be applied.
func (q *dataQuota) enabled() bool {
	return q != nil && q.mode == dataQuotaSetquota
}

// normaliseDataQuotaMode validates WT_DATA_QUOTA_MODE and fills in the monitor default.
func normaliseDataQuotaMode(mode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", dataQuotaMonitor:
		return dataQuotaMonitor, nil
	case dataQuotaSetquota:
		return dataQuotaSetquota, nil
	default:
		return "", fmt.Errorf("WT_DATA_QUOTA_MODE must be %s or %s, got %q", dataQuotaMonitor, dataQuotaSetquota, mode)
	}
}

// validateDataQuotaConfig normalises the mode and checks the binaries that setquota mode needs.
func validateDataQuotaConfig(cfg *config) error {
	mode, err := normaliseDataQuotaMode(cfg.dataQuotaMode)
	if err != nil {
		return err
	}

	cfg.dataQuotaMode = mode
	if mode != dataQuotaSetquota {
		return nil
	}

	if strings.TrimSpace(cfg.setquota) == "" {
		return fmt.Errorf("WT_SETQUOTA must not be empty when WT_DATA_QUOTA_MODE=%s", dataQuotaSetquota)
	}

	if strings.TrimSpace(cfg.dataQuotaFS) == "" && strings.TrimSpace(cfg.findmnt) == "" {
		return fmt.Errorf("WT_FINDMNT must not be empty when WT_DATA_QUOTA_MODE=%s and WT_DATA_QUOTA_FS is unset", dataQuotaSetquota)
	}

	return nil
}

// apply sets equal soft and hard byte and inode limits for one app uid.
func (q *dataQuota) apply(uid int, bytes, entries int64) error {
	if !q.enabled() {
		return nil
	}

	if uid <= 0 {
		return fmt.Errorf("data quota needs a positive uid, got %d", uid)
	}

	if bytes < 0 || entries < 0 {
		return errors.New("data quota limits must not be negative")
	}
	// setquota counts blocks in 1 KiB units. disk_mb is a whole number of MiB, so this is exact.
	return q.set(uid, bytes>>10, entries)
}

// clear removes every quota for one app uid.
func (q *dataQuota) clear(uid int) error {
	if !q.enabled() {
		return nil
	}

	if uid <= 0 {
		return fmt.Errorf("data quota needs a positive uid, got %d", uid)
	}

	return q.set(uid, 0, 0)
}

// set runs setquota with equal soft and hard limits. Zero means no limit, which clear uses.
func (q *dataQuota) set(uid int, blocks, entries int64) error {
	filesystem, err := q.mountPoint()
	if err != nil {
		return err
	}

	value := strconv.FormatInt(blocks, 10)
	limit := strconv.FormatInt(entries, 10)

	args := []string{"-u", strconv.Itoa(uid), value, value, limit, limit, filesystem}
	if _, err := q.run(q.setquota, args...); err != nil {
		return fmt.Errorf("data quota for uid %d on %s: %w", uid, filesystem, err)
	}

	return nil
}

// mountPoint returns WT_DATA_QUOTA_FS when set, otherwise the mount point containing
// WT_STATE_DIR. The findmnt result is cached for the life of the supervisor.
func (q *dataQuota) mountPoint() (string, error) {
	if strings.TrimSpace(q.filesystem) != "" {
		return q.filesystem, nil
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	if q.resolved != "" {
		return q.resolved, nil
	}

	out, err := q.run(q.findmnt, "-n", "-o", "TARGET", "--target", q.stateDir)
	if err != nil {
		return "", fmt.Errorf("resolve data quota filesystem: %w", err)
	}

	mount := strings.TrimSpace(out)
	if mount == "" {
		return "", fmt.Errorf("resolve data quota filesystem: %s -n -o TARGET --target %s returned no mount point", q.findmnt, q.stateDir)
	}

	q.resolved = mount

	return mount, nil
}

// execQuotaCommand runs a quota or findmnt command and includes its output in any error.
func execQuotaCommand(bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(context.Background(), bin, args...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", bin, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}

	return string(out), nil
}

// clearAppDataQuota removes an app uid's quota on delete. The uid is never reused, so this is best effort.
func (s *supervisor) clearAppDataQuota(name string, a *app) {
	if !s.quota.enabled() {
		return
	}

	uid := 0

	if a != nil {
		a.mu.Lock()
		uid = a.uid
		a.mu.Unlock()
	}

	if uid == 0 && s.uids != nil {
		uid, _ = s.uids.UID(name)
	}

	if uid == 0 {
		return
	}

	if err := s.quota.clear(uid); err != nil {
		log.Printf("app %s: clear data quota for uid %d: %v", name, uid, err)
	}
}
