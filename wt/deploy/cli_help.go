package main

import (
	"fmt"
	"io"
)

// helpPathValue is the placeholder used for file and path flag arguments.
const helpPathValue = "PATH"

// helpFlag is one flag line in a command's help.
type helpFlag struct {
	name, value, description string
}

// helpCommand describes one command or subcommand for per-command help.
type helpCommand struct {
	name     string
	summary  string
	usage    []string
	details  []string
	flags    []helpFlag
	examples []string
	subs     []helpCommand
	client   bool
}

// helpCommands is the help registry in the order shown by wt help.
var helpCommands = []helpCommand{
	{
		name:    "apps",
		summary: "list deployed apps",
		usage:   []string{"wt apps [flags]"},
		details: []string{"Prints a table of apps with their host, status, readiness, PID and digest. --json returns the raw API response."},
		examples: []string{
			"wt apps",
			"wt apps --json",
		},
		client: true,
	},
	{
		name:    "logs",
		summary: "print or follow an app's logs",
		usage:   []string{"wt logs <app> [flags]"},
		details: []string{"Without --follow the supervisor returns one page of entries. --follow consumes its SSE stream until interrupted."},
		flags: []helpFlag{
			{"--tail", "N", "number of entries to fetch (default 200)"},
			{"--level", "LEVEL", "filter by level, for example warn"},
			{"--source", "app|supervisor", "filter by log source"},
			{"--q", "TEXT", "filter by a text query"},
			{"--follow", "", "stream entries as they arrive"},
		},
		examples: []string{
			"wt logs notes --tail 200 --level warn --source app",
			"wt logs notes --follow",
		},
		client: true,
	},
	{
		name:    "jobs",
		summary: "inspect and manage durable jobs",
		usage:   []string{"wt jobs <app> <action> [flags]"},
		client:  true,
		subs: []helpCommand{
			{
				name:    "list",
				summary: "list an app's jobs",
				usage:   []string{"wt jobs <app> list [flags]"},
				flags: []helpFlag{
					{"--status", "STATUS", "pending, running, succeeded, failed, dead or canceled"},
				},
				examples: []string{"wt jobs ticker list --status dead"},
			},
			{
				name:     "show",
				summary:  "show one job",
				usage:    []string{"wt jobs <app> show <id> [flags]"},
				examples: []string{"wt jobs ticker show <id>"},
			},
			{
				name:    "enqueue",
				summary: "create a job",
				usage:   []string{"wt jobs <app> enqueue [flags]"},
				flags: []helpFlag{
					{"--path", helpPathValue, "target path, for example /jobs/email (required)"},
					{"--method", "METHOD", "HTTP method (default POST)"},
					{"--body", "JSON", "request body"},
					{"--file", helpPathValue, "read the request body from a file"},
					{"--header", "KEY: VALUE", "request header; repeatable"},
					{"--run-at", "TIME", "RFC3339 time to run the job"},
					{"--max-attempts", "N", "maximum attempts (1-20)"},
					{"--idempotency-key", "KEY", "return the existing job for this key"},
				},
				examples: []string{`wt jobs ticker enqueue --path /jobs/beat --body '{"slot":1}' --max-attempts 5`},
			},
			{
				name:     "cancel",
				summary:  "cancel a pending job",
				usage:    []string{"wt jobs <app> cancel <id> [flags]"},
				examples: []string{"wt jobs ticker cancel <id>"},
			},
			{
				name:     "retry",
				summary:  "retry a failed or dead job",
				usage:    []string{"wt jobs <app> retry <id> [flags]"},
				examples: []string{"wt jobs ticker retry <id>"},
			},
		},
	},
	{
		name:    "blobs",
		summary: "inspect and clean up an app's blobs",
		usage:   []string{"wt blobs <app> <action> [flags]"},
		client:  true,
		subs: []helpCommand{
			{
				name:     "list",
				summary:  "list stored blobs",
				usage:    []string{"wt blobs <app> list [flags]"},
				examples: []string{"wt blobs blobbox list"},
			},
			{
				name:     "delete",
				summary:  "remove one blob by digest",
				usage:    []string{"wt blobs <app> delete <digest> [flags]"},
				examples: []string{"wt blobs blobbox delete <digest>"},
			},
			{
				name:    "gc",
				summary: "remove old blobs, or preview the removal",
				usage:   []string{"wt blobs <app> gc [flags]"},
				details: []string{"The cutoff is explicit. Pass --keep for every digest the app still references, or run --dry-run first."},
				flags: []helpFlag{
					{"--older-than", "DURATION", "minimum age, for example 720h (required)"},
					{"--keep", "DIGEST", "digest to protect; repeatable or comma-separated"},
					{"--dry-run", "", "report what would be removed"},
				},
				examples: []string{
					"wt blobs blobbox gc --older-than 720h --dry-run",
					"wt blobs blobbox gc --older-than 720h --keep <digest>,<digest>",
				},
			},
		},
	},
	{
		name:    "secrets",
		summary: "list, set and delete an app's secrets",
		usage:   []string{"wt secrets <app> <action> [flags]"},
		details: []string{"Values are never printed. A change that the supervisor cannot apply live reports that a restart is required."},
		client:  true,
		subs: []helpCommand{
			{
				name:     "list",
				summary:  "print secret key names",
				usage:    []string{"wt secrets <app> list [flags]"},
				examples: []string{"wt secrets passport list"},
			},
			{
				name:    "set",
				summary: "write one secret from stdin or a file",
				usage:   []string{"wt secrets <app> set <KEY> [flags]"},
				details: []string{"The value is read from stdin unless --file is given. One trailing newline is trimmed and the value is capped at 64 KiB."},
				flags: []helpFlag{
					{"--file", helpPathValue, "read the value from a file instead of stdin"},
				},
				examples: []string{"printf 'client-secret' | wt secrets passport set OAUTH_CLIENT_SECRET"},
			},
			{
				name:     "delete",
				summary:  "remove one secret",
				usage:    []string{"wt secrets <app> delete <KEY> [flags]"},
				examples: []string{"wt secrets passport delete OAUTH_CLIENT_SECRET"},
			},
		},
	},
	{
		name:    "route",
		summary: "check that an app host is routed",
		usage:   []string{"wt route check <app> [flags]"},
		flags: []helpFlag{
			{"--path", helpPathValue, "path to request (default /healthz)"},
			{"--timeout", "DURATION", "probe timeout (default 8s)"},
		},
		examples: []string{"wt route check passport"},
		client:   true,
	},
	{
		name:    "doctor",
		summary: "report the supervisor runtime and optionally one app's route",
		usage:   []string{"wt doctor [app] [flags]"},
		flags: []helpFlag{
			{"--path", helpPathValue, "path to request (default /healthz)"},
			{"--timeout", "DURATION", "probe timeout (default 8s)"},
		},
		examples: []string{
			"wt doctor",
			"wt doctor passport",
		},
		client: true,
	},
	{
		name:    "runtime",
		summary: "compare the local and supervisor wasmtime releases",
		usage:   []string{"wt runtime [flags]"},
		flags: []helpFlag{
			{"--wasmtime", helpPathValue, "local wasmtime binary (default wasmtime)"},
		},
		examples: []string{"wt runtime"},
		client:   true,
	},
	{
		name:    "inspect",
		summary: "show an app's route, deployment, config, env and secrets",
		usage:   []string{"wt inspect <app> [flags]"},
		details: []string{"With --dir, the local app.json is compared against the deployed configuration."},
		flags: []helpFlag{
			{"--dir", "DIR", "local app directory to compare (default .)"},
		},
		examples: []string{"wt inspect passport --dir ./passport"},
		client:   true,
	},
	{
		name:    "deploy",
		summary: "build and upload an app to the supervisor",
		usage:   []string{"wt deploy [flags]"},
		details: []string{"Runs app.json's build commands, precompiles the component with wasmtime and uploads the bundle. The host is required and can come from --host, WT_DEPLOY_HOST or the config file."},
		flags: []helpFlag{
			{"--dir", "DIR", `app directory containing app.json (default ".")`},
			{"--app", "NAME", "app name (default app.json name or directory name)"},
			{"--host", "URL", "supervisor URL, for example http://127.0.0.1:8080"},
			{"--token", "TOKEN", "deploy bearer token"},
			{"--wasmtime", helpPathValue, "wasmtime binary (default wasmtime)"},
			{"--target", "TRIPLE", "wasmtime compile target (default supervisor target)"},
			{"--app-host", "HOST", "override the app host in the uploaded app.json"},
			{"--config", helpPathValue, "config file (default ~/.config/wt/config.json)"},
			{"--json", "", "print only the raw API response"},
			{"--force-build", "", "rebuild the component and static phases"},
			{"--skip-build", "", "skip running app.json's build commands"},
			{"--skip-compile", "", "do not compile; include main.cwasm only if it exists"},
			{"--skip-route-check", "", "do not verify that the app host is already routed"},
		},
		examples: []string{"wt deploy --dir ./notes --app-host notes.example.com"},
	},
	{
		name:    "rollback",
		summary: "restore a previous deployment, optionally with a snapshot",
		usage:   []string{"wt rollback <app> <deployment-id> [flags]"},
		flags: []helpFlag{
			{"--backup", "SNAPSHOT", "also restore this snapshot, swapping code and data together"},
		},
		examples: []string{
			"wt rollback notes 20261101T120000.000000000Z",
			"wt rollback notes 20261101T120000.000000000Z --backup storage-20261101T120000.000000000Z.db",
		},
		client: true,
	},
	{
		name:    "backup",
		summary: "list, create, restore, download and import snapshots",
		usage:   []string{"wt backup <action> <app> [flags]"},
		client:  true,
		subs: []helpCommand{
			{
				name:     "list",
				summary:  "list retained snapshots and mark the current one",
				usage:    []string{"wt backup list <app> [flags]"},
				examples: []string{"wt backup list notes"},
			},
			{
				name:     "create",
				summary:  "take a snapshot now",
				usage:    []string{"wt backup create <app> [flags]"},
				examples: []string{"wt backup create notes"},
			},
			{
				name:     "restore",
				summary:  "replace the app's data with a snapshot",
				usage:    []string{"wt backup restore <app> <snapshot> [flags]"},
				examples: []string{"wt backup restore notes storage-20261101T120000.000000000Z.db"},
			},
			{
				name:    "download",
				summary: "download a snapshot archive",
				usage:   []string{"wt backup download <app> <snapshot> [flags]"},
				flags: []helpFlag{
					{"--out", "FILE", "output path (default <snapshot>.tar.gz)"},
				},
				examples: []string{"wt backup download notes storage-20261101T120000.000000000Z.db --out notes.tar.gz"},
			},
			{
				name:     "import",
				summary:  "store an exported snapshot archive without restoring it",
				usage:    []string{"wt backup import <app> <archive.tar.gz> [flags]"},
				examples: []string{"wt backup import notes notes.tar.gz"},
			},
		},
	},
	{
		name:    "config",
		summary: "show or patch an app's runtime config",
		usage:   []string{"wt config <action> <app> [flags]"},
		client:  true,
		subs: []helpCommand{
			{
				name:     "show",
				summary:  "print the effective config and its ETag",
				usage:    []string{"wt config show <app> [flags]"},
				examples: []string{"wt config show notes"},
			},
			{
				name:    "set",
				summary: "apply a JSON merge patch with optional If-Match",
				usage:   []string{"wt config set <app> [flags]"},
				flags: []helpFlag{
					{"--data", "JSON", "inline JSON patch"},
					{"--file", helpPathValue, "read the JSON patch from a file"},
					{"--if-match", "ETAG", "only apply when the ETag still matches"},
				},
				examples: []string{`wt config set notes --data '{"memory_mb":256}' --if-match '"etag"'`},
			},
		},
	},
	{
		name:    "open",
		summary: "open an app in the browser",
		usage:   []string{"wt open <app> [flags]"},
		flags: []helpFlag{
			{"--print", "", "print the URL instead of opening it"},
		},
		examples: []string{"wt open notes"},
		client:   true,
	},
	{
		name:    "build",
		summary: "build an upload bundle locally, without a supervisor",
		usage:   []string{"wt build [flags]", "wt bundle [flags]"},
		details: []string{"Runs the component and static phases, precompiles the component when wasmtime is available and writes the tarball. This is the local build path for the pinned Containerfile."},
		flags: []helpFlag{
			{"--dir", "DIR", `app directory containing app.json (default ".")`},
			{"--wasmtime", helpPathValue, "wasmtime binary (default wasmtime)"},
			{"--target", "TRIPLE", "wasmtime compile target (default x86_64-unknown-linux-musl)"},
			{"--out", "FILE", "output file (default <name>.tar.gz)"},
			{"--force-build", "", "rebuild the component and static phases"},
			{"--skip-build", "", "skip running app.json's build commands"},
			{"--skip-compile", "", "do not compile; include main.cwasm only if it exists"},
		},
		examples: []string{"wt build --dir ./notes --out notes.tar.gz"},
	},
	{
		name:    "dev",
		summary: "run an app locally with the dev supervisor",
		usage:   []string{"wt dev [flags]"},
		details: []string{"Builds and deploys into a local supervisor, serves the app at <app>.localhost and runs the SvelteKit dev server when a web directory exists."},
		flags: []helpFlag{
			{"--dir", "DIR", `app directory containing app.json (default ".")`},
			{"--workspace", "DIR", "serve every child app together"},
			{"--port", "N", "loopback port for the dev supervisor (default 8080)"},
			{"--admin-port", "N", "separate admin port (default app port + 1)"},
			{"--wasmtime", helpPathValue, "wasmtime binary (default wasmtime)"},
			{"--target", "TRIPLE", "wasmtime compile target (default match the local wasmtime)"},
			{"--supervisor", helpPathValue, "supervisor binary (default PATH, then build from ../../supervisor)"},
			{"--app-host", "HOST", "override the app host (default <app>.localhost)"},
			{"--skip-build", "", "skip running app.json's build commands"},
			{"--skip-compile", "", "reuse an existing main.cwasm"},
			{"--watch", "BOOL", "rebuild and redeploy when inputs change (default true)"},
			{"--vite", "BOOL", "run the SvelteKit dev server with HMR when web exists (default true)"},
		},
		examples: []string{
			"wt dev --dir ./notes",
			"wt dev --workspace ./example",
		},
	},
	{
		name:    "new",
		summary: "scaffold a new app",
		usage:   []string{"wt new [flags]"},
		flags: []helpFlag{
			{"--dir", helpPathValue, `directory to scaffold the app in (default ".")`},
			{"--module", helpPathValue, "Go module path for a standalone app (default app name)"},
			{"--sdk", helpPathValue, "replace the published Go SDK with a local wt/packages checkout"},
			{"--contract", "valibot|none", "contract pipeline (default valibot)"},
		},
		examples: []string{
			"wt new --dir ./notes",
			"wt new --dir ./notes --sdk ../wt/packages --contract none",
		},
	},
	{
		name:    "contract",
		summary: "generate or check an app's JSON Schema and Go contract",
		usage:   []string{"wt contract [flags]"},
		flags: []helpFlag{
			{"--dir", "DIR", `app directory containing app.json (default ".")`},
			{"--check", "", "verify the generated files without writing them"},
		},
		examples: []string{
			"wt contract --dir ./passport",
			"wt contract --dir ./passport --check",
		},
	},
	{
		name:    "completion",
		summary: "print a shell completion script",
		usage:   []string{"wt completion <bash|zsh|fish>"},
		examples: []string{
			"wt completion bash",
			"wt completion zsh",
			"wt completion fish",
		},
	},
	{
		name:    "help",
		summary: "show this help or one command's help",
		usage:   []string{"wt help [command] [subcommand]"},
		details: []string{"Every command also accepts -h or --help, for example wt jobs enqueue --help."},
		examples: []string{
			"wt help",
			"wt help logs",
			"wt help jobs enqueue",
		},
	},
}

