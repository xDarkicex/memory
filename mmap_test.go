package memory

import (
	"os"
	"testing"
)

// tempFile creates a temp file with the given content, returns the open file
// and its path. The caller must close and remove it.
func tempFile(t *testing.T, content []byte) *os.File {
	t.Helper()
	dir := t.TempDir()
	f, err := os.CreateTemp(dir, "mmap_test")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if len(content) > 0 {
		if _, err := f.Write(content); err != nil {
			f.Close()
			t.Fatalf("Write: %v", err)
		}
	}
	// Sync to ensure content is on disk before mmap.
	if err := f.Sync(); err != nil {
		f.Close()
		t.Fatalf("Sync: %v", err)
	}
	t.Cleanup(func() {
		f.Close()
	})
	return f
}

func TestMmapFileReadOnly(t *testing.T) {
	content := []byte("hello, mmap read-only world!")
	f := tempFile(t, content)

	data, err := MmapFileReadOnly(int(f.Fd()), 0, len(content))
	if err != nil {
		t.Fatalf("MmapFileReadOnly: %v", err)
	}
	if len(data) != len(content) {
		t.Fatalf("len(data)=%d, want %d", len(data), len(content))
	}
	for i := range content {
		if data[i] != content[i] {
			t.Fatalf("data[%d]=%d, want %d", i, data[i], content[i])
		}
	}
	// Read-only: writing should fault. We don't test the fault itself;
	// verifying read correctness is sufficient.

	if err := Munmap(data); err != nil {
		t.Fatalf("Munmap: %v", err)
	}
}

func TestMmapFileReadOnly_Offset(t *testing.T) {
	content := []byte("0123456789abcdef")
	f := tempFile(t, content)

	offset := 4
	want := content[offset:]

	data, err := MmapFileReadOnly(int(f.Fd()), int64(offset), len(want))
	if err != nil {
		t.Fatalf("MmapFileReadOnly(offset=%d): %v", offset, err)
	}
	if len(data) != len(want) {
		t.Fatalf("len(data)=%d, want %d", len(data), len(want))
	}
	for i := range want {
		if data[i] != want[i] {
			t.Fatalf("data[%d]=%d, want %d", i, data[i], want[i])
		}
	}

	if err := Munmap(data); err != nil {
		t.Fatalf("Munmap: %v", err)
	}
}

func TestMmapFileReadOnly_PartialSize(t *testing.T) {
	content := []byte("0123456789abcdef")
	f := tempFile(t, content)

	size := 6
	want := content[:size]

	data, err := MmapFileReadOnly(int(f.Fd()), 0, size)
	if err != nil {
		t.Fatalf("MmapFileReadOnly(size=%d): %v", size, err)
	}
	if len(data) != size {
		t.Fatalf("len(data)=%d, want %d", len(data), size)
	}
	for i := range want {
		if data[i] != want[i] {
			t.Fatalf("data[%d]=%d, want %d", i, data[i], want[i])
		}
	}

	if err := Munmap(data); err != nil {
		t.Fatalf("Munmap: %v", err)
	}
}

func TestMmapFile_Writable(t *testing.T) {
	content := make([]byte, 64)
	for i := range content {
		content[i] = byte(i)
	}
	f := tempFile(t, content)

	data, err := MmapFile(int(f.Fd()), 0, len(content), true)
	if err != nil {
		t.Fatalf("MmapFile(writable=true): %v", err)
	}
	defer Munmap(data)

	// Verify initial content matches.
	for i := range content {
		if data[i] != content[i] {
			t.Fatalf("data[%d]=%d, want %d", i, data[i], content[i])
		}
	}

	// Write through the mapping.
	const newVal byte = 0xAA
	for i := range data {
		data[i] = newVal
	}

	// Flush to ensure write is visible through the file descriptor.
	if err := f.Sync(); err != nil {
		t.Fatalf("Sync after write: %v", err)
	}

	// Read back via the file to verify persistence.
	buf := make([]byte, len(content))
	if _, err := f.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	for i := range buf {
		if buf[i] != newVal {
			t.Fatalf("buf[%d]=%d, want %d (write not persisted)", i, buf[i], newVal)
		}
	}
}

