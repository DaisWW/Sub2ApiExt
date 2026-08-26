package stats

import (
	"testing"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

func TestSummarize(t *testing.T) {
	got := Summarize([]int{9, 1, 5, 3})
	if *got.FastestMs != 1 || *got.SlowestMs != 9 || *got.MedianMs != 4 {
		t.Fatalf("unexpected stats: %+v", got)
	}
	odd := Summarize([]int{7, 2, 4})
	if *odd.MedianMs != 4 {
		t.Fatalf("unexpected odd median: %+v", odd)
	}
	if got := Summarize(nil); got.FastestMs != nil || got.MedianMs != nil || got.SlowestMs != nil {
		t.Fatalf("empty stats should be null: %+v", got)
	}
}

func TestStatusFromResults(t *testing.T) {
	cases := []struct {
		name string
		in   []model.ProbeResult
		want string
	}{
		{"empty", nil, model.StatusUnknown},
		{"all ok", []model.ProbeResult{{Status: model.StatusOperational}, {Status: model.StatusDegraded}}, model.StatusOperational},
		{"mixed", []model.ProbeResult{{Status: model.StatusOperational}, {Status: model.StatusFailed}}, model.StatusDegraded},
		{"none", []model.ProbeResult{{Status: model.StatusError}}, model.StatusFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StatusFromResults(tc.in); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAggregateGroup(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	group := model.Group{ID: 3, Name: "primary"}
	results := []model.ProbeResult{
		{Status: model.StatusOperational, LatencyMs: intPtr(10), FirstByteMs: intPtr(4)},
		{Status: model.StatusFailed, LatencyMs: intPtr(90)},
	}
	got := AggregateGroup("group:3", group, results, now)
	if got.Status != model.StatusDegraded || got.LatencyMs == nil || *got.LatencyMs != 10 || got.CheckedAt != now {
		t.Fatalf("unexpected group result: %+v", got)
	}
}

func intPtr(value int) *int { return &value }
