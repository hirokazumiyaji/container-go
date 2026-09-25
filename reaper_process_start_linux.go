//go:build linux

package container

import (
	"context"
	"os"
	"strconv"
	"strings"
)

func reaperProcessStartTime(ctx context.Context, pid int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", err
	}
	// The command name is parenthesized and may itself contain spaces or
	// parentheses. The final ')' therefore separates the fixed prefix from
	// the space-separated fields beginning with state (field 3).
	closeParen := strings.LastIndexByte(string(data), ')')
	if closeParen < 0 {
		return "", os.ErrInvalid
	}
	fields := strings.Fields(string(data[closeParen+1:]))
	const startTimeIndex = 19 // field 22 overall, after field 3 starts at index 0
	if len(fields) <= startTimeIndex {
		return "", os.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return fields[startTimeIndex], nil
}
