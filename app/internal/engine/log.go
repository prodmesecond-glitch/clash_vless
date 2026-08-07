package engine

import (
	"fmt"
	"os"
	"sync"
	"time"

	xlog "github.com/xtls/xray-core/common/log"
)

// funcSink is an xray log Handler that forwards every message to fn.
type funcSink struct{ fn func(string) }

func (h funcSink) Handle(m xlog.Message) {
	if h.fn != nil {
		h.fn(m.String())
	}
}

// SetLogSink redirects xray-core's process-wide log output to fn instead of the
// terminal — including messages emitted at config-parse time (the deprecated-
// transport warnings), which the per-config loglevel can't suppress. Call once,
// before starting instances; used by the TUI-embedded daemon so xray never writes
// to the alt-screen. A no-op fn discards.
func SetLogSink(fn func(string)) {
	xlog.RegisterHandler(funcSink{fn})
}

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
