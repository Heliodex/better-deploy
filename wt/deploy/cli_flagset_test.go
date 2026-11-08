package main

import (
	"errors"
	"flag"
	"io"
	"testing"
)

func TestNewFlagSetHidesLegacyUsage(t *testing.T) {
	flags := newFlagSet("deploy")
	if flags.Output() != io.Discard {
		t.Errorf("flag output = %v, want io.Discard", flags.Output())
	}

	if err := flags.Parse([]string{"-bogus"}); err == nil {
		t.Error("unknown flag was accepted")
	}

	helpFlags := newFlagSet("deploy")
	if err := helpFlags.Parse([]string{"-help"}); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-help error = %v, want flag.ErrHelp", err)
	}
}
