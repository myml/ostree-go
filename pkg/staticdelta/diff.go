package staticdelta

// This file implements the two content reuse schemes libostree applies when
// generating a static delta from an existing commit:
//
//   - bupsplit/rollsum chunk matching (src/libostree/ostree-rollsum.c and
//     bupsplit.c), emitted as OPEN/WRITE/SET_READ_SOURCE operations;
//   - binary diffs in the mendsley/bsdiff stream format used by libostree
//     (bsdiff/bspatch libraries), emitted as SET_READ_SOURCE/OPEN/BSPATCH.
//
// Both replace whole content objects in the part payload, which is what makes
// deltas between similar trees small.  The bspatch side is also needed to
// apply deltas produced by the C implementation.

import (
	"bytes"
	"fmt"
	"hash/crc32"
	"math"
	"sort"
)

// --- bupsplit -----------------------------------------------------------

const (
	bupBlobBits       = 13
	bupBlobSize       = 1 << bupBlobBits
	bupWindowBits     = 7
	bupWindowSize     = 1 << (bupWindowBits - 1)
	rollsumCharOffset = 31
	// rollsumBlobMax mirrors ROLLSUM_BLOB_MAX (8192 * 4).
	rollsumBlobMax = 8192 * 4
)

type rollsum struct {
	s1, s2 uint32
	window [bupWindowSize]byte
	wofs   int
}

func newRollsum() *rollsum {
	return &rollsum{
		s1: bupWindowSize * rollsumCharOffset,
		s2: bupWindowSize * (bupWindowSize - 1) * rollsumCharOffset,
	}
}

func (r *rollsum) add(drop, add byte) {
	r.s1 += uint32(add) - uint32(drop)
	r.s2 += r.s1 - bupWindowSize*(uint32(drop)+rollsumCharOffset)
}

func (r *rollsum) roll(ch byte) {
	r.add(r.window[r.wofs], ch)
	r.window[r.wofs] = ch
	r.wofs = (r.wofs + 1) % bupWindowSize
}

func (r *rollsum) digest() uint32 {
	return (r.s1 << 16) | (r.s2 & 0xffff)
}

// bupsplitFindOfs returns the offset (count+1) of the first content-defined
// chunk boundary in buf, or 0 when there is none.  It mirrors
// bupsplit_find_ofs().
func bupsplitFindOfs(buf []byte) int {
	r := newRollsum()
	for count := 0; count < len(buf); count++ {
		r.roll(buf[count])
		if r.s2&(bupBlobSize-1) == ^uint32(0)&(bupBlobSize-1) {
			return count + 1
		}
	}
	return 0
}

type rollChunk struct {
	crc   uint32
	start uint64
	size  uint64
}

