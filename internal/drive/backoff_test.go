package drive

import (
	"testing"
	"time"
)

func TestNoteRateLimitStatusExtendsWindow(t *testing.T) {
	c := &Client{
		rateLimit: rateLimit{},
	}

	c.noteRateLimitStatus(429, nil)

	c.rateLimit.mu.Lock()
	firstUntil := c.rateLimit.until
	c.rateLimit.mu.Unlock()

	time.Sleep(10 * time.Millisecond)

	c.noteRateLimitStatus(429, nil)

	c.rateLimit.mu.Lock()
	secondUntil := c.rateLimit.until
	c.rateLimit.mu.Unlock()

	if !secondUntil.After(firstUntil) {
		t.Errorf("window not extended: first=%v, second=%v", firstUntil, secondUntil)
	}
}
