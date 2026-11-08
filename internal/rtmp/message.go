package rtmp

import "time"

// RTMP message type ids (subset that matters for live streaming).
const (
	MsgSetChunkSize     = 1
	MsgAbort            = 2
	MsgAck              = 3
	MsgUserControl      = 4
	MsgWindowAckSize    = 5
	MsgSetPeerBandwidth = 6
	MsgAudio            = 8
	MsgVideo            = 9
	MsgDataAMF3         = 15
	MsgSharedObject     = 16
	MsgCommandAMF3      = 17
	MsgDataAMF0         = 18
	MsgCommandAMF0      = 20
	MsgAggregate        = 22
)

// User control event types.
const (
	ucStreamBegin      = 0
	ucStreamEOF        = 1
	ucStreamDry        = 2
	ucSetBufferLength  = 3
	ucStreamIsRecorded = 4
	ucPingRequest      = 6
	ucPingResponse     = 7
)

// Message is a fully assembled RTMP message.
//
// Payload is owned by the message and must never be mutated after it has been
// handed to the server: the same buffer is shared by every subscriber, which is
// what keeps fan-out cheap (no per-viewer copy for thousands of viewers).
type Message struct {
	TypeID    uint8
	StreamID  uint32
	Timestamp uint32
	Payload   []byte

	// Cached classification, filled in once by the publisher path.
	IsKeyframe  bool
	IsSeqHeader bool

	// ReceivedAt is when the server finished assembling the message.
	ReceivedAt time.Time
}

// Size returns the on-the-wire message size, used for buffer accounting.
func (m *Message) Size() int {
	return len(m.Payload) + 12
}

func (m *Message) clone() *Message {
	c := *m
	return &c
}

// isVideoKeyframe reports whether an FLV/RTMP video tag starts a keyframe.
// Supports both legacy FLV framing and the enhanced-RTMP (HEVC/AV1) framing bit.
func isVideoKeyframe(payload []byte) bool {
	if len(payload) < 1 {
		return false
	}
	b := payload[0]
	if b&0x80 != 0 { // enhanced RTMP: IsExHeader set
		return (b>>4)&0x07 == 1
	}
	return b>>4 == 1
}

// isVideoSeqHeader reports whether the tag carries a codec configuration
// (AVC/HEVC/AV1 sequence header) rather than a frame.
func isVideoSeqHeader(payload []byte) bool {
	if len(payload) < 2 {
		return false
	}
	if payload[0]&0x80 != 0 {
		return payload[1]&0x0F == 0 // SequenceStart
	}
	// Legacy: frameType(4) + codecID(4), then AVCPacketType == 0.
	switch payload[0] & 0x0F {
	case 7, 12, 13: // AVC, HEVC, AV1
		return payload[1] == 0
	}
	return false
}

// isAudioSeqHeader reports whether the tag carries an AAC/MP3 configuration.
func isAudioSeqHeader(payload []byte) bool {
	if len(payload) < 2 {
		return false
	}
	soundFormat := payload[0] >> 4
	if soundFormat != 10 { // AAC
		return soundFormat == 2 && (payload[0]&0x0F) == 0 // MP3 config marker
	}
	return payload[1] == 0
}
