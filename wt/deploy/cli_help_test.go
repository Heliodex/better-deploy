package main

import (
	"strings"
	"testing"
)

// helpOutput renders help for one topic.
func helpOutput(t *testing.T, args ...string) string {
	t.Helper()

	var out strings.Builder
	if err := writeHelp(&out, args); err != nil {
		t.Fatalf("help %v: %v", args, err)
	}

	return out.String()
}

func TestHelpCoversEveryCommandAndSubcommand(t *testing.T) {
	for _, command := range helpCommands {
		out := helpOutput(t, command.name)
		for _, fragment := range []string{"wt " + command.name, "usage:", command.summary} {
			if !strings.Contains(out, fragment) {
				t.Errorf("help for %s missing %q:\n%s", command.name, fragment, out)
			}
		}

		for _, sub := range command.subs {
			out := helpOutput(t, command.name, sub.name)
			for _, fragment := range []string{"wt " + command.name + " " + sub.name, "usage:", sub.summary} {
				if !strings.Contains(out, fragment) {
					t.Errorf("help for %s %s missing %q:\n%s", command.name, sub.name, fragment, out)
				}
			}
		}
	}
}

func TestHelpShowsFlagsAndExamples(t *testing.T) {
	tests := []struct {
		args      []string
		fragments []string
	}{
		{[]string{"logs"}, []string{"--follow", "--tail N", "wt logs notes --follow"}},
		{[]string{"deploy"}, []string{"--skip-route-check", "--app-host HOST", "wt deploy --dir"}},
		{[]string{"jobs", "enqueue"}, []string{"--max-attempts N", "--idempotency-key KEY", "wt jobs ticker enqueue"}},
		{[]string{"secrets", "set"}, []string{"--file PATH", "stdin"}},
		{[]string{"build"}, []string{"--out FILE", "wt bundle [flags]"}},
	}

	for _, test := range tests {
		out := helpOutput(t, test.args...)
		for _, fragment := range test.fragments {
			if !strings.Contains(out, fragment) {
				t.Errorf("help %v missing %q:\n%s", test.args, fragment, out)
			}
		}
	}
}

func TestHelpShowsClientFlagsOnlyForRemoteCommands(t *testing.T) {
	if out := helpOutput(t, "logs"); !strings.Contains(out, "global flags") {
		t.Errorf("logs help has no global flags:\n%s", out)
	}

	if out := helpOutput(t, "new"); strings.Contains(out, "global flags") {
		t.Errorf("new help should not list client flags:\n%s", out)
	}
}

func TestHelpTargetFindsSubcommandBeforeHelpFlag(t *testing.T) {
	if got := strings.Join(helpTarget("jobs", []string{"ticker", "enqueue", "--help"}), " "); got != "jobs enqueue" {
		t.Errorf("jobs target = %q", got)
	}

	if got := strings.Join(helpTarget("backup", []string{"restore", "notes", "-h"}), " "); got != "backup restore" {
		t.Errorf("backup target = %q", got)
	}

	if got := strings.Join(helpTarget("deploy", []string{"--help"}), " "); got != "deploy" {
		t.Errorf("deploy target = %q", got)
	}
}

func TestHelpRejectsUnknownTopics(t *testing.T) {
	if err := writeHelp(new(strings.Builder), []string{"nope"}); err == nil {
		t.Error("unknown command was accepted")
	}

	if err := writeHelp(new(strings.Builder), []string{"jobs", "nope"}); err == nil {
		t.Error("unknown subcommand was accepted")
	}
}

func TestHelpIndexListsCommands(t *testing.T) {
	out := helpOutput(t)

	for _, command := range helpCommands {
		if !strings.Contains(out, command.name) || !strings.Contains(out, command.summary) {
			t.Errorf("index missing %s: %s", command.name, command.summary)
		}
	}

	if !strings.Contains(out, "wt help <command>") {
		t.Errorf("index does not point at per-command help:\n%s", out)
	}
}
