package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

func frame(v any) []byte { return append(append([]byte("data: "), encode(v)...), []byte("\n\n")...) }

func (h *harness) normal() {
	h.setResponder(func(w http.ResponseWriter, r *http.Request, b []byte, n int) {
		writeEvents(w, textEvent("fixture-ok"), doneEvent())
	})
	status, raw := h.request(h.payload("follow-up-"+uuid.NewString(), true))
	d := decode(h.protocol, raw)
	h.require("follow-up-healthy", status == 200 && d.Complete && !d.Error && d.Text == "fixture-ok", "complete fixture-ok", d)
}

func (h *harness) tools(p map[string]any, custom bool) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}, "required": []string{"value"}}
	switch h.protocol {
	case "responses":
		if custom {
			p["tools"] = []any{map[string]any{"type": "custom", "name": "apply_patch", "format": map[string]any{"type": "text"}}}
		} else {
			p["tools"] = []any{map[string]any{"type": "function", "name": "echo_fixture", "parameters": schema}}
		}
	case "messages":
		p["tools"] = []any{map[string]any{"name": "echo_fixture", "input_schema": schema}}
	default:
		p["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": "echo_fixture", "parameters": schema}}}
	}
}

func (h *harness) continuation(prompt string, c call, result string) map[string]any {
	p := h.payload(prompt, true)
	h.tools(p, false)
	switch h.protocol {
	case "responses":
		p["input"] = []any{map[string]any{"role": "user", "content": prompt}, map[string]any{"type": "function_call", "call_id": c.ID, "name": c.Name, "arguments": c.Input}, map[string]any{"type": "function_call_output", "call_id": c.ID, "output": result}}
	case "messages":
		p["messages"] = []any{map[string]any{"role": "user", "content": prompt}, map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name, "input": json.RawMessage(c.Input)}}}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": c.ID, "content": result}}}}
	default:
		p["messages"] = []any{map[string]any{"role": "user", "content": prompt}, map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": c.ID, "type": "function", "function": map[string]any{"name": c.Name, "arguments": c.Input}}}}, map[string]any{"role": "tool", "tool_call_id": c.ID, "content": result}}
	}
	return p
}

func runScenario(h *harness, id, variant string) {
	switch id {
	case "TC-A3-001":
		incremental(h)
	case "TC-A3-002":
		fragmentation(h)
	case "TC-A3-004":
		earlyEOF(h)
	case "TC-A4-003":
		freeform(h)
	case "TC-A4-005":
		interleaved(h)
	case "TC-A5-002":
		outOfOrder(h)
	case "TC-A6-001":
		httpErrors(h)
	case "TC-A6-002":
		embeddedErrors(h)
	case "TC-A6-003":
		retries(h)
	case "TC-A6-004":
		cancellation(h)
	case "TC-A8-001":
		concurrency(h, variant)
	case "TC-A8-002":
		slowReader(h, variant)
	case "TC-A8-003":
		lifecycle(h)
	default:
		h.t.Fatalf("No executable contract for %s; unsupported cases must be BLOCKED by the coordinator", id)
	}
}

func incremental(h *harness) {
	release := make(chan struct{})
	defer close(release)
	h.setResponder(func(w http.ResponseWriter, r *http.Request, b []byte, n int) {
		writeEvents(w, textEvent("first"))
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		writeEvents(w, textEvent("last"), doneEvent())
	})
	ctx, cancel := context.WithTimeout(h.t.Context(), 10*time.Second)
	defer cancel()
	resp, err := h.open(ctx, h.payload("incremental", true), "fixture-proxy-key")
	checkErr(h.t, err)
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	var received []byte
	for scanner.Scan() {
		received = append(received, append(bytes.Clone(scanner.Bytes()), '\n')...)
		if decode(h.protocol, received).Text == "first" {
			break
		}
	}
	checkErr(h.t, scanner.Err())
	h.require("text-before-upstream-completion", decode(h.protocol, received).Text == "first", "first while upstream blocked", string(received))
}

