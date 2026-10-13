// Package torrentzip implements the creation of TorrentZip archives.
//
// A TorrentZip archive is a zip archive written in a canonical form so that
// archives with the same contents are byte-for-byte identical, regardless
// of the tool or platform used to create them. Every file is compressed
// with deflate exactly as zlib does at maximum compression, files are
// sorted by name, all timestamps and attributes are fixed and the archive
// comment records the CRC-32 of the central directory.
//
// The API mirrors archive/zip's Writer. Each file is compressed exactly
// once and the archive is written in a single sequential pass; nothing is
// written to a temporary archive and rewritten afterwards.
package torrentzip

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"io/fs"
	"slices"
	"strings"

	"github.com/bodgit/torrentzip/internal/zlib"
)

const (
	fileHeaderSignature      = 0x04034b50
	directoryHeaderSignature = 0x02014b50
	directoryEndSignature    = 0x06054b50
	directory64LocSignature  = 0x07064b50
	directory64EndSignature  = 0x06064b50

	fileHeaderLen      = 30
	directoryHeaderLen = 46
	directoryEndLen    = 22
	directory64LocLen  = 20
	directory64EndLen  = 56

	zip64ExtraID = 0x0001

	versionMadeBy = 0
	version20     = 20 // 2.0
	version45     = 45 // 4.5 (reads and writes zip64 archives)

	flagMaxCompression = 0x0002
	flagUTF8           = 0x0800

	// Every entry is dated 1996-12-24 23:32:00.
	modTime = 0xbc00
	modDate = 0x2198

	uint16max = (1 << 16) - 1
	uint32max = (1 << 32) - 1

	commentPrefix = "TORRENTZIPPED-"
)

var (
	// ErrNotSorted is returned by a Writer created with NewStreamWriter
	// when a file is added out of TorrentZip order.
	ErrNotSorted = errors.New("torrentzip: files not added in torrentzip order")

	// ErrSizeMismatch is returned when a Writer created with
	// NewStreamWriter writing to an io.WriteSeeker is given a
	// FileHeader.UncompressedSize64 hint that disagrees with whether the
	// file needed zip64 extensions.
	ErrSizeMismatch = errors.New("torrentzip: file size does not match FileHeader.UncompressedSize64 hint")

	errDirData    = errors.New("torrentzip: write to directory")
	errClosed     = errors.New("torrentzip: writer closed")
	errEntryDone  = errors.New("torrentzip: write to file after the next file was created")
	errLongName   = errors.New("torrentzip: FileHeader.Name too long")
	errNonRegular = errors.New("torrentzip: cannot add non-regular file")
	errNegative   = errors.New("torrentzip: negative offset")
)

// FileHeader describes a file within a zip file. It is archive/zip's
// FileHeader; only the Name field is used, everything else in the stored
// header is dictated by the TorrentZip format. UncompressedSize64 is used
// as a size hint by a Writer created with NewStreamWriter.
type FileHeader = zip.FileHeader

// entry records everything needed to write the headers for a file.
type entry struct {
	name   string
	raw    []byte // encoded name
	flags  uint16
	crc32  uint32
	csize  uint64
	usize  uint64
	offset uint64
	data   *bytes.Buffer // compressed data, nil when written directly
}

func (e *entry) isDir() bool { return isDir(e.name) }

func (e *entry) isZip64() bool {
	return e.csize >= uint32max || e.usize >= uint32max
}

// Writer implements a TorrentZip file writer.
type Writer struct {
	bw     *bufio.Writer
	cw     *countWriter
	dir    []*entry
	last   *fileWriter
	closed bool
	zw     *zlib.Writer

	// Fields only used by a Writer created with NewStreamWriter.
	stream  bool
	ws      io.WriteSeeker // set when the underlying writer can seek
	prev    *entry         // the most recently added entry
	pending *entry         // a directory not yet known to be needed
}

// NewWriter returns a new Writer writing a TorrentZip archive to w.
//
// Files may be added in any order. Each file is compressed as it is
// written and the compressed data is held in memory; on Close the files are
// written out in TorrentZip order. Use NewStreamWriter to avoid holding
// the compressed data in memory when the files are already sorted.
func NewWriter(w io.Writer) *Writer {
	bw := bufio.NewWriter(w)

	return &Writer{
		bw: bw,
		cw: &countWriter{w: bw},
		zw: zlib.NewWriter(nil),
	}
}

