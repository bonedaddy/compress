package fse

import (
	"fmt"
	"github.com/pkg/errors"
	"math"
	"math/bits"
)

const (
	/*!MEMORY_USAGE :
	 *  Memory usage formula : N->2^N Bytes (examples : 10 -> 1KB; 12 -> 4KB ; 16 -> 64KB; 20 -> 1MB; etc.)
	 *  Increasing memory usage improves compression ratio
	 *  Reduced memory usage can improve speed, due to cache effect
	 *  Recommended max value is 14, for 16KB, which nicely fits into Intel x86 L1 cache */
	maxMemoryUsage     = 14
	defaultMemoryUsage = 13

	maxTableLog      = maxMemoryUsage - 2
	maxTablesize     = 1 << maxTableLog
	maxtablesizeMask = maxTablesize - 1
	defaultTablelog  = defaultMemoryUsage - 2
	minTablelog      = 5
	maxSymbolValue   = 255
	ctableSize       = 1 + (1 << (maxTableLog - 1)) + (maxSymbolValue + 1)
)

type Scratch struct {
	// Private
	count          [maxSymbolValue + 1]uint32
	norm           [maxSymbolValue + 1]int16
	cTable         []uint32 // May contain values from last run.
	clearCount     bool     // clear count
	length         int      // input length
	symbolLen      uint16
	actualTableLog uint8

	// Out is output buffer
	Out []byte

	// Per block parameters
	MaxSymbolValue uint8
	TableLog       uint8
}

func (s *Scratch) prepare(in []byte) (*Scratch, error) {
	if s == nil {
		s = &Scratch{}
	}
	s.length = len(in)
	if s.MaxSymbolValue == 0 {
		s.MaxSymbolValue = 255
	}
	if s.TableLog == 0 {
		s.TableLog = defaultTablelog
	}
	if s.TableLog > maxTableLog {
		return nil, fmt.Errorf("tableLog (%d) > maxTableLog (%d)", s.TableLog, maxTableLog)
	}
	if cap(s.Out) == 0 {
		s.Out = make([]byte, 0, len(in))
	}
	if s.clearCount {
		for i := range s.count {
			s.count[i] = 0
		}
		s.clearCount = false
	}
	cTableSize := 1 + (1 << (uint(s.TableLog) - 1)) + ((int(s.MaxSymbolValue) + 1) * 2)
	if cap(s.cTable) < cTableSize {
		s.cTable = make([]uint32, 0, cTableSize)
	}
	s.cTable = s.cTable[:cTableSize]
	return s, nil
}

// countSimple will create a simple histogram in s.count
// Returns the biggest count.
func (s *Scratch) countSimple(in []byte) (max int) {
	s.clearCount = true
	for _, v := range in {
		s.count[v]++
	}
	m := uint32(0)
	for i, v := range s.count[:] {
		if v > m {
			m = v
		}
		if v > 0 {
			s.symbolLen = uint16(i) + 1
		}
	}
	return int(m)
}

// minTableLog provides the minimum logSize to safely represent a distribution.
func (s *Scratch) minTableLog() uint8 {
	minBitsSrc := bits.Len32(uint32(s.length-1)) + 1
	minBitsSymbols := bits.Len32(uint32(s.symbolLen-1)) + 2
	if minBitsSrc < minBitsSymbols {
		return uint8(minBitsSrc)
	}
	return uint8(minBitsSymbols)
}

// optimalTableLog calculates and sets the optimal tableLog in s.actualTableLog
func (s *Scratch) optimalTableLog() {
	tableLog := s.TableLog
	minBits := s.minTableLog()
	maxBitsSrc := uint8(bits.Len32(uint32(s.length - 2)))
	if maxBitsSrc < s.actualTableLog {
		// Accuracy can be reduced
		s.actualTableLog = maxBitsSrc
	}
	if minBits > tableLog {
		tableLog = minBits
	}
	/* Need a minimum to safely represent all symbol values */
	if tableLog < minTablelog {
		tableLog = minTablelog
	}
	if tableLog > maxTableLog {
		tableLog = maxTableLog
	}
	s.actualTableLog = tableLog
}

var rtbTable = [...]uint32{0, 473195, 504333, 520860, 550000, 700000, 750000, 830000}

