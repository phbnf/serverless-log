// Copyright 2024 Google LLC. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// hammer is a tool to load test a serverless log.
package main

import (
	"context"
	crand "crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/rivo/tview"
	"github.com/transparency-dev/merkle/rfc6962"
	"github.com/transparency-dev/serverless-log/client"
	"golang.org/x/mod/sumdb/note"
	"k8s.io/klog/v2"
)

func init() {
	flag.Var(&logURL, "log_url", "Log storage root URL (can be specified multiple times), e.g. https://log.server/and/path/")
}

var (
	logURL multiStringFlag

	bearerToken   = flag.String("bearer_token", "", "The bearer token for auth. For GCP this is the result of `gcloud auth print-identity-token`")
	logPubKeyFile = flag.String("log_public_key", "", "Location of log public key file. If unset, uses the contents of the SERVERLESS_LOG_PUBLIC_KEY environment variable")
	origin        = flag.String("origin", "", "Expected first line of checkpoints from log")

	maxReadOpsPerSecond = flag.Int("max_read_ops", 20, "The maximum number of read operations per second")
	numReadersRandom    = flag.Int("num_readers_random", 4, "The number of readers looking for random leaves")
	numReadersFull      = flag.Int("num_readers_full", 4, "The number of readers downloading the whole log")

	maxWriteOpsPerSecond = flag.Int("max_write_ops", 0, "The maximum number of write operations per second")
	numWriters           = flag.Int("num_writers", 0, "The number of independent write tasks to run")

	leafBundleSize = flag.Int("leaf_bundle_size", 1, "The log-configured number of leaves in each leaf bundle")
	leafMinSize    = flag.Int("leaf_min_size", 0, "Minimum size in bytes of individual leaves")

	showUI = flag.Bool("show_ui", true, "Set to false to disable the text-based UI")

	hc = &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        256,
			MaxIdleConnsPerHost: 256,
			DisableKeepAlives:   false,
		},
		Timeout: 5 * time.Second,
	}
)

type roundRobinFetcher struct {
	sync.Mutex
	idx int
	f   []client.Fetcher
}

func (rr *roundRobinFetcher) next() client.Fetcher {
	rr.Lock()
	defer rr.Unlock()

	f := rr.f[rr.idx]
	rr.idx = (rr.idx + 1) % len(rr.f)

	return f
}

func (rr *roundRobinFetcher) Fetch(ctx context.Context, path string) ([]byte, error) {
	f := rr.next()
	return f(ctx, path)
}

func main() {
	klog.InitFlags(nil)
	flag.Parse()

	ctx := context.Background()

	logSigV, _, err := logSigVerifier(*logPubKeyFile)
	if err != nil {
		klog.Exitf("failed to read log public key: %v", err)
	}

	if len(logURL) == 0 {
		klog.Exitf("--log_url must be provided")
	}

	var rootURL *url.URL
	fetchers := []client.Fetcher{}
	for _, s := range logURL {
		// url must reference a directory, by definition
		if !strings.HasSuffix(s, "/") {
			s += "/"
		}

		rootURL, err = url.Parse(s)
		if err != nil {
			klog.Exitf("Invalid log URL: %v", err)
		}
		fetchers = append(fetchers, newFetcher(rootURL))

	}
	f := roundRobinFetcher{f: fetchers}

	var cpRaw []byte
	cons := client.UnilateralConsensus(f.Fetch)
	hasher := rfc6962.DefaultHasher
	tracker, err := client.NewLogStateTracker(ctx, f.Fetch, hasher, cpRaw, logSigV, *origin, cons)
	if err != nil {
		klog.Exitf("Failed to create LogStateTracker: %v", err)
	}
	// Fetch initial state of log
	_, _, _, err = tracker.Update(ctx)
	if err != nil {
		klog.Exitf("Failed to get initial state of the log: %v", err)
	}

	addURL, err := rootURL.Parse("add")
	if err != nil {
		klog.Exitf("Failed to create add URL: %v", err)
	}
	hammer := NewHammer(&tracker, f.Fetch, addURL)
	hammer.Run(ctx)

	if *showUI {
		hostUI(ctx, hammer)
	} else {
		<-ctx.Done()
	}
}

