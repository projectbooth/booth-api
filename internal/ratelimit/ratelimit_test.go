package ratelimit

import (
	"testing"
	"time"
)

func TestBurstThenRate(t *testing.T) {
	now := time.Unix(1000, 0)
	l := New(10, 50)
	l.Now = func() time.Time { return now }
	for i := 0; i < 50; i++ {
		if ok, _ := l.Allow("app"); !ok {
			t.Fatalf("write %d of the burst refused", i+1)
		}
	}
	ok, wait := l.Allow("app")
	if ok || wait <= 0 || wait > 100*time.Millisecond {
		t.Fatalf("51st write: ok=%v wait=%v; want refused with a wait of at most 100ms", ok, wait)
	}
	if ok, _ := l.Allow("other-app"); !ok {
		t.Error("another principal shares the bucket")
	}
	now = now.Add(time.Second) // 10 more tokens
	for i := 0; i < 10; i++ {
		if ok, _ := l.Allow("app"); !ok {
			t.Fatalf("write %d after a second refused", i+1)
		}
	}
	if ok, _ := l.Allow("app"); ok {
		t.Error("more than 10 writes allowed in a second after the burst")
	}
}

func TestSweepForgetsIdleBuckets(t *testing.T) {
	now := time.Unix(1000, 0)
	l := New(10, 50)
	l.Now = func() time.Time { return now }
	l.Allow("a")
	now = now.Add(2 * time.Minute)
	l.Allow("b")
	if _, ok := l.buckets["a"]; ok {
		t.Error("an idle, refilled bucket was kept")
	}
}
