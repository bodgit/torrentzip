package zlib

// This file is a port of zlib's trees.c, which builds the Huffman trees
// for each block and emits the compressed bit stream.

const (
	maxBits     = 15  // all codes must not exceed maxBits bits
	maxBLBits   = 7   // bit length codes must not exceed maxBLBits bits
	lengthCodes = 29  // number of length codes, not counting the special endBlock code
	literals    = 256 // number of literal bytes 0..255
	lCodes      = literals + 1 + lengthCodes
	dCodes      = 30 // number of distance codes
	blCodes     = 19 // number of codes used to transfer the bit lengths
	heapSize    = 2*lCodes + 1
	endBlock    = 256 // end of block literal code
	rep3To6     = 16  // repeat previous bit length 3-6 times (2 bits of repeat count)
	repz3To10   = 17  // repeat a zero length 3-10 times (3 bits of repeat count)
	repz11To138 = 18  // repeat a zero length 11-138 times (7 bits of repeat count)
	smallest    = 1   // index within the heap array of least frequent node

	storedBlock = 0
	staticTrees = 1
	dynTrees    = 2

	distCodeLen = 512
)

// Lookup tables from zlib's trees.c, which are never modified.
//
//nolint:gochecknoglobals // constant tables, as in zlib
var (
	extraLBits = [lengthCodes]int{0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 2, 2, 3, 3, 3, 3, 4, 4, 4, 4, 5, 5, 5, 5, 0}
	extraDBits = [dCodes]int{
		0, 0, 0, 0, 1, 1, 2, 2, 3, 3, 4, 4, 5, 5, 6, 6, 7, 7, 8, 8, 9, 9, 10, 10, 11, 11, 12, 12, 13, 13,
	}
	extraBLBits = [blCodes]int{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2, 3, 7}
	blOrder     = [blCodes]uint8{16, 17, 18, 0, 8, 7, 9, 6, 10, 5, 11, 4, 12, 3, 13, 2, 14, 1, 15}
)

// ctData mirrors zlib's ct_data, including its unions: fc holds either the
// frequency count or the bit string, dl holds either the father node in the
// Huffman tree or the length of the bit string. The code relies on the
// aliasing, exactly as zlib does.
type ctData struct {
	fc uint16 // freq / code
	dl uint16 // dad / len
}

type staticTreeDesc struct {
	staticTree []ctData // static tree or nil
	extraBits  []int    // extra bits for each code or nil
	extraBase  int      // base index for extraBits
	elems      int      // max number of elements in the tree
	maxLength  int      // max bit length for the codes
}

type treeDesc struct {
	dynTree  []ctData // the dynamic tree
	maxCode  int      // largest code with non zero frequency
	statDesc *staticTreeDesc
}

// Tables computed once at startup, as zlib's tr_static_init does, and never
// modified afterwards.
//
//nolint:gochecknoglobals // constant tables, as in zlib
var (
	lengthCode, baseLength   = initLengthCodes()
	distCode, baseDist       = initDistCodes()
	staticLTree, staticDTree = initStaticTrees()

	staticLDesc  = staticTreeDesc{staticLTree[:], extraLBits[:], literals + 1, lCodes, maxBits}
	staticDDesc  = staticTreeDesc{staticDTree[:], extraDBits[:], 0, dCodes, maxBits}
	staticBLDesc = staticTreeDesc{nil, extraBLBits[:], 0, blCodes, maxBLBits}
)

func initLengthCodes() ([maxMatch - minMatch + 1]uint8, [lengthCodes]int) {
	var (
		lengthCode [maxMatch - minMatch + 1]uint8
		baseLength [lengthCodes]int
	)

	length := 0

	for code := range lengthCodes - 1 {
		baseLength[code] = length
		for range 1 << extraLBits[code] {
			lengthCode[length] = uint8(code)
			length++
		}
	}
	// The length 255 (match length 258) can be represented in two
	// different ways: code 284 + 5 bits or code 285, so overwrite
	// lengthCode[255] to use the best encoding. The loop above fills
	// every entry, so this is the last one.
	lengthCode[len(lengthCode)-1] = lengthCodes - 1

	return lengthCode, baseLength
}

