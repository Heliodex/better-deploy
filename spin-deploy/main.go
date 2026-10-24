// Command spin-deploy uploads Spin applications to a spin supervisor.
package main

import (
	"fmt"
	"os"
)

const version = "0.0.0"

func usage() {
	fmt.Fprintf(os.Stderr, `spin-deploy deploys Spin applications to a spin supervisor.

Usage:
  spin-deploy <command> [flags]

Commands:
  deploy   build the app and upload it to the supervisor
  version  print the version and exit
  help     print this help and exit

Run 'spin-deploy deploy --help' for deploy flags.
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "deploy":
		if err := runDeploy(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "spin-deploy: %v\n", err)
			os.Exit(1)
		}
	case "version", "--version", "-v":
		fmt.Println("spin-deploy " + version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "spin-deploy: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}