func NewLeafConsumer() *LeafConsumer {
	lookup, err := lru.New[string, uint64](1024)
	if err != nil {
		panic(err)
	}
	return &LeafConsumer{
		leafchan: make(chan Leaf, 256),
		lookup:   lookup,
	}
}

// LeafConsumer eats leaves from the channel and performs analysis
// that is somewhat global. At the moment this just checks how many
// times it sees a duplicate leaf (i.e. a leaf that appears at multiple
// indices). This could be extended to measure integration time etc.
type LeafConsumer struct {
	leafchan       chan Leaf
	lookup         *lru.Cache[string, uint64]
	duplicateCount uint64
}

func (c *LeafConsumer) Run(ctx context.Context) {
	defer close(c.leafchan)
	for {
		select {
		case <-ctx.Done(): //context cancelled
			return
		case l := <-c.leafchan:
			strData := string(l.Data)
			if oIdx, found := c.lookup.Get(strData); found {
				if oIdx != l.Index {
					c.duplicateCount++
					klog.V(2).Infof("Found two indices for data %q: (%d, %d)", strData, oIdx, l.Index)
				}
			} else {
				c.lookup.Add(strData, l.Index)
			}
		}
	}
}

func (c *LeafConsumer) String() string {
	return fmt.Sprintf("Duplicates: %d", c.duplicateCount)
}

func NewHammer(tracker *client.LogStateTracker, f client.Fetcher, addURL *url.URL) *Hammer {
	readThrottle := NewThrottle(*maxReadOpsPerSecond)
	writeThrottle := NewThrottle(*maxWriteOpsPerSecond)
	errChan := make(chan error, 20)
	leafConsumer := NewLeafConsumer()
	go leafConsumer.Run(context.Background())

	gen := newLeafGenerator(tracker.LatestConsistent.Size, *leafMinSize)
	randomReaders := newWorkerPool(func() worker {
		return NewLeafReader(tracker, f, RandomNextLeaf(), *leafBundleSize, readThrottle.tokenChan, errChan, leafConsumer.leafchan)
	})
	fullReaders := newWorkerPool(func() worker {
		return NewLeafReader(tracker, f, MonotonicallyIncreasingNextLeaf(), *leafBundleSize, readThrottle.tokenChan, errChan, leafConsumer.leafchan)
	})
	writers := newWorkerPool(func() worker {
		return NewLogWriter(hc, addURL, gen, writeThrottle.tokenChan, errChan, leafConsumer.leafchan)
	})
	return &Hammer{
		randomReaders: randomReaders,
		fullReaders:   fullReaders,
		writers:       writers,
		readThrottle:  readThrottle,
		writeThrottle: writeThrottle,
		tracker:       tracker,
		leafConsumer:  leafConsumer,
		errChan:       errChan,
	}
}

type Hammer struct {
	randomReaders workerPool
	fullReaders   workerPool
	writers       workerPool
	readThrottle  *Throttle
	writeThrottle *Throttle
	tracker       *client.LogStateTracker
	leafConsumer  *LeafConsumer
	errChan       chan error
}

func (h *Hammer) Run(ctx context.Context) {
	// Kick off readers & writers
	for i := 0; i < *numReadersRandom; i++ {
		h.randomReaders.Grow(ctx)
	}
	for i := 0; i < *numReadersFull; i++ {
		h.fullReaders.Grow(ctx)
	}
	for i := 0; i < *numWriters; i++ {
		h.writers.Grow(ctx)
	}

	// Set up logging for any errors
	go func() {
		for {
			select {
			case <-ctx.Done(): //context cancelled
				return
			case err := <-h.errChan:
				klog.Warning(err)
			}
		}
	}()

	// Start the throttles
	go h.readThrottle.Run(ctx)
	go h.writeThrottle.Run(ctx)

	go func() {
		tick := time.NewTicker(1 * time.Second)
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				size := h.tracker.LatestConsistent.Size
				_, _, _, err := h.tracker.Update(ctx)
				if err != nil {
					klog.Warning(err)
					inconsistentErr := client.ErrInconsistency{}
					if errors.As(err, &inconsistentErr) {
						klog.Fatalf("Last Good Checkpoint:\n%s\n\nFirst Bad Checkpoint:\n%s\n\n%v", string(inconsistentErr.SmallerRaw), string(inconsistentErr.LargerRaw), inconsistentErr)
					}
				}
				newSize := h.tracker.LatestConsistent.Size
				if newSize > size {
					klog.V(1).Infof("Updated checkpoint from %d to %d", size, newSize)
				}
			}
		}
	}()
}

