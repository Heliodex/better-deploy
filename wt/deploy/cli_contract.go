package main

import "fmt"

// runContract regenerates or checks the app's JSON Schema and Go contract from the Valibot registries.
func runContract(args []string) error {
	flags := newFlagSet("contract")

	dir := flags.String("dir", ".", "app directory containing app.json")

	check := flags.Bool("check", false, "verify the generated files without writing them")
	compat := flags.Bool("compat", false, "report breaking schema changes against the contract source at a git ref without writing them")
	compatRef := flags.String("compat-ref", "HEAD", "git ref the compat check compares against")

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

	if (*check || *compat) && m.Contract != "valibot" {
		fmt.Println("contract: no generated contract")
		return nil
	}

	if *check {
		if err := checkContract(*dir, webDirName(m), m.Name); err != nil {
			return err
		}

		fmt.Println("contract: up to date")
	}

	if *compat {
		return runCompat(*dir, webDirName(m), m.Name, *compatRef)
	}

	if *check {
		return nil
	}

	if err := generateContract(*dir, webDirName(m), m.Name); err != nil {
		return err
	}

	fmt.Println("contract: generated")

	return nil
}
