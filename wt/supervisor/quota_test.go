package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

type quotaCall struct {
	bin  string
	args []string
}

type fakeQuotaRunner struct {
	calls   []quotaCall
	respond func(bin string, args []string) (string, error)
}

func (f *fakeQuotaRunner) run(bin string, args ...string) (string, error) {
	f.calls = append(f.calls, quotaCall{bin: bin, args: append([]string(nil), args...)})
	if f.respond != nil {
		return f.respond(bin, args)
	}
	return "", nil
}

func TestDataQuotaApplyAndClearCommands(t *testing.T) {
	runner := &fakeQuotaRunner{}
	quota := newDataQuota(dataQuotaSetquota, "/srv/wt", t.TempDir(), "setquota", "findmnt", runner.run)

	if err := quota.apply(20001, 256<<20, 100000); err != nil {
		t.Fatal(err)
	}
	if err := quota.clear(20001); err != nil {
		t.Fatal(err)
	}
	want := []quotaCall{
		{bin: "setquota", args: []string{"-u", "20001", "262144", "262144", "100000", "100000", "/srv/wt"}},
		{bin: "setquota", args: []string{"-u", "20001", "0", "0", "0", "0", "/srv/wt"}},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Errorf("commands = %+v\nwant %+v", runner.calls, want)
	}
}

func TestDataQuotaDetectsAndCachesFilesystem(t *testing.T) {
	runner := &fakeQuotaRunner{respond: func(bin string, _ []string) (string, error) {
		if bin == "findmnt" {
			return "/var/lib/wt\n", nil
		}
		return "", nil
	}}
	quota := newDataQuota(dataQuotaSetquota, "", "/var/lib/wt", "setquota", "findmnt", runner.run)

	for range 2 {
		if err := quota.apply(20002, 64<<20, 50); err != nil {
			t.Fatal(err)
		}
	}

	findmntCalls := 0
	for _, call := range runner.calls {
		if call.bin != "findmnt" {
			continue
		}
		findmntCalls++
		want := []string{"-n", "-o", "TARGET", "--target", "/var/lib/wt"}
		if !reflect.DeepEqual(call.args, want) {
			t.Errorf("findmnt args = %v, want %v", call.args, want)
		}
	}
	if findmntCalls != 1 {
		t.Errorf("findmnt ran %d times, want once with a cached result", findmntCalls)
	}
	if len(runner.calls) != 3 {
		t.Errorf("calls = %+v, want one findmnt and two setquota", runner.calls)
	}
	last := runner.calls[len(runner.calls)-1]
	if last.bin != "setquota" || last.args[len(last.args)-1] != "/var/lib/wt" {
		t.Errorf("setquota did not use the detected filesystem: %+v", last)
	}
}

func TestDataQuotaFilesystemDetectionErrors(t *testing.T) {
	tests := map[string]func(string, []string) (string, error){
		"empty output": func(string, []string) (string, error) { return " \n", nil },
		"command failure": func(string, []string) (string, error) {
			return "", errors.New("findmnt: not found")
		},
	}
	for name, respond := range tests {
		t.Run(name, func(t *testing.T) {
			runner := &fakeQuotaRunner{respond: respond}
			quota := newDataQuota(dataQuotaSetquota, "", "/var/lib/wt", "setquota", "findmnt", runner.run)
			err := quota.apply(20003, 1<<20, 1)
			if err == nil {
				t.Fatal("expected filesystem detection to fail")
			}
			for _, call := range runner.calls {
				if call.bin == "setquota" {
					t.Errorf("setquota ran after detection failed: %+v", call)
				}
			}
		})
	}
}

func TestDataQuotaSetquotaFailureIsNotIgnored(t *testing.T) {
	runner := &fakeQuotaRunner{respond: func(bin string, _ []string) (string, error) {
		if bin == "setquota" {
			return "", errors.New("setquota: quotafile not found")
		}
		return "", nil
	}}
	quota := newDataQuota(dataQuotaSetquota, "/srv/wt", t.TempDir(), "setquota", "findmnt", runner.run)
	err := quota.apply(20004, 16<<20, 10)
	if err == nil || !strings.Contains(err.Error(), "quotafile not found") {
		t.Fatalf("error = %v, want the command failure", err)
	}
}

func TestExecQuotaCommandWrapsCommandAndOutput(t *testing.T) {
	_, err := execQuotaCommand("sh", "-c", "echo quota offline >&2; exit 1")
	if err == nil {
		t.Fatal("expected the command to fail")
	}
	message := err.Error()
	if !strings.Contains(message, "sh -c") || !strings.Contains(message, "quota offline") {
		t.Errorf("error = %q, want the command and its output", message)
	}
}

func TestDataQuotaModeValidation(t *testing.T) {
	cases := map[string]string{
		"":         dataQuotaMonitor,
		"monitor":  dataQuotaMonitor,
		" MONITOR": dataQuotaMonitor,
		"setquota": dataQuotaSetquota,
	}
	for input, want := range cases {
		got, err := normalizeDataQuotaMode(input)
		if err != nil || got != want {
			t.Errorf("normalizeDataQuotaMode(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := normalizeDataQuotaMode("project"); err == nil {
		t.Error("an unknown quota mode should be rejected")
	}

	cfg := config{dataQuotaMode: "setquota", setquota: "setquota", findmnt: "findmnt"}
	if err := validateDataQuotaConfig(&cfg); err != nil {
		t.Errorf("valid setquota config: %v", err)
	}
	if err := validateDataQuotaConfig(&config{dataQuotaMode: "setquota", findmnt: "findmnt"}); err == nil {
		t.Error("setquota mode without a setquota binary should be rejected")
	}
	cfg = config{dataQuotaMode: "setquota", setquota: "setquota"}
	if err := validateDataQuotaConfig(&cfg); err == nil {
		t.Error("setquota mode without findmnt or an explicit filesystem should be rejected")
	}
	cfg = config{dataQuotaMode: "setquota", setquota: "setquota", dataQuotaFS: "/srv/wt"}
	if err := validateDataQuotaConfig(&cfg); err != nil {
		t.Errorf("an explicit filesystem should not need findmnt: %v", err)
	}
}

func TestDataQuotaMonitorIsNoOp(t *testing.T) {
	runner := &fakeQuotaRunner{respond: func(string, []string) (string, error) {
		t.Fatal("monitor mode should not run a command")
		return "", nil
	}}
	quota := newDataQuota(dataQuotaMonitor, "", t.TempDir(), "setquota", "findmnt", runner.run)
	if quota.enabled() {
		t.Fatal("monitor mode should not be enabled")
	}
	if err := quota.apply(20005, 1<<20, 5); err != nil {
		t.Fatal(err)
	}
	if err := quota.clear(20005); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("monitor mode ran commands: %+v", runner.calls)
	}
}
