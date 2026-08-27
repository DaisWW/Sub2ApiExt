package main

import (
	"context"
	"io"
	"log"
	"testing"
	"time"
)

func TestAcquireCheckSlotHonorsCancellation(t *testing.T) {
	semaphore := make(chan struct{}, 1)
	semaphore <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		done <- acquireCheckSlot(ctx, semaphore)
	}()

	cancel()
	select {
	case acquired := <-done:
		if acquired {
			t.Fatal("acquireCheckSlot acquired a slot after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("acquireCheckSlot did not return after cancellation")
	}
}

func TestRunChannelChecksDoesNotStartAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	channel := testChannel("https://upstream.example", 0.1)
	syncer := &Syncer{
		config: &Config{SyncTarget: "group"},
		state:  newState(),
		logger: log.New(io.Discard, "", 0),
	}

	stats := syncer.runChannelChecks(ctx, []Channel{channel}, time.Now(), nil, newSyncReport("group", []Channel{channel}))
	if stats.checked != 0 || len(syncer.state.Rules) != 0 {
		t.Fatalf("canceled checks were started: stats=%+v rules=%d", stats, len(syncer.state.Rules))
	}
}
