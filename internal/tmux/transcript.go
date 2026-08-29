package tmux

import (
	"bytes"
	"io"
)

// ParseTranscriptBytes parses complete lines in data. An incomplete trailing
// line is not consumed so a tail-parser can resume from the returned offset.
func ParseTranscriptBytes(t Tool, data []byte) (turns []Turn, consumed int64, err error) {
	if len(data) == 0 {
		return nil, 0, nil
	}
	cut := len(data)
	if data[len(data)-1] != '\n' {
		i := bytes.LastIndexByte(data, '\n')
		if i < 0 {
			return nil, 0, nil
		}
		cut = i + 1
	}
	turns, err = t.ParseTranscript(bytes.NewReader(data[:cut]))
	return turns, int64(cut), err
}

func unsupportedParse(io.Reader) ([]Turn, error) {
	return nil, ErrTranscriptUnsupported
}