func initDistCodes() ([distCodeLen]uint8, [dCodes]int) {
	var (
		distCode [distCodeLen]uint8
		baseDist [dCodes]int
	)

	dist := 0

	for code := range 16 {
		baseDist[code] = dist
		for range 1 << extraDBits[code] {
			distCode[dist] = uint8(code)
			dist++
		}
	}

	dist >>= 7 // from now on, all distances are divided by 128
	for code := 16; code < dCodes; code++ {
		baseDist[code] = dist << 7
		for range 1 << (extraDBits[code] - 7) {
			distCode[256+dist] = uint8(code)
			dist++
		}
	}

	return distCode, baseDist
}

func initStaticTrees() ([lCodes + 2]ctData, [dCodes]ctData) {
	var (
		staticLTree [lCodes + 2]ctData
		staticDTree [dCodes]ctData
		blCount     [maxBits + 1]uint16
	)

	for n := range staticLTree {
		var length uint16

		switch {
		case n <= 143:
			length = 8
		case n <= 255:
			length = 9
		case n <= 279:
			length = 7
		default:
			length = 8
		}

		staticLTree[n].dl = length
		blCount[length]++
	}

	genCodes(staticLTree[:], lCodes+1, blCount[:])

	for n := range dCodes {
		staticDTree[n].dl = 5
		staticDTree[n].fc = biReverse(uint16(n), 5)
	}

	return staticLTree, staticDTree
}

func dCode(dist int) uint8 {
	if dist < 256 {
		return distCode[dist]
	}

	return distCode[256+(dist>>7)]
}

// biReverse reverses the first len bits of a code.
func biReverse(code uint16, length int) uint16 {
	var res uint16
	for {
		res |= code & 1
		code >>= 1
		res <<= 1

		length--
		if length <= 0 {
			break
		}
	}

	return res >> 1
}

// genCodes generates the codes for a given tree and bit counts (which need
// not be optimal).
func genCodes(tree []ctData, maxCode int, blCount []uint16) {
	var (
		nextCode [maxBits + 1]uint16
		code     uint16
	)
	for bits := 1; bits <= maxBits; bits++ {
		code = (code + blCount[bits-1]) << 1
		nextCode[bits] = code
	}

	for n := 0; n <= maxCode; n++ {
		length := int(tree[n].dl)
		if length == 0 {
			continue
		}

		tree[n].fc = biReverse(nextCode[length], length)
		nextCode[length]++
	}
}

func (s *state) trInit() {
	s.lDesc = treeDesc{dynTree: s.dynLTree[:], statDesc: &staticLDesc}
	s.dDesc = treeDesc{dynTree: s.dynDTree[:], statDesc: &staticDDesc}
	s.blDesc = treeDesc{dynTree: s.blTree[:], statDesc: &staticBLDesc}
	s.bitBuf = 0
	s.bitCount = 0
	s.initBlock()
}

func (s *state) initBlock() {
	for n := range lCodes {
		s.dynLTree[n].fc = 0
	}

	for n := range dCodes {
		s.dynDTree[n].fc = 0
	}

	for n := range blCodes {
		s.blTree[n].fc = 0
	}

	s.dynLTree[endBlock].fc = 1
	s.optLen = 0
	s.staticLen = 0
	s.symNext = 0
	s.matches = 0
}

func smaller(tree []ctData, n, m int, depth []uint8) bool {
	return tree[n].fc < tree[m].fc || (tree[n].fc == tree[m].fc && depth[n] <= depth[m])
}

// pqdownheap restores the heap property by moving down the tree starting
// at node k, exchanging a node with the smallest of its two sons if
// necessary, stopping when the heap property is re-established.
func (s *state) pqdownheap(tree []ctData, k int) {
	v := s.heap[k]

	j := k << 1 // left son of k
	for j <= s.heapLen {
		if j < s.heapLen && smaller(tree, s.heap[j+1], s.heap[j], s.depth[:]) {
			j++
		}

		if smaller(tree, v, s.heap[j], s.depth[:]) {
			break
		}

		s.heap[k] = s.heap[j]
		k = j
		j <<= 1
	}

	s.heap[k] = v
}

