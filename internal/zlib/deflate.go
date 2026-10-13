// Package zlib is a pure Go port of the parts of zlib's deflate needed to
// produce raw deflate streams that are bit-for-bit identical to those
// produced by zlib with deflateInit2(strm, Z_BEST_COMPRESSION, Z_DEFLATED,
// -MAX_WBITS, 8, Z_DEFAULT_STRATEGY), which is what TorrentZip requires.
//
// The standard library compress/flate produces valid but different output,
// so it can't be used to create TorrentZip archives.
//
// This is an altered version of zlib; see LICENSE for zlib's licence.
package zlib

import (
	"encoding/binary"
	"errors"
	"io"
	"math/bits"
)

const (
	minMatch = 3
	maxMatch = 258

	wBits = 15 // MAX_WBITS
	wSize = 1 << wBits
	wMask = wSize - 1

	memLevel  = 8
	hashBits  = memLevel + 7
	hashSize  = 1 << hashBits
	hashMask  = hashSize - 1
	hashShift = (hashBits + minMatch - 1) / minMatch

	windowSize   = 2 * wSize
	minLookahead = maxMatch + minMatch + 1
	maxDist      = wSize - minLookahead

	// Matches of length 3 are discarded if their distance exceeds tooFar.
	tooFar = 4096

	litBufsize = 1 << (memLevel + 6)

	// Configuration for level 9 (Z_BEST_COMPRESSION).
	goodMatch      = 32
	maxLazyMatch   = 258
	niceMatch      = 258
	maxChainLength = 4096

	nilPos = 0 // tail of hash chains
)

var errClosed = errors.New("zlib: write to closed writer")

type sym struct {
	dist uint16
	lc   uint8
}

type state struct {
	out []byte // pending output

	in []byte // next_in

	// The window has some slack at the end so that longestMatch can
	// compare eight bytes at a time without going out of bounds.
	window [windowSize + 8]byte
	prev   [wSize]uint16
	head   [hashSize]uint16

	insH        uint // hash index of string to be inserted
	blockStart  int  // window position at the beginning of the current output block, may be negative
	matchLength int  // length of best match
	prevMatch   int  // previous match
	matchAvail  bool // set if previous match exists
	strstart    int  // start of string to insert
	matchStart  int  // start of matching string
	lookahead   int  // number of valid bytes ahead in window
	prevLength  int  // length of the best match at previous step

	dynLTree [heapSize]ctData
	dynDTree [2*dCodes + 1]ctData
	blTree   [2*blCodes + 1]ctData

	lDesc, dDesc, blDesc treeDesc

	blCount [maxBits + 1]uint16
	heap    [2*lCodes + 1]int
	heapLen int
	heapMax int
	depth   [2*lCodes + 1]uint8

	symBuf  [litBufsize]sym
	symNext int

	optLen    uint64 // bit length of current block with optimal trees
	staticLen uint64 // bit length of current block with static trees
	matches   int    // number of string matches in current block

	bitBuf   uint64
	bitCount uint
}

func (s *state) reset() {
	clear(s.head[:])
	s.strstart = 0
	s.blockStart = 0
	s.lookahead = 0
	s.matchLength = minMatch - 1
	s.prevLength = minMatch - 1
	s.matchAvail = false
	s.insH = 0
	s.in = nil
	s.out = s.out[:0]
	s.trInit()
}

func (s *state) updateHash(c byte) {
	s.insH = ((s.insH << hashShift) ^ uint(c)) & hashMask
}

// insertString inserts string str in the dictionary and returns the
// previous head of the hash chain.
func (s *state) insertString(str int) int {
	s.updateHash(s.window[str+minMatch-1])
	h := s.head[s.insH]
	s.prev[str&wMask] = h
	s.head[s.insH] = uint16(str) //nolint:gosec // str < windowSize

	return int(h)
}

func (s *state) slideHash() {
	for i, m := range s.head {
		if m >= wSize {
			s.head[i] = m - wSize
		} else {
			s.head[i] = nilPos
		}
	}

	for i, m := range s.prev {
		if m >= wSize {
			s.prev[i] = m - wSize
		} else {
			s.prev[i] = nilPos
		}
	}
}

