package main

import (
	"fmt"
	"strings"
)

// runCompletion prints a completion script for the selected shell.
func runCompletion(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: wt completion <bash|zsh|fish>")
	}
	commands := []string{"apps", "logs", "jobs", "deploy", "bundle", "dev", "new", "rollback", "backup", "config", "open", "completion", "help"}
	switch args[0] {
	case "bash":
		fmt.Printf("_wt() {\n  local cur\n  cur=\"${COMP_WORDS[COMP_CWORD]}\"\n  COMPREPLY=( $(compgen -W %q -- \"$cur\") )\n}\ncomplete -F _wt wt-deploy\n", strings.Join(commands, " "))
	case "zsh":
		fmt.Printf("#compdef wt-deploy\n_arguments '1:command:(%s)'\n", strings.Join(commands, " "))
	case "fish":
		for _, command := range commands {
			fmt.Printf("complete -c wt-deploy -f -n '__fish_use_subcommand' -a %s\n", command)
		}
	default:
		return fmt.Errorf("unknown shell %q", args[0])
	}
	return nil
}