// NewStreamWriter returns a new Writer writing a TorrentZip archive to w,
// which writes each file to w as soon as it has been compressed rather
// than at Close.
//
// Files must be added in TorrentZip order, as defined by CompareNames;
// adding a file out of order returns ErrNotSorted. AddFS sorts the files
// itself.
//
// If w implements io.WriteSeeker, compressed data is written straight to
// w and the local file header is rewritten afterwards, so no file data is
// held in memory. In that case a file of 4 GiB or more needs its size given
// in advance with CreateHeader and FileHeader.UncompressedSize64, as the
// size of the local file header depends on it. If w can't seek, each file's
// compressed data is held in memory until the file is complete.
func NewStreamWriter(w io.Writer) *Writer {
	zw := NewWriter(w)

	zw.stream = true
	if ws, ok := w.(io.WriteSeeker); ok {
		zw.ws = ws
	}

	return zw
}

// SetOffset sets the offset of the beginning of the zip data within the
// underlying writer. It should be used when the zip data is appended to an
// existing file, such as a binary executable. It must be called before any
// data is written.
func (w *Writer) SetOffset(n int64) {
	if w.cw.count != 0 {
		panic("torrentzip: SetOffset called after data was written")
	}

	if n < 0 {
		panic(errNegative)
	}

	w.cw.count = uint64(n)
}

// Flush flushes any buffered data to the underlying writer. Calling Flush
// is not normally necessary; calling Close is sufficient.
func (w *Writer) Flush() error {
	if err := w.bw.Flush(); err != nil {
		return fmt.Errorf("torrentzip: %w", err)
	}

	return nil
}

// Create adds a file to the zip file using the provided name. It returns a
// Writer to which the file contents should be written. The name must be a
// relative path: it must not start with a drive letter (e.g. C:) or
// leading slash. Backslashes are converted to forward slashes. To create a
// directory instead of a file, add a trailing slash to the name. A
// directory is only stored if it would otherwise be empty. The file's
// contents must be written to the io.Writer before the next call to
// Create, CreateHeader, Copy or Close.
func (w *Writer) Create(name string) (io.Writer, error) {
	return w.CreateHeader(&FileHeader{Name: name, Method: zip.Deflate})
}

