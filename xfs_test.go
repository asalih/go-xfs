package xfs

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"
	"time"
)

func TestReadDir_ExistingImages(t *testing.T) {
	images := []string{
		"cmd/testdata/image.xfs",
		"cmd/testdata/image40.xfs",
	}

	for _, img := range images {
		t.Run(img, func(t *testing.T) {
			f, err := os.Open(img)
			if err != nil {
				t.Skipf("test image not available: %v", err)
			}
			defer f.Close()

			fs, err := NewFS(f)
			if err != nil {
				t.Fatalf("NewFS: %v", err)
			}

			entries, err := fs.ReadDir("/")
			if err != nil {
				t.Fatalf("ReadDir(/): %v", err)
			}
			if len(entries) == 0 {
				t.Fatal("expected at least one entry in root directory")
			}
		})
	}
}

func TestNrext64_ExtentCountExtraction(t *testing.T) {
	// Build a minimal 512-byte inode buffer with NREXT64 layout.
	// We only need the InodeCore (176 bytes) to be valid; the rest
	// is the data fork which we don't parse in this test.
	buf := make([]byte, 512)

	// offset 0: magic = XFS_DINODE_MAGIC
	binary.BigEndian.PutUint16(buf[0:2], XFS_DINODE_MAGIC)
	// offset 2: mode = S_IFDIR (0x4000) | 0755
	binary.BigEndian.PutUint16(buf[2:4], 0x41ed)
	// offset 4: version = 3
	buf[4] = 3
	// offset 5: format = XFS_DINODE_FMT_EXTENTS
	buf[5] = XFS_DINODE_FMT_EXTENTS

	// offset 24: di_big_nextents = 42 (the real data extent count under NREXT64)
	binary.BigEndian.PutUint64(buf[24:32], 42)

	// offset 76: di_big_anextents = 7 (attr extent count under NREXT64)
	binary.BigEndian.PutUint32(buf[76:80], 7)

	// offset 120: Flags2 with XFS_DIFLAG2_NREXT64 set
	binary.BigEndian.PutUint64(buf[120:128], XFS_DIFLAG2_NREXT64)

	// Parse InodeCore and apply NREXT64 fixup (same logic as ReadInode)
	var ic InodeCore
	r := newBigEndianReader(buf)
	if err := binary.Read(r, binary.BigEndian, &ic); err != nil {
		t.Fatalf("binary.Read InodeCore: %v", err)
	}

	if ic.Magic != XFS_DINODE_MAGIC {
		t.Fatalf("magic mismatch: got 0x%x", ic.Magic)
	}
	if ic.Version != 3 {
		t.Fatalf("version mismatch: got %d", ic.Version)
	}

	// Before fixup, Nextents reads di_big_anextents (7) and
	// Anextents reads di_nrext64_pad (0) due to field reuse.
	if ic.Nextents != 7 {
		t.Fatalf("before fixup: expected Nextents=7 (big_anextents), got %d", ic.Nextents)
	}

	// Apply the NREXT64 fixup
	if ic.Flags2&XFS_DIFLAG2_NREXT64 != 0 {
		ic.Nextents = uint32(binary.BigEndian.Uint64(buf[24:32]))
		ic.Anextents = uint16(binary.BigEndian.Uint32(buf[76:80]))
	}

	if ic.Nextents != 42 {
		t.Errorf("after fixup: expected Nextents=42, got %d", ic.Nextents)
	}
	if ic.Anextents != 7 {
		t.Errorf("after fixup: expected Anextents=7, got %d", ic.Anextents)
	}
}