func fragmentation(h *harness) {
	text := "中文 user's \\path\nline"
	args := string(encode(map[string]string{"value": text}))
	raw := bytes.Join([][]byte{textEvent(text), toolEvent("fragment-call", "echo_fixture", args, "function"), doneEvent()}, nil)
	for _, variant := range []string{"utf8", "json_escape", "delimiter", "coalesced", "lf", "crlf"} {
		wire := bytes.Clone(raw)
		if variant == "crlf" {
			wire = bytes.ReplaceAll(wire, []byte("\n"), []byte("\r\n"))
		}
		h.setResponder(func(w http.ResponseWriter, r *http.Request, b []byte, n int) {
			if variant == "coalesced" || variant == "lf" || variant == "crlf" {
				writeEvents(w, wire)
				return
			}
			// Byte-sized writes split every UTF-8 sequence, escape and SSE delimiter.
			for i := range wire {
				writeEvents(w, wire[i:i+1])
			}
		})
		p := h.payload("fragment-"+variant, true)
		h.tools(p, false)
		status, result := h.request(p)
		d := decode(h.protocol, result)
		h.require(variant+"/content", status == 200 && d.Text == text && d.Complete && !d.Error, text, d)
		h.require(variant+"/tool", len(d.Calls) == 1 && d.Calls[0] == (call{"fragment-call", "echo_fixture", args}), call{"fragment-call", "echo_fixture", args}, d.Calls)
	}
}

func earlyEOF(h *harness) {
	for _, variant := range []string{"empty", "text", "tool_arguments"} {
		h.setResponder(func(w http.ResponseWriter, r *http.Request, b []byte, n int) {
			switch variant {
			case "empty":
				w.(http.Flusher).Flush()
			case "text":
				writeEvents(w, textEvent("partial"))
			case "tool_arguments":
				writeEvents(w, toolEvent("incomplete", "echo_fixture", `{"value":"unterminated`, "function"))
			}
		})
		p := h.payload("eof-"+variant, true)
		h.tools(p, false)
		status, result := h.request(p)
		d := decode(h.protocol, result)
		h.require(variant+"/no-fabricated-success", status != 200 || d.Error || !d.Complete, "error or incomplete", d)
		h.require(variant+"/no-incomplete-tool-dispatch", len(d.Calls) == 0, 0, d.Calls)
		h.normal()
	}
}

func freeform(h *harness) {
	inputs := []string{"*** Begin Patch\n*** Update File: probe.py\n@@\n-x = 1\n+x = 'user's'\n*** End Patch", "SELECT 'active', \"active\" FROM records;", "C:\\Users\\fixture\\中文", " \tleading\ntrailing \t\n", "*** Begin Patch\n*** Add File: a.txt\n+中文 user's\n+second\n*** End Patch\n"}
	for i, input := range inputs {
		h.setResponder(func(w http.ResponseWriter, r *http.Request, b []byte, n int) {
			writeEvents(w, toolEvent("patch-call", "apply_patch", input, "custom"), doneEvent())
		})
		p := h.payload(fmt.Sprintf("freeform-%d", i), true)
		h.tools(p, true)
		status, raw := h.request(p)
		d := decode(h.protocol, raw)
		got := ""
		if len(d.Calls) == 1 {
			got = d.Calls[0].Input
		}
		h.require(fmt.Sprintf("bytes-%d", i), status == 200 && len(d.Calls) == 1 && got == input, map[string]any{"bytes": len(input), "sha256": digestBytes([]byte(input))}, map[string]any{"bytes": len(got), "sha256": digestBytes([]byte(got)), "input": got})
		for _, damaged := range []string{input + " ", strings.ReplaceAll(input, "'", "\""), strings.TrimSpace(input), strings.ReplaceAll(input, "\\", "")} {
			if damaged != input {
				h.require(fmt.Sprintf("negative-control-%d", i), digestBytes([]byte(damaged)) != digestBytes([]byte(input)), "damaged fixture rejected", true)
			}
		}
	}
}

