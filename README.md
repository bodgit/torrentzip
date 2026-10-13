[![GitHub release](https://img.shields.io/github/v/release/bodgit/torrentzip)](https://github.com/bodgit/torrentzip/releases)
[![Build Status](https://img.shields.io/github/actions/workflow/status/bodgit/torrentzip/build.yml?branch=main)](https://github.com/bodgit/torrentzip/actions?query=workflow%3ABuild)
[![Coverage Status](https://coveralls.io/repos/github/bodgit/torrentzip/badge.svg?branch=main)](https://coveralls.io/github/bodgit/torrentzip?branch=main)
[![GoDoc](https://godoc.org/github.com/bodgit/torrentzip?status.svg)](https://godoc.org/github.com/bodgit/torrentzip)
![GitHub go.mod Go version](https://img.shields.io/github/go-mod/go-version/bodgit/torrentzip)

# TorrentZip archives

The [github.com/bodgit/torrentzip](https://github.com/bodgit/torrentzip) package writes [TorrentZip](https://sourceforge.net/projects/trrntzip/) archives, the canonical zip format used by ROM managers such as [RomVault](https://www.romvault.com). Archives with the same contents are byte-for-byte identical, whatever tool created them.

* Mirrors the `archive/zip` `Writer` API: `Create`, `CreateHeader`, `Copy`, `AddFS`, `SetOffset`, `Flush` and `Close` all behave like their `archive/zip` counterparts.
* Compresses with a pure Go port of zlib's deflate at level 9, as Go's `compress/flate` produces different output. No cgo is needed.
* Compresses each file once and writes the archive in a single pass, without a temporary file.
* Sorts entries, encodes names as code page 437 or UTF-8 and writes zip64 records exactly as RomVault does.
* Produces byte-identical archives for every file in a full MAME ROM set, and for archives larger than 4 GiB.

`SetComment`, `CreateRaw` and `RegisterCompressor` aren't provided, as TorrentZip fixes the comment and compression.

How to create an archive:
```golang
package main

import (
	"os"

	"github.com/bodgit/torrentzip"
)

func main() {
	f, err := os.Create("archive.zip")
	if err != nil {
		panic(err)
	}
	defer f.Close()

	w := torrentzip.NewWriter(f)

	fw, err := w.Create("hello.txt")
	if err != nil {
		panic(err)
	}

	if _, err = fw.Write([]byte("Hello, TorrentZip!")); err != nil {
		panic(err)
	}

	if err = w.Close(); err != nil {
		panic(err)
	}
}
```

How to convert an existing zip archive:
```golang
package main

import (
	"archive/zip"
	"os"

	"github.com/bodgit/torrentzip"
)

func main() {
	r, err := zip.OpenReader("original.zip")
	if err != nil {
		panic(err)
	}
	defer r.Close()

	f, err := os.Create("archive.zip")
	if err != nil {
		panic(err)
	}
	defer f.Close()

	w := torrentzip.NewWriter(f)

	for _, file := range r.File {
		// Copy decompresses and recompresses each file
		if err = w.Copy(file); err != nil {
			panic(err)
		}
	}

	if err = w.Close(); err != nil {
		panic(err)
	}
}
```

A whole directory can be added with `w.AddFS(os.DirFS("dir"))`.

### Large archives

`NewWriter` accepts files in any order, so it keeps each file's compressed data in memory until `Close` writes everything in TorrentZip order. For archives too large for that, `NewStreamWriter` writes each file as soon as it is complete, but files must be added in TorrentZip order; `CompareNames` compares two names in that order, and `AddFS` sorts files itself:

```golang
w := torrentzip.NewStreamWriter(f)

fh := &torrentzip.FileHeader{
	Name:               "disk.chd",
	UncompressedSize64: uint64(fi.Size()), // needed for files of 4 GiB or more
}

fw, err := w.CreateHeader(fh)
```

If the destination is an `io.WriteSeeker`, such as an `*os.File`, compressed data goes straight to it and only the local file header is rewritten afterwards, so file data is never held in memory. A file of 4 GiB or more needs a zip64 local header, so its size must be given in `UncompressedSize64` before it is written; if the size turns out to be on the other side of 4 GiB, writing the file fails with `ErrSizeMismatch`.

## Testing

Some tests compare the output against the system zlib and an independent reference implementation, `testdata/reference.py`, using `python3`. They are skipped if `python3` isn't available.