func TestSuperBlock_HasNrext64(t *testing.T) {
	sb := SuperBlock{}
	if sb.HasNrext64() {
		t.Error("expected HasNrext64()=false for zero FeaturesIncompat")
	}

	sb.FeaturesIncompat = XFS_SB_FEAT_INCOMPAT_NREXT64
	if !sb.HasNrext64() {
		t.Error("expected HasNrext64()=true when NREXT64 bit is set")
	}

	sb.FeaturesIncompat = 0x23 // NREXT64 (0x20) | FTYPE (0x01) | SPINODES (0x02)
	if !sb.HasNrext64() {
		t.Error("expected HasNrext64()=true with multiple bits set")
	}
}

func TestBigtime_TimestampDecoding(t *testing.T) {
	// Known reference: 2024-01-15 10:30:00 UTC
	refTime := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)

	t.Run("classic encoding", func(t *testing.T) {
		sec := uint64(refTime.Unix())
		nsec := uint64(refTime.Nanosecond())
		raw := (sec << 32) | nsec
		got := xfsTimestamp(raw, false)
		if !got.Equal(refTime) {
			t.Errorf("classic: expected %v, got %v", refTime, got)
		}
	})

	t.Run("bigtime encoding", func(t *testing.T) {
		sec := refTime.Unix()
		nsec := int64(refTime.Nanosecond())
		raw := uint64((sec+XFS_BIGTIME_EPOCH_OFFSET)*1_000_000_000 + nsec)
		got := xfsTimestamp(raw, true)
		if !got.Equal(refTime) {
			t.Errorf("bigtime: expected %v, got %v", refTime, got)
		}
	})

	t.Run("bigtime epoch boundary", func(t *testing.T) {
		// ts=0 in bigtime should decode to the bigtime epoch (Dec 13, 1901)
		got := xfsTimestamp(0, true)
		expected := time.Unix(-XFS_BIGTIME_EPOCH_OFFSET, 0)
		if !got.Equal(expected) {
			t.Errorf("bigtime epoch: expected %v, got %v", expected, got)
		}
	})
}

func TestInodeCore_IsBigtime(t *testing.T) {
	ic := InodeCore{}
	if ic.isBigtime() {
		t.Error("expected isBigtime()=false for zero Flags2")
	}

	ic.Flags2 = XFS_DIFLAG2_BIGTIME
	if !ic.isBigtime() {
		t.Error("expected isBigtime()=true when BIGTIME bit is set")
	}

	ic.Flags2 = XFS_DIFLAG2_BIGTIME | XFS_DIFLAG2_NREXT64
	if !ic.isBigtime() {
		t.Error("expected isBigtime()=true with multiple flags set")
	}
}

