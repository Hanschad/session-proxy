package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	MsgTypeOutputStreamData = "output_stream_data"
	MsgTypeInputStreamData  = "input_stream_data"
	MsgTypeAcknowledge      = "acknowledge"
	MsgTypeChannelClosed    = "channel_closed"

	SchemaVersion = 1

	// Field lengths
	HeaderLen         = 4
	MessageTypeLen    = 32
	SchemaVersionLen  = 4
	CreatedDateLen    = 8
	SequenceNumberLen = 8
	FlagsLen          = 8
	MessageIdLen      = 16
	PayloadDigestLen  = 32
	PayloadTypeLen    = 4
	PayloadLengthLen  = 4

	// TotalHeaderLen is the full binary header size (used for serialization offset)
	TotalHeaderLen = HeaderLen + MessageTypeLen + SchemaVersionLen + CreatedDateLen + SequenceNumberLen + FlagsLen + MessageIdLen + PayloadDigestLen + PayloadTypeLen + PayloadLengthLen

	// HeaderLengthValue is the value to put in the HeaderLength field
	// Per AWS plugin: payload offset = HeaderLength + 4 (PayloadLengthLength)
	// So HeaderLength = offset to PayloadLength field, NOT to Payload
	HeaderLengthValue = HeaderLen + MessageTypeLen + SchemaVersionLen + CreatedDateLen + SequenceNumberLen + FlagsLen + MessageIdLen + PayloadDigestLen + PayloadTypeLen

	// AgentMessageFlag values (bitmask for stream control)
	FlagData = 0 // Normal data
	FlagSyn  = 1 // Stream start (bit 0)
	FlagFin  = 2 // Stream end (bit 1)

	// PayloadType values
	PayloadTypeOutput                  uint32 = 1
	PayloadTypeError                   uint32 = 2
	PayloadTypeSize                    uint32 = 3
	PayloadTypeParameter               uint32 = 4
	PayloadTypeHandshakeRequest        uint32 = 5
	PayloadTypeHandshakeResponse       uint32 = 6
	PayloadTypeHandshakeComplete       uint32 = 7
	PayloadTypeEncChallengeRequest     uint32 = 8
	PayloadTypeEncChallengeResponse    uint32 = 9
	PayloadTypeFlag                    uint32 = 10
	PayloadTypeStdErr                  uint32 = 11
	PayloadTypeExitCode                uint32 = 12
)

type AgentMessage struct {
	Header  AgentMessageHeader
	Payload []byte
}

type AgentMessageHeader struct {
	HeaderLength   uint32
	MessageType    string // 32 bytes, right padded
	SchemaVersion  uint32
	CreatedDate    uint64
	SequenceNumber int64
	Flags          uint64
	MessageId      uuid.UUID
	PayloadDigest  [32]byte
	PayloadType    uint32
	PayloadLength  uint32
}

func NewInputMessage(payload []byte, seq int64) (*AgentMessage, error) {
	id := uuid.New()
	return &AgentMessage{
		Header: AgentMessageHeader{
			HeaderLength:   uint32(HeaderLengthValue),
			MessageType:    MsgTypeInputStreamData,
			SchemaVersion:  SchemaVersion,
			CreatedDate:    uint64(time.Now().UnixMilli()),
			SequenceNumber: seq,
			Flags:          FlagData,
			MessageId:      id,
			PayloadType:    PayloadTypeOutput,
			PayloadLength:  uint32(len(payload)),
		},
		Payload: payload,
	}, nil
}

func NewAcknowledgeMessage(refMsgType string, refMsgId uuid.UUID, refSeq int64) (*AgentMessage, error) {
	// ACK payload is pure JSON (no internal length prefix)
	// Agent uses Flags=3 and PayloadType=0 for ACK messages
	ackJson := fmt.Sprintf(`{"AcknowledgedMessageType":"%s","AcknowledgedMessageId":"%s","AcknowledgedMessageSequenceNumber":%d,"IsSequentialMessage":true}`,
		refMsgType, refMsgId.String(), refSeq)
	payloadBytes := []byte(ackJson)

	id := uuid.New()
	return &AgentMessage{
		Header: AgentMessageHeader{
			HeaderLength:   uint32(HeaderLengthValue),
			MessageType:    MsgTypeAcknowledge,
			SchemaVersion:  SchemaVersion,
			CreatedDate:    uint64(time.Now().UnixMilli()),
			SequenceNumber: 0,
			Flags:          3, // Agent uses Flags=3 for ACK
			MessageId:      id,
			PayloadType:    0, // Agent uses PayloadType=0 for ACK
			PayloadLength:  uint32(len(payloadBytes)),
		},
		Payload: payloadBytes,
	}, nil
}

