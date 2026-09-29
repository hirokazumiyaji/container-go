package container

import (
	"errors"
	"strings"
	"testing"
)

func TestReaperIDValidationPortable(t *testing.T) {
	fullDockerID := strings.Repeat("ab", 32)
	if immutable, err := validateReaperEntry("rm", fullDockerID, ""); err != nil || !immutable {
		t.Fatalf("full Docker ID validation = (%v, %v), want accepted immutable ID", immutable, err)
	}
	if immutable, err := validateReaperEntry("rm", fullDockerID, "not-a-generation"); err != nil || !immutable {
		t.Fatalf("full Docker ID generation normalization = (%v, %v), want accepted immutable ID", immutable, err)
	}
	if _, err := validateReaperEntry("rm", "ctr", ""); err == nil || !strings.Contains(err.Error(), "no generation") {
		t.Fatalf("generationless Docker name error = %v, want explicit rejection", err)
	}
	if _, err := validateReaperEntry("rm", "ctr", "0123456789abcdef"); err != nil {
		t.Fatalf("generation-bound Docker name validation: %v", err)
	}
	for _, id := range []string{"", strings.Repeat("A", 64), strings.Repeat("a", 63) + "g", strings.Repeat("a", 64) + "0"} {
		if _, err := validateReaperEntry("rm", id, "0123456789abcdef"); err == nil {
			t.Errorf("malformed Docker identity %q was accepted", id)
		}
	}
	for _, id := range []string{"", "bad id", "a;b", "x\ny", "-leading", strings.Repeat("A", 64), strings.Repeat("g", 64), strings.Repeat("a", 65)} {
		if _, err := validateReaperEntry("delete", id, ""); err == nil {
			t.Errorf("delete entry %q was accepted", id)
		}
	}
	if _, err := validateReaperEntry("delete", "ctr", "not-hex"); err == nil {
		t.Error("invalid creation was accepted")
	}
}

func TestReaperScriptValidationIsPortable(t *testing.T) {
	if !strings.Contains(reaperScript, `timeout="${4:-30}"`) ||
		!strings.Contains(reaperScript, `"$sleep_bin" "$wait_seconds"`) ||
		!strings.Contains(reaperScript, "kill -9") {
		t.Error("reaper script must bound each backend call with a pinned sleep/kill helper")
	}
	if !strings.Contains(reaperScript, `run_with_timeout "$timeout" process_entry`) {
		t.Error("reaper must put the complete entry pipeline behind its timeout")
	}
	for _, required := range []string{
		"valid_docker_id \"$uid\"",
		"\"$pgrep_bin\" -P \"$parent\"",
		"pgrep_disabled=1",
		"\"$awk_bin\" -v key=",
		"\"$ps_bin\" -o pid= -o \"$ps_start_field=\"",
		"ps_start_field=\"${11:-lstart}\"",
		"CONTAINERGO_REAPER_DISABLE_MONITOR",
		"kill_helper_descendants",
		"capture_quiesced_process_table",
		"\"$rm_bin\" -f",
		"work_dir=\"$5\"",
		"ulimit -f",
		"snapshot_branch",
		"revalidate_identity",
		"tombstoned_pids",
		"kill -9 \"$snapshot_pid\"",
		"max_descendant_lookups",
		"cleanup_helper_budget",
		"cleanup_enumeration_budget",
		"consume_cleanup_budget",
		"kill_stopped_processes",
	} {
		if !strings.Contains(reaperScript, required) {
			t.Errorf("reaper script missing bounded cleanup fragment %q", required)
		}
	}
	for _, forbidden := range []string{"sed -n", "head -n", "tail -n", "grep -q", "mktemp", "stime"} {
		if strings.Contains(reaperScript, forbidden) {
			t.Errorf("reaper script still uses unbounded or CPU-time helper %q", forbidden)
		}
	}
	if !strings.Contains(reaperScript, `if [ "$registered_entries" -lt "$max_registered_entries" ]; then`) {
		t.Error("registration input must drain and discard records beyond the bounded prefix")
	}
}

func TestReaperHelperValidationReportsMissingDependency(t *testing.T) {
	if err := (reaperHelperPaths{awk: "relative-awk"}).validate(); !errors.Is(err, errReaperHelperUnavailable) {
		t.Fatalf("helper validation error = %v, want unavailable dependency", err)
	}
}

func TestBreQuoteEscapesLabelKey(t *testing.T) {
	if got := breQuote("com.github.x-y"); got != `com\.github\.x-y` {
		t.Errorf("breQuote = %q", got)
	}
}
