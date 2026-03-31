package main

import (
	"fmt"
	"io"
	"log"
	"os"

	"github.com/asalih/go-xfs"
)

func main() {
	xfsFile, err := os.Open(os.Getenv("XFS_PATH"))
	if err != nil {
		log.Fatalf("file err: %v", err)
	}

	fs, err := xfs.NewFS(xfsFile)
	if err != nil {
		log.Fatalf("stat err: %v", err)
	}

	ents, err := fs.ReadDir("/")
	fmt.Println(err, ents)

	f, err := fs.Open("/symlink")
	if err != nil {
		log.Fatalf("open err: %v", err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		log.Fatalf("stat err: %v", err)
	}
	fmt.Println(st.Name(), st.IsDir(), st.ModTime(), st.Sys())

	all, err := io.ReadAll(f)
	if err != nil {
		log.Fatalf("read err: %v", err)
	}
	fmt.Println(len(all))
	fmt.Println(string(all))
}
