package main

import (
	"flag"
	"io"
)

// newFlagSet returns a command flag set that leaves usage to `wt help` instead of printing Go's default flag listing.
func newFlagSet(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {}

	return flags
}
