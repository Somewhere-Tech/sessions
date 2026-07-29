package watch

import (
	"errors"
	"io"
	"os"
)

func readRange(file *os.File, start, length int64) ([]byte, error) {
	if length <= 0 {
		return []byte{}, nil
	}
	buffer := make([]byte, int(length))
	total := 0
	for total < len(buffer) {
		count, err := file.ReadAt(buffer[total:], start+int64(total))
		total += count
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return buffer[:total], err
		}
		if count == 0 {
			break
		}
	}
	return buffer[:total], nil
}
