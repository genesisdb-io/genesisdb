package fsutil

import (
	"bufio"
	"io"
)

// ScanLines visits newline separated records of any length. offset is the
// byte offset of the line start, counted from startOffset. isTerminated is
// false only for a final line without a trailing newline. The line slice is
// only valid during the visit.
func ScanLines(reader io.Reader, startOffset int64, visit func(line []byte, offset int64, isTerminated bool) error) error {
	buffered := bufio.NewReaderSize(reader, 256*1024)
	offset := startOffset
	var longLine []byte
	for {
		chunk, err := buffered.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			longLine = append(longLine, chunk...)
			continue
		}
		if len(longLine) > 0 {
			longLine = append(longLine, chunk...)
			chunk = longLine
		}
		if len(chunk) > 0 {
			isTerminated := chunk[len(chunk)-1] == '\n'
			line := chunk
			if isTerminated {
				line = chunk[:len(chunk)-1]
			}
			if visitErr := visit(line, offset, isTerminated); visitErr != nil {
				return visitErr
			}
			offset += int64(len(chunk))
		}
		longLine = longLine[:0]
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
