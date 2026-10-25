package api

import (
	"testing"
	"time"
)

func TestRateLimiter(t *testing.T) {
	var l rateLimiter
	now := time.Now()
	for i := 0; i < 3; i++ {
		if !l.allow("a", 3, now) {
			t.Fatalf("call %d refused", i)
		}
	}
	if l.allow("a", 3, now) {
		t.Fatal("4th call in the hour allowed")
	}
	if !l.allow("b", 3, now) {
		t.Fatal("another person was limited")
	}
	if !l.allow("a", 3, now.Add(61*time.Minute)) {
		t.Fatal("not allowed again after an hour")
	}
}