func (s *state) pqremove(tree []ctData) int {
	top := s.heap[smallest]
	s.heap[smallest] = s.heap[s.heapLen]
	s.heapLen--
	s.pqdownheap(tree, smallest)

	return top
}

// genBitlen computes the optimal bit lengths for a tree and updates the
// total bit length for the current block.
//
//nolint:cyclop,funlen // kept structurally identical to zlib's gen_bitlen
func (s *state) genBitlen(desc *treeDesc) {
	tree := desc.dynTree
	maxCode := desc.maxCode
	stree := desc.statDesc.staticTree
	extra := desc.statDesc.extraBits
	base := desc.statDesc.extraBase
	maxLength := desc.statDesc.maxLength
	overflow := 0

	for bits := 0; bits <= maxBits; bits++ {
		s.blCount[bits] = 0
	}

	// In a first pass, compute the optimal bit lengths (which may
	// overflow in the case of the bit length tree).
	tree[s.heap[s.heapMax]].dl = 0 // root of the heap

	h := s.heapMax + 1
	for ; h < heapSize; h++ {
		n := s.heap[h]

		bits := int(tree[tree[n].dl].dl) + 1
		if bits > maxLength {
			bits = maxLength
			overflow++
		}

		tree[n].dl = uint16(bits) //nolint:gosec // bits <= maxLength <= 15
		// We overwrite tree[n].Dad which is no longer needed

		if n > maxCode {
			continue // not a leaf node
		}

		s.blCount[bits]++

		xbits := 0
		if n >= base {
			xbits = extra[n-base]
		}

		f := uint64(tree[n].fc)

		s.optLen += f * uint64(bits+xbits)
		if stree != nil {
			s.staticLen += f * uint64(int(stree[n].dl)+xbits) //nolint:gosec // non-negative
		}
	}

	if overflow == 0 {
		return
	}

	// Find the first bit length which could increase
	for {
		bits := maxLength - 1
		for s.blCount[bits] == 0 {
			bits--
		}

		s.blCount[bits]--      // move one leaf down the tree
		s.blCount[bits+1] += 2 // move one overflow item as its brother
		s.blCount[maxLength]--
		// The brother of the overflow item also moves one step up,
		// but this does not affect blCount[maxLength]
		overflow -= 2
		if overflow <= 0 {
			break
		}
	}

	// Now recompute all bit lengths, scanning in increasing frequency.
	for bits := maxLength; bits != 0; bits-- {
		n := int(s.blCount[bits])
		for n != 0 {
			h--

			m := s.heap[h]
			if m > maxCode {
				continue
			}

			if int(tree[m].dl) != bits {
				// This may be negative, relying on unsigned wraparound as zlib does.
				s.optLen += uint64((int64(bits) - int64(tree[m].dl)) * int64(tree[m].fc)) //nolint:gosec // see above
				tree[m].dl = uint16(bits)                                                 //nolint:gosec // bits <= 15
			}

			n--
		}
	}
}

