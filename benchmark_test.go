package main

import (
	"fmt"
	"os"
	"testing"

	"uk.ac.bris.cs/gameoflife/gol"
)

func runParallelGameOfLifeBenchmark(b *testing.B, threads int) {

	os.Stdout = nil

	p := gol.Params{
		Turns:       100,
		ImageWidth:  512,
		ImageHeight: 512,
		Threads:     threads,
	}

	testName := fmt.Sprintf("%d_workers", threads)
	b.Run(testName, func(t *testing.B) {
		defer func() { os.Stdout = nil }()
		for i := 0; i < b.N; i++ {
			events := make(chan gol.Event)
			go gol.Run(p, events, nil)
			for event := range events {
				switch event.(type) {
				case gol.FinalTurnComplete:
				}
			}
		}
	})
}

func BenchmarkDoublingParallelGameOfLife(b *testing.B) {
	for threads := 1; threads <= 16; threads *= 2 {
		runParallelGameOfLifeBenchmark(b, threads)
	}
}

func BenchmarkIncrementalParallelGameOfLife(b *testing.B) {
	for threads := 1; threads <= 16; threads++ {
		runParallelGameOfLifeBenchmark(b, threads)
	}
}