func (s *Scratch) normalizeCount() error {
	var (
		tableLog          = s.actualTableLog
		scale             = 62 - uint64(tableLog)
		step              = (1 << 62) / uint64(s.length)
		vStep             = uint64(1) << (scale - 20)
		stillToDistribute = int16(1 << tableLog)
		largest           int
		largestP          int16
		lowThreshold      = (uint32)(s.length >> tableLog)
	)

	for i, cnt := range s.count[:s.symbolLen] {
		// already handled
		// if (count[s] == s.length) return 0;   /* rle special case */

		if cnt == 0 {
			s.norm[i] = 0
			continue
		}
		if cnt <= lowThreshold {
			s.norm[i] = -1
			stillToDistribute--
		} else {
			proba := (int16)((uint64(cnt) * step) >> scale)
			if proba < 8 {
				restToBeat := vStep * uint64(rtbTable[proba])
				v := uint64(cnt)*step - (uint64(proba) << scale)
				if v > restToBeat {
					proba++
				}
			}
			if proba > largestP {
				largestP = proba
				largest = i
			}
			s.norm[i] = proba
			stillToDistribute -= proba
		}
	}

	if -stillToDistribute >= (s.norm[largest] >> 1) {
		// corner case, need another normalization method
		return s.normalizeCount2()
	} else {
		s.norm[largest] += stillToDistribute
	}
	return nil
}

// writeCount will write the count to header.
func (s *Scratch) writeCount() error {
	var (
		tableLog  = s.actualTableLog
		tableSize = 1 << tableLog
		previous0 bool
		charnum   uint8

		maxHeaderSize = ((int(s.symbolLen) * int(tableLog)) >> 3) + 3

		// Write Table Size
		bitStream = uint32(tableLog - minTablelog)
		bitCount  = uint(4)
		remaining = int16(tableSize + 1) /* +1 for extra accuracy */
		threshold = int16(tableSize)
		nbBits    = uint(tableLog + 1)
	)
	if cap(s.Out) < maxHeaderSize {
		s.Out = make([]byte, 0, s.length+maxHeaderSize)
	}
	outP := uint(0)
	out := s.Out[:maxHeaderSize]

	for remaining > 1 { /* stops at 1 */
		if previous0 {
			start := charnum
			for s.norm[charnum] == 0 {
				charnum++
			}
			for charnum >= start+24 {
				start += 24
				bitStream += uint32(0xFFFF) << bitCount
				out[outP] = byte(bitStream)
				out[outP+1] = byte(bitStream >> 8)
				outP += 2
				bitStream >>= 16
			}
			for charnum >= start+3 {
				start += 3
				bitStream += 3 << bitCount
				bitCount += 2
			}
			bitStream += uint32(charnum-start) << bitCount
			bitCount += 2
			if bitCount > 16 {
				out[outP] = byte(bitStream)
				out[outP+1] = byte(bitStream >> 8)
				outP += 2
				bitStream >>= 16
				bitCount -= 16
			}
		}

		count := s.norm[charnum]
		charnum++
		max := (2*threshold - 1) - remaining
		if count < 0 {
			remaining += count
		} else {
			remaining -= count
		}
		count++ // +1 for extra accuracy
		if count >= threshold {
			count += max // [0..max[ [max..threshold[ (...) [threshold+max 2*threshold[
		}
		bitStream += uint32(count) << bitCount
		bitCount += nbBits
		if count < max {
			bitCount--
		}

		previous0 = count == 1
		if remaining < 1 {
			return errors.New("internal error: remaining<1")
		}
		for remaining < threshold {
			nbBits--
			threshold >>= 1
		}

		if bitCount > 16 {
			out[outP] = byte(bitStream)
			out[outP+1] = byte(bitStream >> 8)
			outP += 2
			bitStream >>= 16
			bitCount -= 16
		}
	}

	out[outP] = byte(bitStream)
	out[outP+1] = byte(bitStream >> 8)
	outP += (bitCount + 7) / 8

	if uint16(charnum) > s.symbolLen {
		return errors.New("internal error: charnum > s.symbolLen")
	}
	s.Out = out[:outP]
	return nil
}