func TestDir2SfHdr_I8CountParent(t *testing.T) {
	t.Run("4-byte parent (i8count=0)", func(t *testing.T) {
		var buf bytes.Buffer
		buf.WriteByte(2)                                  // count = 2 entries
		buf.WriteByte(0)                                  // i8count = 0 (4-byte inodes)
		binary.Write(&buf, binary.BigEndian, uint32(128)) // parent inode = 128

		// Write 2 short-form entries with 4-byte inode numbers
		for _, e := range []struct {
			name string
			ino  uint32
		}{
			{"file1", 200},
			{"file2", 300},
		} {
			buf.WriteByte(uint8(len(e.name)))           // namelen
			buf.Write([]byte{0, 0})                     // offset (unused for this test)
			buf.WriteString(e.name)                     // name
			buf.WriteByte(1)                            // filetype (regular)
			binary.Write(&buf, binary.BigEndian, e.ino) // 4-byte inode
		}

		inode := Inode{
			inodeCore: InodeCore{
				Mode:   0x4000, // S_IFDIR
				Format: XFS_DINODE_FMT_LOCAL,
			},
		}
		fs := &FileSystem{}
		r := bytes.NewReader(buf.Bytes())
		result, err := fs.inodeFormatLocal(r, inode)
		if err != nil {
			t.Fatalf("inodeFormatLocal: %v", err)
		}
		if result.directoryLocal == nil {
			t.Fatal("expected directoryLocal to be set")
		}
		hdr := result.directoryLocal.dir2SfHdr
		if hdr.Parent != 128 {
			t.Errorf("parent: expected 128, got %d", hdr.Parent)
		}
		if len(result.directoryLocal.entries) != 2 {
			t.Fatalf("expected 2 entries, got %d", len(result.directoryLocal.entries))
		}
		if result.directoryLocal.entries[0].EntryName != "file1" {
			t.Errorf("entry[0] name: expected file1, got %s", result.directoryLocal.entries[0].EntryName)
		}
		if result.directoryLocal.entries[0].Inumber != 200 {
			t.Errorf("entry[0] ino: expected 200, got %d", result.directoryLocal.entries[0].Inumber)
		}
		if result.directoryLocal.entries[1].EntryName != "file2" {
			t.Errorf("entry[1] name: expected file2, got %s", result.directoryLocal.entries[1].EntryName)
		}
		if result.directoryLocal.entries[1].Inumber != 300 {
			t.Errorf("entry[1] ino: expected 300, got %d", result.directoryLocal.entries[1].Inumber)
		}
	})

	t.Run("8-byte parent (i8count!=0)", func(t *testing.T) {
		var buf bytes.Buffer
		buf.WriteByte(2)                                          // count = 2 entries
		buf.WriteByte(2)                                          // i8count = 2 (8-byte inodes)
		binary.Write(&buf, binary.BigEndian, uint64(0x100000080)) // parent inode (exceeds 32-bit)

		// Write 2 short-form entries with 8-byte inode numbers
		for _, e := range []struct {
			name string
			ino  uint64
		}{
			{"bigfile1", 0x200000001},
			{"bigfile2", 0x300000002},
		} {
			buf.WriteByte(uint8(len(e.name)))           // namelen
			buf.Write([]byte{0, 0})                     // offset
			buf.WriteString(e.name)                     // name
			buf.WriteByte(1)                            // filetype (regular)
			binary.Write(&buf, binary.BigEndian, e.ino) // 8-byte inode
		}

		inode := Inode{
			inodeCore: InodeCore{
				Mode:   0x4000, // S_IFDIR
				Format: XFS_DINODE_FMT_LOCAL,
			},
		}
		fs := &FileSystem{}
		r := bytes.NewReader(buf.Bytes())
		result, err := fs.inodeFormatLocal(r, inode)
		if err != nil {
			t.Fatalf("inodeFormatLocal: %v", err)
		}
		if result.directoryLocal == nil {
			t.Fatal("expected directoryLocal to be set")
		}
		hdr := result.directoryLocal.dir2SfHdr
		if hdr.Parent != 0x100000080 {
			t.Errorf("parent: expected 0x100000080, got 0x%x", hdr.Parent)
		}
		if len(result.directoryLocal.entries) != 2 {
			t.Fatalf("expected 2 entries, got %d", len(result.directoryLocal.entries))
		}
		if result.directoryLocal.entries[0].EntryName != "bigfile1" {
			t.Errorf("entry[0] name: expected bigfile1, got %s", result.directoryLocal.entries[0].EntryName)
		}
		if result.directoryLocal.entries[0].Inumber != 0x200000001 {
			t.Errorf("entry[0] ino: expected 0x200000001, got 0x%x", result.directoryLocal.entries[0].Inumber)
		}
		if result.directoryLocal.entries[1].EntryName != "bigfile2" {
			t.Errorf("entry[1] name: expected bigfile2, got %s", result.directoryLocal.entries[1].EntryName)
		}
		if result.directoryLocal.entries[1].Inumber != 0x300000002 {
			t.Errorf("entry[1] ino: expected 0x300000002, got 0x%x", result.directoryLocal.entries[1].Inumber)
		}
	})
}

type bigEndianReader struct {
	data []byte
	pos  int
}

func newBigEndianReader(data []byte) *bigEndianReader {
	return &bigEndianReader{data: data}
}

func (r *bigEndianReader) Read(p []byte) (int, error) {
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}
