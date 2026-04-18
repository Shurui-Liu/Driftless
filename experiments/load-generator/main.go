// load-generator submits tasks to the ingest API at a controlled rate.
//
// Usage:
//   go run . -rate=500 -duration=2m -url=http://<ingest-api>/tasks
//
// Flags:
//   -rate          tasks per minute (default 100)
//   -duration      how long to run (default 2m)
//   -url           ingest API endpoint
//   -payload-bytes size of each task payload in bytes (default 512)
//   -concurrency   max simultaneous in-flight requests (default 50)
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"sync/atomic"
	"time"
)

func main() {
	rate        := flag.Int("rate", 100, "tasks per minute")
	dur         := flag.Duration("duration", 2*time.Minute, "run duration")
	url         := flag.String("url", "http://localhost:8080/tasks", "ingest API URL")
	payloadSize := flag.Int("payload-bytes", 512, "task payload size in bytes")
	concurrency := flag.Int("concurrency", 50, "max in-flight requests")
	flag.Parse()

	if *rate <= 0 {
		log.Fatal("-rate must be > 0")
	}

	interval := time.Minute / time.Duration(*rate)
	ticker   := time.NewTicker(interval)
	deadline := time.After(*dur)

	var submitted, failed int64
	start := time.Now()

	sem := make(chan struct{}, *concurrency)

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			MaxIdleConnsPerHost: *concurrency,
		},
	}

	log.Printf("load generator: rate=%d/min duration=%s url=%s payload=%dB",
		*rate, *dur, *url, *payloadSize)

	for {
		select {
		case <-deadline:
			elapsed   := time.Since(start)
			totalSent := atomic.LoadInt64(&submitted)
			totalFail := atomic.LoadInt64(&failed)
			actualRate := float64(totalSent) / elapsed.Minutes()
			fmt.Printf("submitted=%d failed=%d elapsed=%s actual_rate=%.1f/min\n",
				totalSent, totalFail, elapsed.Round(time.Second), actualRate)
			return

		case <-ticker.C:
			sem <- struct{}{}
			go func() {
				defer func() { <-sem }()

				payload := make([]byte, *payloadSize)
				rand.Read(payload)

				body, _ := json.Marshal(map[string]interface{}{
					"payload":  payload,
					"priority": rand.Intn(10) + 1,
				})

				resp, err := client.Post(*url, "application/json", bytes.NewReader(body))
				if err != nil {
					atomic.AddInt64(&failed, 1)
					return
				}
				resp.Body.Close()
				if resp.StatusCode == http.StatusAccepted {
					atomic.AddInt64(&submitted, 1)
				} else {
					atomic.AddInt64(&failed, 1)
					log.Printf("unexpected status %d", resp.StatusCode)
				}
			}()
		}
	}
}