// Secondary normalization method.
// To be used when primary method fails.
// TODO: Find data that triggers this.
func (s *Scratch) normalizeCount2() error {
	const notYetAssigned = -2
	var (
		distributed  uint32
		total        = uint32(s.length)
		tableLog     = s.actualTableLog
		lowThreshold = uint32(total >> tableLog)
		lowOne       = uint32((total * 3) >> (tableLog + 1))
	)
	for i, cnt := range s.count[:s.symbolLen] {
		if cnt == 0 {
			s.norm[i] = 0
			continue
		}
		if cnt <= lowThreshold {
			s.norm[i] = -1
			distributed++
			total -= cnt
			continue
		}
		if cnt <= lowOne {
			s.norm[i] = 1
			distributed++
			total -= cnt
			continue
		}
		s.norm[i] = notYetAssigned
	}
	toDistribute := (1 << tableLog) - distributed

	if (total / toDistribute) > lowOne {
		// risk of rounding to zero
		lowOne = uint32((total * 3) / (toDistribute * 2))
		for i, cnt := range s.count[:s.symbolLen] {
			if (s.norm[i] == notYetAssigned) && (cnt <= lowOne) {
				s.norm[i] = 1
				distributed++
				total -= cnt
				continue
			}
		}
		toDistribute = (1 << tableLog) - distributed
	}
	if distributed == uint32(s.symbolLen)+1 {
		// all values are pretty poor;
		//   probably incompressible data (should have already been detected);
		//   find max, then give all remaining points to max
		var maxV int
		var maxC uint32
		for i, cnt := range s.count[:s.symbolLen] {
			if cnt > maxC {
				maxV = i
				maxC = cnt
			}
		}
		s.norm[maxV] += int16(toDistribute)
		return nil
	}

	if total == 0 {
		// all of the symbols were low enough for the lowOne or lowThreshold
		for i := uint32(0); toDistribute > 0; i = (i + 1) % (uint32(s.symbolLen)) {
			if s.norm[i] > 0 {
				toDistribute--
				s.norm[i]++
			}
		}
		return nil
	}

	var (
		vStepLog = 62 - uint64(tableLog)
		mid      = uint64((1 << (vStepLog - 1)) - 1)
		rStep    = (((1 << vStepLog) * uint64(toDistribute)) + mid) / uint64(total) // scale on remaining
		tmpTotal = mid
	)
	for i, cnt := range s.count[:s.symbolLen] {
		if s.norm[i] == notYetAssigned {
			var (
				end    = tmpTotal + uint64(cnt)*rStep
				sStart = uint32(tmpTotal >> vStepLog)
				sEnd   = uint32(end >> vStepLog)
				weight = sEnd - sStart
			)
			if weight < 1 {
				return errors.New("weight < 1")
			}
			s.norm[i] = int16(weight)
			tmpTotal = end
		}
	}
	return nil
}

func (s *Scratch) log() error {
	var total int
	fmt.Printf("selected TableLog: %d, Symbol length: %d\n", s.actualTableLog, s.symbolLen)
	for i, v := range s.norm[:s.symbolLen] {
		if v >= 0 {
			total += int(v)
		} else {
			total -= int(v)
		}
		fmt.Printf("%3d: %5d -> %4d \n", i, s.count[i], v)
	}
	if total != (1 << s.actualTableLog) {
		return fmt.Errorf("warning: Total == %d != %d", total, 1<<s.actualTableLog)
	}
	for i, v := range s.count[s.symbolLen:] {
		if v != 0 {
			return fmt.Errorf("warning: Found symbol out of range, %d after cut", i)
		}
	}
	return nil
}

func Compress(in []byte, s *Scratch) ([]byte, error) {
	if len(in) <= 1 {
		return nil, nil
	}
	if len(in) > math.MaxUint32 {
		return nil, errors.New("input too big, must be < 2GB")
	}
	s, err := s.prepare(in)
	if err != nil {
		return nil, err
	}

	// Create histogram
	maxCount := s.countSimple(in)
	if maxCount == len(in) {
		// One symbol, use RLE
		// TODO: ???
	}
	if maxCount == 1 || maxCount < (len(in)>>7) {
		// Each symbol present maximum once or too well distributed.
		// Uncompressible.
		return nil, nil
	}
	s.optimalTableLog()
	err = s.normalizeCount()
	if err != nil {
		return nil, err
	}
	err = s.writeCount()
	if err != nil {
		return nil, err
	}

	return s.Out, s.log()
}
