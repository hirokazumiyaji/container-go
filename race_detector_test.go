//go:build race

package container

// raceDetectorEnabled lets timing-based tests skip under -race, where
// spawning a helper process per entry is much slower and the timing margin
// says nothing about the behavior under test.
const raceDetectorEnabled = true
