//go:build tools

// Package tools declares tool dependencies for Dependabot tracking.
package tools

import (
	_ "github.com/rhysd/actionlint/cmd/actionlint"
	_ "golang.org/x/vuln/cmd/govulncheck"
)