func TestMmapFile_Writable_Offset(t *testing.T) {
	content := make([]byte, 128)
	for i := range content {
		content[i] = byte(i & 0xFF)
	}
	f := tempFile(t, content)

	offset := int64(16)
	size := 48
	want := content[offset : offset+int64(size)]

	data, err := MmapFile(int(f.Fd()), offset, size, true)
	if err != nil {
		t.Fatalf("MmapFile(writable, offset=%d): %v", offset, err)
	}
	defer Munmap(data)

	// Verify initial content at offset.
	for i := range want {
		if data[i] != want[i] {
			t.Fatalf("data[%d]=%d, want %d", i, data[i], want[i])
		}
	}

	// Write through the mapped region.
	const newVal byte = 0xBB
	for i := range data {
		data[i] = newVal
	}
	f.Sync()

	// The region before the offset should be untouched.
	buf := make([]byte, len(content))
	f.ReadAt(buf, 0)
	for i := int64(0); i < offset; i++ {
		if buf[i] != content[i] {
			t.Fatalf("byte before offset altered: buf[%d]=%d, want %d", i, buf[i], content[i])
		}
	}
	// The mapped region should have the new value.
	for i := offset; i < offset+int64(size); i++ {
		if buf[i] != newVal {
			t.Fatalf("mapped region buf[%d]=%d, want %d", i, buf[i], newVal)
		}
	}
	// The region after the mapped region should be untouched.
	for i := offset + int64(size); i < int64(len(content)); i++ {
		if buf[i] != content[i] {
			t.Fatalf("byte after offset altered: buf[%d]=%d, want %d", i, buf[i], content[i])
		}
	}
}

func TestMmapFile_NonWritable_MatchesReadOnly(t *testing.T) {
	content := []byte("non-writable check data")
	f := tempFile(t, content)

	data, err := MmapFile(int(f.Fd()), 0, len(content), false)
	if err != nil {
		t.Fatalf("MmapFile(writable=false): %v", err)
	}
	defer Munmap(data)

	if len(data) != len(content) {
		t.Fatalf("len(data)=%d, want %d", len(data), len(content))
	}
	for i := range content {
		if data[i] != content[i] {
			t.Fatalf("data[%d]=%d, want %d", i, data[i], content[i])
		}
	}
}

