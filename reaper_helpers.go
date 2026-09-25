package container

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var errReaperHelperUnavailable = errors.New("reaper helper unavailable")

type reaperHelperPaths struct {
	awk   string
	pgrep string
	ps    string
	rm    string
	sleep string
}

func (p reaperHelperPaths) complete() bool {
	return p.awk != "" && p.ps != "" && p.rm != "" && p.sleep != ""
}

func (p reaperHelperPaths) validate() error {
	for name, path := range map[string]string{
		"awk": p.awk, "ps": p.ps, "rm": p.rm, "sleep": p.sleep,
	} {
		if path == "" || !filepath.IsAbs(path) {
			return fmt.Errorf("%w: %s", errReaperHelperUnavailable, name)
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("%w: %s (%q)", errReaperHelperUnavailable, name, path)
		}
	}
	if p.pgrep != "" {
		if !filepath.IsAbs(p.pgrep) {
			return fmt.Errorf("%w: pgrep (%q)", errReaperHelperUnavailable, p.pgrep)
		}
		info, err := os.Stat(p.pgrep)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("%w: pgrep (%q)", errReaperHelperUnavailable, p.pgrep)
		}
	}
	return nil
}

func trustedReaperHelperPath(name string) (string, error) {
	for _, dir := range []string{"/usr/bin", "/bin"} {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		switch filepath.Dir(resolved) {
		case "/usr/bin", "/bin", "/usr/sbin", "/sbin":
			return path, nil
		}
	}
	return "", fmt.Errorf("%w: trusted helper %q", errReaperHelperUnavailable, name)
}

func trustedReaperHelpers() (reaperHelperPaths, error) {
	var paths reaperHelperPaths
	var err error
	if paths.awk, err = trustedReaperHelperPath("awk"); err != nil {
		return reaperHelperPaths{}, err
	}
	if path, pathErr := trustedReaperHelperPath("pgrep"); pathErr == nil {
		paths.pgrep = path
	}
	if paths.ps, err = trustedReaperHelperPath("ps"); err != nil {
		return reaperHelperPaths{}, err
	}
	if paths.rm, err = trustedReaperHelperPath("rm"); err != nil {
		return reaperHelperPaths{}, err
	}
	if paths.sleep, err = trustedReaperHelperPath("sleep"); err != nil {
		return reaperHelperPaths{}, err
	}
	return paths, nil
}