// buildTree constructs one Huffman tree and assigns the code bit strings
// and lengths. It updates the total bit length for the current block.
//
//nolint:funlen // kept structurally identical to zlib's build_tree
func (s *state) buildTree(desc *treeDesc) {
	tree := desc.dynTree
	stree := desc.statDesc.staticTree
	elems := desc.statDesc.elems
	maxCode := -1

	// Construct the initial heap, with least frequent element in
	// heap[smallest]. The sons of heap[n] are heap[2*n] and heap[2*n+1].
	// heap[0] is not used.
	s.heapLen = 0
	s.heapMax = heapSize

	for n := range elems {
		if tree[n].fc != 0 {
			s.heapLen++
			s.heap[s.heapLen] = n
			maxCode = n
			s.depth[n] = 0
		} else {
			tree[n].dl = 0
		}
	}

	// The pkzip format requires that at least one distance code exists,
	// and that at least one bit should be sent even if there is only one
	// possible code. So to avoid special checks later on we force at least
	// two codes of non zero frequency.
	for s.heapLen < 2 {
		var node int

		if maxCode < 2 {
			maxCode++
			node = maxCode
		}

		s.heapLen++
		s.heap[s.heapLen] = node
		tree[node].fc = 1
		s.depth[node] = 0

		s.optLen--
		if stree != nil {
			s.staticLen -= uint64(stree[node].dl)
		}
		// node is 0 or 1 so it does not have extra bits
	}

	desc.maxCode = maxCode

	// The elements heap[heapLen/2+1 .. heapLen] are leaves of the tree,
	// establish sub-heaps of increasing lengths.
	for n := s.heapLen / 2; n >= 1; n-- {
		s.pqdownheap(tree, n)
	}

	// Construct the Huffman tree by repeatedly combining the least two
	// frequent nodes.
	node := elems // next internal node of the tree

	for {
		n := s.pqremove(tree) // n = node of least frequency
		m := s.heap[smallest] // m = node of next least frequency

		s.heapMax--
		s.heap[s.heapMax] = n // keep the nodes sorted by frequency
		s.heapMax--
		s.heap[s.heapMax] = m

		// Create a new node father of n and m
		tree[node].fc = tree[n].fc + tree[m].fc

		dn, dm := s.depth[n], s.depth[m]
		if dn >= dm {
			s.depth[node] = dn + 1
		} else {
			s.depth[node] = dm + 1
		}

		tree[n].dl = uint16(node) //nolint:gosec // node < heapSize
		tree[m].dl = uint16(node) //nolint:gosec // node < heapSize

		// and insert the new node in the heap
		s.heap[smallest] = node
		node++

		s.pqdownheap(tree, smallest)

		if s.heapLen < 2 {
			break
		}
	}

	s.heapMax--
	s.heap[s.heapMax] = s.heap[smallest]

	// At this point, the fields freq and dad are set. We can now
	// generate the bit lengths.
	s.genBitlen(desc)

	// The field len is now set, we can generate the bit codes
	genCodes(tree, maxCode, s.blCount[:])
}

// runLimits returns the maximum and minimum repeat counts for the run of
// code lengths following curlen, given the next length.
func runLimits(curlen, nextlen int) (int, int) {
	switch {
	case nextlen == 0:
		return 138, 3
	case curlen == nextlen:
		return 6, 3
	default:
		return 7, 4
	}
}

// scanTree scans a literal or distance tree to determine the frequencies
// of the codes in the bit length tree.
func (s *state) scanTree(tree []ctData, maxCode int) {
	prevlen := -1
	nextlen := int(tree[0].dl)
	count := 0
	maxCount, minCount := runLimits(prevlen, nextlen)

	tree[maxCode+1].dl = 0xffff // guard

	for n := 0; n <= maxCode; n++ {
		curlen := nextlen
		nextlen = int(tree[n+1].dl)

		count++

		switch {
		case count < maxCount && curlen == nextlen:
			continue
		case count < minCount:
			s.blTree[curlen].fc += uint16(count)
		case curlen != 0:
			if curlen != prevlen {
				s.blTree[curlen].fc++
			}

			s.blTree[rep3To6].fc++
		case count <= 10:
			s.blTree[repz3To10].fc++
		default:
			s.blTree[repz11To138].fc++
		}

		count = 0
		prevlen = curlen
		maxCount, minCount = runLimits(curlen, nextlen)
	}
}

