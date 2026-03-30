package xfs

import (
	"bytes"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/xerrors"
)

var (
	_ fs.FS        = &FileSystem{}
	_ fs.ReadDirFS = &FileSystem{}
	_ fs.StatFS    = &FileSystem{}

	_ fs.File     = &File{}
	_ fs.FileInfo = &FileInfo{}
	_ fs.DirEntry = dirEntry{}

	ErrOpenSymlink = xerrors.New("symlink open not supported")
)

var (
	ErrReadSizeFormat = "failed to read size error: actual(%d), expected(%d)"
)

type FileSystem struct {
	dev       io.ReaderAt
	PrimaryAG *AG
	AGs       []*AG
}

func Check(r io.ReaderAt) bool {
	_, err := readSuperBlock(r, 0)
	return err == nil
}

func NewFS(r io.ReaderAt) (*FileSystem, error) {
	primaryAG, err := ReadAG(r, 0)
	if err != nil {
		return nil, xerrors.Errorf("failed to read primary allocation group: %w", err)
	}

	f := &FileSystem{
		dev:       r,
		PrimaryAG: primaryAG,
		AGs:       []*AG{primaryAG},
	}

	agSize := int64(primaryAG.SuperBlock.Agblocks) * int64(primaryAG.SuperBlock.BlockSize)
	for i := int64(1); i < int64(primaryAG.SuperBlock.Agcount); i++ {
		ag, err := ReadAG(r, agSize*i)
		if err != nil {
			return nil, xerrors.Errorf("failed to read allocation group %d: %w", i, err)
		}
		f.AGs = append(f.AGs, ag)
	}

	return f, nil
}

func (f *FileSystem) Close() error {
	return nil
}

func (f *FileSystem) readBlock(blockNum int64, count uint32) ([]byte, error) {
	blockSize := int64(f.PrimaryAG.SuperBlock.BlockSize)
	buf := make([]byte, blockSize*int64(count))
	_, err := f.dev.ReadAt(buf, blockNum*blockSize)
	if err != nil {
		return nil, xerrors.Errorf("failed to read block: %w", err)
	}
	return buf, nil
}

func (f *FileSystem) getRootInode() (*Inode, error) {
	inode, err := f.ReadInode(f.PrimaryAG.SuperBlock.Rootino)
	if err != nil {
		return nil, xerrors.Errorf("failed to read root inode: %w", err)
	}
	return inode, nil
}

func (f *FileSystem) Stat(name string) (fs.FileInfo, error) {
	const op = "stat"

	fi, err := f.Open(name)
	if err != nil {
		info, err := f.ReadDirInfo(name)
		if err != nil {
			return nil, &fs.PathError{Op: op, Path: name, Err: xerrors.Errorf("failed to read dir info: %w", err)}
		}
		return info, nil
	}
	return fi.Stat()
}

func (f *FileSystem) Open(name string) (fs.File, error) {
	const op = "open"

	name = strings.TrimPrefix(name, "/")
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}

	dirName, fileName := filepath.Split(name)
	dirEntries, err := f.ReadDir(dirName)
	if err != nil {
		return nil, &fs.PathError{Op: op, Path: name, Err: xerrors.Errorf("failed to read directory: %w", err)}
	}

	for _, entry := range dirEntries {
		if !entry.IsDir() && entry.Name() == fileName {
			if dir, ok := entry.(dirEntry); ok {
				if dir.Type().Perm()&0xA000 != 0 {
					return nil, ErrOpenSymlink
				}

				file, err := f.newFile(dir)
				if err != nil {
					return nil, xerrors.Errorf("failed to create file: %w", err)
				}

				return file, nil
			}
		}
	}
	return nil, fs.ErrNotExist
}

func (f *FileSystem) ReadDir(name string) ([]fs.DirEntry, error) {
	const op = "read directory"

	dirEntries, err := f.readDirEntry(name)
	if err != nil {
		return nil, &fs.PathError{Op: op, Path: name, Err: err}
	}
	return dirEntries, nil
}

func (f *FileSystem) ReadDirInfo(name string) (fs.FileInfo, error) {
	if name == "/" {
		inode, err := f.getRootInode()
		if err != nil {
			return nil, xerrors.Errorf("failed to parse root inode: %w", err)
		}
		return FileInfo{
			name:  "/",
			inode: inode,
		}, nil
	}
	name = strings.TrimRight(name, string(filepath.Separator))

	dirs, dir := path.Split(name)
	dirEntries, err := f.readDirEntry(dirs)
	if err != nil {
		return nil, xerrors.Errorf("failed to read dir entry: %w", err)
	}
	for _, entry := range dirEntries {
		if entry.Name() == strings.Trim(dir, string(filepath.Separator)) {
			return entry.Info()
		}
	}

	return nil, fs.ErrNotExist
}

