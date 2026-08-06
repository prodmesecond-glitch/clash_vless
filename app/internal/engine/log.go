package engine

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// EventFileSink returns an onLog callback that forwards to base and also appends
// timestamped lines to path (used by --debug). If the file can't be opened it
// falls back to base alone.
func EventFileSink(path string, base func(string)) func(string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return base
	}
	var mu sync.Mutex
	return func(l string) {
		if base != nil {
			base(l)
		}
		mu.Lock()
		fmt.Fprintf(f, "%s  %s\n", time.Now().Format("15:04:05"), l)
		mu.Unlock()
	}
}
