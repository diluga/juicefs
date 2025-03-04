/*
 * JuiceFS, Copyright 2021 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/juicedata/juicefs/pkg/chunk"
	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/object"
	osync "github.com/juicedata/juicefs/pkg/sync"
	"github.com/juicedata/juicefs/pkg/utils"

	"github.com/urfave/cli/v2"
)

func checkFlags() *cli.Command {
	return &cli.Command{
		Name:      "fsck",
		Usage:     "Check consistency of file system",
		ArgsUsage: "META-URL",
		Action:    fsck,
	}
}

func fsck(ctx *cli.Context) error {
	setLoggerLevel(ctx)
	if ctx.Args().Len() < 1 {
		return fmt.Errorf("META-URL is needed")
	}
	m := meta.NewClient(ctx.Args().Get(0), &meta.Config{Retries: 10, Strict: true})
	format, err := m.Load()
	if err != nil {
		logger.Fatalf("load setting: %s", err)
	}

	chunkConf := chunk.Config{
		BlockSize: format.BlockSize * 1024,
		Compress:  format.Compression,

		GetTimeout: time.Second * 60,
		PutTimeout: time.Second * 60,
		MaxUpload:  20,
		BufferSize: 300 << 20,
		CacheDir:   "memory",
	}

	blob, err := createStorage(format)
	if err != nil {
		logger.Fatalf("object storage: %s", err)
	}
	logger.Infof("Data use %s", blob)
	blob = object.WithPrefix(blob, "chunks/")
	objs, err := osync.ListAll(blob, "", "")
	if err != nil {
		logger.Fatalf("list all blocks: %s", err)
	}

	// Find all blocks in object storage
	progress := utils.NewProgress(false, false)
	blockDSpin := progress.AddDoubleSpinner("Found blocks")
	var blocks = make(map[string]int64)
	for obj := range objs {
		if obj == nil {
			break // failed listing
		}
		if obj.IsDir() {
			continue
		}

		logger.Debugf("found block %s", obj.Key())
		parts := strings.Split(obj.Key(), "/")
		if len(parts) != 3 {
			continue
		}
		name := parts[2]
		blocks[name] = obj.Size()
		blockDSpin.IncrInt64(obj.Size())
	}
	blockDSpin.Done()
	if progress.Quiet {
		c, b := blockDSpin.Current()
		logger.Infof("Found %d blocks (%d bytes)", c, b)
	}

	// List all slices in metadata engine
	sliceCSpin := progress.AddCountSpinner("Listed slices")
	var c = meta.NewContext(0, 0, []uint32{0})
	slices := make(map[meta.Ino][]meta.Slice)
	r := m.ListSlices(c, slices, false, sliceCSpin.Increment)
	if r != 0 {
		logger.Fatalf("list all slices: %s", r)
	}
	sliceCSpin.Done()

	// Scan all slices to find lost blocks
	sliceCBar := progress.AddCountBar("Scanned slices", sliceCSpin.Current())
	sliceBSpin := progress.AddByteSpinner("Scanned slices")
	lostDSpin := progress.AddDoubleSpinner("Lost blocks")
	brokens := make(map[meta.Ino]string)
	for inode, ss := range slices {
		for _, s := range ss {
			n := (s.Size - 1) / uint32(chunkConf.BlockSize)
			for i := uint32(0); i <= n; i++ {
				sz := chunkConf.BlockSize
				if i == n {
					sz = int(s.Size) - int(i)*chunkConf.BlockSize
				}
				key := fmt.Sprintf("%d_%d_%d", s.Chunkid, i, sz)
				if _, ok := blocks[key]; !ok {
					if _, err := blob.Head(key); err != nil {
						if _, ok := brokens[inode]; !ok {
							if p, st := meta.GetPath(m, meta.Background, inode); st == 0 {
								brokens[inode] = p
							} else {
								logger.Warnf("getpath of inode %d: %s", inode, st)
								brokens[inode] = st.Error()
							}
						}
						logger.Errorf("can't find block %s for file %s: %s", key, brokens[inode], err)
						lostDSpin.IncrInt64(int64(sz))
					}
				}
			}
			sliceCBar.Increment()
			sliceBSpin.IncrInt64(int64(s.Size))
		}
	}
	progress.Done()
	if progress.Quiet {
		logger.Infof("Used by %d slices (%d bytes)", sliceCBar.Current(), sliceBSpin.Current())
	}
	if lc, lb := lostDSpin.Current(); lc > 0 {
		msg := fmt.Sprintf("%d objects are lost (%d bytes), %d broken files:\n", lc, lb, len(brokens))
		msg += fmt.Sprintf("%13s: PATH\n", "INODE")
		var fileList []string
		for i, p := range brokens {
			fileList = append(fileList, fmt.Sprintf("%13d: %s", i, p))
		}
		sort.Strings(fileList)
		msg += strings.Join(fileList, "\n")
		logger.Fatal(msg)
	}

	return nil
}
