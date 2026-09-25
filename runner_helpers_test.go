package container

import "context"

type genRunner struct {
	*fakeRunner
	inspectJSON string
	deleted     []string
}

func (g *genRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		g.mu.Lock()
		g.calls = append(g.calls, args)
		g.mu.Unlock()
		return []byte(g.inspectJSON), nil, nil
	case "delete", "rm":
		g.mu.Lock()
		g.calls = append(g.calls, args)
		g.deleted = append(g.deleted, args[len(args)-1])
		g.mu.Unlock()
		return nil, nil, nil
	default:
		return g.fakeRunner.Run(ctx, args...)
	}
}
