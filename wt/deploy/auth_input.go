package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// authInput reads prompts from a terminal or a pipe and hides the token only on a terminal.
type authInput struct {
	reader      *bufio.Reader
	file        *os.File
	errOut      io.Writer
	interactive bool
}

func newAuthInput(in io.Reader, errOut io.Writer) *authInput {
	input := &authInput{reader: bufio.NewReader(in), errOut: errOut}

	if file, ok := in.(*os.File); ok {
		input.file = file
		input.interactive = term.IsTerminal(int(file.Fd()))
	}

	return input
}

// line prints one prompt and returns a trimmed line, falling back to the current value on an empty line.
func (i *authInput) line(prompt, fallback string) (string, error) {
	if fallback != "" {
		fmt.Fprintf(i.errOut, "%s [%s]: ", prompt, fallback)
	} else {
		fmt.Fprintf(i.errOut, "%s: ", prompt)
	}

	line, err := i.reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}

	line = strings.TrimSpace(line)
	if line == "" {
		return fallback, nil
	}

	return line, nil
}

// secret reads a token with hidden input on a terminal and as one line from a pipe.
func (i *authInput) secret(prompt string) (string, error) {
	if !i.interactive || i.file == nil {
		return i.line(prompt, "")
	}

	fmt.Fprint(i.errOut, prompt)

	data, err := term.ReadPassword(int(i.file.Fd()))
	fmt.Fprintln(i.errOut)

	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}

	return strings.TrimSpace(string(data)), nil
}
