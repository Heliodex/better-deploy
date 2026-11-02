// Per-app egress rules for untrusted tenants. The app keeps inherit-network so it can reach the
// loopback storage service, and iptables/ip6tables owner matches limit that uid.

package main

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

const (
	egressChain  = "WT-EGRESS"
	egressChain6 = "WT-EGRESS6"
)

type egressRun func(args ...string) error

// egressAppRules keeps the exact rule suffixes installed for one app, so they can be deleted again.
type egressAppRules struct {
	v4, v6 [][]string
}

// egressManager owns the IPv4 and IPv6 chains and the rules for each app. The runners are injectable so tests never touch a live firewall.
type egressManager struct {
	mu                 sync.Mutex
	storageIP          string
	storagePort        int
	internalIP         string
	internalPort       int
	run4, run6         egressRun
	ipv6               bool
	chainReady, chain6 bool
	rules              map[string]egressAppRules
}

func newEgressManager(bin, bin6 string, storageIP string, storagePort int, runner func(args ...string) error) *egressManager {
	manager := &egressManager{
		storageIP:   storageIP,
		storagePort: storagePort,
		ipv6:        true,
		rules:       map[string]egressAppRules{},
	}
	if runner != nil {
		manager.run4 = runner
		manager.run6 = runner
	} else {
		manager.run4 = commandRunner(bin)
		manager.run6 = commandRunner(bin6)
	}

	return manager
}

// commandRunner executes one iptables invocation and includes its output in the error.
func commandRunner(bin string) egressRun {
	return func(args ...string) error {
		cmd := exec.Command(bin, args...)

		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s %s: %w: %s", bin, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}

		return nil
	}
}

// ensureChain creates one chain if needed and hooks it into OUTPUT once. The caller holds the manager lock.
func ensureChain(run egressRun, chain string, ready *bool) error {
	if *ready {
		return nil
	}

	if err := run("-w", "-N", chain); err != nil {
		// The chain normally exists already; flush it so old per-app rules do not survive a restart.
		if flushErr := run("-w", "-F", chain); flushErr != nil {
			return fmt.Errorf("create egress chain %s: %w", chain, err)
		}
	}

	if err := run("-w", "-C", "OUTPUT", "-j", chain); err != nil {
		if err := run("-w", "-A", "OUTPUT", "-j", chain); err != nil {
			return fmt.Errorf("hook egress chain %s into OUTPUT: %w", chain, err)
		}
	}

	*ready = true

	return nil
}

// setInternal allows each app to reach the supervisor's internal fetch listener. It is called before any app launches.
func (m *egressManager) setInternal(ip string, port int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.internalIP, m.internalPort = ip, port
}

// rulesFor is the complete allow-then-reject IPv4 rule set for one app. The reply rule is owner-scoped too.
func (m *egressManager) rulesFor(uid, appPort int, extra ...egressRule) [][]string {
	rules := [][]string{
		{"-m", "owner", "--uid-owner", strconv.Itoa(uid), "-d", m.storageIP, "-p", "tcp", "--dport", strconv.Itoa(m.storagePort), "-j", "ACCEPT"},
	}
	if m.internalIP != "" && m.internalPort > 0 {
		rules = append(rules, []string{"-m", "owner", "--uid-owner", strconv.Itoa(uid), "-d", m.internalIP, "-p", "tcp", "--dport", strconv.Itoa(m.internalPort), "-j", "ACCEPT"})
	}

	for _, rule := range extra {
		rules = append(rules, []string{"-m", "owner", "--uid-owner", strconv.Itoa(uid), "-d", rule.CIDR, "-p", "tcp", "--dport", strconv.Itoa(rule.Port), "-j", "ACCEPT"})
	}

	return append(rules,
		[]string{"-m", "owner", "--uid-owner", strconv.Itoa(uid), "-d", "127.0.0.1", "-p", "tcp", "--sport", strconv.Itoa(appPort), "-j", "ACCEPT"},
		[]string{"-m", "owner", "--uid-owner", strconv.Itoa(uid), "-j", "REJECT"},
	)
}

