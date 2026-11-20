package xm

import (
	"github.com/quasilyte/xm/internal/xmdb"
)

// ChannelState is a point on a channel state timeline.
//
// A state snapshot is produced for every channel on every tick while
// the stream is being read. The snapshots describe the logical
// (non-audio) playback state after the row/tick effects were applied,
// but before the tick PCM bytes are rendered.
//
// Experimental: the channel timeline API may change in the future.
type ChannelState struct {
	// Channel is the XM module channel ID (0-based).
	Channel int

	// Pattern is the pattern order position currently being played
	// (an index into the module pattern order table).
	Pattern int

	// Row is the pattern row (0-based) being played on this tick.
	Row int

	// Tick is the tick index inside the current row (0-based).
	Tick int

	// Time is the playback offset in seconds, counted from the
	// beginning of the module (or the last rewind/seek).
	Time float64

	// Note is the raw XM note number of the row note (1..96 for the
	// playable notes, 97 for a key-off note). It is 0 when the row
	// has no note; the channel may still sound a note started earlier.
	Note int

	// Instrument is the 1-based instrument number selected on the row,
	// or -1 when the row has no instrument or the instrument is empty.
	Instrument int

	// Volume is the current channel volume in the [0, 1] range after
	// all volume effects of this tick. A value of 0 means the channel
	// is muted (for example, by a note cut).
	Volume float64

	// Panning is the current channel panning in the [0, 1] range,
	// where 0 is the far left and 1 is the far right.
	Panning float64

	// Active reports whether the channel is producing samples on this
	// tick. A channel becomes inactive when it has no instrument, or
	// when a non-looping sample reached its end.
	Active bool

	// KeyOn reports whether the channel is in the key-on state.
	// It becomes false after a key-off note or an effect key-off.
	KeyOn bool

	// SamplePosition is the playback position inside the instrument
	// sample data, in sample frames, at the beginning of this tick.
	// It is -1 when the channel has no instrument.
	SamplePosition int

	// Effects are the effective effect operations attached to the
	// current row. They are the same operations the stream executes,
	// including both the volume-column and the effect-column effects.
	Effects []xmdb.EffectOp
}

// ChannelTickEvent groups the per-channel states of a single tick.
//
// The channel states always follow the channel ID order and cover
// every module channel, including the inactive ones.
//
// Experimental: the channel timeline API may change in the future.
type ChannelTickEvent struct {
	// Pattern is the pattern order position played on this tick.
	Pattern int

	// Row is the pattern row (0-based) played on this tick.
	Row int

	// Tick is the tick index inside the row (0-based).
	Tick int

	// Time is the playback offset in seconds.
	Time float64

	// Channels holds one state per module channel, in channel ID order.
	Channels []ChannelState
}

// ChannelTimeline is a Stream event handler that records a channel
// state timeline.
//
// Install it with Stream.SetEventHandler and feed the stream with
// Read calls; every rendered tick appends one ChannelTickEvent.
// Recording is a pure observation: the timeline never changes the
// rendered PCM bytes or the playback position.
//
// A timeline grows without a bound, so long-running streams should
// either consume the events periodically or call Clear/rewind.
//
// Experimental: the channel timeline API may change in the future.
type ChannelTimeline struct {
	events []ChannelTickEvent
}

// NewChannelTimeline allocates an empty timeline.
func NewChannelTimeline() *ChannelTimeline {
	return &ChannelTimeline{}
}

// HandleStreamEvent makes the timeline usable as a Stream event
// handler. Only the per-tick channel state events are recorded;
// note and sync events are ignored.
func (t *ChannelTimeline) HandleStreamEvent(e StreamEvent) {
	if e.Kind != EventChannelState {
		return
	}
	t.events = append(t.events, e.channelEvent)
}

// Events returns the recorded tick events in the actual playback
// order, including jumps, loops and delayed notes.
//
// The returned slice is owned by the timeline and is valid until
// the next Read-produced event or a Clear call.
func (t *ChannelTimeline) Events() []ChannelTickEvent {
	return t.events
}

// At returns the channel state for the given channel at event index i
// (i is a tick sequence number, 0-based). It is a shorthand for
// Events()[i].Channels[channel].
func (t *ChannelTimeline) At(i, channel int) ChannelState {
	return t.events[i].Channels[channel]
}

// Len returns the number of recorded tick events.
func (t *ChannelTimeline) Len() int {
	return len(t.events)
}

// Clear drops all recorded events.
func (t *ChannelTimeline) Clear() {
	t.events = t.events[:0]
}
