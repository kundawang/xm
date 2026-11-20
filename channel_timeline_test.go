package xm

import (
	"bytes"
	"hash/fnv"
	"io"
	"math"
	"testing"

	"github.com/quasilyte/xm/internal/xmdb"
	"github.com/quasilyte/xm/xmfile"
)

// XM note numbers used by the fixtures.
const (
	xmNoteC4     = 49
	xmNoteE4     = 53
	xmNoteG4     = 55
	xmNoteKeyOff = 97
)

const fixtureTicksPerRow = 3

// parseFixture runs the real xmfile parser over the fixture bytes.
func parseFixture(t *testing.T, data []byte) *xmfile.Module {
	t.Helper()
	p := xmfile.NewParser(xmfile.ParserConfig{})
	m, err := p.ParseFromBytes(data)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return m
}

func newTestStream(t *testing.T, m *xmfile.Module) *Stream {
	t.Helper()
	s := NewStream()
	if err := s.LoadModule(m, LoadModuleConfig{}); err != nil {
		t.Fatalf("load module: %v", err)
	}
	return s
}

// readTickBuf returns a buffer large enough for a whole tick
// (Read requires len(b) > bytesPerTick).
func readTickBuf(s *Stream) []byte {
	return make([]byte, s.GetInfo().BytesPerTick*2)
}