// rules6For rejects every IPv6 packet from the app uid. Storage is IPv4 loopback only.
func (m *egressManager) rules6For(uid int) [][]string {
	return [][]string{
		{"-m", "owner", "--uid-owner", strconv.Itoa(uid), "-j", "REJECT"},
	}
}

// deleteRules deletes rules in order, returning the first failure after trying the rest.
func deleteRules(run egressRun, chain string, rules [][]string) error {
	var firstErr error

	for _, rule := range rules {
		args := append([]string{"-w", "-D", chain}, rule...)
		if err := run(args...); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	return firstErr
}

// appendRules appends rules and rolls back those it already added on failure.
func appendRules(run egressRun, chain string, rules [][]string) ([][]string, error) {
	appended := make([][]string, 0, len(rules))
	for _, rule := range rules {
		args := append([]string{"-w", "-A", chain}, rule...)
		if err := run(args...); err != nil {
			_ = deleteRules(run, chain, appended)
			return nil, err
		}

		appended = append(appended, rule)
	}

	return appended, nil
}

// add installs the app's rules. Any previous rules for the app are removed first, because a restart gets a new port.
func (m *egressManager) add(app string, uid, appPort int, extra ...egressRule) error {
	if uid <= 0 {
		return errors.New("egress rules need a non-zero app uid")
	}

	if appPort <= 0 {
		return errors.New("egress rules need the app's listening port")
	}

	if m.storageIP == "" || m.storagePort <= 0 {
		return errors.New("egress rules need the storage address")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := ensureChain(m.run4, egressChain, &m.chainReady); err != nil {
		return err
	}

	if m.ipv6 {
		if err := ensureChain(m.run6, egressChain6, &m.chain6); err != nil {
			return err
		}
	}

	if previous, ok := m.rules[app]; ok {
		if err := deleteRules(m.run4, egressChain, previous.v4); err != nil {
			return fmt.Errorf("remove previous IPv4 egress rules for %s: %w", app, err)
		}

		if len(previous.v6) > 0 {
			if err := deleteRules(m.run6, egressChain6, previous.v6); err != nil {
				return fmt.Errorf("remove previous IPv6 egress rules for %s: %w", app, err)
			}
		}

		delete(m.rules, app)
	}

	appended4, err := appendRules(m.run4, egressChain, m.rulesFor(uid, appPort, extra...))
	if err != nil {
		return fmt.Errorf("add IPv4 egress rule for %s: %w", app, err)
	}

	installed := egressAppRules{v4: appended4}

	if m.ipv6 {
		appended6, err := appendRules(m.run6, egressChain6, m.rules6For(uid))
		if err != nil {
			_ = deleteRules(m.run4, egressChain, appended4)
			return fmt.Errorf("add IPv6 egress rule for %s: %w", app, err)
		}

		installed.v6 = appended6
	}

	m.rules[app] = installed

	return nil
}

// remove deletes one app's rules, leaving both chains for other apps.
func (m *egressManager) remove(app string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	rules, ok := m.rules[app]
	if !ok {
		return nil
	}

	delete(m.rules, app)
	err4 := deleteRules(m.run4, egressChain, rules.v4)

	var err6 error
	if len(rules.v6) > 0 {
		err6 = deleteRules(m.run6, egressChain6, rules.v6)
	}

	if err4 != nil {
		return err4
	}

	return err6
}

// shutdown removes both OUTPUT jumps, flushes the chains and deletes them.
func (m *egressManager) shutdown() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.rules = map[string]egressAppRules{}

	var firstErr error

	for _, chain := range []struct {
		name  string
		run   egressRun
		ready *bool
	}{
		{egressChain, m.run4, &m.chainReady},
		{egressChain6, m.run6, &m.chain6},
	} {
		if !*chain.ready {
			continue
		}

		for _, args := range [][]string{
			{"-w", "-D", "OUTPUT", "-j", chain.name},
			{"-w", "-F", chain.name},
			{"-w", "-X", chain.name},
		} {
			if err := chain.run(args...); err != nil && firstErr == nil {
				firstErr = err
			}
		}

		*chain.ready = false
	}

	return firstErr
}
