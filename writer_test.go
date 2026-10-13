package torrentzip_test

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bodgit/torrentzip"
)

// Values from the TorrentZip and zip specifications, defined independently
// of the package so the tests check the format rather than the constants.
const (
	flagMaxCompression = 0x0002
	flagUTF8           = 0x0800
	dosTime            = 0xbc00 // 23:32:00
	dosDate            = 0x2198 // 1996-12-24

	directoryEndLen         = 22
	directory64LocLen       = 20
	directory64EndLen       = 56
	directory64EndSignature = 0x06064b50
)

type file struct {
	name string
	data []byte
}

// newRand returns a deterministic random number generator.
func newRand(seed byte) (*rand.Rand, *rand.ChaCha8) {
	src := rand.NewChaCha8([32]byte{seed})

	return rand.New(src), src //nolint:gosec // deterministic test data
}

func testFiles() []file {
	_, src := newRand(3)

	random := make([]byte, 100000)
	_, _ = src.Read(random)

	return []file{
		{"readme.txt", []byte(strings.Repeat("TorrentZip all the things. ", 500))},
		{"Zebra.bin", random},
		{"alpha/Beta.dat", []byte("beta")},
		{"alpha/", nil},
		{"empty/", nil},
		{"alpha/gamma/", nil},
		{"ALPHA.txt", []byte("upper")},
		{"alpha.txt", []byte("lower")},
		{"empty.txt", nil},
		{"_underscore", []byte("_ sorts after upper case letters but before lower case in ASCII")},
		{"café.txt", []byte("code page 437")},
		{string([]rune{0x65e5, 0x672c, 0x8a9e}) + ".txt", []byte("utf-8")},
		{"☺ glyph", []byte("code page 437 glyph for 0x01")},
		{"tab\tname", []byte("control characters force utf-8")},
		{"a-b", []byte("dash")},
		{"a/b", []byte("slash")},
	}
}

// storedName returns the name archive/zip reads back for a test file.
// Names stored in code page 437 come back as the raw bytes.
func storedName(name string) string {
	switch name {
	case "café.txt":
		return "caf\x82.txt"
	case "☺ glyph":
		return "\x01 glyph"
	default:
		return name
	}
}

func shuffled(files []file) []file {
	files = slices.Clone(files)
	r, _ := newRand(5)
	r.Shuffle(len(files), func(i, j int) { files[i], files[j] = files[j], files[i] })

	return files
}

func sorted(files []file) []file {
	files = slices.Clone(files)
	slices.SortStableFunc(files, func(a, b file) int { return torrentzip.CompareNames(a.name, b.name) })

	return files
}

func write(t *testing.T, zw *torrentzip.Writer, files []file) {
	t.Helper()

	for _, f := range files {
		w, err := zw.Create(f.name)
		if err != nil {
			t.Fatal(err)
		}
		// Write in uneven pieces
		for data := f.data; len(data) > 0; {
			n := min(len(data), 7777)
			if _, err := w.Write(data[:n]); err != nil {
				t.Fatal(err)
			}

			data = data[n:]
		}
	}

	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func reference(t *testing.T, files []file) []byte {
	t.Helper()

	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}

	in := make([][2]string, 0, len(files))
	for _, f := range files {
		in = append(in, [2]string{f.name, base64.StdEncoding.EncodeToString(f.data)})
	}

	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}

	script, err := filepath.Abs(filepath.Join("testdata", "reference.py"))
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), python, "-I", script) //nolint:gosec // python3 from PATH
	cmd.Stdin = bytes.NewReader(b)

	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}

	return out
}

func newReader(t *testing.T, b []byte) *zip.Reader {
	t.Helper()

	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}

	return zr
}

func createFile(t *testing.T) *os.File {
	t.Helper()

	f, err := os.Create(filepath.Join(t.TempDir(), "test.zip"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = f.Close() })

	return f
}

// onlyWriter hides any other methods, such as Seek.
type onlyWriter struct{ io.Writer }

func TestWriterMatchesReference(t *testing.T) {
	t.Parallel()

	files := testFiles()
	want := reference(t, files)

	tests := []struct {
		name  string
		write func(t *testing.T) []byte
	}{
		{"NewWriter", func(t *testing.T) []byte {
			t.Helper()

			var buf bytes.Buffer
			write(t, torrentzip.NewWriter(&buf), shuffled(files))

			return buf.Bytes()
		}},
		{"NewStreamWriter", func(t *testing.T) []byte {
			t.Helper()

			var buf bytes.Buffer
			write(t, torrentzip.NewStreamWriter(onlyWriter{&buf}), sorted(files))

			return buf.Bytes()
		}},
		{"NewStreamWriterSeeker", func(t *testing.T) []byte {
			t.Helper()

			f := createFile(t)
			write(t, torrentzip.NewStreamWriter(f), sorted(files))

			b, err := os.ReadFile(f.Name())
			if err != nil {
				t.Fatal(err)
			}

			return b
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.write(t); !bytes.Equal(got, want) {
				t.Fatalf("archive differs from reference (got %d bytes, want %d bytes)", len(got), len(want))
			}
		})
	}
}