// readAll renders the whole module and returns the PCM bytes.
func readAll(t *testing.T, s *Stream) []byte {
	t.Helper()
	buf := readTickBuf(s)
	var pcm []byte
	for {
		n, err := s.Read(buf)
		pcm = append(pcm, buf[:n]...)
		if err == io.EOF {
			return pcm
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
}

// pcmHash collapses PCM bytes into a stable 64-bit golden value.
func pcmHash(pcm []byte) uint64 {
	h := fnv.New64a()
	h.Write(pcm)
	return h.Sum64()
}

// buildTimelineFixture creates the main fixture containing a pattern
// break jump, an EDx note delay, a key-off sustain and a note cut.
//
// Played rows (tempo 3):
//
//	pattern 0: row 0 (two notes), row 1 (EDx delay + D01 break)
//	pattern 1: row 1 (key-off + EC2 cut), row 2 (bad instrument), row 3
func buildTimelineFixture() *xmFixture {
	f := newXMFixture()
	f.order = []uint8{0, 1}

	mk := func(notes ...xmFixtureNote) []xmFixtureNote {
		row := make([]xmFixtureNote, f.channels)
		copy(row, notes)
		return row
	}

	// Pattern 0: pattern break on row 1 skips rows 2-3.
	pat0 := []xmFixtureNote{}
	pat0 = append(pat0, mk(
		// Volume column 0x50 => EffectSetVolume arg 64 => 1.0.
		xmFixtureNote{note: xmNoteC4, instrument: 1, volume: 0x50},
		// Volume column 0x30 => EffectSetVolume arg 32 => 0.5.
		xmFixtureNote{note: xmNoteE4, instrument: 1, volume: 0x30},
	)...)
	pat0 = append(pat0, mk(
		// EDx note delay: G-4 triggers on tick 2 (ED2 = effect 0x0E, arg 0xD2).
		xmFixtureNote{note: xmNoteG4, instrument: 1, volume: 0x50, effect: 0x0E, effectArg: 0xD2},
		// Dxx pattern break to next pattern, row 1 (BCD 01).
		xmFixtureNote{effect: 0x0D, effectArg: 0x01},
	)...)
	pat0 = append(pat0, mk()...) // rows 2 and 3 are never played
	pat0 = append(pat0, mk()...)

	// Pattern 1 (the break lands directly on row 1):
	// row 0: never played
	// row 1: key-off on channel 0, note cut tick 2 on channel 1
	// row 2: bad instrument index on channel 0
	// row 3: empty (zero-sample) instrument on channel 0, plain note on 1
	pat1 := []xmFixtureNote{}
	pat1 = append(pat1, mk()...)
	pat1 = append(pat1, mk(
		xmFixtureNote{note: xmNoteKeyOff},
		xmFixtureNote{note: xmNoteC4, instrument: 1, effect: 0x0E, effectArg: 0xC2},
	)...)
	pat1 = append(pat1, mk(
		xmFixtureNote{note: xmNoteC4, instrument: 42}, // instrument 42 does not exist
	)...)
	pat1 = append(pat1, mk(
		xmFixtureNote{note: xmNoteE4, instrument: 2}, // instrument 2 exists but has no samples
		xmFixtureNote{note: xmNoteG4, instrument: 1, volume: 0x40},
	)...)

	f.patterns = [][]xmFixtureNote{pat0, pat1}

	f.instruments = []xmFixtureInstrument{
		{
			volume:       64,
			panning:      128,
			sampleLength: 16,
			loopStart:    4,
			loopLength:   8,
			sampleAt: func(i int) int16 {
				// Distinct, deterministic ramp so sample positions
				// can be cross-checked against the PCM output.
				return int16(i*1200 - 8000)
			},
		},
		{empty: true},
	}

	return f
}

func TestChannelTimelineEventOrder(t *testing.T) {
	f := buildTimelineFixture()
	m := parseFixture(t, f.bytes())
	s := newTestStream(t, m)

	timeline := NewChannelTimeline()
	s.SetEventHandler(timeline.HandleStreamEvent)
	pcm := readAll(t, s)

	// Every played row emits exactly tempo tick events,
	// one snapshot per channel.
	playedRows := []struct{ pattern, row int }{
		{0, 0}, {0, 1}, // pattern 0, jump after row 1
		{1, 1}, {1, 2}, {1, 3}, // pattern 1, break lands on row 1
	}
	wantTicks := len(playedRows) * fixtureTicksPerRow
	if timeline.Len() != wantTicks {
		t.Fatalf("timeline len: got %d events, want %d", timeline.Len(), wantTicks)
	}

	// The (pattern order position, row, tick) coordinates must follow
	// the actual playback order, including the jump.
	type coord struct{ pattern, row, tick int }
	var gotCoords []coord
	for _, ev := range timeline.Events() {
		gotCoords = append(gotCoords, coord{ev.Pattern, ev.Row, ev.Tick})
		if len(ev.Channels) != f.channels {
			t.Fatalf("channel count: got %d, want %d", len(ev.Channels), f.channels)
		}
		for i, st := range ev.Channels {
			if st.Channel != i {
				t.Fatalf("event channels out of order: %d", i)
			}
			if st.Pattern != ev.Pattern || st.Row != ev.Row || st.Tick != ev.Tick {
				t.Fatalf("channel state coordinates mismatch")
			}
		}
	}
	var wantCoords []coord
	for _, r := range playedRows {
		for tick := 0; tick < fixtureTicksPerRow; tick++ {
			wantCoords = append(wantCoords, coord{r.pattern, r.row, tick})
		}
	}
	for i := range wantCoords {
		if gotCoords[i] != wantCoords[i] {
			t.Fatalf("coord[%d]: got %+v, want %+v", i, gotCoords[i], wantCoords[i])
		}
	}

	// Time must be strictly monotonic.
	for i := 1; i < timeline.Len(); i++ {
		if timeline.Events()[i].Time <= timeline.Events()[i-1].Time {
			t.Fatalf("time not monotonic at event %d", i)
		}
	}

	// Row 0 of pattern 0: two triggered notes with explicit volume
	// column values that override the instrument base volume.
	first := timeline.At(0, 0)
	if first.Note != xmNoteC4 || first.Instrument != 1 || !first.Active || !first.KeyOn {
		t.Fatalf("first snapshot ch0: %+v", first)
	}
	if math.Abs(first.Volume-1.0) > 1e-9 {
		t.Fatalf("ch0 volume: got %v, want 1.0 (volume column 0x50)", first.Volume)
	}
	firstCh1 := timeline.At(0, 1)
	if firstCh1.Note != xmNoteE4 || firstCh1.Instrument != 1 {
		t.Fatalf("first snapshot ch1: %+v", firstCh1)
	}
	// Volume column 0x30 (32) sets the channel to 32/64 = 0.5.
	if math.Abs(firstCh1.Volume-0.5) > 1e-9 {
		t.Fatalf("ch1 volume: got %v, want 0.5", firstCh1.Volume)
	}

	// The EDx delayed note: channel 0 keeps the row-0 note until
	// tick 2 of row 1, then G-4 appears.
	row1Base := fixtureTicksPerRow // row 1, tick 0
	if st := timeline.At(row1Base, 0); st.Note != xmNoteC4 {
		t.Fatalf("pre-delay note: got %d, want %d", st.Note, xmNoteC4)
	}
	if st := timeline.At(row1Base+1, 0); st.Note != xmNoteC4 {
		t.Fatalf("pre-delay note tick1: got %d", st.Note)
	}
	delayed := timeline.At(row1Base+2, 0)
	if delayed.Note != xmNoteG4 || delayed.Instrument != 1 || !delayed.Active {
		t.Fatalf("delayed note snapshot: %+v", delayed)
	}
	if !containsOp(delayed.Effects, xmdb.EffectNoteDelay) {
		t.Fatalf("delayed snapshot effects missing EDx: %+v", delayed.Effects)
	}

	// The pattern break row effect is visible on channel 1 snapshots.
	for tick := 0; tick < fixtureTicksPerRow; tick++ {
		st := timeline.At(row1Base+tick, 1)
		if !containsOp(st.Effects, xmdb.EffectPatternBreak) {
			t.Fatalf("pattern break effect missing at tick %d: %+v", tick, st.Effects)
		}
	}

	// Pattern 1 row 1 begins right after the two rows of pattern 0.
	p1base := 2 * fixtureTicksPerRow

	// Key-off: channel 0 keeps sounding (active) but KeyOn flips false.
	keyOff := timeline.At(p1base, 0)
	if keyOff.Note != xmNoteKeyOff {
		t.Fatalf("keyoff note number: got %d", keyOff.Note)
	}
	if keyOff.KeyOn || !keyOff.Active {
		t.Fatalf("keyoff snapshot: %+v", keyOff)
	}

	// Note cut EC2 on channel 1: volume is normal until tick 2, then 0.
	if st := timeline.At(p1base+1, 1); st.Volume == 0 {
		t.Fatalf("note cut happened too early")
	}
	cut := timeline.At(p1base+2, 1)
	if cut.Volume != 0 {
		t.Fatalf("note cut volume: got %v, want 0", cut.Volume)
	}

	// Bad instrument index: note present but instrument reported -1,
	// channel inactive and without a sample position.
	bad := timeline.At(p1base+fixtureTicksPerRow, 0)
	if bad.Note != xmNoteC4 || bad.Instrument != -1 || bad.Active || bad.SamplePosition != -1 {
		t.Fatalf("bad instrument snapshot: %+v", bad)
	}

	// Empty (zero-sample) instrument: instrument 2 reports -1 and the
	// channel is inactive (it has no samples to render).
	empty := timeline.At(p1base+2*fixtureTicksPerRow, 0)
	if empty.Note != xmNoteE4 || empty.Instrument != -1 || empty.Active {
		t.Fatalf("empty instrument snapshot: %+v", empty)
	}
	if empty.SamplePosition != -1 {
		t.Fatalf("empty instrument sample position: got %d, want -1", empty.SamplePosition)
	}

	// Log the golden PCM hash; the PCM-identity test below is the
	// real rendering regression guard.
	t.Logf("pcm golden hash: 0x%x (%d bytes)", pcmHash(pcm), len(pcm))
}

func containsOp(ops []xmdb.EffectOp, want xmdb.EffectOp) bool {
	for _, op := range ops {
		if op == want {
			return true
		}
	}
	return false
}

func TestChannelTimelineDoesNotChangePCM(t *testing.T) {
	f := buildTimelineFixture()

	render := func(installHandler bool) []byte {
		m := parseFixture(t, f.bytes())
		s := newTestStream(t, m)
		if installHandler {
			s.SetEventHandler(NewChannelTimeline().HandleStreamEvent)
		}
		return readAll(t, s)
	}

	without := render(false)
	with := render(true)
	if !bytes.Equal(without, with) {
		t.Fatalf("PCM output differs with timeline handler installed (%d vs %d bytes)",
			len(without), len(with))
	}

	// Querying the timeline must not change subsequent PCM either.
	m := parseFixture(t, f.bytes())
	s := newTestStream(t, m)
	timeline := NewChannelTimeline()
	s.SetEventHandler(timeline.HandleStreamEvent)
	buf := readTickBuf(s)
	var pcm1 []byte
	for i := 0; i < 6; i++ {
		n, err := s.Read(buf)
		pcm1 = append(pcm1, buf[:n]...)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	// Inspect (query) the accumulated state.
	_ = timeline.Events()
	_ = timeline.At(3, 0)
	rest := readAll(t, s)
	full := append(pcm1, rest...)
	reference := render(false)
	if !bytes.Equal(full, reference) {
		t.Fatalf("PCM changed after timeline queries")
	}
}

func TestChannelTimelineSamplePosition(t *testing.T) {
	f := buildTimelineFixture()
	m := parseFixture(t, f.bytes())
	s := newTestStream(t, m)
	timeline := NewChannelTimeline()
	s.SetEventHandler(timeline.HandleStreamEvent)
	_ = readAll(t, s)

	compiled := &s.module
	inst := &compiled.instruments[0]

	if got := timeline.At(0, 0).SamplePosition; got != 0 {
		t.Fatalf("initial sample position: got %d, want 0", got)
	}

	// The first note spans pattern 0 row 0 (events 0..2). The sample
	// position moves through the instrument data; it may wrap backwards
	// once a forward loop reaches its end, so assert it stays inside
	// the sample bounds rather than strict monotonicity.
	for evIdx := 1; evIdx < fixtureTicksPerRow; evIdx++ {
		st := timeline.At(evIdx, 0)
		if st.SamplePosition < 0 || st.SamplePosition >= len(inst.samples) {
			t.Fatalf("tick %d sample position %d out of bounds %d",
				evIdx, st.SamplePosition, len(inst.samples))
		}
		if st.SamplePosition == timeline.At(evIdx-1, 0).SamplePosition {
			t.Fatalf("tick %d sample position stuck at %d", evIdx, st.SamplePosition)
		}
	}

	// Row 1 ticks 0 and 1 still play the first (delayed) note.
	for _, evIdx := range []int{3, 4} {
		if st := timeline.At(evIdx, 0); st.Note != xmNoteC4 {
			t.Fatalf("event %d: expected delayed-predecessor note C4, got %d", evIdx, st.Note)
		}
	}

	// On tick 2 (event 5) the delayed G-4 triggers and the sample
	// position restarts from zero.
	delayed := timeline.At(5, 0)
	if delayed.Note != xmNoteG4 {
		t.Fatalf("delayed note: got %d, want %d", delayed.Note, xmNoteG4)
	}
	if delayed.SamplePosition != 0 {
		t.Fatalf("delayed note sample position: got %d, want 0", delayed.SamplePosition)
	}

	// The instrument loops forward; positions must stay inside the
	// instrument sample data for every active snapshot.
	for i := 0; i < timeline.Len(); i++ {
		st := timeline.At(i, 0)
		if st.Instrument != 1 || !st.Active {
			continue
		}
		if st.SamplePosition < 0 || st.SamplePosition >= len(inst.samples) {
			t.Fatalf("event %d sample position %d out of sample bounds %d",
				i, st.SamplePosition, len(inst.samples))
		}
	}
}

func TestChannelTimelineSeekRewind(t *testing.T) {
	f := buildTimelineFixture()
	m := parseFixture(t, f.bytes())
	s := newTestStream(t, m)
	timeline := NewChannelTimeline()
	s.SetEventHandler(timeline.HandleStreamEvent)

	buf := readTickBuf(s)
	for i := 0; i < 6; i++ {
		if _, err := s.Read(buf); err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if timeline.Len() != 6 {
		t.Fatalf("events before seek: got %d", timeline.Len())
	}

	// Seek to start replays the same events.
	pos, err := s.Seek(0, io.SeekStart)
	if err != nil || pos != 0 {
		t.Fatalf("seek: pos=%d err=%v", pos, err)
	}
	if timeline.Len() != 6 {
		t.Fatalf("rewind should not drop recorded events: got %d", timeline.Len())
	}
	firstRound := make([]ChannelTickEvent, timeline.Len())
	copy(firstRound, timeline.Events())

	for i := 0; i < 6; i++ {
		if _, err := s.Read(buf); err != nil && err != io.EOF {
			t.Fatalf("replay read: %v", err)
		}
	}
	if timeline.Len() != 12 {
		t.Fatalf("events after replay: got %d", timeline.Len())
	}
	for i := range firstRound {
		a, b := firstRound[i], timeline.Events()[6+i]
		if a.Pattern != b.Pattern || a.Row != b.Row || a.Tick != b.Tick {
			t.Fatalf("replayed event %d mismatch: %+v vs %+v", i, a, b)
		}
		if a.Time != b.Time {
			t.Fatalf("replayed event %d time mismatch", i)
		}
	}

	// SeekCurrent only reports the position and must not mutate state.
	bytePos, err := s.Seek(0, io.SeekCurrent)
	if err != nil || int(bytePos) != s.bytePos {
		t.Fatalf("seek current: pos=%d err=%v", bytePos, err)
	}
	if timeline.Len() != 12 {
		t.Fatalf("SeekCurrent changed the timeline length")
	}
}

func TestChannelTimelineLoopLimit(t *testing.T) {
	f := buildTimelineFixture()

	// Count the ticks of a single non-looping pass.
	ref := newTestStream(t, parseFixture(t, f.bytes()))
	refTimeline := NewChannelTimeline()
	ref.SetEventHandler(refTimeline.HandleStreamEvent)
	_ = readAll(t, ref)
	onePassTicks := refTimeline.Len()

	m := parseFixture(t, f.bytes())
	s := newTestStream(t, m)
	s.SetLooping(true)
	s.SetLoopLimit(2)
	timeline := NewChannelTimeline()
	s.SetEventHandler(timeline.HandleStreamEvent)

	buf := readTickBuf(s)
	var err error
	for {
		_, err = s.Read(buf)
		if err != nil {
			break
		}
	}
	if err != io.EOF {
		t.Fatalf("expected EOF after loop limit, got %v", err)
	}
	// 1 initial pass + 2 restarts.
	if want := onePassTicks * 3; timeline.Len() != want {
		t.Fatalf("loop timeline: got %d events, want %d", timeline.Len(), want)
	}
	if s.loopCount != 2 {
		t.Fatalf("loop count: got %d, want 2", s.loopCount)
	}

	// Without a limit the same stream would keep restarting; a limit
	// of 0 means unlimited.
	unlimited := newTestStream(t, parseFixture(t, f.bytes()))
	unlimited.SetLooping(true)
	reps := 0
	for {
		_, rerr := unlimited.Read(buf)
		if rerr != nil {
			t.Fatalf("unlimited loop ended early: %v", rerr)
		}
		reps++
		if reps >= onePassTicks*4 {
			break
		}
	}
}

func TestChannelTimelineBadPatternIndex(t *testing.T) {
	// An order entry pointing past the last pattern is remapped to a
	// generated empty pattern instead of panicking or corrupting state.
	f := newXMFixture()
	f.order = []uint8{0, 9}
	f.patterns = [][]xmFixtureNote{
		{
			xmFixtureNote{note: xmNoteC4, instrument: 1},
			xmFixtureNote{},
			xmFixtureNote{},
			xmFixtureNote{},
			xmFixtureNote{},
			xmFixtureNote{},
			xmFixtureNote{},
			xmFixtureNote{},
		},
	}
	f.instruments = []xmFixtureInstrument{
		{
			volume: 64, panning: 128, sampleLength: 8,
			loopStart: 0, loopLength: 8,
			sampleAt: func(i int) int16 { return int16(i * 1000) },
		},
	}

	m := parseFixture(t, f.bytes())
	s := newTestStream(t, m)
	timeline := NewChannelTimeline()
	s.SetEventHandler(timeline.HandleStreamEvent)
	if pcm := readAll(t, s); len(pcm) == 0 {
		t.Fatalf("no PCM rendered")
	}

	// First 4 rows belong to the real pattern 0; the rest to the
	// generated empty pattern at order position 1.
	realRows := 4
	emptyStart := realRows * fixtureTicksPerRow
	for i := 0; i < emptyStart; i++ {
		if timeline.Events()[i].Pattern != 0 {
			t.Fatalf("event %d not in pattern 0", i)
		}
	}
	// The empty pattern's rows contain no new notes, so the note
	// started in pattern 0 keeps sounding (standard XM behavior): the
	// row note number is zero while the channel stays active.
	sawEmptyRow := false
	for i := emptyStart; i < timeline.Len(); i++ {
		ev := timeline.Events()[i]
		if ev.Pattern != 1 {
			t.Fatalf("empty pattern event %d: pattern=%d", i, ev.Pattern)
		}
		for _, st := range ev.Channels {
			if st.Note != 0 {
				t.Fatalf("empty pattern row should carry no note: %+v", st)
			}
		}
		sawEmptyRow = true
	}
	if !sawEmptyRow {
		t.Fatalf("no empty pattern events recorded")
	}
	// The standard empty pattern has 64 rows.
	wantEvents := (realRows + 64) * fixtureTicksPerRow
	if timeline.Len() != wantEvents {
		t.Fatalf("bad-index timeline: got %d events, want %d", timeline.Len(), wantEvents)
	}
}

func TestChannelTimelineTruncatedModule(t *testing.T) {
	f := buildTimelineFixture()
	full := f.bytes()

	// Cut the file in the middle of the sample data; the parser must
	// return a ParseError rather than panic, and no stream is created.
	truncated := full[:len(full)*3/4]
	p := xmfile.NewParser(xmfile.ParserConfig{})
	if _, err := p.ParseFromBytes(truncated); err == nil {
		t.Fatalf("expected a parse error for the truncated module")
	} else if _, ok := err.(*xmfile.ParseError); !ok {
		t.Fatalf("expected *xmfile.ParseError, got %T: %v", err, err)
	}

	// A truncated pattern section must be rejected as well.
	headerEnd := 60 + 4 + 8*2 + len(f.order)
	truncatedPattern := full[:headerEnd+20]
	if _, err := xmfile.NewParser(xmfile.ParserConfig{}).ParseFromBytes(truncatedPattern); err == nil {
		t.Fatalf("expected a parse error for the truncated pattern data")
	}

	// Truncation inside the header itself must be rejected too, at
	// every stage, without panicking.
	for _, cut := range []int{10, 60, 70} {
		if _, err := xmfile.NewParser(xmfile.ParserConfig{}).ParseFromBytes(full[:cut]); err == nil {
			t.Fatalf("cut=%d: expected a parse error", cut)
		} else if _, ok := err.(*xmfile.ParseError); !ok {
			t.Fatalf("cut=%d: expected *xmfile.ParseError, got %T", cut, err)
		}
	}
}

func TestChannelTimelineOverflowJump(t *testing.T) {
	// A pattern break whose target row exceeds the pattern size must
	// be clamped to the last row instead of reading out of bounds.
	f := newXMFixture()
	f.order = []uint8{0, 1}
	f.patterns = [][]xmFixtureNote{
		{
			xmFixtureNote{note: xmNoteC4, instrument: 1},
			xmFixtureNote{},
			xmFixtureNote{effect: 0x0D, effectArg: 0x63}, // D63: row 63, patterns only have 4 rows
			xmFixtureNote{},
			xmFixtureNote{},
			xmFixtureNote{},
			xmFixtureNote{},
			xmFixtureNote{},
		},
		{
			xmFixtureNote{},
			xmFixtureNote{},
			xmFixtureNote{},
			xmFixtureNote{},
			xmFixtureNote{},
			xmFixtureNote{},
			xmFixtureNote{},
			xmFixtureNote{},
		},
	}
	f.instruments = []xmFixtureInstrument{
		{
			volume: 64, panning: 128, sampleLength: 8,
			loopStart: 0, loopLength: 8,
			sampleAt: func(i int) int16 { return int16(i * 1000) },
		},
	}

	m := parseFixture(t, f.bytes())
	s := newTestStream(t, m)
	timeline := NewChannelTimeline()
	s.SetEventHandler(timeline.HandleStreamEvent)
	if pcm := readAll(t, s); len(pcm) == 0 {
		t.Fatalf("no PCM rendered")
	}
	last := timeline.Events()[timeline.Len()-1]
	if last.Row != f.numRows-1 {
		t.Fatalf("overflow jump row: got %d, want %d", last.Row, f.numRows-1)
	}
}

func singlePatternFixture(rows ...[]xmFixtureNote) *xmFixture {
	f := newXMFixture()
	f.order = []uint8{0}
	flat := []xmFixtureNote{}
	for _, r := range rows {
		row := make([]xmFixtureNote, f.channels)
		copy(row, r)
		flat = append(flat, row...)
	}
	for len(flat) < f.numRows*f.channels {
		flat = append(flat, xmFixtureNote{})
	}
	f.patterns = [][]xmFixtureNote{flat}
	f.instruments = []xmFixtureInstrument{
		{
			volume: 64, panning: 128, sampleLength: 8,
			loopStart: 0, loopLength: 8,
			sampleAt: func(i int) int16 { return int16(i * 1000) },
		},
	}
	return f
}

func TestChannelTimelineNoteDelayZeroTick(t *testing.T) {
	// ED0 triggers the note immediately on tick 0.
	f := singlePatternFixture(
		[]xmFixtureNote{
			{note: xmNoteC4, instrument: 1, effect: 0x0E, effectArg: 0xD0},
			{},
		},
	)
	m := parseFixture(t, f.bytes())
	s := newTestStream(t, m)
	timeline := NewChannelTimeline()
	s.SetEventHandler(timeline.HandleStreamEvent)
	_ = readAll(t, s)

	first := timeline.At(0, 0)
	if first.Note != xmNoteC4 || !first.Active {
		t.Fatalf("ED0 should trigger on tick 0: %+v", first)
	}
	if first.SamplePosition != 0 {
		t.Fatalf("ED0 note should start at sample 0: %d", first.SamplePosition)
	}
	if !containsOp(first.Effects, xmdb.EffectNoteDelay) {
		t.Fatalf("ED0 effect missing from snapshot: %+v", first.Effects)
	}
}

func TestChannelTimelineNoteDelayBeyondTempo(t *testing.T) {
	// ED9 schedules on tick 9, but the row only has 3 ticks: the note
	// must never trigger and the channel stays inactive.
	f := singlePatternFixture(
		[]xmFixtureNote{
			{note: xmNoteC4, instrument: 1, effect: 0x0E, effectArg: 0xD9},
			{},
		},
	)
	m := parseFixture(t, f.bytes())
	s := newTestStream(t, m)
	timeline := NewChannelTimeline()
	s.SetEventHandler(timeline.HandleStreamEvent)
	_ = readAll(t, s)

	for tick := 0; tick < fixtureTicksPerRow; tick++ {
		st := timeline.At(tick, 0)
		if st.Note == xmNoteC4 {
			t.Fatalf("delayed note unexpectedly triggered on tick %d", tick)
		}
		if st.Active {
			t.Fatalf("channel should stay inactive, tick %d: %+v", tick, st)
		}
	}
}

func TestChannelTimelineNoteDelayInterruptedByJump(t *testing.T) {
	// A delayed note that never becomes due because a pattern break
	// ends the row early must be discarded, not carried into the next
	// pattern's channel state.
	f := newXMFixture()
	f.order = []uint8{0, 1}
	f.patterns = [][]xmFixtureNote{
		{
			// Delayed to tick 2 ...
			xmFixtureNote{note: xmNoteG4, instrument: 1, effect: 0x0E, effectArg: 0xD2},
			// ... but a break on the same row jumps away. The break
			// still lets all ticks of this row play first.
			xmFixtureNote{effect: 0x0D, effectArg: 0x00},
			xmFixtureNote{}, xmFixtureNote{},
			xmFixtureNote{}, xmFixtureNote{},
			xmFixtureNote{}, xmFixtureNote{},
		},
		{
			xmFixtureNote{}, xmFixtureNote{},
			xmFixtureNote{}, xmFixtureNote{},
			xmFixtureNote{}, xmFixtureNote{},
			xmFixtureNote{}, xmFixtureNote{},
		},
	}
	f.instruments = []xmFixtureInstrument{
		{
			volume: 64, panning: 128, sampleLength: 8,
			loopStart: 0, loopLength: 8,
			sampleAt: func(i int) int16 { return int16(i * 1000) },
		},
	}

	m := parseFixture(t, f.bytes())
	s := newTestStream(t, m)
	timeline := NewChannelTimeline()
	s.SetEventHandler(timeline.HandleStreamEvent)
	_ = readAll(t, s)

	// The delayed G-4 becomes due on tick 2 of pattern 0 row 0 (the
	// break only changes which row comes next). It appears on that one
	// snapshot and must never leak into pattern 1.
	g4Count := 0
	for _, ev := range timeline.Events() {
		for _, st := range ev.Channels {
			if st.Note == xmNoteG4 {
				g4Count++
				if st.Pattern != 0 || st.Row != 0 || st.Tick != 2 {
					t.Fatalf("delayed note at unexpected position: %+v", st)
				}
			}
		}
	}
	if g4Count != 1 {
		t.Fatalf("expected exactly 1 G-4 snapshot, got %d", g4Count)
	}
}

func TestChannelTimelineJumpPastSongEnd(t *testing.T) {
	// A pattern break targeting a pattern beyond the order table ends
	// the song cleanly (no panic, no out-of-bounds access).
	f := newXMFixture()
	f.order = []uint8{0}
	f.patterns = [][]xmFixtureNote{
		{
			xmFixtureNote{note: xmNoteC4, instrument: 1},
			xmFixtureNote{},
			// D00 from the last order position jumps to pattern 1,
			// which does not exist.
			xmFixtureNote{effect: 0x0D, effectArg: 0x00},
			xmFixtureNote{},
			xmFixtureNote{},
			xmFixtureNote{},
			xmFixtureNote{},
			xmFixtureNote{},
		},
	}
	f.instruments = []xmFixtureInstrument{
		{
			volume: 64, panning: 128, sampleLength: 8,
			loopStart: 0, loopLength: 8,
			sampleAt: func(i int) int16 { return int16(i * 1000) },
		},
	}

	m := parseFixture(t, f.bytes())
	s := newTestStream(t, m)
	timeline := NewChannelTimeline()
	s.SetEventHandler(timeline.HandleStreamEvent)

	pcm, err := func() ([]byte, error) {
		buf := readTickBuf(s)
		var out []byte
		for {
			n, rerr := s.Read(buf)
			out = append(out, buf[:n]...)
			if rerr != nil {
				return out, rerr
			}
		}
	}()
	if err != io.EOF {
		t.Fatalf("expected EOF when jump leaves the song, got %v", err)
	}
	if len(pcm) == 0 {
		t.Fatalf("expected PCM for the rows before the jump")
	}
	// Only rows 0 and 1 are rendered: 2 rows * 3 ticks.
	if want := 2 * fixtureTicksPerRow; timeline.Len() != want {
		t.Fatalf("timeline len: got %d, want %d", timeline.Len(), want)
	}
	last := timeline.Events()[timeline.Len()-1]
	if last.Pattern != 0 || last.Row != 1 {
		t.Fatalf("last event should be pattern 0 row 1, got %+v", last)
	}
}
