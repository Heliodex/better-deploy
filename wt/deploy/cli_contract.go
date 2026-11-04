package main

import (
	"flag"
	"fmt"
)

// runContract regenerates or checks the app's JSON Schema and Go contract from the Valibot registries.
func runContract(args []string) error {
	flags := flag.NewFlagSet("contract", flag.ContinueOnError)

	dir := flags.String("dir", ".", "app directory containing app.json")

	check := flags.Bool("check", false, "verify the generated files without writing them")
	if err := flags.Parse(args); err != nil {
		return err
	}

	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}

	m, err := readManifest(*dir)
	if err != nil {
		return err
	}

	if *check && m.Contract != "valibot" {
		fmt.Println("contract: no generated contract")
		return nil
	}

	if *check {
		if err := checkContract(*dir, webDirName(m), m.Name); err != nil {
			return err
		}

		fmt.Println("contract: up to date")

		return nil
	}

	if err := generateContract(*dir, webDirName(m), m.Name); err != nil {
		return err
	}

	fmt.Println("contract: generated")

	return nil
}
