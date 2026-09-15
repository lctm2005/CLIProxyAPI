package harness

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	logs "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/tidwall/gjson"
)

func concurrency(h *harness, variant string) {
	level, err := strconv.Atoi(strings.TrimPrefix(variant, "concurrency_"))
	checkErr(h.t, err)
	if level != 1 && level != 2 && level != 4 {
		h.t.Fatal("Unbound concurrency level")
	}
	const tasks = 20
	for batch := 0; batch < tasks; batch += level {
		ids := map[string]int{}
		tokens := make([]string, level)
		sessions := make([]string, level)
		release := make([]chan struct{}, level)
		for i := range level {
			sessions[i] = uuid.NewString()
			tokens[i] = fmt.Sprintf("task-%02d-unique", batch+i)
			ids[sessions[i]] = i
			release[i] = make(chan struct{})
		}
		entered := make(chan int, level)
		firstDone := make(chan int, level)
		var wg sync.WaitGroup
		h.setResponder(func(w http.ResponseWriter, r *http.Request, b []byte, n int) {
			i, ok := ids[gjson.GetBytes(b, "session_id").String()]
			if !ok {
				h.require("known-session", false, "batch session", string(b))
				return
			}
			if bytes.Contains(b, []byte("RESULT-"+tokens[i])) {
				writeEvents(w, textEvent("ACK-"+tokens[i]), doneEvent())
				return
			}
			entered <- i
			select {
			case <-release[i]:
			case <-r.Context().Done():
				return
			}
			writeEvents(w, toolEvent("call-"+tokens[i], "echo_fixture", string(encode(map[string]string{"value": tokens[i]})), "function"), doneEvent())
		})
		for i := range level {
			wg.Add(1)
			go func() {
				defer wg.Done()
				p := h.payload(tokens[i], true)
				p["traecli_session_id"] = sessions[i]
				h.tools(p, false)
				status, raw := h.request(p)
				d := decode(h.protocol, raw)
				firstDone <- i
				h.require(tokens[i]+"/first", status == 200 && d.Complete && !d.Error && len(d.Calls) == 1, "one complete tool", d)
				if len(d.Calls) != 1 || !gjson.Valid(d.Calls[0].Input) {
					return
				}
				c := d.Calls[0]
				h.require(tokens[i]+"/isolation", c.ID == "call-"+tokens[i] && gjson.Get(c.Input, "value").String() == tokens[i], tokens[i], c)
				follow := h.continuation(tokens[i], c, "RESULT-"+tokens[i])
				follow["traecli_session_id"] = sessions[i]
				status, raw = h.request(follow)
				d = decode(h.protocol, raw)
				h.require(tokens[i]+"/roundtrip", status == 200 && d.Complete && !d.Error && d.Text == "ACK-"+tokens[i], "ACK-"+tokens[i], d)
			}()
		}
		for range level {
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				h.t.Fatal("concurrency barrier not reached")
			}
		}
		var order []int
		for i := level - 1; i >= 0; i-- {
			close(release[i])
			select {
			case got := <-firstDone:
				order = append(order, got)
				h.require("reverse-completion", got == i, i, got)
			case <-time.After(10 * time.Second):
				h.t.Fatal("completion barrier deadline")
			}
		}
		wg.Wait()
		checkErr(h.t, os.WriteFile(filepath.Join(h.art, fmt.Sprintf("batch-%02d.json", batch)), encode(map[string]any{"level": level, "completion_order": order, "logical_tasks": level}), 0o600))
	}
	h.require("workload", h.count() == tasks*2, tasks*2, h.count())
}

func slowReader(h *harness, variant string) {
	if variant != "slow_complete" && variant != "slow_cancel" {
		h.t.Fatal("Unbound slow reader variant")
	}
	const upstreamBytes = 1 << 20
	want := strings.Repeat("x", upstreamBytes-len(textEvent(""))-len(doneEvent()))
	wire := append(textEvent(want), doneEvent()...)
	h.require("frozen-upstream-bytes", len(wire) == upstreamBytes, upstreamBytes, len(wire))
	h.setResponder(func(w http.ResponseWriter, r *http.Request, b []byte, n int) {
		if bytes.Contains(b, []byte("slow-reader")) {
			writeEvents(w, wire)
		} else {
			writeEvents(w, textEvent("ordinary-ok"), doneEvent())
		}
	})
	ctx, cancel := context.WithCancel(h.t.Context())
	defer cancel()
	resp, err := h.open(ctx, h.payload("slow-reader", true), "fixture-proxy-key")
	checkErr(h.t, err)
	defer resp.Body.Close()
	var collected bytes.Buffer
	chunk := make([]byte, 1024)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	ordinaryDone := make(chan struct{})
	go func() {
		defer close(ordinaryDone)
		status, raw := h.request(h.payload("ordinary-concurrent", true))
		d := decode(h.protocol, raw)
		h.require("ordinary-request-progress", status == 200 && d.Complete && d.Text == "ordinary-ok", "ordinary-ok", d)
	}()
	started := time.Now()
	for {
		<-ticker.C
		n, errRead := resp.Body.Read(chunk)
		collected.Write(chunk[:n])
		if variant == "slow_cancel" && collected.Len() >= 65536 {
			cancel()
			break
		}
		if errRead == io.EOF {
			break
		}
		if errRead != nil {
			logs.CtxError(ctx, "slow reader: %v", errRead)
			h.t.Fatal(errRead)
		}
	}
	wait(h.t, ordinaryDone, "ordinary request during backpressure")
	if variant == "slow_complete" {
		d := decode(h.protocol, collected.Bytes())
		h.require("complete-slow-stream", d.Complete && !d.Error && d.Text == want, "all bytes, one completion", map[string]any{"text_bytes": len(d.Text), "text_sha256": digestBytes([]byte(d.Text)), "complete": d.Complete, "error": d.Error})
	} else {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) && (h.active.Load() != 0 || h.frontActive.Load() != 0) {
			time.Sleep(10 * time.Millisecond)
		}
		h.require("cancel-drained", h.active.Load() == 0 && h.frontActive.Load() == 0, "zero active upstream/downstream", []int64{h.active.Load(), h.frontActive.Load()})
	}
	checkErr(h.t, os.WriteFile(filepath.Join(h.art, "slow-reader.json"), encode(map[string]any{"upstream_bytes": len(wire), "downstream_bytes_read": collected.Len(), "chunk_bytes": 1024, "interval_ms": 50, "seconds": time.Since(started).Seconds()}), 0o600))
	h.normal()
}

