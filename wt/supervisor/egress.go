// Per-app egress rules for untrusted tenants. The app keeps inherit-network so it can reach the
// loopback storage service, and an iptables owner match limits that uid to the storage address.

package main

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

const egressChain = "WT-EGRESS"

// egressManager owns the WT-EGRESS chain and the rules for each app. The runner is injectable so tests never touch a live firewall.
type egressManager struct {
	mu          sync.Mutex
	storageIP   string
	storagePort int
	run         func(args ...string) error
	chainReady  bool
	rules       map[string][][]string
}

func newEgressManager(bin string, storageIP string, storagePort int, runner func(args ...string) error) *egressManager {
	if runner == nil {
		runner = commandRunner(bin)
	}
	return &egressManager{
		storageIP:   storageIP,
		storagePort: storagePort,
		run:         runner,
		rules:       map[string][][]string{},
	}
}

// commandRunner executes one iptables invocation and includes its output in the error.
func commandRunner(bin string) func(args ...string) error {
	return func(args ...string) error {
		cmd := exec.Command(bin, args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s %s: %w: %s", bin, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
}

// ensureChainLocked creates WT-EGRESS if needed and hooks it into OUTPUT once.
func (m *egressManager) ensureChainLocked() error {
	if m.chainReady {
		return nil
	}
	if err := m.run("-w", "-N", egressChain); err != nil {
		// The chain normally exists already; flush it so old per-app rules do not survive a restart.
		if flushErr := m.run("-w", "-F", egressChain); flushErr != nil {
			return fmt.Errorf("create egress chain: %w", err)
		}
	}
	if err := m.run("-w", "-C", "OUTPUT", "-j", egressChain); err != nil {
		if err := m.run("-w", "-A", "OUTPUT", "-j", egressChain); err != nil {
			return fmt.Errorf("hook egress chain into OUTPUT: %w", err)
		}
	}
	m.chainReady = true
	return nil
}

// rulesFor is the complete allow-then-reject rule set for one app.
func (m *egressManager) rulesFor(uid, appPort int) [][]string {
	return [][]string{
		{"-m", "owner", "--uid-owner", strconv.Itoa(uid), "-d", m.storageIP, "-p", "tcp", "--dport", strconv.Itoa(m.storagePort), "-j", "ACCEPT"},
		{"-d", "127.0.0.1", "-p", "tcp", "--sport", strconv.Itoa(appPort), "-j", "ACCEPT"},
		{"-m", "owner", "--uid-owner", strconv.Itoa(uid), "-j", "REJECT"},
	}
}

func (m *egressManager) deleteRulesLocked(rules [][]string) error {
	var firstErr error
	for _, rule := range rules {
		args := append([]string{"-w", "-D", egressChain}, rule...)
		if err := m.run(args...); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// add installs the app's rules. Any previous rules for the app are removed first, because a restart gets a new port.
func (m *egressManager) add(app string, uid, appPort int) error {
	if uid <= 0 {
		return fmt.Errorf("egress rules need a non-zero app uid")
	}
	if appPort <= 0 {
		return fmt.Errorf("egress rules need the app's listening port")
	}
	if m.storageIP == "" || m.storagePort <= 0 {
		return fmt.Errorf("egress rules need the storage address")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ensureChainLocked(); err != nil {
		return err
	}
	if previous, ok := m.rules[app]; ok {
		if err := m.deleteRulesLocked(previous); err != nil {
			return fmt.Errorf("remove previous egress rules for %s: %w", app, err)
		}
		delete(m.rules, app)
	}

	rules := m.rulesFor(uid, appPort)
	appended := make([][]string, 0, len(rules))
	for _, rule := range rules {
		args := append([]string{"-w", "-A", egressChain}, rule...)
		if err := m.run(args...); err != nil {
			_ = m.deleteRulesLocked(appended)
			return fmt.Errorf("add egress rule for %s: %w", app, err)
		}
		appended = append(appended, rule)
	}
	m.rules[app] = appended
	return nil
}

// remove deletes one app's rules, leaving the chain for other apps.
func (m *egressManager) remove(app string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rules, ok := m.rules[app]
	if !ok {
		return nil
	}
	delete(m.rules, app)
	return m.deleteRulesLocked(rules)
}

// shutdown removes the OUTPUT jump, flushes the chain and deletes it.
func (m *egressManager) shutdown() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.chainReady {
		return nil
	}
	m.rules = map[string][][]string{}
	var firstErr error
	for _, args := range [][]string{
		{"-w", "-D", "OUTPUT", "-j", egressChain},
		{"-w", "-F", egressChain},
		{"-w", "-X", egressChain},
	} {
		if err := m.run(args...); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	m.chainReady = false
	return firstErr
}
