package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nikunj-taneja/seedhibaat/internal/store"
)

func main() {
	count := 10000
	if len(os.Args) > 1 {
		parsed, err := strconv.Atoi(os.Args[1])
		if err != nil || parsed < 1 {
			panic("count must be positive")
		}
		count = parsed
	}
	directory, err := os.MkdirTemp("", "seedhibaat-loadtest-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(directory)
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(directory, "load.db"))
	if err != nil {
		panic(err)
	}
	defer database.Close()
	started := time.Now()
	for i := 0; i < count; i++ {
		job := store.Job{ID: fmt.Sprintf("job-%d", i), StepID: "load", Kind: "noop", Payload: []byte("{}")}
		inserted, err := database.EnqueueJob(ctx, job, fmt.Sprintf("load-%d", i), time.Now())
		if err != nil || !inserted {
			panic(fmt.Sprintf("enqueue %d: %v", i, err))
		}
	}
	enqueueDuration := time.Since(started)
	var completed atomic.Int64
	workers := 8
	var wg sync.WaitGroup
	workStarted := time.Now()
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for {
				job, ok, err := database.ClaimJob(ctx, fmt.Sprintf("load-worker-%d", worker), time.Now())
				if err != nil {
					panic(err)
				}
				if !ok {
					return
				}
				if err := database.CompleteJob(ctx, job.ID, time.Now()); err != nil {
					panic(err)
				}
				completed.Add(1)
			}
		}(i)
	}
	wg.Wait()
	workDuration := time.Since(workStarted)
	if int(completed.Load()) != count {
		panic(fmt.Sprintf("completed %d of %d", completed.Load(), count))
	}
	fmt.Printf("jobs=%d enqueue_rate=%.0f/s process_rate=%.0f/s workers=%d\n", count, float64(count)/enqueueDuration.Seconds(), float64(count)/workDuration.Seconds(), workers)
}
