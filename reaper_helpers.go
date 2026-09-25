package container

import (
	"fmt"
	"os"
	"path/filepath"
)

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
	return "", fmt.Errorf("reaper: trusted helper %q is unavailable", name)
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
