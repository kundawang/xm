package xm

import (
	"encoding/binary"
)

// This file contains a small synthetic XM module builder used by the
// channel timeline tests. It emits real binary XM data that goes
// through the xmfile parser and the module compiler, so the tests
// exercise the full decoding pipeline.

type xmFixtureNote struct {
	note       uint8
	instrument uint8
	volume     uint8
	effect     uint8
	effectArg  uint8
}

type xmFixtureInstrument struct {
	empty bool

	volume  int
	panning int

	sampleLength int
	loopStart    int
	loopLength   int

	// sampleAt generates sample i (a value in the int16 range).
	sampleAt func(i int) int16
}

type xmFixture struct {
	name        string
	channels    int
	bpm         int
	tempo       int
	numRows     int
	order       []uint8
	patterns    [][]xmFixtureNote // every pattern has numRows*channels notes
	instruments []xmFixtureInstrument
}

func newXMFixture() *xmFixture {
	return &xmFixture{
		name:     "timeline-test",
		channels: 2,
		bpm:      125,
		tempo:    3,
		numRows:  4,
	}
}

func (f *xmFixture) emptyNote() xmFixtureNote {
	return xmFixtureNote{}
}

func (f *xmFixture) bytes() []byte {
	var b []byte
	put := func(data ...byte) { b = append(b, data...) }
	putWord := func(v uint16) { b = binary.LittleEndian.AppendUint16(b, v) }
	putDword := func(v uint32) { b = binary.LittleEndian.AppendUint32(b, v) }
	putString := func(s string, l int) {
		var buf [256]byte
		copy(buf[:], s)
		b = append(b, buf[:l]...)
	}

	// ---- Module header (fixed part is 17+20+1+20+2=60 bytes) ----
	putString("Extended Module: ", 17)
	putString(f.name, 20)
	put(0x1a)
	putString("go-test", 20)
	putWord(0x0104) // version 1.4

	// The parser computes header end as current offset + (headerSize-4).
	// The remaining header holds 8 words and the order table.
	headerBodySize := uint32(8*2 + len(f.order))
	headerDwordPos := len(b)
	putDword(4 + headerBodySize)

	putWord(uint16(len(f.order)))    // song length
	putWord(0)                       // restart position
	putWord(uint16(f.channels))      // number of channels
	putWord(uint16(len(f.patterns))) // number of patterns
	putWord(uint16(len(f.instruments)))
	putWord(1) // flags: linear frequency table
	putWord(uint16(f.tempo))
	putWord(uint16(f.bpm))
	put(f.order...)

	// Everything after the header size dword must match its declared body.
	if got := len(b) - headerDwordPos - 4; uint32(got) != headerBodySize {
		panic("xm fixture header size mismatch")
	}

	// ---- Patterns ----
	for _, pat := range f.patterns {
		if len(pat) != f.numRows*f.channels {
			panic("xm fixture pattern note count mismatch")
		}
		var packed []byte
		for _, n := range pat {
			// Always use the explicit 5-byte form; note values below
			// 0x80 would otherwise be mistaken for compact headers.
			packed = append(packed, n.note, n.instrument, n.volume, n.effect, n.effectArg)
		}
		putDword(9) // pattern header length
		put(0)      // packing type
		putWord(uint16(f.numRows))
		putWord(uint16(len(packed)))
		b = append(b, packed...)
	}

	// ---- Instruments ----
	for instIndex, inst := range f.instruments {
		// Build the instrument header in a sub-buffer so its declared
		// size always matches the bytes that follow it.
		var hdr []byte
		hPut := func(data ...byte) { hdr = append(hdr, data...) }
		hPutWord := func(v uint16) { hdr = binary.LittleEndian.AppendUint16(hdr, v) }
		hPutDword := func(v uint32) { hdr = binary.LittleEndian.AppendUint32(hdr, v) }
		hPutString := func(s string, l int) {
			var buf [256]byte
			copy(buf[:], s)
			hdr = append(hdr, buf[:l]...)
		}

		if inst.empty {
			hPutString("empty", 22)
			hPut(0)     // instrument type
			hPutWord(0) // number of samples
			putDword(uint32(4 + len(hdr)))
			b = append(b, hdr...)
			continue
		}

		hPutString("inst", 22)
		hPut(0)                                // instrument type
		hPutWord(1)                            // number of samples
		hPutDword(44)                          // sample header size
		hdr = append(hdr, make([]byte, 96)...) // keymap assignments

		// 12 volume envelope points + 12 panning envelope points.
		for i := 0; i < 12; i++ {
			hPutWord(0)
			hPutWord(0)
		}
		for i := 0; i < 12; i++ {
			hPutWord(0)
			hPutWord(32) // centered panning
		}

		hPut(0)          // num volume envelope points
		hPut(0)          // num panning envelope points
		hPut(0, 0, 0)    // volume sustain/loop start/loop end
		hPut(0, 0, 0)    // panning sustain/loop start/loop end
		hPut(0)          // volume flags
		hPut(0)          // panning flags
		hPut(0, 0, 0, 0) // vibrato type/sweep/depth/rate
		hPutWord(0)      // volume fadeout

		putDword(uint32(4 + len(hdr)))
		b = append(b, hdr...)

		// ---- Sample header (40 bytes; declared size is 44) ----
		sampleHeaderStart := len(b)
		putDword(uint32(inst.sampleLength))
		putDword(uint32(inst.loopStart))
		putDword(uint32(inst.loopLength))
		put(uint8(inst.volume))
		put(0) // finetune
		loopType := byte(0)
		if inst.loopLength > 0 {
			loopType = 1 // forward loop
		}
		put(loopType)
		put(uint8(inst.panning))
		put(0) // relative note
		put(0) // format: delta packed
		putString("sample", 22)
		if got := len(b) - sampleHeaderStart; got != 40 {
			panic("xm fixture sample header size mismatch")
		}

		// ---- 8-bit delta-packed sample data ----
		var prev int8
		for i := 0; i < inst.sampleLength; i++ {
			v := int8(inst.sampleAt(i) >> 8)
			delta := v - prev
			put(byte(delta))
			prev = v
		}
		_ = instIndex
	}

	return b
}