// rollsumChunks splits data into bupsplit chunks and groups them by CRC, like
// rollsum_chunks_crc32().  The order slice lists each group's first
// appearance, matching the reference implementation's iteration order.
func rollsumChunks(data []byte) (map[uint32][]rollChunk, []uint32) {
	groups := map[uint32][]rollChunk{}
	var order []uint32
	start := 0
	rollsumEnd := false
	for remaining := len(data); remaining > 0; {
		var offset int
		if !rollsumEnd {
			offset = bupsplitFindOfs(data[start : start+minInt(remaining, math.MaxInt32)])
			if offset == 0 {
				rollsumEnd = true
				offset = minInt(rollsumBlobMax, remaining)
			} else if offset > rollsumBlobMax {
				offset = rollsumBlobMax
			}
		} else {
			offset = minInt(rollsumBlobMax, remaining)
		}
		// Note: libostree hashes data[0:offset] here, not
		// data[start:start+offset] (see rollsum_chunks_crc32()).  The
		// choice of delta optimization depends on it, so it is replicated
		// exactly; actual chunk equality is still verified with memcmp
		// below, so this cannot corrupt a generated delta.
		crc := crc32.ChecksumIEEE(data[:offset])
		if _, ok := groups[crc]; !ok {
			order = append(order, crc)
		}
		groups[crc] = append(groups[crc], rollChunk{
			crc: crc, start: uint64(start), size: uint64(offset),
		})
		start += offset
		remaining -= offset
	}
	return groups, order
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// rollsumMatch is one chunk of the new content copied from the old content.
type rollsumMatch struct {
	size      uint64
	toStart   uint64
	fromStart uint64
}

// rollsumMatches holds the result of comparing two content objects.
type rollsumMatches struct {
	matches    []rollsumMatch
	crcMatches uint64
	bufMatches uint64
	total      uint64
	matchSize  uint64
}

// computeRollsumMatches mirrors _ostree_compute_rollsum_matches().
func computeRollsumMatches(from, to []byte) *rollsumMatches {
	fromChunks, _ := rollsumChunks(from)
	toChunks, toOrder := rollsumChunks(to)
	res := &rollsumMatches{}
	for _, crc := range toOrder {
		tchunks := toChunks[crc]
		fchunks, ok := fromChunks[crc]
		if ok {
			res.crcMatches++
			for _, tc := range tchunks {
				for _, fc := range fchunks {
					if tc.size != fc.size {
						continue
					}
					if bytes.Equal(from[fc.start:fc.start+fc.size], to[tc.start:tc.start+tc.size]) {
						res.matches = append(res.matches, rollsumMatch{
							size:      tc.size,
							toStart:   tc.start,
							fromStart: fc.start,
						})
						res.bufMatches++
						res.matchSize += tc.size
						break
					}
				}
			}
		}
		res.total += uint64(len(tchunks))
	}
	sort.Slice(res.matches, func(i, j int) bool {
		return res.matches[i].toStart < res.matches[j].toStart
	})
	return res
}

// --- suffix sorting and binary diff ------------------------------------

// offtout serializes one mendsley/bsdiff stream integer: 8 bytes, little
// endian magnitude with the sign in the top bit.
func offtout(x int64, buf []byte) {
	y := x
	if y < 0 {
		y = -y
	}
	for i := 0; i < 8; i++ {
		buf[i] = byte(y)
		y >>= 8
	}
	if x < 0 {
		buf[7] |= 0x80
	}
}

// offtin is the inverse of offtout (and matches offtin() in bspatch.c).
func offtin(buf []byte) int64 {
	y := int64(buf[7] & 0x7f)
	for i := 6; i >= 0; i-- {
		y = y*256 + int64(buf[i])
	}
	if buf[7]&0x80 != 0 {
		y = -y
	}
	return y
}

// vget reads V[i], treating an out-of-range index as the empty suffix's rank
// (0).  The reference implementation can read past V once h exceeds the
// longest common prefix; this keeps the sort well defined.
func vget(V []int64, i int64) int64 {
	if i < 0 || i >= int64(len(V)) {
		return 0
	}
	return V[i]
}

// splitSuffixes mirrors split() from the reference qsufsort.
func splitSuffixes(I, V []int64, start, length, h int64) {
	if length < 16 {
		k := start
		for k < start+length {
			j := int64(1)
			x := vget(V, I[k]+h)
			for i := int64(1); k+i < start+length; i++ {
				v := vget(V, I[k+i]+h)
				if v < x {
					x = v
					j = 0
				}
				if v == x {
					I[k+j], I[k+i] = I[k+i], I[k+j]
					j++
				}
			}
			for i := int64(0); i < j; i++ {
				V[I[k+i]] = k + j - 1
			}
			if j == 1 {
				I[k] = -1
			}
			k += j
		}
		return
	}

	x := vget(V, I[start+length/2]+h)
	var jj, kk int64
	for i := start; i < start+length; i++ {
		v := vget(V, I[i]+h)
		if v < x {
			jj++
		} else if v == x {
			kk++
		}
	}
	jj += start
	kk += jj

	i, j, k := start, int64(0), int64(0)
	for i < jj {
		v := vget(V, I[i]+h)
		if v < x {
			i++
		} else if v == x {
			I[i], I[jj+j] = I[jj+j], I[i]
			j++
		} else {
			I[i], I[kk+k] = I[kk+k], I[i]
			k++
		}
	}
	for jj+j < kk {
		if vget(V, I[jj+j]+h) == x {
			j++
		} else {
			I[jj+j], I[kk+k] = I[kk+k], I[jj+j]
			k++
		}
	}

	if jj > start {
		splitSuffixes(I, V, start, jj-start, h)
	}
	for i := int64(0); i < kk-jj; i++ {
		V[I[jj+i]] = kk - 1
	}
	if jj == kk-1 {
		I[jj] = -1
	}
	if start+length > kk {
		splitSuffixes(I, V, kk, start+length-kk, h)
	}
}

// qsufsort builds the suffix array I (with scratch ranks in V) of old.
func qsufsort(I, V []int64, old []byte) {
	oldsize := int64(len(old))
	var buckets [256]int64
	for _, b := range old {
		buckets[b]++
	}
	for i := 1; i < 256; i++ {
		buckets[i] += buckets[i-1]
	}
	for i := 255; i > 0; i-- {
		buckets[i] = buckets[i-1]
	}
	buckets[0] = 0

	for i := int64(0); i < oldsize; i++ {
		buckets[old[i]]++
		I[buckets[old[i]]] = i
	}
	I[0] = oldsize
	for i := int64(0); i < oldsize; i++ {
		V[i] = buckets[old[i]]
	}
	V[oldsize] = 0
	for i := 1; i < 256; i++ {
		if buckets[i] == buckets[i-1]+1 {
			I[buckets[i]] = -1
		}
	}
	I[0] = -1

	for h := int64(1); I[0] != -(oldsize + 1); h += h {
		var length int64
		var i int64
		for i = 0; i < oldsize+1; {
			if I[i] < 0 {
				length -= I[i]
				i -= I[i]
				continue
			}
			if length != 0 {
				I[i-length] = -length
			}
			length = V[I[i]] + 1 - i
			splitSuffixes(I, V, i, length, h)
			i += length
			length = 0
		}
		if length != 0 {
			I[i-length] = -length
		}
	}
	for i := int64(0); i < oldsize+1; i++ {
		I[V[i]] = i
	}
}

func matchlenSuffix(old, new []byte) int64 {
	var i int64
	for i < int64(len(old)) && i < int64(len(new)) && old[i] == new[i] {
		i++
	}
	return i
}

func searchSuffix(I []int64, old, new []byte, st, en int64) (int64, int64) {
	if en-st < 2 {
		x := matchlenSuffix(old[I[st]:], new)
		y := matchlenSuffix(old[I[en]:], new)
		if x > y {
			return x, I[st]
		}
		return y, I[en]
	}
	x := st + (en-st)/2
	oldsize := int64(len(old))
	newsize := int64(len(new))
	n := oldsize - I[x]
	if newsize < n {
		n = newsize
	}
	if bytes.Compare(old[I[x]:I[x]+n], new[:n]) < 0 {
		return searchSuffix(I, old, new, x, en)
	}
	return searchSuffix(I, old, new, st, x)
}

// bsdiff produces a binary patch in the raw stream format written by the
// mendsley/bsdiff library used by libostree (no outer compression, no
// signature): a sequence of [ctrl: 3x int64][diff bytes][extra bytes].
func bsdiff(old, new []byte) []byte {
	oldsize := int64(len(old))
	newsize := int64(len(new))
	I := make([]int64, oldsize+1)
	V := make([]int64, 2*(oldsize+1))
	qsufsort(I, V, old)

	var out []byte
	var ctl [8]byte
	writeCtl := func(x int64) {
		offtout(x, ctl[:])
		out = append(out, ctl[:]...)
	}

	var scan, length, pos int64
	var lastscan, lastpos, lastoffset int64

	for scan < newsize {
		oldscore := int64(0)
		scan += length
		scsc := scan
		for ; scan < newsize; scan++ {
			length, pos = searchSuffix(I, old, new[scan:], 0, oldsize)
			for ; scsc < scan+length; scsc++ {
				if scsc+lastoffset < oldsize && scsc+lastoffset >= 0 &&
					old[scsc+lastoffset] == new[scsc] {
					oldscore++
				}
			}
			if (length == oldscore && length != 0) || length > oldscore+8 {
				break
			}
			if scan+lastoffset < oldsize && scan+lastoffset >= 0 &&
				old[scan+lastoffset] == new[scan] {
				oldscore--
			}
		}

		if length != oldscore || scan == newsize {
			var s, Sf, lenf int64
			for i := int64(0); lastscan+i < scan && lastpos+i < oldsize; {
				if old[lastpos+i] == new[lastscan+i] {
					s++
				}
				i++
				if s*2-i > Sf*2-lenf {
					Sf = s
					lenf = i
				}
			}

			var lenb, Sb int64
			if scan < newsize {
				s = 0
				for i := int64(1); scan >= lastscan+i && pos >= i; i++ {
					if old[pos-i] == new[scan-i] {
						s++
					}
					if s*2-i > Sb*2-lenb {
						Sb = s
						lenb = i
					}
				}
			}

			if lastscan+lenf > scan-lenb {
				overlap := (lastscan + lenf) - (scan - lenb)
				s = 0
				var Ss, lens int64
				for i := int64(0); i < overlap; i++ {
					if new[lastscan+lenf-overlap+i] == old[lastpos+lenf-overlap+i] {
						s++
					}
					if new[scan-lenb+i] == old[pos-lenb+i] {
						s--
					}
					if s > Ss {
						Ss = s
						lens = i + 1
					}
				}
				lenf += lens - overlap
				lenb -= lens
			}

			writeCtl(lenf)
			writeCtl((scan - lenb) - (lastscan + lenf))
			writeCtl((pos - lenb) - (lastpos + lenf))
			for i := int64(0); i < lenf; i++ {
				out = append(out, new[lastscan+i]-old[lastpos+i])
			}
			out = append(out, new[lastscan+lenf:scan-lenb]...)

			lastscan = scan - lenb
			lastpos = pos - lenb
			lastoffset = pos - scan
		}
	}
	return out
}

// bspatch applies a mendsley/bsdiff stream to old, producing exactly newsize
// bytes.  It mirrors bspatch() from the same library.
func bspatch(old []byte, newsize int64, patch []byte) ([]byte, error) {
	if newsize < 0 {
		return nil, fmt.Errorf("static delta: invalid bspatch output size %d", newsize)
	}
	out := make([]byte, newsize)
	oldsize := int64(len(old))
	var oldpos, newpos int64
	for newpos < newsize {
		if len(patch) < 24 {
			return nil, fmt.Errorf("static delta: truncated bspatch control block")
		}
		ctrl := [3]int64{
			offtin(patch[0:8]), offtin(patch[8:16]), offtin(patch[16:24]),
		}
		patch = patch[24:]
		if ctrl[0] < 0 || ctrl[0] > math.MaxInt32 ||
			ctrl[1] < 0 || ctrl[1] > math.MaxInt32 ||
			newpos+ctrl[0] > newsize {
			return nil, fmt.Errorf("static delta: invalid bspatch control block")
		}
		if int64(len(patch)) < ctrl[0] {
			return nil, fmt.Errorf("static delta: truncated bspatch diff block")
		}
		copy(out[newpos:newpos+ctrl[0]], patch[:ctrl[0]])
		patch = patch[ctrl[0]:]
		for i := int64(0); i < ctrl[0]; i++ {
			if oldpos+i >= 0 && oldpos+i < oldsize {
				out[newpos+i] += old[oldpos+i]
			}
		}
		newpos += ctrl[0]
		oldpos += ctrl[0]
		if newpos+ctrl[1] > newsize {
			return nil, fmt.Errorf("static delta: invalid bspatch extra length")
		}
		if int64(len(patch)) < ctrl[1] {
			return nil, fmt.Errorf("static delta: truncated bspatch extra block")
		}
		copy(out[newpos:newpos+ctrl[1]], patch[:ctrl[1]])
		patch = patch[ctrl[1]:]
		newpos += ctrl[1]
		oldpos += ctrl[2]
	}
	return out, nil
}