// globalHelpFlags is the flag section shown for commands that take the shared client flags.
var globalHelpFlags = []helpFlag{
	{"--host", "URL", "supervisor URL (default WT_DEPLOY_HOST or the config file)"},
	{"--token", "TOKEN", "bearer token (default WT_TOKEN or the config file)"},
	{"--config", helpPathValue, "config file (default ~/.config/wt/config.json)"},
	{"--json", "", "print the raw API response"},
	{"--timeout", "DURATION", "request timeout (default 30s)"},
}

// findHelpCommand returns the top-level help entry, following the build alias.
func findHelpCommand(name string) *helpCommand {
	if name == "bundle" {
		name = "build"
	}

	for index := range helpCommands {
		if helpCommands[index].name == name {
			return &helpCommands[index]
		}
	}

	return nil
}

// findHelpSub returns the named subcommand of cmd.
func findHelpSub(cmd *helpCommand, name string) *helpCommand {
	for index := range cmd.subs {
		if cmd.subs[index].name == name {
			return &cmd.subs[index]
		}
	}

	return nil
}

// helpTarget turns command arguments into a help lookup path. If a help flag is present, the tokens before it are searched for a subcommand so wt jobs enqueue --help shows enqueue's help.
func helpTarget(command string, args []string) []string {
	target := []string{command}

	topic := findHelpCommand(command)
	if topic == nil || len(topic.subs) == 0 {
		return target
	}

	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			break
		}

		if sub := findHelpSub(topic, arg); sub != nil {
			return append(target, sub.name)
		}
	}

	return target
}