// CreateHeader adds a file to the zip archive using the provided
// FileHeader for the file name. Apart from Name, and UncompressedSize64
// when used as a size hint by a Writer created with NewStreamWriter, the
// FileHeader is ignored as TorrentZip fixes the method, times, attributes
// and so on. The returned io.Writer is valid until the next call to
// Create, CreateHeader, Copy or Close.
func (w *Writer) CreateHeader(fh *FileHeader) (io.Writer, error) {
	if w.closed {
		return nil, errClosed
	}

	if err := w.closeLast(); err != nil {
		return nil, err
	}

	// TorrentZip requires forward slashes, and the conversion must
	// happen before sorting.
	name := strings.ReplaceAll(fh.Name, `\`, "/")

	raw, utf8 := encodeName(name)
	if len(raw) > uint16max {
		return nil, errLongName
	}

	e := &entry{name: name, raw: raw, flags: flagMaxCompression}
	if utf8 {
		e.flags |= flagUTF8
	}

	fw := &fileWriter{w: w, e: e, crc: crc32.NewIEEE()}

	if !w.stream {
		e.data = new(bytes.Buffer)
		fw.dst = e.data
	} else if err := w.startStream(fw, fh.UncompressedSize64); err != nil {
		return nil, err
	}

	w.zw.Reset(&fw.compCount)
	fw.compCount.w = fw.dst
	w.last = fw
	w.dir = append(w.dir, e)

	return fw, nil
}

// Copy copies the file f (obtained from an archive/zip Reader) into w. The
// file is decompressed and recompressed as TorrentZip requires. A name
// stored without the UTF-8 flag is decoded as code page 437.
func (w *Writer) Copy(f *zip.File) error {
	r, err := f.Open()
	if err != nil {
		return fmt.Errorf("torrentzip: %w", err)
	}

	// Names without the UTF-8 flag are in code page 437.
	name := f.Name
	if f.Flags&flagUTF8 == 0 {
		name = decodeCP437(name)
	}

	err = w.copyFrom(name, f.UncompressedSize64, r)

	return errors.Join(err, wrap(r.Close()))
}

// AddFS adds the files from fs.FS to the archive. It walks the directory
// tree starting at the root of the filesystem, adding each regular file
// and each empty directory to the archive in TorrentZip order.
func (w *Writer) AddFS(fsys fs.FS) error {
	var names []string

	// Every error returned by the callback is wrapped.
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return wrap(err)
		}

		if name == "." {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("torrentzip: %w", err)
		}

		switch {
		case d.IsDir():
			names = append(names, name+"/")
		case info.Mode().IsRegular():
			names = append(names, name)
		default:
			return errNonRegular
		}

		return nil
	})
	if err != nil {
		return err //nolint:wrapcheck // the callback wraps errors
	}

	slices.SortStableFunc(names, CompareNames)

	for _, name := range names {
		if err := w.addFSFile(fsys, name); err != nil {
			return err
		}
	}

	return nil
}

// Close finishes writing the zip file by writing the central directory. It
// does not close the underlying writer.
func (w *Writer) Close() error {
	if w.closed {
		return errClosed
	}

	if err := w.closeLast(); err != nil {
		return err
	}

	w.closed = true

	var err error
	if w.stream {
		err = w.writePending()
	} else {
		err = w.writeSorted()
	}

	if err != nil {
		return err
	}

	return w.writeCentralDirectory()
}

func (w *Writer) startStream(fw *fileWriter, hint uint64) error {
	e := fw.e
	if w.prev != nil && CompareNames(w.prev.name, e.name) > 0 {
		return ErrNotSorted
	}

	w.prev = e

	// A directory is redundant if the next entry is inside it.
	if w.pending != nil && hasPrefixFold(e.name, w.pending.name) {
		// The pending directory is always the last entry added.
		w.dir = w.dir[:len(w.dir)-1]
		w.pending = nil
	} else if err := w.writePending(); err != nil {
		return err
	}

	// Directories are always empty, so buffer them until it is known
	// whether they need to be written.
	if e.isDir() || w.ws == nil {
		e.data = new(bytes.Buffer)
		fw.dst = e.data

		return nil
	}

	// Write a placeholder local file header, which is rewritten once
	// the CRC-32 and sizes are known.
	fw.reserved64 = hint >= uint32max
	if fw.reserved64 {
		e.usize, e.csize = uint32max, uint32max
	}

	e.offset = w.cw.count
	if err := w.writeLocalHeader(e); err != nil {
		return err
	}

	fw.dst = w.cw

	return nil
}

// writePending writes the pending directory, if any.
func (w *Writer) writePending() error {
	if w.pending == nil {
		return nil
	}

	e := w.pending
	w.pending = nil

	return w.writeEntry(e)
}

func (w *Writer) closeLast() error {
	if w.last == nil {
		return nil
	}

	fw := w.last
	w.last = nil

	if err := fw.close(); err != nil {
		return err
	}

	if !w.stream {
		return nil
	}

	e := fw.e
	switch {
	case e.isDir():
		w.pending = e

		return nil
	case e.data != nil:
		return w.writeEntry(e)
	case e.isZip64() != fw.reserved64:
		return ErrSizeMismatch
	default:
		return w.rewriteLocalHeader(e)
	}
}

// rewriteLocalHeader goes back and rewrites the local file header for e,
// whose data has been written straight to the output.
func (w *Writer) rewriteLocalHeader(e *entry) error {
	if err := w.Flush(); err != nil {
		return err
	}

	end := w.cw.count
	back := end - e.offset

	if _, err := w.ws.Seek(-int64(back), io.SeekCurrent); err != nil { //nolint:gosec // back <= bytes written
		return fmt.Errorf("torrentzip: %w", err)
	}

	w.cw.count = e.offset
	if err := w.writeLocalHeader(e); err != nil {
		return err
	}

	if err := w.Flush(); err != nil {
		return err
	}

	if _, err := w.ws.Seek(int64(end-w.cw.count), io.SeekCurrent); err != nil { //nolint:gosec // as above
		return fmt.Errorf("torrentzip: %w", err)
	}

	w.cw.count = end

	return nil
}

// copyFrom adds a file called name with the contents of r.
func (w *Writer) copyFrom(name string, size uint64, r io.Reader) error {
	fw, err := w.CreateHeader(&FileHeader{Name: name, UncompressedSize64: size})
	if err != nil {
		return err
	}

	if _, err := io.Copy(fw, r); err != nil {
		return fmt.Errorf("torrentzip: %w", err)
	}

	return nil
}

func (w *Writer) addFSFile(fsys fs.FS, name string) error {
	if isDir(name) {
		_, err := w.Create(name)

		return err
	}

	f, err := fsys.Open(name)
	if err != nil {
		return fmt.Errorf("torrentzip: %w", err)
	}

	info, err := f.Stat()
	if err != nil {
		err = wrap(err)
	} else {
		err = w.copyFrom(name, uint64(max(info.Size(), 0)), f)
	}

	return errors.Join(err, wrap(f.Close()))
}

// writeSorted writes all of the buffered entries in TorrentZip order,
// dropping any redundant directories.
func (w *Writer) writeSorted() error {
	slices.SortStableFunc(w.dir, func(a, b *entry) int { return CompareNames(a.name, b.name) })

	// Drop directories that contain other entries. Anything inside a
	// directory sorts immediately after it.
	keep := w.dir[:0]

	for i, e := range w.dir {
		if e.isDir() && i+1 < len(w.dir) && hasPrefixFold(w.dir[i+1].name, e.name) {
			continue
		}

		keep = append(keep, e)
	}

	w.dir = keep

	for _, e := range w.dir {
		if err := w.writeEntry(e); err != nil {
			return err
		}
	}

	return nil
}

// writeEntry writes a local file header followed by the buffered
// compressed data.
func (w *Writer) writeEntry(e *entry) error {
	e.offset = w.cw.count
	if err := w.writeLocalHeader(e); err != nil {
		return err
	}

	_, err := e.data.WriteTo(w.cw)
	e.data = nil

	return err //nolint:wrapcheck // countWriter wraps errors
}

func (w *Writer) writeLocalHeader(e *entry) error {
	var extra []byte

	version := uint16(version20)
	csize, usize := clamp32(e.csize), clamp32(e.usize)

	if e.isZip64() {
		version = version45
		csize, usize = uint32max, uint32max
		extra = make([]byte, 0, 20)
		extra = binary.LittleEndian.AppendUint16(extra, zip64ExtraID)
		extra = binary.LittleEndian.AppendUint16(extra, 16)
		extra = binary.LittleEndian.AppendUint64(extra, e.usize)
		extra = binary.LittleEndian.AppendUint64(extra, e.csize)
	}

	b := writeBuf(make([]byte, 0, fileHeaderLen+len(e.raw)+len(extra)))
	b.uint32(fileHeaderSignature)
	b.uint16(version)
	b.uint16(e.flags)
	b.uint16(zip.Deflate)
	b.uint16(modTime)
	b.uint16(modDate)
	b.uint32(e.crc32)
	b.uint32(csize)
	b.uint32(usize)
	b.uint16(clamp16(len(e.raw)))
	b.uint16(clamp16(len(extra)))
	b = append(b, e.raw...)
	b = append(b, extra...)

	_, err := w.cw.Write(b)

	return err
}

func (w *Writer) writeCentralDirectory() error {
	start := w.cw.count
	crc := crc32.NewIEEE()
	out := io.MultiWriter(w.cw, crc)

	for _, e := range w.dir {
		if _, err := out.Write(directoryHeader(e)); err != nil {
			return err //nolint:wrapcheck // countWriter wraps errors
		}
	}

	if err := w.writeDirectoryEnd(start, crc.Sum32()); err != nil {
		return err
	}

	return w.Flush()
}

// directoryHeader returns the central directory header for e.
func directoryHeader(e *entry) []byte {
	// Each field that doesn't fit is stored in the zip64 extra field,
	// in this order, and replaced with 0xffffffff.
	var extra []byte

	for _, v := range []uint64{e.usize, e.csize, e.offset} {
		if v >= uint32max {
			extra = binary.LittleEndian.AppendUint64(extra, v)
		}
	}

	version := uint16(version20)

	if extra != nil {
		version = version45

		extra = append(binary.LittleEndian.AppendUint16(
			binary.LittleEndian.AppendUint16(nil, zip64ExtraID), clamp16(len(extra))), extra...)
	}

	b := writeBuf(make([]byte, 0, directoryHeaderLen+len(e.raw)+len(extra)))
	b.uint32(directoryHeaderSignature)
	b.uint16(versionMadeBy)
	b.uint16(version)
	b.uint16(e.flags)
	b.uint16(zip.Deflate)
	b.uint16(modTime)
	b.uint16(modDate)
	b.uint32(e.crc32)
	b.uint32(clamp32(e.csize))
	b.uint32(clamp32(e.usize))
	b.uint16(clamp16(len(e.raw)))
	b.uint16(clamp16(len(extra)))
	b.uint16(0) // file comment length
	b.uint16(0) // disk number start
	b.uint16(0) // internal file attributes
	b.uint32(0) // external file attributes
	b.uint32(clamp32(e.offset))
	b = append(b, e.raw...)
	b = append(b, extra...)

	return b
}

// writeDirectoryEnd writes the end of central directory record, preceded
// by the zip64 end of central directory record and locator if needed.
func (w *Writer) writeDirectoryEnd(start uint64, crc uint32) error {
	end := w.cw.count
	records := uint64(len(w.dir))
	size := end - start

	if records >= uint16max || size >= uint32max || start >= uint32max {
		b := writeBuf(make([]byte, 0, directory64EndLen+directory64LocLen))

		// zip64 end of central directory record
		b.uint32(directory64EndSignature)
		b.uint64(directory64EndLen - 12) // length minus signature (uint32) and length fields (uint64)
		b.uint16(version45)              // version made by
		b.uint16(version45)              // version needed to extract
		b.uint32(0)                      // number of this disk
		b.uint32(0)                      // number of the disk with the start of the central directory
		b.uint64(records)                // total number of entries in the central directory on this disk
		b.uint64(records)                // total number of entries in the central directory
		b.uint64(size)                   // size of the central directory
		b.uint64(start)                  // offset of start of central directory with respect to the starting disk number

		// zip64 end of central directory locator
		b.uint32(directory64LocSignature)
		b.uint32(0)   // number of the disk with the start of the zip64 end of central directory
		b.uint64(end) // relative offset of the zip64 end of central directory record
		b.uint32(1)   // total number of disks

		if _, err := w.cw.Write(b); err != nil {
			return err
		}
	}

	comment := fmt.Sprintf("%s%08X", commentPrefix, crc)

	// Fields that don't fit are stored as their maximum value to signal
	// that the zip64 values should be used instead. Fields that fit are
	// left as is, matching RomVault.
	b := writeBuf(make([]byte, 0, directoryEndLen+len(comment)))
	b.uint32(directoryEndSignature)
	b.uint16(0) // number of this disk
	b.uint16(0) // number of the disk with the start of the central directory
	b.uint16(clamp16(len(w.dir)))
	b.uint16(clamp16(len(w.dir)))
	b.uint32(clamp32(size))
	b.uint32(clamp32(start))
	b.uint16(clamp16(len(comment)))
	b = append(b, comment...)

	_, err := w.cw.Write(b)

	return err
}

type fileWriter struct {
	w          *Writer
	e          *entry
	dst        io.Writer
	crc        hash.Hash32
	usize      uint64
	compCount  byteCounter
	closed     bool
	reserved64 bool
}

func (fw *fileWriter) Write(p []byte) (int, error) {
	if fw.closed {
		return 0, errEntryDone
	}

	if len(p) > 0 && fw.e.isDir() {
		return 0, errDirData
	}

	_, _ = fw.crc.Write(p) // never returns an error
	fw.usize += uint64(len(p))

	return fw.w.zw.Write(p) //nolint:wrapcheck // countWriter wraps errors
}

func (fw *fileWriter) close() error {
	fw.closed = true
	if err := fw.w.zw.Close(); err != nil {
		return err //nolint:wrapcheck // countWriter wraps errors
	}

	fw.e.crc32 = fw.crc.Sum32()
	fw.e.usize = fw.usize
	fw.e.csize = fw.compCount.count

	return nil
}

// countWriter counts the bytes written to w and wraps any errors.
type countWriter struct {
	w     io.Writer
	count uint64
}

func (w *countWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.count += uint64(max(n, 0))

	if err != nil {
		return n, fmt.Errorf("torrentzip: %w", err)
	}

	return n, nil
}

// byteCounter counts the bytes written to w. Errors are passed through
// unchanged, as w is either a bytes.Buffer or a countWriter.
type byteCounter struct {
	w     io.Writer
	count uint64
}

func (w *byteCounter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.count += uint64(max(n, 0))

	return n, err //nolint:wrapcheck // see above
}

type writeBuf []byte

func (b *writeBuf) uint16(v uint16) { *b = binary.LittleEndian.AppendUint16(*b, v) }
func (b *writeBuf) uint32(v uint32) { *b = binary.LittleEndian.AppendUint32(*b, v) }
func (b *writeBuf) uint64(v uint64) { *b = binary.LittleEndian.AppendUint64(*b, v) }

// clamp16 returns n, or 0xffff if n doesn't fit in 16 bits.
func clamp16(n int) uint16 {
	if n < 0 || n >= uint16max {
		return uint16max
	}

	return uint16(n)
}

// clamp32 returns v, or 0xffffffff if v doesn't fit in 32 bits.
func clamp32(v uint64) uint32 {
	if v >= uint32max {
		return uint32max
	}

	return uint32(v)
}

// wrap prefixes any non-nil error with the package name.
func wrap(err error) error {
	if err == nil {
		return nil
	}

	return fmt.Errorf("torrentzip: %w", err)
}