// sendTree sends a literal or distance tree in compressed form, using the
// codes in blTree.
func (s *state) sendTree(tree []ctData, maxCode int) {
	// tree[maxCode+1].dl = -1 guard already set
	prevlen := -1
	nextlen := int(tree[0].dl)
	count := 0
	maxCount, minCount := runLimits(prevlen, nextlen)

	for n := 0; n <= maxCode; n++ {
		curlen := nextlen
		nextlen = int(tree[n+1].dl)

		count++

		switch {
		case count < maxCount && curlen == nextlen:
			continue
		case count < minCount:
			for ; count != 0; count-- {
				s.sendCode(curlen, s.blTree[:])
			}
		case curlen != 0:
			if curlen != prevlen {
				s.sendCode(curlen, s.blTree[:])

				count--
			}

			s.sendCode(rep3To6, s.blTree[:])
			s.sendBits(uint(count-3), 2)
		case count <= 10:
			s.sendCode(repz3To10, s.blTree[:])
			s.sendBits(uint(count-3), 3)
		default:
			s.sendCode(repz11To138, s.blTree[:])
			s.sendBits(uint(count-11), 7)
		}

		count = 0
		prevlen = curlen
		maxCount, minCount = runLimits(curlen, nextlen)
	}
}

// buildBLTree constructs the Huffman tree for the bit lengths and returns
// the index in blOrder of the last bit length code to send.
func (s *state) buildBLTree() int {
	// Determine the bit length frequencies for literal and distance trees
	s.scanTree(s.dynLTree[:], s.lDesc.maxCode)
	s.scanTree(s.dynDTree[:], s.dDesc.maxCode)

	// Build the bit length tree:
	s.buildTree(&s.blDesc)
	// optLen now includes the length of the tree representations, except
	// the lengths of the bit lengths codes and the 5+5+4 bits for the
	// counts.

	// Determine the number of bit length codes to send. The pkzip format
	// requires that at least 4 bit length codes be sent.
	maxBLIndex := blCodes - 1
	for ; maxBLIndex >= 3; maxBLIndex-- {
		if s.blTree[blOrder[maxBLIndex]].dl != 0 {
			break
		}
	}
	// Update optLen to include the bit length tree and counts
	s.optLen += 3*(uint64(maxBLIndex)+1) + 5 + 5 + 4

	return maxBLIndex
}

// sendAllTrees sends the header for a block using dynamic Huffman trees:
// the counts, the lengths of the bit length codes, the literal tree and
// the distance tree.
func (s *state) sendAllTrees(lcodes, dcodes, blcodes int) {
	s.sendBits(uint(lcodes-257), 5) // not +255 as stated in appnote.txt
	s.sendBits(uint(dcodes-1), 5)
	s.sendBits(uint(blcodes-4), 4) // not -3 as stated in appnote.txt

	for rank := range blcodes {
		s.sendBits(uint(s.blTree[blOrder[rank]].dl), 3)
	}

	s.sendTree(s.dynLTree[:], lcodes-1) // literal tree
	s.sendTree(s.dynDTree[:], dcodes-1) // distance tree
}

// trStoredBlock sends a stored block.
func (s *state) trStoredBlock(buf []byte, last bool) {
	s.sendBits(storedBlock<<1+b2u(last), 3) // send block type
	s.biWindup()                            // align on byte boundary

	n := len(buf)
	s.out = append(s.out, byte(n), byte(n>>8), byte(^n), byte(^n>>8)) //nolint:gosec // n <= 0xffff, truncation intended
	s.out = append(s.out, buf...)
}

