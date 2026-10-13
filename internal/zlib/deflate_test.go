package zlib_test

import (
	"bytes"
	"compress/flate"
	"io"
	"math/rand/v2"
	"os/exec"
	"strings"
	"testing"

	"github.com/bodgit/torrentzip/internal/zlib"
)

// windowSize is the size of the sliding window used by deflate, 2x32 KiB.
const windowSize = 2 << 15

// referenceDeflate compresses data with the system zlib via Python, using
// the same parameters as TorrentZip.
func referenceDeflate(t *testing.T, data []byte) []byte {
	t.Helper()

	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}

	const script = `import sys, zlib
c = zlib.compressobj(9, zlib.DEFLATED, -15, 8, zlib.Z_DEFAULT_STRATEGY)
sys.stdout.buffer.write(c.compress(sys.stdin.buffer.read()) + c.flush())`

	cmd := exec.CommandContext(t.Context(), python, "-I", "-c", script) //nolint:gosec // python3 from PATH
	cmd.Stdin = bytes.NewReader(data)

	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}

	return out
}

func compress(t *testing.T, data []byte, chunk int) []byte {
	t.Helper()

	var buf bytes.Buffer

	w := zlib.NewWriter(&buf)

	for len(data) > 0 {
		n := min(chunk, len(data))
		if _, err := w.Write(data[:n]); err != nil {
			t.Fatal(err)
		}

		data = data[n:]
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

type testCase struct {
	name string
	data []byte
}

// generator produces deterministic test data.
type generator struct {
	r   *rand.Rand
	src *rand.ChaCha8
}

func newGenerator() *generator {
	src := rand.NewChaCha8([32]byte{1, 2})

	return &generator{r: rand.New(src), src: src} //nolint:gosec // deterministic test data
}

func (g *generator) random(n int) []byte {
	b := make([]byte, n)
	_, _ = g.src.Read(b)

	return b
}

// lowEntropy returns data with lots of short and long matches at all
// distances.
func (g *generator) lowEntropy(n, alphabet int) []byte {
	const letters = "abcdefghijklmnopqrstuvwxyz"

	b := make([]byte, n)
	for i := range b {
		b[i] = letters[g.r.IntN(alphabet)]
	}

	return b
}

// text returns text-like data built from a small vocabulary.
func (g *generator) text(n int) []byte {
	words := strings.Fields("the quick brown fox jumps over lazy dog torrent zip archive " +
		"deflate window hash chain match literal length distance block tree huffman")

	var sb strings.Builder

	for sb.Len() < n {
		sb.WriteString(words[g.r.IntN(len(words))])

		if g.r.IntN(10) == 0 {
			sb.WriteString(".\n")
		} else {
			sb.WriteByte(' ')
		}
	}

	return []byte(sb.String()[:n])
}

// mixed returns a mixture of incompressible and compressible regions to
// exercise stored, static and dynamic blocks.
func (g *generator) mixed() []byte {
	var b []byte

	for range 20 {
		n := g.r.IntN(100000)

		switch g.r.IntN(4) {
		case 0:
			b = append(b, g.random(n)...)
		case 1:
			b = append(b, g.text(n)...)
		case 2:
			b = append(b, bytes.Repeat(g.random(1), n)...)
		case 3:
			b = append(b, g.lowEntropy(n, 1+g.r.IntN(4))...)
		}
	}

	return b
}

func testCases() []testCase {
	g := newGenerator()

	return []testCase{
		{"empty", nil},
		{"one", []byte{'a'}},
		{"two", []byte("ab")},
		{"three", []byte("abc")},
		{"short", []byte("hello, hello world")},
		{"zeros", make([]byte, 1<<20)},
		{"random-small", g.random(1000)},
		{"random", g.random(300000)},
		{"text", g.text(1 << 20)},
		{"lowentropy2", g.lowEntropy(500000, 2)},
		{"lowentropy4", g.lowEntropy(500000, 4)},
		{"lowentropy16", g.lowEntropy(500000, 16)},
		{"mixed", g.mixed()},
		{"mixed2", g.mixed()},
		{"window-edge", g.text(windowSize + 1)},
	}
}

// checkChunks compresses data written in various chunk sizes and checks
// that the output always matches want.
func checkChunks(t *testing.T, data, want []byte) {
	t.Helper()

	for _, chunk := range []int{1 << 30, 4096, 1, 1000003} {
		if chunk == 1 && len(data) > 100000 {
			continue
		}

		got := compress(t, data, chunk)
		if !bytes.Equal(got, want) {
			i := 0
			for i < len(got) && i < len(want) && got[i] == want[i] {
				i++
			}

			t.Fatalf("chunk %d: output differs from zlib at byte %d (got %d bytes, want %d bytes)",
				chunk, i, len(got), len(want))
		}
	}
}

func TestDeflateMatchesZlib(t *testing.T) {
	t.Parallel()

	for _, tc := range testCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			want := referenceDeflate(t, tc.data)
			checkChunks(t, tc.data, want)

			// Sanity check that the stream round trips.
			got, err := io.ReadAll(flate.NewReader(bytes.NewReader(want)))
			if err != nil {
				t.Fatal(err)
			}

			if !bytes.Equal(got, tc.data) {
				t.Fatal("round trip failed")
			}
		})
	}
}

func TestEmpty(t *testing.T) {
	t.Parallel()

	if got := compress(t, nil, 1); !bytes.Equal(got, []byte{0x03, 0x00}) {
		t.Fatalf("got %x, want 0300", got)
	}
}

func TestReset(t *testing.T) {
	t.Parallel()

	data := []byte(strings.Repeat("reset me please ", 1000))

	var a, b bytes.Buffer

	w := zlib.NewWriter(&a)
	_, _ = w.Write([]byte("some unrelated data to dirty the state"))
	_ = w.Close()

	a.Reset()
	w.Reset(&a)
	_, _ = w.Write(data)
	_ = w.Close()

	w = zlib.NewWriter(&b)
	_, _ = w.Write(data)
	_ = w.Close()

	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("Reset writer produced different output")
	}
}

func BenchmarkDeflate(b *testing.B) {
	data := newGenerator().text(1 << 20)
	w := zlib.NewWriter(io.Discard)

	b.SetBytes(int64(len(data)))

	for b.Loop() {
		w.Reset(io.Discard)
		_, _ = w.Write(data)
		_ = w.Close()
	}
}
