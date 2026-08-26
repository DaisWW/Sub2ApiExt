package monitor

import (
	"context"
	"sync"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

func runProbes(
	ctx context.Context,
	accounts []model.Account,
	concurrency int,
	probeAccount func(context.Context, model.Account) model.ProbeResult,
) ([]model.ProbeResult, error) {
	if len(accounts) == 0 {
		return nil, ctx.Err()
	}
	if concurrency < 1 {
		concurrency = 1
	}
	workerCount := min(concurrency, len(accounts))
	jobs := make(chan model.Account)
	results := make(chan model.ProbeResult, workerCount)
	var workers sync.WaitGroup
	for range workerCount {
		workers.Add(1)
		go probeWorker(ctx, &workers, jobs, results, probeAccount)
	}
	go feedProbeJobs(ctx, accounts, jobs)
	go func() {
		workers.Wait()
		close(results)
	}()

	collected := make([]model.ProbeResult, 0, len(accounts))
	for result := range results {
		collected = append(collected, result)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return collected, nil
}

func probeWorker(
	ctx context.Context,
	workers *sync.WaitGroup,
	jobs <-chan model.Account,
	results chan<- model.ProbeResult,
	probeAccount func(context.Context, model.Account) model.ProbeResult,
) {
	defer workers.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case account, ok := <-jobs:
			if !ok {
				return
			}
			select {
			case results <- probeAccount(ctx, account):
			case <-ctx.Done():
				return
			}
		}
	}
}

func feedProbeJobs(ctx context.Context, accounts []model.Account, jobs chan<- model.Account) {
	defer close(jobs)
	for _, account := range accounts {
		select {
		case jobs <- account:
		case <-ctx.Done():
			return
		}
	}
}
