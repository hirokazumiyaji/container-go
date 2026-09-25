package container

import (
	"errors"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// cliErrorLinePrefixes are wrappers emitted by the supported CLIs around
// their structured error messages. Removing only these prefixes lets the
// backend matchers require an error kind to start a stderr line, rather
// than finding the same words in an application or configuration message.
var cliErrorLinePrefixes = []string{
	"docker: ",
	"container: ",
	"error response from daemon: ",
	"error: ",
}

func cliErrorLines(err error) ([]string, bool) {
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) {
		return nil, false
	}

	var lines []string
	for _, line := range strings.Split(cliErr.Stderr, "\n") {
		line = normalizeCLIErrorLine(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, true
}

func normalizeCLIErrorLine(line string) string {
	line = strings.ToLower(strings.TrimSpace(line))
	for {
		changed := false
		for _, prefix := range cliErrorLinePrefixes {
			if strings.HasPrefix(line, prefix) {
				line = strings.TrimSpace(strings.TrimPrefix(line, prefix))
				changed = true
				break
			}
		}
		if !changed {
			return line
		}
	}
}

func hasCLIErrorLine(err error, match func(string) bool) bool {
	lines, ok := cliErrorLines(err)
	if !ok {
		return false
	}
	for _, line := range lines {
		if match(line) {
			return true
		}
	}
	return false
}

func hasCLIErrorPrefix(err error, prefixes ...string) bool {
	return hasCLIErrorLine(err, func(line string) bool {
		for _, prefix := range prefixes {
			if strings.HasPrefix(line, prefix) {
				return true
			}
		}
		return false
	})
}
