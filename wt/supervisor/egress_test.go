package main

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

type fakeRunner struct {
	commands [][]string
	fail     func(args []string) error
}

func (f *fakeRunner) run(args ...string) error {
	f.commands = append(f.commands, append([]string(nil), args...))
	if f.fail != nil {
		return f.fail(args)
	}

	return nil
}

func (f *fakeRunner) joined() []string {
	out := make([]string, len(f.commands))
	for i, args := range f.commands {
		out[i] = strings.Join(args, " ")
	}

	return out
}

func containsCommand(commands []string, want string) bool {
	return slices.Contains(commands, want)
}

func TestEgressRuleLifecycle(t *testing.T) {
	runner := &fakeRunner{}

	manager := newEgressManager("iptables", "ip6tables", "127.0.0.1", 9999, runner.run)
	if err := manager.add("demo", 20001, 8081); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"-w -N WT-EGRESS",
		"-w -C OUTPUT -j WT-EGRESS",
		"-w -N WT-EGRESS6",
		"-w -C OUTPUT -j WT-EGRESS6",
		"-w -A WT-EGRESS -m owner --uid-owner 20001 -d 127.0.0.1 -p tcp --dport 9999 -j ACCEPT",
		"-w -A WT-EGRESS -m owner --uid-owner 20001 -d 127.0.0.1 -p tcp --sport 8081 -j ACCEPT",
		"-w -A WT-EGRESS -m owner --uid-owner 20001 -j REJECT",
		"-w -A WT-EGRESS6 -m owner --uid-owner 20001 -j REJECT",
	}
	if got := runner.joined(); !reflect.DeepEqual(got, want) {
		t.Errorf("add commands =\n%v\nwant\n%v", got, want)
	}

	runner.commands = nil

	if err := manager.remove("demo"); err != nil {
		t.Fatal(err)
	}

	want = []string{
		"-w -D WT-EGRESS -m owner --uid-owner 20001 -d 127.0.0.1 -p tcp --dport 9999 -j ACCEPT",
		"-w -D WT-EGRESS -m owner --uid-owner 20001 -d 127.0.0.1 -p tcp --sport 8081 -j ACCEPT",
		"-w -D WT-EGRESS -m owner --uid-owner 20001 -j REJECT",
		"-w -D WT-EGRESS6 -m owner --uid-owner 20001 -j REJECT",
	}
	if got := runner.joined(); !reflect.DeepEqual(got, want) {
		t.Errorf("remove commands =\n%v\nwant\n%v", got, want)
	}

	runner.commands = nil

	if err := manager.shutdown(); err != nil {
		t.Fatal(err)
	}

	want = []string{
		"-w -D OUTPUT -j WT-EGRESS",
		"-w -F WT-EGRESS",
		"-w -X WT-EGRESS",
		"-w -D OUTPUT -j WT-EGRESS6",
		"-w -F WT-EGRESS6",
		"-w -X WT-EGRESS6",
	}
	if got := runner.joined(); !reflect.DeepEqual(got, want) {
		t.Errorf("shutdown commands =\n%v\nwant\n%v", got, want)
	}
}

func TestEgressIPv6OptOut(t *testing.T) {
	runner := &fakeRunner{}
	manager := newEgressManager("iptables", "ip6tables", "127.0.0.1", 9999, runner.run)

	manager.ipv6 = false
	if err := manager.add("demo", 20001, 8081); err != nil {
		t.Fatal(err)
	}

	for _, command := range runner.joined() {
		if strings.Contains(command, "WT-EGRESS6") {
			t.Errorf("IPv6 rule installed despite opt-out: %q", command)
		}
	}
}

func TestEgressFailsClosedWhenIPv6RulesFail(t *testing.T) {
	runner := &fakeRunner{fail: func(args []string) error {
		if len(args) > 1 && args[0] == "-w" && args[1] == "-A" && strings.Contains(args[2], "WT-EGRESS6") {
			return errors.New("ip6tables: unavailable")
		}

		return nil
	}}

	manager := newEgressManager("iptables", "ip6tables", "127.0.0.1", 9999, runner.run)
	if err := manager.add("demo", 20001, 8081); err == nil {
		t.Fatal("expected an error when the IPv6 rule cannot be installed")
	}

	commands := runner.joined()
	if !containsCommand(commands, "-w -D WT-EGRESS -m owner --uid-owner 20001 -j REJECT") {
		t.Errorf("IPv4 rules were not rolled back after the IPv6 failure: %v", commands)
	}

	if _, ok := manager.rules["demo"]; ok {
		t.Error("failed IPv6 install left tracked rules")
	}
}

func TestEgressFailsClosedWhenIPv6ChainCannotBeCreated(t *testing.T) {
	runner := &fakeRunner{}
	manager := newEgressManager("iptables", "ip6tables", "127.0.0.1", 9999, runner.run)

	manager.run6 = func(args ...string) error {
		if len(args) > 1 && args[0] == "-w" && (args[1] == "-N" || args[1] == "-F") {
			return errors.New("ip6tables: not found")
		}

		return nil
	}
	if err := manager.add("demo", 20001, 8081); err == nil {
		t.Fatal("expected an error when the IPv6 chain cannot be created")
	}

	if manager.chain6 {
		t.Error("IPv6 chain should not be marked ready")
	}
}

