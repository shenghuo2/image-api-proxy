package proxy

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/vmihailenco/msgpack/v5"
)

// streamOutcome examines complete frames after forwarding them to the client.
// It keeps only one frame at a time and never changes the upstream response.
type streamOutcome struct {
	header    [4]byte
	headerLen int
	frame     []byte
	frameLen  int
	final     bool
	invalid   bool
}

func (s *streamOutcome) write(data []byte) {
	for len(data) > 0 && !s.invalid {
		if s.headerLen < len(s.header) {
			n := copy(s.header[s.headerLen:], data)
			s.headerLen += n
			data = data[n:]
			if s.headerLen < len(s.header) {
				return
			}
			s.frameLen = int(binary.BigEndian.Uint32(s.header[:]))
			if s.frameLen == 0 || s.frameLen > maxImageBytes {
				s.invalid = true
				return
			}
			s.frame = make([]byte, s.frameLen)
		}
		n := copy(s.frame[len(s.frame)-s.frameLen:], data)
		s.frameLen -= n
		data = data[n:]
		if s.frameLen > 0 {
			return
		}
		var message map[string]any
		if err := msgpack.Unmarshal(s.frame, &message); err != nil {
			s.invalid = true
			return
		}
		if code, ok := message["code"]; ok && code != nil && fmt.Sprint(code) != "200" {
			s.invalid = true
			return
		}
		if message["step_ix"] == nil {
			if image, ok := message["image"].([]byte); ok && bytes.HasPrefix(image, []byte("\x89PNG\r\n\x1a\n")) {
				s.final = true
			}
		}
		s.headerLen = 0
		s.frame = nil
	}
}

func (s *streamOutcome) success() bool {
	return !s.invalid && s.final && s.headerLen == 0 && s.frame == nil
}
