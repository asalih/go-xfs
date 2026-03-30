# go-xfs

Pure Go, read-only XFS filesystem implementation that satisfies the `io/fs` interfaces (`fs.FS`, `fs.ReadDirFS`, `fs.StatFS`).

## Usage

```go
package main

import (
	"io/fs"
	"os"

	xfs "github.com/asalih/go-xfs"
)

func main() {
	f, _ := os.Open("image.xfs")
	defer f.Close()

	fsys, _ := xfs.NewFS(f)

	// Walk the filesystem
	fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		// ...
		return nil
	})

	// Open and read a file
	file, _ := fsys.Open("path/to/file")
	defer file.Close()

	// Read directory
	entries, _ := fsys.ReadDir("some/dir")

	// Stat
	info, _ := fsys.Stat("some/file")
}
```

## Features

- Implements `fs.FS`, `fs.ReadDirFS`, `fs.StatFS`
- Accepts any `io.ReaderAt` (disk images, partitions, etc.)
- Supports short-form, extent-based, and B+tree directories
- Supports extent-based and B+tree regular file data forks
- Supports inline and local symlinks