func TestMmapFile_EmptyFile(t *testing.T) {
	f := tempFile(t, nil)

	// Mapping size 0 from an empty file.
	data, err := MmapFileReadOnly(int(f.Fd()), 0, 0)
	if err != nil {
		t.Fatalf("MmapFileReadOnly(empty, size=0): %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("len(data)=%d, want 0", len(data))
	}
	if err := Munmap(data); err != nil {
		t.Fatalf("Munmap: %v", err)
	}
}

func TestMmapFile_LargeFile(t *testing.T) {
	// Allocate a file larger than a typical page (e.g., 3 pages).
	pageSize := PageSize
	if pageSize == 0 {
		pageSize = 4096
	}
	fileSize := 3 * pageSize
	content := make([]byte, fileSize)
	for i := range content {
		content[i] = byte(i % 251)
	}
	f := tempFile(t, content)

	data, err := MmapFileReadOnly(int(f.Fd()), 0, fileSize)
	if err != nil {
		t.Fatalf("MmapFileReadOnly(large): %v", err)
	}
	defer Munmap(data)

	// Spot-check a few locations: start, middle, end, page boundaries.
	check := func(offset int) {
		if data[offset] != content[offset] {
			t.Fatalf("data[%d]=%d, want %d", offset, data[offset], content[offset])
		}
	}
	check(0)               // first byte
	check(fileSize - 1)    // last byte
	check(pageSize)        // page boundary
	check(pageSize * 2)    // page boundary
	check(pageSize - 1)    // byte before page boundary
	check(fileSize/2)      // middle
}

func TestMmapFile_InvalidFd(t *testing.T) {
	_, err := MmapFileReadOnly(-1, 0, 4096)
	if err == nil {
		t.Fatal("MmapFileReadOnly with invalid fd should error")
	}
}

func TestMmapFile_ZeroSize_NegativeOffset(t *testing.T) {
	content := []byte("some data for testing")
	f := tempFile(t, content)

	// Negative offset should fail.
	_, err := MmapFileReadOnly(int(f.Fd()), -1, 4)
	if err == nil {
		t.Fatal("MmapFileReadOnly with negative offset should error")
	}
}

func TestMunmap_NilSlice(t *testing.T) {
	// Munmap on nil should return an error or be safe (implementation-dependent).
	// We just verify it doesn't panic.
	_ = Munmap(nil)
}

func TestMunmap_EmptySlice(t *testing.T) {
	_ = Munmap([]byte{})
}

func TestMunmap_DoubleUnmap(t *testing.T) {
	content := []byte("double unmap test data")
	f := tempFile(t, content)

	data, err := MmapFileReadOnly(int(f.Fd()), 0, len(content))
	if err != nil {
		t.Fatalf("MmapFileReadOnly: %v", err)
	}

	if err := Munmap(data); err != nil {
		t.Fatalf("first Munmap: %v", err)
	}
	// Second Munmap on already-unmapped region should return an error.
	if err := Munmap(data); err == nil {
		t.Fatal("second Munmap on freed mapping should error")
	}
}

func TestMmapFile_MultipleMaps(t *testing.T) {
	content := []byte("multiple maps from same fd")
	f := tempFile(t, content)

	a, err := MmapFileReadOnly(int(f.Fd()), 0, len(content))
	if err != nil {
		t.Fatalf("first MmapFileReadOnly: %v", err)
	}
	defer Munmap(a)

	b, err := MmapFileReadOnly(int(f.Fd()), 0, len(content))
	if err != nil {
		t.Fatalf("second MmapFileReadOnly: %v", err)
	}
	defer Munmap(b)

	// Both should see the same data.
	for i := range content {
		if a[i] != content[i] || b[i] != content[i] {
			t.Fatalf("mismatch at offset %d: a=%d b=%d want=%d", i, a[i], b[i], content[i])
		}
	}
	// The pointers should be different (different mappings).
	if &a[0] == &b[0] {
		t.Fatal("two mappings unexpectedly share the same address")
	}
}

func TestMmapFile_Writable_MapBeyondFileEndFails(t *testing.T) {
	content := []byte("short file")
	f := tempFile(t, content)

	// Map more bytes than the file contains. This should fail.
	_, err := MmapFileReadOnly(int(f.Fd()), 0, 4096)
	if err == nil {
		t.Fatal("MmapFileReadOnly beyond file end should error")
	}
}

func TestMmapFile_FullMmapUnmapRoundTrip(t *testing.T) {
	// Verify that after Munmap the memory is truly released. We do this
	// by mapping and unmapping in a loop; a leak would eventually surface.
	f := tempFile(t, make([]byte, pageSize()))
	const iterations = 100
	for i := 0; i < iterations; i++ {
		data, err := MmapFileReadOnly(int(f.Fd()), 0, pageSize())
		if err != nil {
			t.Fatalf("iteration %d MmapFileReadOnly: %v", i, err)
		}
		// Touch every page to ensure they're faulted in.
		for offset := 0; offset < pageSize(); offset += pageSize() {
			_ = data[offset]
		}
		if err := Munmap(data); err != nil {
			t.Fatalf("iteration %d Munmap: %v", i, err)
		}
	}
}

func TestMmapFile_ZeroSizeMap(t *testing.T) {
	f := tempFile(t, []byte("x"))

	data, err := MmapFile(int(f.Fd()), 0, 0, true)
	if err != nil {
		t.Fatalf("MmapFile(writable, size=0): %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("len(data)=%d, want 0", len(data))
	}
	_ = Munmap(data)

	// Same for read-only path.
	data, err = MmapFileReadOnly(int(f.Fd()), 0, 0)
	if err != nil {
		t.Fatalf("MmapFileReadOnly(size=0): %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("len(data)=%d, want 0", len(data))
	}
	_ = Munmap(data)
}

func TestMmapFile_LargePageSizeFile(t *testing.T) {
	// Use a file whose size is not an exact multiple of the page size.
	ps := pageSize()
	if ps == 0 {
		ps = 4096
	}
	fileSize := ps + ps/2
	content := make([]byte, fileSize)
	for i := range content {
		content[i] = byte(i & 0xFF)
	}
	f := tempFile(t, content)

	data, err := MmapFileReadOnly(int(f.Fd()), 0, fileSize)
	if err != nil {
		t.Fatalf("MmapFileReadOnly(non-page-aligned size): %v", err)
	}
	defer Munmap(data)

	for i := 0; i < fileSize; i++ {
		if data[i] != content[i] {
			t.Fatalf("data[%d]=%d, want %d", i, data[i], content[i])
		}
	}
}

func TestMmapFile_Writable_SharedMapping(t *testing.T) {
	// Verify MAP_SHARED semantics: writes through the mapping are
	// visible to other file descriptors after an fsync.
	f := tempFile(t, make([]byte, 32))

	// Open a second fd to the same file for cross-reader verification.
	f2, err := os.Open(f.Name())
	if err != nil {
		t.Fatalf("Open second fd: %v", err)
	}
	defer f2.Close()

	data, err := MmapFile(int(f.Fd()), 0, 32, true)
	if err != nil {
		t.Fatalf("MmapFile(writable): %v", err)
	}
	defer Munmap(data)

	const written byte = 0xCC
	for i := range data {
		data[i] = written
	}
	// fsync forces the mapping writes to stable storage so the
	// second fd sees them.  On Unix this is equivalent to msync+fsync;
	// on Windows FlushViewOfFile is implicit in the mapping close,
	// but CreateFileMapping is FILE_MAP_WRITE so the page cache
	// already has the dirty data.
	if err := f.Sync(); err != nil {
		t.Fatalf("Sync after mmap write: %v", err)
	}

	buf := make([]byte, 32)
	if _, err := f2.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt via second fd: %v", err)
	}
	for i := range buf {
		if buf[i] != written {
			t.Fatalf("buf[%d]=%d, want %d (shared mapping write not visible)", i, buf[i], written)
		}
	}
}

// pageSize returns the system page size, falling back to 4096.
func pageSize() int {
	ps := PageSize
	if ps == 0 {
		return 4096
	}
	return ps
}