func (f *FileSystem) readDirEntry(name string) ([]fs.DirEntry, error) {
	inode, err := f.getRootInode()
	if err != nil {
		return nil, xerrors.Errorf("failed to get root inode: %w", err)
	}

	fileInfos, err := f.listFileInfo(inode.inodeCore.Ino)
	if err != nil {
		return nil, xerrors.Errorf("failed to list root inode directory entries: %w", err)
	}

	currentInode := inode
	dirs := strings.Split(strings.Trim(filepath.Clean(name), string(filepath.Separator)), string(filepath.Separator))
	for i, dir := range dirs {
		found := false
		for _, fileInfo := range fileInfos {
			if fileInfo.Name() == dir {
				if !fileInfo.IsDir() {
					return nil, xerrors.Errorf("%s is file, directory: %w", fileInfo.Name(), fs.ErrNotExist)
				}
				found = true
				currentInode = fileInfo.inode
				break
			}
		}
		if !found && (dir != "" && dir != ".") {
			return nil, fs.ErrNotExist
		}

		fileInfos, err = f.listFileInfo(currentInode.inodeCore.Ino)
		if err != nil {
			return nil, xerrors.Errorf("failed to list directory entries inode: %d: %w", currentInode.inodeCore.Ino, err)
		}

		if i == len(dirs)-1 {
			var dirEntries []fs.DirEntry
			for _, fileInfo := range fileInfos {
				if fileInfo.Name() == "." || fileInfo.Name() == ".." {
					continue
				}
				dirEntries = append(dirEntries, dirEntry{fileInfo})
			}
			return dirEntries, nil
		}
	}
	return nil, fs.ErrNotExist
}

func (f *FileSystem) listFileInfo(ino uint64) ([]FileInfo, error) {
	entries, err := f.listEntries(ino)
	if err != nil {
		return nil, xerrors.Errorf("failed to list entries: %w", err)
	}

	var fileInfos []FileInfo
	for _, entry := range entries {
		inode, err := f.ReadInode(entry.InodeNumber())
		if err != nil {
			return nil, xerrors.Errorf("failed to read inode %d: %w", entry.InodeNumber(), err)
		}
		fileInfos = append(fileInfos,
			FileInfo{
				name:  entry.Name(),
				inode: inode,
			},
		)
	}
	return fileInfos, nil
}

