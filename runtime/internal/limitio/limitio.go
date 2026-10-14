// Package limitio reads untrusted bodies with a hard size limit and says so when the
// limit is hit, instead of silently truncating.
package limitio

import (
	"errors"
	"io"
)

var ErrTooLarge = errors.New("body exceeds size limit")

// ReadAll reads at most max bytes. A longer body returns ErrTooLarge.
func ReadAll(r io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, ErrTooLarge
	}
	return data, nil
}

// Reader fails with ErrTooLarge once more than max bytes have been read in total.
type Reader struct {
	r    io.Reader
	left int64
}

func NewReader(r io.Reader, max int64) *Reader { return &Reader{r: r, left: max} }

func (l *Reader) Read(p []byte) (int, error) {
	if l.left < 0 {
		return 0, ErrTooLarge
	}
	if int64(len(p)) > l.left+1 {
		p = p[:l.left+1]
	}
	n, err := l.r.Read(p)
	l.left -= int64(n)
	if l.left < 0 {
		return n, ErrTooLarge
	}
	return n, err
}
