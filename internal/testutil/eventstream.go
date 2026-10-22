// Package testutil builds deterministic AWS event-stream fixtures for tests.
package testutil

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
)

func EventFrame(event, payload string) []byte {
	var headers bytes.Buffer
	for _, pair := range [][2]string{{":message-type", "event"}, {":event-type", event}, {":content-type", "application/json"}} {
		headers.WriteByte(byte(len(pair[0])))
		headers.WriteString(pair[0])
		headers.WriteByte(7)
		_ = binary.Write(&headers, binary.BigEndian, uint16(len(pair[1])))
		headers.WriteString(pair[1])
	}
	var frame bytes.Buffer
	_ = binary.Write(&frame, binary.BigEndian, uint32(16+headers.Len()+len(payload)))
	_ = binary.Write(&frame, binary.BigEndian, uint32(headers.Len()))
	_ = binary.Write(&frame, binary.BigEndian, crc32.ChecksumIEEE(frame.Bytes()))
	frame.Write(headers.Bytes())
	frame.WriteString(payload)
	_ = binary.Write(&frame, binary.BigEndian, crc32.ChecksumIEEE(frame.Bytes()))
	return frame.Bytes()
}