func (f *FileSystem) parseTree(bmbtRecs []BmbtRec) ([]Entry, error) {
	var entries []Entry
	for _, b := range bmbtRecs {
		p := b.Unpack()
		blockEntries, err := f.parseDir2Block(p)
		if err != nil {
			return nil, xerrors.Errorf("failed to parse dir2 block: %w", err)
		}
		for _, entry := range blockEntries {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

func (f *FileSystem) listEntries(ino uint64) ([]Entry, error) {
	inode, err := f.ReadInode(ino)
	if err != nil {
		return nil, xerrors.Errorf("failed to read inode: %w", err)
	}

	if !inode.inodeCore.IsDir() {
		return nil, xerrors.New("error inode is not directory")
	}

	var entries []Entry
	if inode.directoryLocal != nil {
		for _, entry := range inode.directoryLocal.entries {
			entries = append(entries, entry)
		}
	} else if inode.directoryExtents != nil {
		if len(inode.directoryExtents.bmbtRecs) == 0 {
			return nil, xerrors.New("directory extents tree bmbtRecs is empty error")
		}
		entries, err = f.parseTree(inode.directoryExtents.bmbtRecs)
		if err != nil {
			return nil, xerrors.Errorf("failed to parse extents tree: %w", err)
		}
	} else if inode.directoryBtree != nil {
		if len(inode.directoryBtree.bmbtRecs) == 0 {
			return nil, xerrors.New("directory extents btree bmbtRecs is empty error")
		}
		entries, err = f.parseTree(inode.directoryBtree.bmbtRecs)
		if err != nil {
			return nil, xerrors.Errorf("failed to parse btree: %w", err)
		}
	} else {
		return nil, xerrors.New("not found entries")
	}

	return entries, nil
}

func (f *FileSystem) newFile(de dirEntry) (*File, error) {
	var recs []BmbtRec
	if de.inode.regularExtent != nil {
		recs = de.inode.regularExtent.bmbtRecs
	} else if de.inode.regularBtree != nil {
		recs = de.inode.regularBtree.bmbtRecs
	} else {
		return nil, xerrors.Errorf("unsupported inode: %+v", de.inode)
	}

	dt := make(dataTable)
	for _, rec := range recs {
		p := rec.Unpack()
		physicalBlockOffset := f.PrimaryAG.SuperBlock.BlockToPhysicalOffset(p.StartBlock)
		for i := int64(0); i < int64(p.BlockCount); i++ {
			dt[int64(p.StartOff)+i] = physicalBlockOffset + i
		}
	}

	return &File{
		fs:           f,
		FileInfo:     de.FileInfo,
		buffer:       bytes.NewBuffer(nil),
		blockSize:    int64(f.PrimaryAG.SuperBlock.BlockSize),
		currentBlock: -1,
		table:        dt,
	}, nil
}

// FileInfo implements fs.FileInfo
type FileInfo struct {
	name  string
	inode *Inode
}

func (i FileInfo) IsDir() bool {
	return i.inode.inodeCore.IsDir()
}

func (i FileInfo) ModTime() time.Time {
	return time.Unix(int64(i.inode.inodeCore.Mtime), 0)
}

func (i FileInfo) Size() int64 {
	return int64(i.inode.inodeCore.Size)
}

func (i FileInfo) Name() string {
	return i.name
}

func (i FileInfo) Sys() interface{} {
	return nil
}

func (i FileInfo) Mode() fs.FileMode {
	m := i.inode.inodeCore.Mode
	translatedMode := fs.FileMode(m & 0o777)

	if m&0o1000 != 0 {
		translatedMode |= fs.ModeSticky
	}
	if m&0o2000 != 0 {
		translatedMode |= fs.ModeSetuid
	}
	if m&0o4000 != 0 {
		translatedMode |= fs.ModeSetgid
	}

	switch m & 0xF000 {
	case 0xC000:
		translatedMode |= fs.ModeSocket
	case 0xA000:
		translatedMode |= fs.ModeSymlink
	case 0x8000:
		// Regular file
	case 0x6000:
		translatedMode |= fs.ModeDevice
	case 0x4000:
		translatedMode |= fs.ModeDir
	case 0x2000:
		translatedMode |= fs.ModeCharDevice
	case 0x1000:
		translatedMode |= fs.ModeNamedPipe
	default:
		translatedMode |= fs.ModeIrregular
	}

	return translatedMode
}

// dirEntry implements fs.DirEntry
type dirEntry struct {
	FileInfo
}

func (d dirEntry) Type() fs.FileMode {
	return d.FileInfo.Mode().Type()
}

func (d dirEntry) Info() (fs.FileInfo, error) { return d.FileInfo, nil }

// File implements fs.File
type File struct {
	fs *FileSystem
	FileInfo

	buffer *bytes.Buffer

	blockSize    int64
	currentBlock int64
	table        dataTable
}

type dataTable map[int64]int64

func (fi *File) Stat() (fs.FileInfo, error) {
	return &fi.FileInfo, nil
}

func (fi *File) Read(buf []byte) (int, error) {
	if fi.buffer == nil {
		return 0, io.EOF
	}
	if fi.buffer.Len() == 0 {
		fi.currentBlock++
		if fi.currentBlock*fi.blockSize >= fi.Size() {
			fi.buffer = nil
			return 0, io.EOF
		}
	} else {
		return fi.buffer.Read(buf)
	}

	offset, ok := fi.table[fi.currentBlock]
	if !ok {
		if fi.Size()-fi.blockSize*fi.currentBlock < fi.blockSize {
			fi.buffer.Write(make([]byte, fi.Size()-fi.blockSize*fi.currentBlock))
		} else {
			fi.buffer.Write(make([]byte, fi.blockSize))
		}
	} else {
		b, err := fi.fs.readBlock(offset, 1)
		if err != nil {
			return 0, xerrors.Errorf("failed to read block: %w", err)
		}

		if fi.Size()-fi.blockSize*fi.currentBlock < fi.blockSize {
			b = b[:fi.Size()-fi.blockSize*fi.currentBlock]
		}
		n, err := fi.buffer.Write(b)
		if n != len(b) {
			return 0, xerrors.Errorf("write buffer error: actual(%d), expected(%d)", n, len(b))
		}
	}

	return fi.buffer.Read(buf)
}

func (fi *File) Close() error {
	return nil
}