func TestWriterReadBack(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	files := testFiles()
	write(t, torrentzip.NewWriter(&buf), shuffled(files))

	b := buf.Bytes()
	zr := newReader(t, b)

	// Redundant directories are dropped, everything else is in order.
	want := make([]string, 0, len(files))
	contents := make(map[string][]byte, len(files))

	for _, f := range sorted(files) {
		if f.name != "alpha/" {
			want = append(want, storedName(f.name))
			contents[storedName(f.name)] = f.data
		}
	}

	got := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		got = append(got, f.Name)
	}

	if !slices.Equal(got, want) {
		t.Fatalf("got names %q, want %q", got, want)
	}

	for _, f := range zr.File {
		checkHeader(t, f)
		checkContents(t, f, contents[f.Name])
	}

	checkComment(t, b, zr.Comment)
}

// checkHeader checks the fields that TorrentZip fixes.
func checkHeader(t *testing.T, f *zip.File) {
	t.Helper()

	checkMethod(t, f)

	if f.ModifiedTime != dosTime || f.ModifiedDate != dosDate {
		t.Errorf("%s: bad timestamp", f.Name)
	}

	if len(f.Extra) != 0 || f.CreatorVersion != 0 || f.ReaderVersion != 20 || f.ExternalAttrs != 0 {
		t.Errorf("%s: unexpected header fields", f.Name)
	}

	if f.UncompressedSize64 == 0 && f.CompressedSize64 != 2 {
		t.Errorf("%s: empty file has compressed size %d", f.Name, f.CompressedSize64)
	}
}

func checkMethod(t *testing.T, f *zip.File) {
	t.Helper()

	if f.Method != zip.Deflate || f.Flags&^flagUTF8 != flagMaxCompression {
		t.Errorf("%s: method %d, flags %#x", f.Name, f.Method, f.Flags)
	}
}

func checkContents(t *testing.T, f *zip.File, want []byte) {
	t.Helper()

	r, err := f.Open()
	if err != nil {
		t.Fatal(err)
	}

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(data, want) {
		t.Errorf("%s: contents differ", f.Name)
	}
}

// checkComment verifies the archive comment is the CRC-32 of the central
// directory.
func checkComment(t *testing.T, b []byte, comment string) {
	t.Helper()

	eocd := b[len(b)-directoryEndLen-len(comment):]
	size := binary.LittleEndian.Uint32(eocd[12:])
	offset := binary.LittleEndian.Uint32(eocd[16:])

	crc := crc32.ChecksumIEEE(b[offset : offset+size])
	if want := fmt.Sprintf("TORRENTZIPPED-%08X", crc); comment != want {
		t.Fatalf("got comment %q, want %q", comment, want)
	}
}

func TestEmptyArchive(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	write(t, torrentzip.NewWriter(&buf), nil)

	if want := reference(t, nil); !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("got %x, want %x", buf.Bytes(), want)
	}

	if !strings.HasSuffix(buf.String(), "TORRENTZIPPED-00000000") {
		t.Fatalf("bad comment in %q", buf.String())
	}
}

func TestStreamWriterOrder(t *testing.T) {
	t.Parallel()

	zw := torrentzip.NewStreamWriter(io.Discard)
	if _, err := zw.Create("b"); err != nil {
		t.Fatal(err)
	}

	if _, err := zw.Create("A"); !errors.Is(err, torrentzip.ErrNotSorted) {
		t.Fatalf("got %v, want ErrNotSorted", err)
	}
}

func TestStreamWriterSizeHint(t *testing.T) {
	t.Parallel()

	zw := torrentzip.NewStreamWriter(createFile(t))

	w, err := zw.CreateHeader(&torrentzip.FileHeader{Name: "small", UncompressedSize64: 1 << 33})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := w.Write([]byte("not that big")); err != nil {
		t.Fatal(err)
	}

	if err := zw.Close(); !errors.Is(err, torrentzip.ErrSizeMismatch) {
		t.Fatalf("got %v, want ErrSizeMismatch", err)
	}
}

func TestDirectoryData(t *testing.T) {
	t.Parallel()

	zw := torrentzip.NewWriter(io.Discard)

	w, err := zw.Create("dir/")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := w.Write([]byte("x")); err == nil {
		t.Fatal("expected error writing to a directory")
	}
}

