package ports

import "context"

// Runner executes backend CLI commands.
type Runner interface {
	Run(ctx context.Context, args ...string) (stdout []byte, stderr []byte, err error)
}
