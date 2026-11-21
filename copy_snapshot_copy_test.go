package container

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestCopySnapshotBytesEnforcesLimit(t *testing.T) {
	dir := t.TempDir()
	src, err := os.OpenFile(filepath.Join(dir, "src"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.WriteString("0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	dst, err := os.OpenFile(filepath.Join(dir, "dst"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	n, err := copySnapshotBytes(context.Background(), dst, src, 10)
	if !errors.Is(err, ErrCopySourceTooLarge) {
		t.Fatalf("error = %v, want ErrCopySourceTooLarge", err)
	}
	if n != 11 {
		t.Fatalf("copied %d bytes, want the limit plus one", n)
	}
}

func TestCopySnapshotBytesAcceptsExactLimit(t *testing.T) {
	dir := t.TempDir()
	src, err := os.OpenFile(filepath.Join(dir, "src"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.WriteString("0123456789"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	dst, err := os.OpenFile(filepath.Join(dir, "dst"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	n, err := copySnapshotBytes(context.Background(), dst, src, 10)
	if err != nil {
		t.Fatalf("copySnapshotBytes: %v", err)
	}
	if n != 10 {
		t.Fatalf("copied %d bytes, want 10", n)
	}
}

func TestCopySnapshotBytesHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := copySnapshotBytes(ctx, io.Discard, bytes.NewReader([]byte("secret")), 16)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestCopySnapshotFileBytesHonorsCanceledContext(t *testing.T) {
	dir := t.TempDir()
	src, err := os.OpenFile(filepath.Join(dir, "src"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.WriteString("secret"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	dst, err := os.OpenFile(filepath.Join(dir, "dst"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n, err := copySnapshotFileBytes(ctx, dst, src, 16)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if n != 0 {
		t.Fatalf("copied %d bytes with a canceled context, want 0", n)
	}
}

func TestCopySnapshotBytesWithFilesHonorsCanceledContext(t *testing.T) {
	dir := t.TempDir()
	src, err := os.OpenFile(filepath.Join(dir, "src"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.WriteString("secret"); err != nil {
		t.Fatal(err)
	}
	dst, err := os.OpenFile(filepath.Join(dir, "dst"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := copySnapshotBytes(ctx, dst, src, 16); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestCopySnapshotFileBytesCopiesAtMostRequestedBytes(t *testing.T) {
	dir := t.TempDir()
	src, err := os.OpenFile(filepath.Join(dir, "src"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.WriteString("0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	dst, err := os.OpenFile(filepath.Join(dir, "dst"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	n, err := copySnapshotFileBytes(context.Background(), dst, src, 10)
	if err != nil {
		t.Fatalf("copySnapshotFileBytes: %v", err)
	}
	if n != 10 {
		t.Fatalf("copied %d bytes, want 10", n)
	}
}