func interleaved(h *harness) {
	for _, variant := range []string{"interleaved", "late_name", "repeated_snapshot"} {
		h.setResponder(func(w http.ResponseWriter, r *http.Request, b []byte, n int) {
			part := func(index int, id, name, args string) []byte {
				return frame(map[string]any{"tool_calls": []any{map[string]any{"index": index, "id": id, "type": "function", "function_call": map[string]string{"name": name, "arguments": args}}}})
			}
			if variant == "repeated_snapshot" {
				for range 2 {
					writeEvents(w, part(0, "call-a", "echo_fixture", `{"value":"A"}`), part(1, "call-b", "echo_fixture", `{"value":"B"}`))
				}
			} else {
				name := "echo_fixture"
				if variant == "late_name" {
					name = ""
				}
				writeEvents(w, part(0, "call-a", name, `{"value":`), part(1, "call-b", name, `{"value":`))
				late := ""
				if variant == "late_name" {
					late = "echo_fixture"
				}
				writeEvents(w, part(1, "", late, `"B"}`), part(0, "", late, `"A"}`))
			}
			writeEvents(w, doneEvent())
		})
		p := h.payload(variant, true)
		h.tools(p, false)
		status, raw := h.request(p)
		d := decode(h.protocol, raw)
		want := []call{{"call-a", "echo_fixture", `{"value":"A"}`}, {"call-b", "echo_fixture", `{"value":"B"}`}}
		h.require(variant, status == 200 && d.Complete && !d.Error && bytes.Equal(encode(want), encode(d.Calls)), want, d.Calls)
	}
}

func outOfOrder(h *harness) {
	for _, oldFails := range []bool{false, true} {
		session := uuid.NewString()
		entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
		start := h.count()
		h.setResponder(func(w http.ResponseWriter, r *http.Request, b []byte, n int) {
			if n == start+1 {
				close(entered)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				if oldFails {
					w.WriteHeader(503)
					writeEvents(w, frame(map[string]any{"code": "UPSTREAM_ERROR", "message": "old-failure"}))
					return
				}
				writeEvents(w, frame(map[string]any{"response": "old", "extra_info": map[string]string{"opaque": "old"}}), doneEvent())
				return
			}
			writeEvents(w, frame(map[string]any{"response": "new", "extra_info": map[string]string{"opaque": "new"}}), doneEvent())
		})
		payload := func(prompt string) map[string]any {
			p := h.payload(prompt, true)
			p["session_id"] = session
			p["traecli_session_id"] = session
			return p
		}
		go func() { defer close(finished); h.request(payload("old")) }()
		wait(h.t, entered, "old request entered")
		h.request(payload("new"))
		close(release)
		wait(h.t, finished, "old request completed")
		h.request(payload("observe"))
		captured := h.captured()
		value := gjson.GetBytes(captured[len(captured)-1], "extra_info.opaque").String()
		h.require(fmt.Sprintf("new-state-preserved-old-fails-%v", oldFails), value == "new", "new", value)
	}
}

func httpErrors(h *harness) {
	for _, code := range []int{401, 403, 429, 500, 503} {
		h.setResponder(func(w http.ResponseWriter, r *http.Request, b []byte, n int) {
			w.WriteHeader(code)
			writeEvents(w, []byte(`{"code":"FIXTURE_FAILURE","message":"controlled failure"}`))
		})
		before := h.count()
		status, raw := h.request(h.payload(fmt.Sprint("http-", code), true))
		h.require(fmt.Sprint("status-", code), status == code, code, status)
		h.require(fmt.Sprint("attempts-", code), h.count()-before == 1, 1, h.count()-before)
		h.require(fmt.Sprint("error-body-", code), gjson.GetBytes(raw, "error").Exists() || gjson.GetBytes(raw, "code").String() == "FIXTURE_FAILURE", "structured fixture error (protocol wrapper or preserved upstream body)", string(raw))
	}
}