// trFlushBlock determines the best encoding for the current block: dynamic
// trees, static trees or store, and writes out the encoded block. buf is
// nil if the block's input is no longer available in the window.
func (s *state) trFlushBlock(buf []byte, storedLen uint64, last bool) {
	// Construct the literal and distance trees
	s.buildTree(&s.lDesc)
	s.buildTree(&s.dDesc)
	// At this point, optLen and staticLen are the total bit lengths of
	// the compressed block data, excluding the tree representations.

	// Build the bit length tree for the above two trees, and get the
	// index in blOrder of the last bit length code to send.
	maxBLIndex := s.buildBLTree()

	// Determine the best encoding. Compute the block lengths in bytes.
	optLenb := (s.optLen + 3 + 7) >> 3
	staticLenb := (s.staticLen + 3 + 7) >> 3

	if staticLenb <= optLenb {
		optLenb = staticLenb
	}

	switch {
	case storedLen+4 <= optLenb && buf != nil:
		// 4: two words for the lengths
		//
		// The test buf != nil is only necessary if LIT_BUFSIZE > WSIZE.
		// Otherwise we can't have processed more than WSIZE input bytes
		// since the last block flush, because compression would have
		// been successful. If LIT_BUFSIZE <= WSIZE, it is never too late
		// to transform a block into a stored block.
		s.trStoredBlock(buf, last)
	case staticLenb == optLenb:
		s.sendBits(staticTrees<<1+b2u(last), 3)
		s.compressBlock(staticLTree[:], staticDTree[:])
	default:
		s.sendBits(dynTrees<<1+b2u(last), 3)
		s.sendAllTrees(s.lDesc.maxCode+1, s.dDesc.maxCode+1, maxBLIndex+1)
		s.compressBlock(s.dynLTree[:], s.dynDTree[:])
	}

	s.initBlock()

	if last {
		s.biWindup()
	}
}

// trTally saves the match info and tallies the frequency counts. Returns
// true if the current block must be flushed.
func (s *state) trTally(dist, lc int) bool {
	s.symBuf[s.symNext] = sym{dist: uint16(dist), lc: uint8(lc)} //nolint:gosec // dist <= 32K, lc <= 255

	s.symNext++
	if dist == 0 {
		// lc is the unmatched char
		s.dynLTree[lc].fc++
	} else {
		s.matches++
		// Here, lc is the match length - minMatch
		dist-- // dist = match distance - 1
		s.dynLTree[int(lengthCode[lc])+literals+1].fc++
		s.dynDTree[dCode(dist)].fc++
	}

	return s.symNext == litBufsize-1
}

// compressBlock sends the block data compressed using the given Huffman
// trees.
func (s *state) compressBlock(ltree, dtree []ctData) {
	for _, sy := range s.symBuf[:s.symNext] {
		dist := int(sy.dist)

		lc := int(sy.lc)
		if dist == 0 {
			s.sendCode(lc, ltree) // send a literal byte

			continue
		}
		// Here, lc is the match length - minMatch
		code := int(lengthCode[lc])
		s.sendCode(code+literals+1, ltree) // send the length code

		if extra := extraLBits[code]; extra != 0 {
			lc -= baseLength[code]
			s.sendBits(uint(lc), extra) // send the extra length bits
		}

		dist-- // dist is now the match distance - 1
		code = int(dCode(dist))
		s.sendCode(code, dtree) // send the distance code

		if extra := extraDBits[code]; extra != 0 {
			dist -= baseDist[code]
			s.sendBits(uint(dist), extra) // send the extra distance bits
		}
	}

	s.sendCode(endBlock, ltree)
}

func (s *state) sendCode(c int, tree []ctData) {
	s.sendBits(uint(tree[c].fc), int(tree[c].dl))
}

// sendBits appends length bits of value to the output, least significant
// bit first. This produces the same byte stream as zlib's 16-bit bi_buf.
func (s *state) sendBits(value uint, length int) {
	s.bitBuf |= uint64(value) << s.bitCount

	s.bitCount += uint(length)
	if s.bitCount >= 32 {
		b := s.bitBuf
		s.out = append(s.out, byte(b), byte(b>>8), byte(b>>16), byte(b>>24)) //nolint:gosec // truncation intended
		s.bitBuf >>= 32
		s.bitCount -= 32
	}
}

// biWindup flushes the bit buffer and aligns the output on a byte
// boundary.
func (s *state) biWindup() {
	for s.bitCount > 0 {
		s.out = append(s.out, byte(s.bitBuf)) //nolint:gosec // truncation intended

		s.bitBuf >>= 8
		if s.bitCount < 8 {
			s.bitCount = 0
		} else {
			s.bitCount -= 8
		}
	}

	s.bitBuf = 0
}

func b2u(b bool) uint {
	if b {
		return 1
	}

	return 0
}
