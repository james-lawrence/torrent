package torrent

import (
	"io"
	"time"

	"golang.org/x/time/rate"
)

type rateLimitedReader struct {
	l *rate.Limiter
	r io.Reader
}

func (t *rateLimitedReader) Read(b []byte) (n int, err error) {
	m := min(len(b), t.l.Burst())
	reserved := t.l.ReserveN(time.Now(), m)
	if !reserved.OK() {
		panic(n)
	}

	time.Sleep(reserved.Delay())
	return io.LimitReader(t.r, int64(m)).Read(b)
}