func genLeaf(n uint64, minLeafSize int) []byte {
	// Make a slice with half the number of requested bytes since we'll
	// hex-encode them below which gets us back up to the full amount.
	filler := make([]byte, minLeafSize/2)
	_, _ = crand.Read(filler)
	return []byte(fmt.Sprintf("%x %d", filler, n))
}

func newLeafGenerator(n uint64, minLeafSize int) func() []byte {
	const dupChance = 0.1
	nextLeaf := genLeaf(n, minLeafSize)
	return func() []byte {
		if rand.Float64() <= dupChance {
			// This one will actually be unique, but the next iteration will
			// duplicate it. In future, this duplication could be randomly
			// selected to include really old leaves too, to test long-term
			// deduplication in the log (if it supports  that).
			return nextLeaf
		}

		n++
		r := nextLeaf
		nextLeaf = genLeaf(n, minLeafSize)
		return r
	}
}

func NewThrottle(opsPerSecond int) *Throttle {
	return &Throttle{
		opsPerSecond: opsPerSecond,
		tokenChan:    make(chan bool, opsPerSecond),
	}
}

type Throttle struct {
	opsPerSecond int
	tokenChan    chan bool

	oversupply int
}

func (t *Throttle) Increase() {
	tokenCount := t.opsPerSecond
	delta := float64(tokenCount) * 0.1
	if delta < 1 {
		delta = 1
	}
	t.opsPerSecond = tokenCount + int(delta)
}

func (t *Throttle) Decrease() {
	tokenCount := t.opsPerSecond
	if tokenCount <= 1 {
		return
	}
	delta := float64(tokenCount) * 0.1
	if delta < 1 {
		delta = 1
	}
	t.opsPerSecond = tokenCount - int(delta)
}

func (t *Throttle) Run(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	for {
		select {
		case <-ctx.Done(): //context cancelled
			return
		case <-ticker.C:
			tokenCount := t.opsPerSecond
			timeout := time.After(1 * time.Second)
		Loop:
			for i := 0; i < t.opsPerSecond; i++ {
				select {
				case t.tokenChan <- true:
					tokenCount--
				case <-timeout:
					break Loop
				}
			}
			t.oversupply = tokenCount
		}
	}
}

func (t *Throttle) String() string {
	return fmt.Sprintf("Current max: %d/s. Oversupply in last second: %d", t.opsPerSecond, t.oversupply)
}

