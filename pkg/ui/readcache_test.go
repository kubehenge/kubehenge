package ui

import (
	"context"
	"testing"
	"time"
)

// A failed read that is forgotten is read again at the next look, while a good one stays fresh.
func TestReadCacheForgetsAFailedRead(t *testing.T) {
	t.Parallel()
	var rc readCache[int]
	at := time.Unix(0, 0)
	now := func() time.Time { return at }
	reads := 0
	read := func(context.Context) int {
		reads++
		return reads
	}
	failed := func(v int) bool { return v == 1 }
	ctx := context.Background()

	if v, err := rc.get(ctx, "k", time.Minute, now, read); err != nil || v != 1 {
		t.Fatalf("first read = %d, %v", v, err)
	}
	rc.forget("k", failed)
	if v, err := rc.get(ctx, "k", time.Minute, now, read); err != nil || v != 2 {
		t.Fatalf("after forgetting = %d, %v, want a new read", v, err)
	}
	rc.forget("k", failed)
	if v, err := rc.get(ctx, "k", time.Minute, now, read); err != nil || v != 2 {
		t.Fatalf("a good read = %d, %v, want it kept", v, err)
	}
}
