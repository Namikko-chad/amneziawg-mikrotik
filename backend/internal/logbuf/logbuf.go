// Package logbuf is a fixed-size ring of log lines that also mirrors to stderr.
package logbuf

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

type Buffer struct {
	mu    sync.Mutex
	lines []string
	next  int
	full  bool
}

func New(size int) *Buffer { return &Buffer{lines: make([]string, size)} }

func (b *Buffer) Printf(format string, args ...any) {
	msg := strings.TrimRight(fmt.Sprintf(format, args...), "\n")
	log.Print(msg)
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, l := range strings.Split(msg, "\n") {
		b.lines[b.next] = time.Now().Format("15:04:05 ") + l
		b.next = (b.next + 1) % len(b.lines)
		if b.next == 0 {
			b.full = true
		}
	}
}

// Write lets the buffer be used as a process stdout/stderr.
func (b *Buffer) Write(p []byte) (int, error) {
	if s := strings.TrimSpace(string(p)); s != "" {
		b.Printf("%s", s)
	}
	return len(p), nil
}

func (b *Buffer) Lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.full {
		return append([]string(nil), b.lines[:b.next]...)
	}
	return append(append([]string(nil), b.lines[b.next:]...), b.lines[:b.next]...)
}