func embeddedErrors(h *harness) {
	for _, after := range []bool{false, true} {
		h.setResponder(func(w http.ResponseWriter, r *http.Request, b []byte, n int) {
			if after {
				writeEvents(w, textEvent("delivered"))
			}
			writeEvents(w, frame(map[string]any{"code": "UPSTREAM_ERROR", "message": "controlled embedded error"}))
		})
		before := h.count()
		status, raw := h.request(h.payload("embedded", true))
		d := decode(h.protocol, raw)
		h.require(fmt.Sprintf("embedded-after-%v", after), status >= 400 || d.Error && !d.Complete, "observable error without success", d)
		h.require("embedded-no-replay", h.count()-before == 1, 1, h.count()-before)
	}
	h.normal()
}

func retries(h *harness) {
	h.manager.SetRetryConfig(1, 2*time.Second, 0)
	for _, variant := range []string{"503_success", "429_success", "persistent503", "after_delivery"} {
		before := h.count()
		h.setResponder(func(w http.ResponseWriter, r *http.Request, b []byte, n int) {
			if variant == "after_delivery" {
				writeEvents(w, textEvent("delivered"), frame(map[string]any{"code": "UPSTREAM_ERROR", "message": "controlled embedded error"}))
				return
			}
			if n == before+1 || variant == "persistent503" {
				code := 503
				if variant == "429_success" {
					code = 429
					w.Header().Set("Retry-After", "1")
				}
				w.WriteHeader(code)
				writeEvents(w, []byte(`{"code":"UPSTREAM_ERROR","message":"retry fixture"}`))
				return
			}
			writeEvents(w, textEvent("recovered"), doneEvent())
		})
		status, raw := h.request(h.payload("retry-"+variant, true))
		d := decode(h.protocol, raw)
		want := 2
		if variant == "after_delivery" {
			want = 1
		}
		h.require(variant+"/attempts", h.count()-before == want, want, h.count()-before)
		if strings.HasSuffix(variant, "_success") {
			h.require(variant+"/recovery", status == 200 && d.Text == "recovered" && d.Complete && !d.Error, "one recovered result", d)
		} else {
			h.require(variant+"/no-success", status >= 400 || d.Error && !d.Complete, "terminal failure", d)
		}
	}
}

func cancelOnce(h *harness, after bool) {
	entered, cancelled := make(chan struct{}), make(chan struct{})
	h.setResponder(func(w http.ResponseWriter, r *http.Request, b []byte, n int) {
		close(entered)
		if after {
			writeEvents(w, textEvent("first"))
		}
		<-r.Context().Done()
		close(cancelled)
	})
	ctx, cancel := context.WithCancel(h.t.Context())
	defer cancel()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		resp, err := h.open(ctx, h.payload("cancel", true), "fixture-proxy-key")
		if err != nil {
			return
		}
		defer resp.Body.Close()
		if after {
			scanner := bufio.NewScanner(resp.Body)
			var raw []byte
			for scanner.Scan() {
				raw = append(raw, append(bytes.Clone(scanner.Bytes()), '\n')...)
				if decode(h.protocol, raw).Text == "first" {
					cancel()
					return
				}
			}
			checkErr(h.t, scanner.Err())
		}
	}()
	wait(h.t, entered, "cancel request entered")
	if !after {
		cancel()
	}
	wait(h.t, cancelled, "upstream cancellation")
	wait(h.t, finished, "downstream exited")
	h.require(fmt.Sprintf("cancel-propagated-after-%v", after), true, "within 10 seconds", true)
}
func cancellation(h *harness) { cancelOnce(h, false); h.normal(); cancelOnce(h, true); h.normal() }