func TestEgressReplacesRulesForANewPort(t *testing.T) {
	runner := &fakeRunner{}

	manager := newEgressManager("iptables", "ip6tables", "127.0.0.1", 9999, runner.run)
	if err := manager.add("demo", 20001, 8081); err != nil {
		t.Fatal(err)
	}

	if err := manager.add("demo", 20001, 8082); err != nil {
		t.Fatal(err)
	}

	commands := runner.joined()
	if !containsCommand(commands, "-w -D WT-EGRESS -m owner --uid-owner 20001 -d 127.0.0.1 -p tcp --sport 8081 -j ACCEPT") {
		t.Errorf("old port rule was not removed: %v", commands)
	}

	if !containsCommand(commands, "-w -A WT-EGRESS -m owner --uid-owner 20001 -d 127.0.0.1 -p tcp --sport 8082 -j ACCEPT") {
		t.Errorf("new port rule was not added: %v", commands)
	}
}

func TestEgressFailsClosedWithoutIptables(t *testing.T) {
	runner := &fakeRunner{fail: func(_ []string) error {
		return errors.New("iptables: not found")
	}}

	manager := newEgressManager("iptables", "ip6tables", "127.0.0.1", 9999, runner.run)
	if err := manager.add("demo", 20001, 8081); err == nil {
		t.Fatal("expected an error when iptables is unavailable")
	}

	if manager.chainReady {
		t.Error("chain should not be marked ready after a failure")
	}
}

func TestEgressRejectsMissingIdentity(t *testing.T) {
	manager := newEgressManager("iptables", "ip6tables", "127.0.0.1", 9999, (&fakeRunner{}).run)
	if err := manager.add("demo", 0, 8081); err == nil {
		t.Error("uid 0 should be rejected")
	}

	if err := manager.add("demo", 20001, 0); err == nil {
		t.Error("port 0 should be rejected")
	}
}

func TestEgressAllowsInternalFetchAndConfiguredCIDR(t *testing.T) {
	runner := &fakeRunner{}
	manager := newEgressManager("iptables", "ip6tables", "127.0.0.1", 9999, runner.run)
	manager.setInternal("127.0.0.1", 9500)

	if err := manager.add("demo", 20001, 8081, egressRule{CIDR: "203.0.113.0/24", Port: 443}); err != nil {
		t.Fatal(err)
	}

	commands := runner.joined()
	for _, want := range []string{
		"-w -A WT-EGRESS -m owner --uid-owner 20001 -d 127.0.0.1 -p tcp --dport 9500 -j ACCEPT",
		"-w -A WT-EGRESS -m owner --uid-owner 20001 -d 203.0.113.0/24 -p tcp --dport 443 -j ACCEPT",
	} {
		if !containsCommand(commands, want) {
			t.Errorf("missing %q in %v", want, commands)
		}
	}

	if commands[len(commands)-1] != "-w -A WT-EGRESS6 -m owner --uid-owner 20001 -j REJECT" {
		t.Errorf("configured egress rules do not precede the reject: %v", commands)
	}
}

func TestEgressAllowsConfiguredIPv6CIDR(t *testing.T) {
	runner := &fakeRunner{}
	manager := newEgressManager("iptables", "ip6tables", "127.0.0.1", 9999, runner.run)

	if err := manager.add("demo", 20001, 8081,
		egressRule{CIDR: "203.0.113.0/24", Port: 443},
		egressRule{CIDR: "2001:db8::/32", Port: 8443},
	); err != nil {
		t.Fatal(err)
	}

	commands := runner.joined()
	for _, want := range []string{
		"-w -A WT-EGRESS -m owner --uid-owner 20001 -d 203.0.113.0/24 -p tcp --dport 443 -j ACCEPT",
		"-w -A WT-EGRESS6 -m owner --uid-owner 20001 -d 2001:db8::/32 -p tcp --dport 8443 -j ACCEPT",
		"-w -A WT-EGRESS6 -m owner --uid-owner 20001 -j REJECT",
	} {
		if !containsCommand(commands, want) {
			t.Errorf("missing %q in %v", want, commands)
		}
	}

	if containsCommand(commands, "-w -A WT-EGRESS -m owner --uid-owner 20001 -d 2001:db8::/32 -p tcp --dport 8443 -j ACCEPT") {
		t.Errorf("IPv6 rule was installed in the IPv4 chain: %v", commands)
	}

	if slices.Index(commands, "-w -A WT-EGRESS6 -m owner --uid-owner 20001 -d 2001:db8::/32 -p tcp --dport 8443 -j ACCEPT") > slices.Index(commands, "-w -A WT-EGRESS6 -m owner --uid-owner 20001 -j REJECT") {
		t.Errorf("IPv6 egress allow does not precede the reject: %v", commands)
	}

	runner.commands = nil

	if err := manager.remove("demo"); err != nil {
		t.Fatal(err)
	}

	if !containsCommand(runner.joined(), "-w -D WT-EGRESS6 -m owner --uid-owner 20001 -d 2001:db8::/32 -p tcp --dport 8443 -j ACCEPT") {
		t.Errorf("IPv6 egress rule was not removed: %v", runner.joined())
	}
}

func TestEgressRejectsIPv6RulesWhenIPv6IsDisabled(t *testing.T) {
	runner := &fakeRunner{}
	manager := newEgressManager("iptables", "ip6tables", "127.0.0.1", 9999, runner.run)

	manager.ipv6 = false
	if err := manager.add("demo", 20001, 8081, egressRule{CIDR: "2001:db8::/32", Port: 443}); err == nil {
		t.Fatal("expected an error when an IPv6 rule is configured with IPv6 egress disabled")
	}

	if len(runner.commands) != 0 {
		t.Errorf("rules were installed despite the reject: %v", runner.joined())
	}
}
