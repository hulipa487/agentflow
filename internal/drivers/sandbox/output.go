package sandbox

import (
	"bytes"
	"sync"
	"unicode/utf8"
)

// capped collects a stream up to a limit and discards the rest. It never
// refuses a write: a command that floods its output must keep running into
// its own time limit, not block on a full pipe.
type capped struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int64
	truncated bool
	tap       func(p []byte) // sees everything, including what is cut
}

func (c *capped) Write(p []byte) (int, error) {
	if c.tap != nil {
		c.tap(p)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	room := c.limit - int64(c.buf.Len())
	if room <= 0 {
		c.truncated = c.truncated || len(p) > 0
		return len(p), nil
	}
	if int64(len(p)) > room {
		c.buf.Write(p[:room])
		c.truncated = true
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

// String returns what was kept, cut back to a whole character: the limit can
// fall in the middle of one, and the text goes to JSON.
func (c *capped) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.buf.Bytes()
	if c.truncated {
		for len(b) > 0 && !utf8.Valid(b) {
			_, size := utf8.DecodeLastRune(b)
			if size > 1 {
				break
			}
			b = b[:len(b)-1]
		}
	}
	return string(bytes.ToValidUTF8(b, []byte("�")))
}

func (c *capped) wasTruncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.truncated
}
