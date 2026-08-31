package era

import (
	"bufio"
	"fmt"
	"io"
)

const openingFrameLimit = 1 << 20

// ReadOpeningFrame reads exactly one LF-delimited frame without changing its
// bytes. The returned remainder is the persistent buffered reader so bytes
// prefetched while locating the opening frame are delivered exactly once.
func ReadOpeningFrame(input io.Reader) (*OpeningFrame, io.Reader, error) {
	if input == nil {
		return nil, nil, fmt.Errorf("read opening frame: nil input")
	}

	reader, ok := input.(*bufio.Reader)
	if !ok {
		reader = bufio.NewReader(input)
	}

	var raw []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(fragment) > 0 {
			if len(raw)+len(fragment) > openingFrameLimit {
				return nil, reader, fmt.Errorf("read opening frame: exceeds %d-byte limit", openingFrameLimit)
			}
			raw = append(raw, fragment...)
		}

		switch err {
		case nil:
			return NewOpeningFrame(raw), reader, nil
		case bufio.ErrBufferFull:
			if len(raw) == openingFrameLimit {
				return nil, reader, fmt.Errorf("read opening frame: exceeds %d-byte limit", openingFrameLimit)
			}
		case io.EOF:
			return nil, reader, io.ErrUnexpectedEOF
		default:
			return nil, reader, fmt.Errorf("read opening frame: %w", err)
		}
	}
}