// writeHelp prints the command index, or one command or subcommand's help.
func writeHelp(w io.Writer, args []string) error {
	if len(args) == 0 {
		printCommandIndex(w)
		return nil
	}

	topic := findHelpCommand(args[0])
	if topic == nil {
		return fmt.Errorf("unknown command %q", args[0])
	}

	inheritClient := topic.client
	title := topic.name

	if len(args) > 1 {
		sub := findHelpSub(topic, args[1])
		if sub == nil {
			return fmt.Errorf("unknown subcommand %q for %s", args[1], topic.name)
		}

		topic = sub
		title += " " + sub.name
	}

	printCommandHelp(w, topic, title, inheritClient)

	return nil
}

// printCommandIndex lists every top-level command and points at per-command help.
func printCommandIndex(w io.Writer) {
	fmt.Fprintln(w, "usage: wt <command> [flags]")
	fmt.Fprintln(w, "       wt                        show the local app, host and config path")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "commands:")

	width := 0
	for _, command := range helpCommands {
		width = max(width, len(command.name))
	}

	for _, command := range helpCommands {
		fmt.Fprintf(w, "  %-*s  %s\n", width, command.name, command.summary)
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "run `wt help <command>` for that command's flags and examples; -h and --help work too")
}

// printCommandHelp prints one command or subcommand.
func printCommandHelp(w io.Writer, command *helpCommand, title string, client bool) {
	fmt.Fprintf(w, "wt %s - %s\n\n", title, command.summary)

	fmt.Fprintln(w, "usage:")

	for _, line := range command.usage {
		fmt.Fprintf(w, "  %s\n", line)
	}

	if len(command.details) > 0 {
		fmt.Fprintln(w)

		for _, line := range command.details {
			fmt.Fprintln(w, line)
		}
	}

	if len(command.flags) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "flags:")
		printHelpFlags(w, command.flags)
	}

	if client {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "global flags (accepted anywhere):")
		printHelpFlags(w, globalHelpFlags)
	}

	if len(command.subs) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "subcommands:")

		width := 0
		for _, sub := range command.subs {
			width = max(width, len(sub.name))
		}

		for _, sub := range command.subs {
			fmt.Fprintf(w, "  %-*s  %s\n", width, sub.name, sub.summary)
		}

		fmt.Fprintf(w, "\nrun `wt help %s <subcommand>` for details\n", command.name)
	}

	if len(command.examples) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "examples:")

		for _, example := range command.examples {
			fmt.Fprintf(w, "  %s\n", example)
		}
	}
}

// printHelpFlags aligns flag names and their descriptions.
func printHelpFlags(w io.Writer, flags []helpFlag) {
	labels := make([]string, len(flags))
	width := 0

	for index, flag := range flags {
		label := flag.name
		if flag.value != "" {
			label += " " + flag.value
		}

		labels[index] = label
		width = max(width, len(label))
	}

	for index, flag := range flags {
		fmt.Fprintf(w, "  %-*s  %s\n", width, labels[index], flag.description)
	}
}
