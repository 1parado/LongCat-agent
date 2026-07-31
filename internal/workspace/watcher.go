package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Watcher is a portable polling watcher. It deliberately skips VCS metadata
// and emits coalesced events, making it suitable for the embedded local UI
// without requiring a platform-specific dependency.
type Watcher struct {
	root     string
	interval time.Duration
	mu       sync.Mutex
	files    map[string]fileFingerprint
	subs     map[int]chan ChangeEvent
	nextSub  int
	stopOnce sync.Once
	stop     chan struct{}
}

func NewWatcher(root string, interval time.Duration) (*Watcher, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	w := &Watcher{root: root, interval: interval, files: make(map[string]fileFingerprint), subs: make(map[int]chan ChangeEvent), stop: make(chan struct{})}
	w.files = w.snapshot()
	return w, nil
}

func (w *Watcher) Root() string { return w.root }

// Start runs until ctx is canceled or Stop is called.
func (w *Watcher) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.scan()
			case <-ctx.Done():
				w.Stop()
				return
			case <-w.stop:
				return
			}
		}
	}()
}

func (w *Watcher) Stop() {
	w.stopOnce.Do(func() {
		close(w.stop)
		w.mu.Lock()
		for id, ch := range w.subs {
			close(ch)
			delete(w.subs, id)
		}
		w.mu.Unlock()
	})
}

// Subscribe returns a buffered event stream and an unsubscribe function.
func (w *Watcher) Subscribe() (<-chan ChangeEvent, func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	ch := make(chan ChangeEvent, 32)
	if isClosed(w.stop) {
		close(ch)
		return ch, func() {}
	}
	id := w.nextSub
	w.nextSub++
	w.subs[id] = ch
	return ch, func() {
		w.mu.Lock()
		if current, ok := w.subs[id]; ok {
			delete(w.subs, id)
			close(current)
		}
		w.mu.Unlock()
	}
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func (w *Watcher) scan() {
	next := w.snapshot()
	w.mu.Lock()
	previous := w.files
	w.files = next
	var events []ChangeEvent
	now := time.Now()
	for path, old := range previous {
		current, ok := next[path]
		if !ok {
			events = append(events, ChangeEvent{Workspace: w.root, Path: path, Kind: ChangeDeleted, At: now})
			continue
		}
		if current != old {
			events = append(events, ChangeEvent{Workspace: w.root, Path: path, Kind: ChangeModified, At: now})
		}
	}
	for path := range next {
		if _, ok := previous[path]; !ok {
			events = append(events, ChangeEvent{Workspace: w.root, Path: path, Kind: ChangeAdded, At: now})
		}
	}
	for _, event := range events {
		for _, ch := range w.subs {
			select {
			case ch <- event:
			default:
			}
		}
	}
	w.mu.Unlock()
}

func (w *Watcher) snapshot() map[string]fileFingerprint {
	out := make(map[string]fileFingerprint)
	_ = filepath.WalkDir(w.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(w.root, path)
		if relErr != nil {
			return nil
		}
		if rel != "." && (rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator))) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, statErr := d.Info()
		if statErr == nil {
			out[filepath.ToSlash(rel)] = fileFingerprint{Size: info.Size(), ModTime: info.ModTime(), Mode: uint32(info.Mode())}
		}
		return nil
	})
	return out
}