// fillWindow fills the window when the lookahead becomes insufficient.
func (s *state) fillWindow() {
	for {
		more := windowSize - s.lookahead - s.strstart

		// If the window is almost full and there is insufficient
		// lookahead, move the upper half to the lower one to make room
		// in the upper half.
		if s.strstart >= wSize+maxDist {
			copy(s.window[:wSize], s.window[wSize:windowSize])
			s.matchStart -= wSize
			s.strstart -= wSize // we now have strstart >= maxDist
			s.blockStart -= wSize
			s.slideHash()

			more += wSize
		}

		if len(s.in) == 0 {
			break
		}

		n := copy(s.window[s.strstart+s.lookahead:s.strstart+s.lookahead+more], s.in)
		s.in = s.in[n:]
		s.lookahead += n

		// Initialize the hash value now that we have some input
		if s.lookahead >= minMatch {
			s.insH = uint(s.window[s.strstart])
			s.updateHash(s.window[s.strstart+1])
		}

		if s.lookahead >= minLookahead || len(s.in) == 0 {
			break
		}
	}
}

// longestMatch finds the longest match starting at curMatch, sets
// matchStart and returns its length.
//
//nolint:cyclop,funlen // kept structurally identical to zlib's longest_match
func (s *state) longestMatch(curMatch int) int {
	chainLength := maxChainLength
	win := &s.window
	scan := s.strstart
	bestLen := s.prevLength
	nice := niceMatch

	limit := nilPos
	if s.strstart > maxDist {
		limit = s.strstart - maxDist
	}

	strend := s.strstart + maxMatch
	scanEnd1 := win[scan+bestLen-1]
	scanEnd := win[scan+bestLen]

	// Do not waste too much time if we already have a good match
	if s.prevLength >= goodMatch {
		chainLength >>= 2
	}
	// Do not look for matches beyond the end of the input. This is
	// necessary to make deflate deterministic.
	if nice > s.lookahead {
		nice = s.lookahead
	}

	for {
		match := curMatch

		// Skip to next match if the match length cannot increase or if
		// the match length is less than 2. scan[2] and match[2] are
		// always equal when the other bytes match, given that the hash
		// keys are equal and that hashBits >= 8.
		if win[match+bestLen] == scanEnd &&
			win[match+bestLen-1] == scanEnd1 &&
			win[match] == win[scan] &&
			win[match+1] == win[scan+1] {
			// Find the first mismatch from offset 3, comparing eight
			// bytes at a time. This gives the same result as zlib,
			// which compares a byte at a time up to strend.
			sc, mt := scan+3, match+3
			for sc < strend {
				if x := binary.LittleEndian.Uint64(win[sc:]) ^ binary.LittleEndian.Uint64(win[mt:]); x != 0 {
					sc += bits.TrailingZeros64(x) >> 3

					break
				}

				sc += 8
				mt += 8
			}

			sc = min(sc, strend)

			length := sc - scan
			if length > bestLen {
				s.matchStart = curMatch

				bestLen = length
				if length >= nice {
					break
				}

				scanEnd1 = win[scan+bestLen-1]
				scanEnd = win[scan+bestLen]
			}
		}

		curMatch = int(s.prev[curMatch&wMask])
		if curMatch <= limit {
			break
		}

		chainLength--
		if chainLength == 0 {
			break
		}
	}

	if bestLen <= s.lookahead {
		return bestLen
	}

	return s.lookahead
}

func (s *state) flushBlock(last bool) {
	var buf []byte
	if s.blockStart >= 0 {
		buf = s.window[s.blockStart:s.strstart]
	}

	s.trFlushBlock(buf, uint64(s.strstart-s.blockStart), last) //nolint:gosec // strstart >= blockStart
	s.blockStart = s.strstart
}

