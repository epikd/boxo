package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path"

	block "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
)

func main() {
	size := 256 * 1024
	n := 1000
	keys := createblk(n, size)
	fmt.Println(len(keys))
}

func createblk(n int, size int) []cid.Cid {

	dir := path.Join(".", "blk")
	if _, err := os.Stat("blk"); errors.Is(err, os.ErrNotExist) {
		err := os.Mkdir(dir, os.ModePerm)
		if err != nil {
			fmt.Println(err)
		}
	}

	var keys []cid.Cid
	for i := 0; i < n; i++ {
		fmt.Printf("generating %d-sized random block\n", size)
		buf := make([]byte, size)
		_, _ = rand.Read(buf)
		blk := block.NewBlock(buf)
		key := cid.NewCidV1(0xec, blk.Multihash())

		fn := path.Join(".", "blk", key.String())

		fp, err := os.Create(fn)
		if err != nil {
			fmt.Println(err)
			return nil
		}
		nb, err := fp.Write(buf)
		if err != nil {
			fmt.Println(err)
		}
		fmt.Println(nb)
		keys = append(keys, key)
	}
	return keys
}
