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

func TestAccountHostsAreAdmittedWithDefaultRechargeDiscount(t *testing.T) {
	channel := testChannel("https://upstream.example", 0.1)
	syncer := &Syncer{
		config: &Config{SyncTarget: "account"},
		state:  newState(),
		logger: log.New(io.Discard, "", 0),
	}
	plan := newChannelCheckPlan("account", []Channel{channel}, nil)
	stats := syncStats{}
	report := newSyncReport("account", []Channel{channel})
	if !plan.admitAccountHost(syncer, &channel, report, &stats) {
		t.Fatal("unconfigured hosts should be admitted with the default recharge discount")
	}
	if report.rows["account:18/group:24"].rechargeDiscount != 1 {
		t.Fatalf("default recharge discount = %.4f", report.rows["account:18/group:24"].rechargeDiscount)
	}
}

func TestConfiguredRechargeDiscountAdmitsAccountHosts(t *testing.T) {
	channel := testChannel("https://lucen.cc", 1)
	syncer := &Syncer{
		config: &Config{
			SyncTarget:        "account",
			RechargeDiscounts: map[string]float64{"lucen.cc": 0.9},
		},
		state:  newState(),
		logger: log.New(io.Discard, "", 0),
	}
	plan := newChannelCheckPlan("account", []Channel{channel}, nil)
	stats := syncStats{}
	report := newSyncReport("account", []Channel{channel})
	if !plan.admitAccountHost(syncer, &channel, report, &stats) {
		t.Fatal("configured hosts should be admitted with their recharge discount")
	}
	if stats.skipped != 0 || stats.failed != 0 {
		t.Fatalf("configured host was not admitted cleanly: %+v", stats)
	}
	if report.rows["account:18/group:24"].rechargeDiscount != 0.9 {
		t.Fatalf("configured recharge discount = %.4f", report.rows["account:18/group:24"].rechargeDiscount)
	}
}

func TestInvalidAccountBaseURLFailsAdmission(t *testing.T) {
	channel := testChannel("not-a-url", 1)
	syncer := &Syncer{
		config: &Config{SyncTarget: "account"},
		state:  newState(),
		logger: log.New(io.Discard, "", 0),
	}
	plan := newChannelCheckPlan("account", []Channel{channel}, nil)
	stats := syncStats{}
	report := newSyncReport("account", []Channel{channel})
	if plan.admitAccountHost(syncer, &channel, report, &stats) {
		t.Fatal("invalid base_url should not be admitted")
	}
	if stats.failed != 1 {
		t.Fatalf("invalid base_url stats=%+v", stats)
	}
}

func TestDuplicateAccountChannelsKeepFirstAdmittedRechargeDiscount(t *testing.T) {
	first := testChannel("https://lucen.cc", 0.1)
	second := first
	second.BaseURL = "https://other-upstream.example"
	syncer := &Syncer{
		config: &Config{
			SyncTarget:        "account",
			RechargeDiscounts: map[string]float64{"lucen.cc": 0.9},
		},
		state:  newState(),
		logger: log.New(io.Discard, "", 0),
	}
	plan := newChannelCheckPlan("account", []Channel{first, second}, nil)
	stats := syncStats{}
	report := newSyncReport("account", []Channel{first, second})
	if !plan.admit(syncer, &first, report, &stats) {
		t.Fatal("first channel should be admitted")
	}
	if plan.admit(syncer, &second, report, &stats) {
		t.Fatal("duplicate account channel should not be checked twice")
	}
	if got := report.rows["account:18/group:24"].rechargeDiscount; got != 0.9 {
		t.Fatalf("duplicate channel overwrote recharge discount: %.4f", got)
	}
	if stats.failed != 0 || stats.skipped != 0 {
		t.Fatalf("duplicate channel changed admission stats: %+v", stats)
	}
}

func TestInvalidFirstAccountChannelCanBeReplacedByValidChannel(t *testing.T) {
	first := testChannel("not-a-url", 0.1)
	second := first
	second.BaseURL = "https://lucen.cc"
	syncer := &Syncer{
		config: &Config{
			SyncTarget:        "account",
			RechargeDiscounts: map[string]float64{"lucen.cc": 0.9},
		},
		state:  newState(),
		logger: log.New(io.Discard, "", 0),
	}
	plan := newChannelCheckPlan("account", []Channel{first, second}, nil)
	stats := syncStats{}
	report := newSyncReport("account", []Channel{first, second})
	if plan.admit(syncer, &first, report, &stats) {
		t.Fatal("invalid first channel should not be admitted")
	}
	if !plan.admit(syncer, &second, report, &stats) {
		t.Fatal("valid replacement channel should be admitted")
	}
	if got := report.rows["account:18/group:24"].rechargeDiscount; got != 0.9 {
		t.Fatalf("replacement channel did not set recharge discount: %.4f", got)
	}
}
