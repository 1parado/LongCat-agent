package cache

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCacheTTLAndLRU(t *testing.T) {
	c := New[string, string](time.Hour, 2)
	c.Set("a", "A")
	c.Set("b", "B")
	if _, ok := c.Get("a"); !ok {
		t.Fatal("expected a")
	}
	c.Set("c", "C")
	if _, ok := c.Get("b"); ok {
		t.Fatal("expected least recently used b to be evicted")
	}
	c = New[string, string](10*time.Millisecond, 2)
	c.Set("x", "X")
	time.Sleep(20 * time.Millisecond)
	if _, ok := c.Get("x"); ok {
		t.Fatal("expected x to expire")
	}
}

func TestCacheGetOrLoadDeduplicates(t *testing.T) {
	c := New[string, int](time.Hour, 10)
	var calls atomic.Int32
	loader := func() (int, error) { calls.Add(1); time.Sleep(10 * time.Millisecond); return 42, nil }
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			value, err := c.GetOrLoad("answer", loader)
			if err != nil || value != 42 {
				t.Errorf("value=%d err=%v", value, err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("loader called %d times", calls.Load())
	}
}