func (m *AgentMessage) MarshalBinary() ([]byte, error) {
	buf := new(bytes.Buffer)

	// 1. HeaderLength
	if err := binary.Write(buf, binary.BigEndian, m.Header.HeaderLength); err != nil {
		return nil, err
	}

	// 2. MessageType (32 bytes string)
	var typeBytes [32]byte
	copy(typeBytes[:], m.Header.MessageType)
	if err := binary.Write(buf, binary.BigEndian, typeBytes); err != nil {
		return nil, err
	}

	// 3. SchemaVersion
	if err := binary.Write(buf, binary.BigEndian, m.Header.SchemaVersion); err != nil {
		return nil, err
	}

	// 4. CreatedDate
	if err := binary.Write(buf, binary.BigEndian, m.Header.CreatedDate); err != nil {
		return nil, err
	}

	// 5. SequenceNumber
	if err := binary.Write(buf, binary.BigEndian, m.Header.SequenceNumber); err != nil {
		return nil, err
	}

	// 6. Flags
	if err := binary.Write(buf, binary.BigEndian, m.Header.Flags); err != nil {
		return nil, err
	}

	// 7. MessageId (16 bytes)
	if err := binary.Write(buf, binary.BigEndian, m.Header.MessageId); err != nil {
		return nil, err
	}

	// 8. PayloadDigest (32 bytes)
	// Calculate SHA256 of payload
	digest := sha256.Sum256(m.Payload)
	if err := binary.Write(buf, binary.BigEndian, digest); err != nil {
		return nil, err
	}

	// 9. PayloadType
	if err := binary.Write(buf, binary.BigEndian, m.Header.PayloadType); err != nil {
		return nil, err
	}

	// 10. PayloadLength
	if err := binary.Write(buf, binary.BigEndian, uint32(len(m.Payload))); err != nil {
		return nil, err
	}

	// Append Payload
	buf.Write(m.Payload)

	return buf.Bytes(), nil
}

func UnmarshalMessage(data []byte) (*AgentMessage, error) {
	if len(data) < TotalHeaderLen {
		return nil, fmt.Errorf("data too short for header")
	}

	r := bytes.NewReader(data)
	var h AgentMessageHeader

	// 1. HeaderLength
	if err := binary.Read(r, binary.BigEndian, &h.HeaderLength); err != nil {
		return nil, err
	}

	// 2. MessageType
	var typeBytes [32]byte
	if err := binary.Read(r, binary.BigEndian, &typeBytes); err != nil {
		return nil, err
	}
	h.MessageType = strings.TrimRight(string(typeBytes[:]), "\x00 ")

	// 3. SchemaVersion
	if err := binary.Read(r, binary.BigEndian, &h.SchemaVersion); err != nil {
		return nil, err
	}

	// 4. CreatedDate
	if err := binary.Read(r, binary.BigEndian, &h.CreatedDate); err != nil {
		return nil, err
	}

	// 5. SequenceNumber
	if err := binary.Read(r, binary.BigEndian, &h.SequenceNumber); err != nil {
		return nil, err
	}

	// 6. Flags
	if err := binary.Read(r, binary.BigEndian, &h.Flags); err != nil {
		return nil, err
	}

	// 7. MessageId
	if err := binary.Read(r, binary.BigEndian, &h.MessageId); err != nil {
		return nil, err
	}

	// 8. PayloadDigest
	if err := binary.Read(r, binary.BigEndian, &h.PayloadDigest); err != nil {
		return nil, err
	}

	// 9. PayloadType
	if err := binary.Read(r, binary.BigEndian, &h.PayloadType); err != nil {
		return nil, err
	}

	// 10. PayloadLength
	if err := binary.Read(r, binary.BigEndian, &h.PayloadLength); err != nil {
		return nil, err
	}

	// Payload offset = HeaderLength + PayloadLengthLen (per AWS plugin format)
	payloadOffset := h.HeaderLength + PayloadLengthLen
	if uint32(len(data)) < payloadOffset+h.PayloadLength {
		return nil, errors.New("incomplete payload data")
	}

	payload := make([]byte, h.PayloadLength)
	copy(payload, data[payloadOffset:payloadOffset+h.PayloadLength])

	return &AgentMessage{
		Header:  h,
		Payload: payload,
	}, nil
}
