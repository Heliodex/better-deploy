package main

import (
	"errors"
	"reflect"
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
	for _, command := range commands {
		if command == want {
			return true
		}
	}
	return false
}

func TestEgressRuleLifecycle(t *testing.T) {
	runner := &fakeRunner{}
	manager := newEgressManager("iptables", "127.0.0.1", 9999, runner.run)
	if err := manager.add("demo", 20001, 8081); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"-w -N WT-EGRESS",
		"-w -C OUTPUT -j WT-EGRESS",
		"-w -A WT-EGRESS -m owner --uid-owner 20001 -d 127.0.0.1 -p tcp --dport 9999 -j ACCEPT",
		"-w -A WT-EGRESS -d 127.0.0.1 -p tcp --sport 8081 -j ACCEPT",
		"-w -A WT-EGRESS -m owner --uid-owner 20001 -j REJECT",
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
		"-w -D WT-EGRESS -d 127.0.0.1 -p tcp --sport 8081 -j ACCEPT",
		"-w -D WT-EGRESS -m owner --uid-owner 20001 -j REJECT",
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
	}
	if got := runner.joined(); !reflect.DeepEqual(got, want) {
		t.Errorf("shutdown commands =\n%v\nwant\n%v", got, want)
	}
}

func TestEgressReplacesRulesForANewPort(t *testing.T) {
	runner := &fakeRunner{}
	manager := newEgressManager("iptables", "127.0.0.1", 9999, runner.run)
	if err := manager.add("demo", 20001, 8081); err != nil {
		t.Fatal(err)
	}
	if err := manager.add("demo", 20001, 8082); err != nil {
		t.Fatal(err)
	}
	commands := runner.joined()
	if !containsCommand(commands, "-w -D WT-EGRESS -d 127.0.0.1 -p tcp --sport 8081 -j ACCEPT") {
		t.Errorf("old port rule was not removed: %v", commands)
	}
	if !containsCommand(commands, "-w -A WT-EGRESS -d 127.0.0.1 -p tcp --sport 8082 -j ACCEPT") {
		t.Errorf("new port rule was not added: %v", commands)
	}
}

func TestEgressFailsClosedWithoutIptables(t *testing.T) {
	runner := &fakeRunner{fail: func(args []string) error {
		return errors.New("iptables: not found")
	}}
	manager := newEgressManager("iptables", "127.0.0.1", 9999, runner.run)
	if err := manager.add("demo", 20001, 8081); err == nil {
		t.Fatal("expected an error when iptables is unavailable")
	}
	if manager.chainReady {
		t.Error("chain should not be marked ready after a failure")
	}
}

func TestEgressRejectsMissingIdentity(t *testing.T) {
	manager := newEgressManager("iptables", "127.0.0.1", 9999, (&fakeRunner{}).run)
	if err := manager.add("demo", 0, 8081); err == nil {
		t.Error("uid 0 should be rejected")
	}
	if err := manager.add("demo", 20001, 0); err == nil {
		t.Error("port 0 should be rejected")
	}
}