func TestWriteAfterNextCreate(t *testing.T) {
	t.Parallel()

	zw := torrentzip.NewWriter(io.Discard)
	w, _ := zw.Create("a")
	_, _ = zw.Create("b")

	if _, err := w.Write([]byte("x")); err == nil {
		t.Fatal("expected error writing to a finished file")
	}
}

func TestAddFS(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{}
	files := testFiles()

	for _, f := range files {
		if dir, ok := strings.CutSuffix(f.name, "/"); ok {
			fsys[dir] = &fstest.MapFile{Mode: os.ModeDir | 0o755}
		} else {
			fsys[f.name] = &fstest.MapFile{Data: f.data}
		}
	}

	// AddFS also adds the parent directories, which are redundant.
	want := reference(t, files)

	for _, stream := range []bool{false, true} {
		var buf bytes.Buffer

		zw := torrentzip.NewWriter(&buf)
		if stream {
			zw = torrentzip.NewStreamWriter(&buf)
		}

		if err := zw.AddFS(fsys); err != nil {
			t.Fatal(err)
		}

		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}

		if !bytes.Equal(buf.Bytes(), want) {
			t.Fatalf("stream=%v: AddFS archive differs from reference", stream)
		}
	}
}

// plainZip returns an ordinary zip archive of files.
func plainZip(t *testing.T, files []file) []byte {
	t.Helper()

	var buf bytes.Buffer

	zw := zip.NewWriter(&buf)
	for _, f := range files {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: f.name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}

		if _, err := w.Write(f.data); err != nil {
			t.Fatal(err)
		}
	}

	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func TestCopy(t *testing.T) {
	t.Parallel()

	files := testFiles()
	want := reference(t, files)

	// Re-zipping a TorrentZip archive with Copy is a no-op, as is
	// re-zipping an ordinary zip archive of the same files.
	for _, src := range [][]byte{want, plainZip(t, shuffled(files))} {
		var buf bytes.Buffer

		zw := torrentzip.NewWriter(&buf)
		for _, f := range newReader(t, src).File {
			if err := zw.Copy(f); err != nil {
				t.Fatal(err)
			}
		}

		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}

		if !bytes.Equal(buf.Bytes(), want) {
			t.Fatal("Copy did not produce the reference archive")
		}
	}
}

func TestCompareNames(t *testing.T) {
	t.Parallel()

	names := []string{"b", "A", "a", "B", "_", "a/", "a-", "a/x", "ab", "\U0001F600", "�", "é"}
	slices.SortFunc(names, torrentzip.CompareNames)

	want := []string{"_", "A", "a", "a-", "a/", "a/x", "ab", "B", "b", "é", "\U0001F600", "�"}
	if !slices.Equal(names, want) {
		t.Fatalf("got %q, want %q", names, want)
	}
}

func TestZip64Records(t *testing.T) {
	t.Parallel()

	const n = 1<<16 + 10

	var buf bytes.Buffer

	zw := torrentzip.NewWriter(&buf)
	for i := range n {
		if _, err := zw.Create(fmt.Sprintf("%05x", n-i)); err != nil {
			t.Fatal(err)
		}
	}

	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	b := buf.Bytes()
	zr := newReader(t, b)

	if len(zr.File) != n {
		t.Fatalf("got %d files, want %d", len(zr.File), n)
	}

	if zr.File[0].Name != "00001" || zr.File[n-1].Name != fmt.Sprintf("%05x", n) {
		t.Fatal("files not sorted")
	}

	// The comment covers the central directory only, not the zip64
	// end of central directory record or locator.
	eocd := b[len(b)-directoryEndLen-len(zr.Comment)-directory64LocLen-directory64EndLen:]
	if binary.LittleEndian.Uint32(eocd) != directory64EndSignature {
		t.Fatal("missing zip64 end of central directory record")
	}

	size := binary.LittleEndian.Uint64(eocd[40:])
	offset := binary.LittleEndian.Uint64(eocd[48:])

	crc := crc32.ChecksumIEEE(b[offset : offset+size])
	if want := fmt.Sprintf("TORRENTZIPPED-%08X", crc); zr.Comment != want {
		t.Fatalf("got comment %q, want %q", zr.Comment, want)
	}
}

func TestBackslashes(t *testing.T) {
	t.Parallel()

	var a, b bytes.Buffer
	write(t, torrentzip.NewWriter(&a), []file{{`dir\b`, []byte("b")}, {"dir/a", []byte("a")}})
	write(t, torrentzip.NewWriter(&b), []file{{"dir/a", []byte("a")}, {"dir/b", []byte("b")}})

	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("backslashes not converted")
	}
}
