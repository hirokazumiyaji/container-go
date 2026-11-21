//go:build linux || freebsd

package container

// copyReadFromSupported reports whether os.File.ReadFrom can hand two file
// descriptors to the kernel for a copy_file_range transfer. Other platforms
// fall back to the bounded pooled-buffer loop instead.
const copyReadFromSupported = true
