package supervisor

import (
	"io"
)

type writeOutcome uint8

const (
	writeNotWritten writeOutcome = iota
	writeAmbiguous
	writeDelivered
)

func writeFrame(writer io.Writer, frame []byte) (writeOutcome, error) {
	buffer := make([]byte, len(frame)+1)
	copy(buffer, frame)
	buffer[len(frame)] = '\n'

	n, err := writer.Write(buffer)
	switch {
	case n == len(buffer):
		return writeDelivered, err
	case n == 0 && err != nil:
		return writeNotWritten, err
	default:
		if err == nil {
			err = io.ErrShortWrite
		}
		return writeAmbiguous, err
	}
}