// deflateSlow is zlib's deflate_slow(). It processes as much input as
// possible; if finish is false it stops when more input is needed,
// otherwise it terminates the stream.
//
//nolint:cyclop,funlen,gocognit // kept structurally identical to zlib's deflate_slow
func (s *state) deflateSlow(finish bool) {
	for {
		// Make sure that we always have enough lookahead, except at the
		// end of the input file. We need maxMatch bytes for the next
		// match, plus minMatch bytes to insert the string following the
		// next match.
		if s.lookahead < minLookahead {
			s.fillWindow()

			if s.lookahead < minLookahead && !finish {
				return
			}

			if s.lookahead == 0 {
				break // flush the current block
			}
		}

		// Insert the string window[strstart .. strstart+2] in the
		// dictionary, and set hashHead to the head of the hash chain.
		hashHead := nilPos
		if s.lookahead >= minMatch {
			hashHead = s.insertString(s.strstart)
		}

		// Find the longest match, discarding those <= prevLength.
		s.prevLength = s.matchLength
		s.prevMatch = s.matchStart
		s.matchLength = minMatch - 1

		if hashHead != nilPos && s.prevLength < maxLazyMatch && s.strstart-hashHead <= maxDist {
			s.matchLength = s.longestMatch(hashHead)

			if s.matchLength <= 5 && s.matchLength == minMatch && s.strstart-s.matchStart > tooFar {
				// If prevMatch is also minMatch, matchStart is garbage
				// but we will ignore the current match anyway.
				s.matchLength = minMatch - 1
			}
		}

		// If there was a match at the previous step and the current
		// match is not better, output the previous match.
		switch {
		case s.prevLength >= minMatch && s.matchLength <= s.prevLength:
			maxInsert := s.strstart + s.lookahead - minMatch
			// Do not insert strings in hash table beyond this.

			bflush := s.trTally(s.strstart-1-s.prevMatch, s.prevLength-minMatch)

			// Insert in hash table all strings up to the end of the
			// match. strstart-1 and strstart are already inserted. If
			// there is not enough lookahead, the last two strings are
			// not inserted in the hash table.
			s.lookahead -= s.prevLength - 1

			s.prevLength -= 2
			for {
				s.strstart++
				if s.strstart <= maxInsert {
					s.insertString(s.strstart)
				}

				s.prevLength--
				if s.prevLength == 0 {
					break
				}
			}

			s.matchAvail = false
			s.matchLength = minMatch - 1
			s.strstart++

			if bflush {
				s.flushBlock(false)
			}
		case s.matchAvail:
			// If there was no match at the previous position, output a
			// single literal. If there was a match but the current
			// match is longer, truncate the previous match to a single
			// literal.
			if s.trTally(0, int(s.window[s.strstart-1])) {
				s.flushBlock(false)
			}

			s.strstart++
			s.lookahead--
		default:
			// There is no previous match to compare with, wait for the
			// next step to decide.
			s.matchAvail = true
			s.strstart++
			s.lookahead--
		}
	}

	if s.matchAvail {
		s.trTally(0, int(s.window[s.strstart-1]))
		s.matchAvail = false
	}

	s.flushBlock(true)
}

// A Writer compresses data written to it as a raw deflate stream that is
// identical to the output of zlib at level 9 with a 32 KiB window and
// memLevel 8.
type Writer struct {
	w      io.Writer
	s      *state
	err    error
	closed bool
}

// NewWriter returns a new Writer writing a raw deflate stream to w.
func NewWriter(w io.Writer) *Writer {
	z := &Writer{s: new(state)}
	z.Reset(w)

	return z
}

// Reset discards the Writer's state and makes it equivalent to the result
// of NewWriter but writing to w instead. The internal buffers are reused.
func (z *Writer) Reset(w io.Writer) {
	z.w = w
	z.err = nil
	z.closed = false
	z.s.reset()
}

// Write compresses p.
func (z *Writer) Write(p []byte) (int, error) {
	if z.err != nil {
		return 0, z.err
	}

	if z.closed {
		return 0, errClosed
	}
	// Bound the amount of pending output by feeding the input in chunks.
	const chunk = 64 << 10

	n := 0

	for len(p) > 0 {
		c := min(len(p), chunk)
		z.s.in = p[:c]
		z.s.deflateSlow(false)
		z.s.in = nil
		p = p[c:]

		n += c
		if err := z.flushOutput(); err != nil {
			return n, err
		}
	}

	return n, nil
}

// Close terminates the deflate stream and flushes any pending output. It
// does not close the underlying writer.
func (z *Writer) Close() error {
	if z.err != nil {
		return z.err
	}

	if z.closed {
		return nil
	}

	z.closed = true
	z.s.deflateSlow(true)

	return z.flushOutput()
}

func (z *Writer) flushOutput() error {
	if len(z.s.out) > 0 {
		_, z.err = z.w.Write(z.s.out)
		z.s.out = z.s.out[:0]
	}

	return z.err
}
