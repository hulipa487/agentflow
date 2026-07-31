package llm

import (
	"bufio"
	"context"
	"io"
	"strings"
)

// sseDatas returns a channel of SSE `data:` payloads from r. The channel
// closes on EOF, stream error, or ctx cancellation. Callers own r.Body's
// lifecycle via ctx.
func sseDatas(ctx context.Context, r io.Reader) <-chan string {
	ch := make(chan string, 16)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			data, ok := strings.CutPrefix(line, "data:")
			if !ok {
				continue // event:, comments, blank separators
			}
			select {
			case ch <- strings.TrimSpace(data):
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}
