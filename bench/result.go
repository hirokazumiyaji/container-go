// Package bench holds the testcontainers-go comparison scenarios and
// the shared result schema surface. It is a separate module so the
// testcontainers-go dependency never enters the library's go.mod.
package bench

import (
	"io"

	ibench "github.com/hirokazumiyaji/container-go/internal/bench"
)

// Aliases of the shared measurement schema (internal/bench). The root
// module's counting scenarios record results in the same format.
type (
	// Result is one measured iteration of one scenario.
	Result = ibench.Result
	// Env records the environment a run happened in.
	Env = ibench.Env
	// Doc is a complete recorded run: environment plus results.
	Doc = ibench.Doc
	// Summary aggregates the iterations of one scenario.
	Summary = ibench.Summary
)

// Library names a code path under measurement.
const (
	LibraryContainerGo      = ibench.LibraryContainerGo
	LibraryTestcontainersGo = ibench.LibraryTestcontainersGo
)

// Summarize groups results by (backend, library, image, scenario) and
// aggregates each group's durations.
func Summarize(results []Result) []Summary { return ibench.Summarize(results) }

// Table renders summaries as a fixed-width human-readable table.
func Table(summaries []Summary) string { return ibench.Table(summaries) }

// WriteJSON writes the doc as formatted JSON.
func WriteJSON(w io.Writer, d Doc) error { return d.WriteJSON(w) }

// ParseDoc reads a doc written by WriteJSON.
func ParseDoc(data []byte) (Doc, error) { return ibench.ParseDoc(data) }
