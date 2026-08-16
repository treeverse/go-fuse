// Copyright 2026 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux

package fs

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/go-fuse/v2/internal/testutil"
	"github.com/hanwen/go-fuse/v2/splice"
	"golang.org/x/sys/unix"
)

// spliceMoveNode splices reads out of a backing file, asking the kernel to move
// the pages instead of copying them.
type spliceMoveNode struct {
	Inode
	fd      uintptr
	promise int64 // size told to the kernel; past EOF it forces the short-read fixup
	flags   int
}

func (n *spliceMoveNode) Open(ctx context.Context, flags uint32) (FileHandle, uint32, syscall.Errno) {
	return nil, fuse.FOPEN_KEEP_CACHE, 0
}

func (n *spliceMoveNode) Getattr(ctx context.Context, fh FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = 0444
	out.Size = uint64(n.promise)
	return 0
}

func (n *spliceMoveNode) Read(ctx context.Context, fh FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if off >= n.promise {
		return fuse.ReadResultData(nil), 0
	}
	total := int(min(off+int64(len(dest)), n.promise) - off)

	pair, err := splice.Get()
	if err != nil {
		return nil, syscall.EIO
	}
	if err := pair.Grow(total); err != nil {
		splice.Done(pair)
		return nil, syscall.EIO
	}
	if _, err := pair.LoadFromAt(n.fd, total, off); err != nil {
		splice.Done(pair)
		return nil, syscall.EIO
	}
	return fuse.ReadResultPipeFlags(pair, total, n.flags), 0
}

// readThroughMount serves f from a mounted filesystem and reads it back.
func readThroughMount(t *testing.T, f *os.File, promise int64, flags int) []byte {
	t.Helper()

	root := &Inode{}
	node := &spliceMoveNode{fd: f.Fd(), promise: promise, flags: flags}
	sec := time.Second
	opts := &Options{
		FirstAutomaticIno: 1,
		EntryTimeout:      &sec,
		AttrTimeout:       &sec,
		OnAdd: func(ctx context.Context) {
			n := root.EmbeddedInode()
			n.AddChild("file", n.NewPersistentInode(ctx, node, StableAttr{}), false)
		},
	}
	opts.Debug = testutil.VerboseTest()

	mnt := t.TempDir()
	server, err := Mount(mnt, root, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Unmount()

	got, err := os.ReadFile(mnt + "/file")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestReadResultPipeSpliceMove reads a file served with SPLICE_F_MOVE. A moved
// page leaves the backing file's cache, so a broken move corrupts or loses the
// payload instead of returning an error.
func TestReadResultPipeSpliceMove(t *testing.T) {
	// Page aligned, or a promise past EOF leaves the kernel a partial page to
	// zero-fill and the read comes back longer than the file. Not a multiple of
	// the 128 KiB read window, so that promise makes the last read straddle EOF.
	const size = 1<<20 - 4096

	// Each 8 bytes hold their own offset, so shifted or duplicated data fails.
	want := make([]byte, size)
	for i := 0; i < len(want); i += 8 {
		binary.LittleEndian.PutUint64(want[i:], uint64(i))
	}

	for _, tc := range []struct {
		name    string
		promise int64
	}{
		{"exact", size},
		{"short read", size + 4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backing := t.TempDir() + "/backing"
			if err := os.WriteFile(backing, want, 0644); err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(backing)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()

			if got := readThroughMount(t, f, tc.promise, unix.SPLICE_F_MOVE); !bytes.Equal(got, want) {
				t.Errorf("read %d bytes, want %d", len(got), len(want))
			}
		})
	}
}

// TestSpliceMoveDropsBackingPages checks what the flag is for: a moved page
// leaves the backing file's page cache rather than being copied into the fuse
// inode's, so the same bytes are not held twice.
//
// Stealing needs the filesystem to hand splice a whole single page it can
// release, which is why this runs on ext4 only: xfs and tmpfs never do, and the
// kernel then copies and reports no error.
func TestSpliceMoveDropsBackingPages(t *testing.T) {
	const size = 8 << 20

	dir := t.TempDir()
	var stat unix.Statfs_t
	if err := unix.Statfs(dir, &stat); err != nil {
		t.Fatal(err)
	}
	if stat.Type != unix.EXT4_SUPER_MAGIC {
		t.Skipf("TMPDIR is on filesystem %#x, whose pages the kernel will not steal; ext4 only", stat.Type)
	}

	cached := backingPagesAfterRead(t, dir, size, 0)
	if cached < size-size/10 {
		t.Skipf("copied read left %d of %d bytes cached; nothing to measure", cached, size)
	}

	moved := backingPagesAfterRead(t, dir, size, unix.SPLICE_F_MOVE)
	if moved > size/10 {
		t.Errorf("moved read left %d of %d bytes in the backing file's cache, want none (copy left %d). "+
			"A kernel serving large folios hands splice buffers bigger than a page and moves nothing",
			moved, size, cached)
	}
}

// backingPagesAfterRead serves a file of size bytes with flags, reads it through
// the mount, and reports how much of the backing file is still in the page cache.
func backingPagesAfterRead(t *testing.T, dir string, size int64, flags int) int64 {
	t.Helper()

	path := dir + "/backing"
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		f.Close()
		os.Remove(path)
	}()

	// Real bytes, not a hole: a hole is not backed by pages everywhere, and then
	// there is nothing to steal and nothing to count.
	buf := make([]byte, 1<<20)
	for off := int64(0); off < size; off += int64(len(buf)) {
		if _, err := f.WriteAt(buf, off); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}

	if n := int64(len(readThroughMount(t, f, size, flags))); n != size {
		t.Fatalf("read %d bytes, want %d", n, size)
	}
	return residentBytes(t, f, size)
}

// residentBytes is how much of f is in the page cache, from mincore(2).
func residentBytes(t *testing.T, f *os.File, size int64) int64 {
	t.Helper()

	data, err := unix.Mmap(int(f.Fd()), 0, int(size), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Munmap(data)

	pageSize := os.Getpagesize()
	vec := make([]byte, (len(data)+pageSize-1)/pageSize)
	if _, _, errno := unix.Syscall(unix.SYS_MINCORE, uintptr(unsafe.Pointer(&data[0])),
		uintptr(len(data)), uintptr(unsafe.Pointer(&vec[0]))); errno != 0 {
		t.Fatal(errno)
	}
	var resident int64
	for _, v := range vec {
		resident += int64(v&1) * int64(pageSize)
	}
	return resident
}