func hostUI(ctx context.Context, hammer *Hammer) {
	grid := tview.NewGrid()
	grid.SetRows(3, 0, 10).SetColumns(0).SetBorders(true)
	// Status box
	statusView := tview.NewTextView()
	grid.AddItem(statusView, 0, 0, 1, 1, 0, 0, false)
	// Log view box
	logView := tview.NewTextView()
	logView.ScrollToEnd()
	logView.SetMaxLines(10000)
	grid.AddItem(logView, 1, 0, 1, 1, 0, 0, false)
	if err := flag.Set("logtostderr", "false"); err != nil {
		klog.Exitf("Failed to set flag: %v", err)
	}
	if err := flag.Set("alsologtostderr", "false"); err != nil {
		klog.Exitf("Failed to set flag: %v", err)
	}
	klog.SetOutput(logView)

	helpView := tview.NewTextView()
	helpView.SetText("+/- to increase/decrease read load\n>/< to increase/decrease write load\nw/W to increase/decrease workers")
	grid.AddItem(helpView, 2, 0, 1, 1, 0, 0, false)

	app := tview.NewApplication()
	ticker := time.NewTicker(1 * time.Second)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				text := fmt.Sprintf("Read: %s\nWrite: %s\nAnalysis: %s", hammer.readThrottle.String(), hammer.writeThrottle.String(), hammer.leafConsumer.String())
				statusView.SetText(text)
				app.Draw()
			}
		}
	}()
	app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Rune() {
		case '+':
			klog.Info("Increasing the read operations per second")
			hammer.readThrottle.Increase()
		case '-':
			klog.Info("Decreasing the read operations per second")
			hammer.readThrottle.Decrease()
		case '>':
			klog.Info("Increasing the write operations per second")
			hammer.writeThrottle.Increase()
		case '<':
			klog.Info("Decreasing the write operations per second")
			hammer.writeThrottle.Decrease()
		case 'w':
			klog.Info("Increasing the number of workers")
			hammer.randomReaders.Grow(ctx)
			hammer.fullReaders.Grow(ctx)
			hammer.writers.Grow(ctx)
		case 'W':
			klog.Info("Decreasing the number of workers")
			hammer.randomReaders.Shrink(ctx)
			hammer.fullReaders.Shrink(ctx)
			hammer.writers.Shrink(ctx)
		}
		return event
	})
	// logView.SetChangedFunc(func() {
	// 	app.Draw()
	// })
	if err := app.SetRoot(grid, true).Run(); err != nil {
		panic(err)
	}
}

// Returns a log signature verifier and the public key bytes it uses.
// Attempts to read key material from f, or uses the SERVERLESS_LOG_PUBLIC_KEY
// env var if f is unset.
func logSigVerifier(f string) (note.Verifier, []byte, error) {
	var pubKey []byte
	var err error
	if len(f) > 0 {
		pubKey, err = os.ReadFile(f)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to read public key from file %q: %v", f, err)
		}
	} else {
		pubKey = []byte(os.Getenv("SERVERLESS_LOG_PUBLIC_KEY"))
		if len(pubKey) == 0 {
			return nil, nil, fmt.Errorf("supply public key file path using --log_public_key or set SERVERLESS_LOG_PUBLIC_KEY environment variable")
		}
	}

	v, err := note.NewVerifier(string(pubKey))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create verifier: %v", err)
	}

	return v, pubKey, nil
}

// newFetcher creates a Fetcher for the log at the given root location.
func newFetcher(root *url.URL) client.Fetcher {
	get := getByScheme[root.Scheme]
	if get == nil {
		panic(fmt.Errorf("unsupported URL scheme %s", root.Scheme))
	}

	return func(ctx context.Context, p string) ([]byte, error) {
		u, err := root.Parse(p)
		if err != nil {
			return nil, err
		}
		return get(ctx, u)
	}
}

var getByScheme = map[string]func(context.Context, *url.URL) ([]byte, error){
	"http":  readHTTP,
	"https": readHTTP,
	"file": func(_ context.Context, u *url.URL) ([]byte, error) {
		return os.ReadFile(u.Path)
	},
}

func readHTTP(ctx context.Context, u *url.URL) ([]byte, error) {
	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	if len(*bearerToken) > 0 {
		req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", *bearerToken))
	}
	resp, err := hc.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			klog.Errorf("resp.Body.Close(): %v", err)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read body: %v", err)
	}

	switch resp.StatusCode {
	case 404:
		klog.Infof("Not found: %q", u.String())
		return nil, os.ErrNotExist
	case 200:
		break
	default:
		return nil, fmt.Errorf("unexpected http status %q", resp.Status)
	}
	return body, nil
}

type multiStringFlag []string

func (ms *multiStringFlag) String() string {
	return strings.Join(*ms, ",")
}

func (ms *multiStringFlag) Set(w string) error {
	*ms = append(*ms, w)
	return nil
}