type sample struct {
	FD         int    `json:"fd"`
	Goroutines int    `json:"goroutines"`
	Heap       uint64 `json:"live_heap_bytes"`
	RSS        uint64 `json:"rss_bytes"`
	Children   int    `json:"children"`
	Active     int64  `json:"active_requests"`
}

func (h *harness) sample() sample {
	runtime.GC()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	fds, err := os.ReadDir("/proc/self/fd")
	checkErr(h.t, err)
	raw, err := os.ReadFile("/proc/self/statm")
	checkErr(h.t, err)
	fields := strings.Fields(string(raw))
	if len(fields) < 2 {
		h.t.Fatal("RSS unavailable")
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	checkErr(h.t, err)
	children := 0
	paths, err := filepath.Glob("/proc/self/task/*/children")
	checkErr(h.t, err)
	for _, p := range paths {
		b, errRead := os.ReadFile(p)
		if os.IsNotExist(errRead) {
			continue
		}
		checkErr(h.t, errRead)
		children += len(strings.Fields(string(b)))
	}
	return sample{len(fds), runtime.NumGoroutine(), mem.HeapAlloc, pages * uint64(os.Getpagesize()), children, h.active.Load() + h.frontActive.Load()}
}

func (h *harness) settled() sample {
	// This is the declared resource settle window, not a synchronization surrogate.
	time.Sleep(10 * time.Second)
	var maximum sample
	var samples []sample
	for i := range 3 {
		if i > 0 {
			time.Sleep(time.Second)
		}
		s := h.sample()
		samples = append(samples, s)
		maximum.FD = max(maximum.FD, s.FD)
		maximum.Goroutines = max(maximum.Goroutines, s.Goroutines)
		maximum.Heap = max(maximum.Heap, s.Heap)
		maximum.RSS = max(maximum.RSS, s.RSS)
		maximum.Children = max(maximum.Children, s.Children)
		maximum.Active = max(maximum.Active, s.Active)
	}
	checkErr(h.t, os.WriteFile(filepath.Join(h.art, fmt.Sprintf("resources-%04d.json", h.count())), encode(samples), 0o600))
	return maximum
}
func lifecycle(h *harness) {
	cycle := func() {
		h.normal()
		h.setResponder(func(w http.ResponseWriter, r *http.Request, b []byte, n int) {
			w.WriteHeader(503)
			writeEvents(w, []byte(`{"code":"UPSTREAM_ERROR","message":"lifecycle failure"}`))
		})
		status, _ := h.request(h.payload("terminal503", true))
		h.require("terminal-503", status == 503, 503, status)
		cancelOnce(h, false)
	}
	for range 3 {
		cycle()
	}
	baseline := h.settled()
	for range 20 {
		cycle()
	}
	last := h.settled()
	h.require("active-zero", last.Active == 0, 0, last.Active)
	h.require("owned-children-zero", last.Children == 0, 0, last.Children)
	h.require("fd-budget", last.FD-baseline.FD <= 8, "delta <= 8", last.FD-baseline.FD)
	h.require("goroutine-budget", last.Goroutines-baseline.Goroutines <= 8, "delta <= 8", last.Goroutines-baseline.Goroutines)
	h.require("heap-budget", int64(last.Heap)-int64(baseline.Heap) <= 32<<20, "delta <= 32 MiB", int64(last.Heap)-int64(baseline.Heap))
	limit := max(uint64(64<<20), baseline.RSS/4)
	h.require("rss-budget", int64(last.RSS)-int64(baseline.RSS) <= int64(limit), limit, int64(last.RSS)-int64(baseline.RSS))
	checkErr(h.t, os.WriteFile(filepath.Join(h.art, "resource-comparison.json"), encode(map[string]any{"warmup_cycles": 3, "measured_cycles": 20, "baseline": baseline, "final": last, "rss_delta_limit": limit, "same_proxy": h.proxy.URL}), 0o600))
}
