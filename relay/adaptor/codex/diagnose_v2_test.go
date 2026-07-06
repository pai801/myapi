package codex_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/pai801/myapi/relay/adaptor/codex"
	"github.com/tidwall/gjson"
)

func TestDiagnoseV2(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	base := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	rawReqPath := filepath.Join(base, "request_raw.txt")
	if _, err := os.Stat(rawReqPath); os.IsNotExist(err) {
		t.Skipf("skip: %s not found", rawReqPath)
	}

	rawReq, err := os.ReadFile(rawReqPath)
	if err != nil { t.Fatalf("read request: %v", err) }
	firstLine := ""
	for _, line := range strings.Split(string(rawReq), "\n") {
		if t := strings.TrimSpace(line); t != "" { firstLine = t; break }
	}
	if firstLine == "" { t.Fatal("empty request") }
	if !gjson.Valid(firstLine) { t.Fatalf("not valid JSON") }

	rawResp, err := os.ReadFile(base + "/response_raw.txt")
	if err != nil { t.Fatalf("read response: %v", err) }

	dataLines := strings.Split(string(rawResp), "\n")
	var chunks []string
	for _, line := range dataLines {
		t := strings.TrimSpace(line)
		if t != "" && strings.HasPrefix(t, "data:") { chunks = append(chunks, t) }
	}

	fmt.Printf("=== Request first JSON (200 chars) ===\n%s...\nChunks: %d\n\n", firstLine[:min(200, len(firstLine))], len(chunks))

	var param any
	var allEvents []string
	for i, chunk := range chunks {
		evts := codex.ConvertOpenAIChatToResponsesWithContext([]byte(firstLine), nil, []byte(chunk), &param, false)
		allEvents = append(allEvents, evts...)
		if len(evts) > 0 { fmt.Printf("[chunk %d] -> %d events\n", i+1, len(evts)) }
	}
	fmt.Printf("Total events: %d\n\n", len(allEvents))

	// ========== 1 ==========
	fmt.Println("=== 1. Event Types in Order ===")
	var etypes []string
	for _, evt := range allEvents {
		for _, l := range strings.Split(evt, "\n") {
			l = strings.TrimSpace(l)
			if strings.HasPrefix(l, "event: ") { etypes = append(etypes, l[7:]); break }
		}
	}
	s := map[string]int{}; ord := []string{}
	for _, e := range etypes { if s[e] == 0 { ord = append(ord, e) }; s[e]++ }
	for _, e := range ord { fmt.Printf("  %s (x%d)\n", e, s[e]) }

	// ========== 2 ==========
	fmt.Println("\n=== 2. Full Tool Item JSON in output_item.added/done ===")
	for i, evt := range allEvents {
		et := extractEventType(evt)
		ds := extractDataStr(evt)
		if et != "response.output_item.added" && et != "response.output_item.done" { continue }
		if !gjson.Valid(ds) { continue }
		p := gjson.Parse(ds)
		it := p.Get("item")
		if !it.Exists() { continue }
		tt := it.Get("type").String()
		if tt != "tool_search_call" && tt != "web_search_call" && tt != "custom_tool_call" && tt != "function_call" { continue }
		fmt.Printf("--- [%d] %s ---\n", i+1, et)
		var m map[string]interface{}; json.Unmarshal([]byte(it.Raw), &m)
		raw, _ := json.MarshalIndent(m, "  ", "  ")
		fmt.Printf("  %s\n", raw)
	}

	// ========== 3 ==========
	fmt.Println("\n=== 3. response.completed output array ===")
	for _, evt := range allEvents {
		if !strings.Contains(evt, "event: response.completed") { continue }
		ds := extractDataStr(evt)
		if !gjson.Valid(ds) { continue }
		r := gjson.Parse(ds)
		out := r.Get("response.output")
		if out.IsArray() && len(out.Array()) > 0 {
			fmt.Printf("  response.id=%s status=%s outlen=%d\n", r.Get("response.id").String(), r.Get("response.status").String(), len(out.Array()))
			for j, it := range out.Array() {
				var m map[string]interface{}; json.Unmarshal([]byte(it.Raw), &m)
				raw, _ := json.MarshalIndent(m, "  ", "  ")
				fmt.Printf("  [output[%d]]:\n    %s\n", j, raw)
			}
		} else { fmt.Println("  OUTPUT EMPTY OR MISSING") }
	}

	// ========== 4 ==========
	fmt.Println("\n=== 4. Comparison with response_codex_raw.txt lines 50-55 ===")
	codexRaw, _ := os.ReadFile(base + "/response_codex_raw.txt")
	cl := strings.Split(string(codexRaw), "\n")
	if len(cl) >= 55 {
		fmt.Println("  --- Codex raw lines 50-55 ---")
		for j := 49; j < 55; j++ {
			fmt.Printf("  [%d] %s\n", j+1, cl[j][:min(200, len(cl[j]))])
		}

		// Strip "data: " prefix helper
		stripData := func(s string) string { s = strings.TrimSpace(s); if strings.HasPrefix(s, "data:") { s = s[5:] }; return s }

		// Parse codex data lines (indices 50, 52, 54)
		d50 := stripData(cl[50])
		d52 := stripData(cl[52])
		d54 := stripData(cl[54])

		fmt.Println("\n  --- Codex Parsed Data ---")
		if gjson.Valid(d50) {
			cp := gjson.Parse(d50)
			fmt.Printf("  added: type=%s id=%s status=%s args=%s call_id=%s execution=%v\n",
				cp.Get("item.type").String(), cp.Get("item.id").String(), cp.Get("item.status").String(),
				cp.Get("item.arguments").Raw, cp.Get("item.call_id").String(), cp.Get("item.execution").String())
		} else { fmt.Printf("  line 51 INVALID JSON: %.80s\n", d50[:min(80, len(d50))]) }

		if gjson.Valid(d52) {
			dp := gjson.Parse(d52)
			fmt.Printf("  done:  type=%s id=%s status=%s args=%s call_id=%s execution=%v\n",
				dp.Get("item.type").String(), dp.Get("item.id").String(), dp.Get("item.status").String(),
				dp.Get("item.arguments").Raw, dp.Get("item.call_id").String(), dp.Get("item.execution").String())
		} else { fmt.Printf("  line 53 INVALID JSON: %.80s\n", d52[:min(80, len(d52))]) }

		if gjson.Valid(d54) {
			dp := gjson.Parse(d54)
			out := dp.Get("response.output")
			if out.IsArray() {
				fmt.Printf("  completed: output count=%d\n", len(out.Array()))
				for j, it := range out.Array() {
					fmt.Printf("    [%d] type=%s id=%s call_id=%s name=%v", j, it.Get("type").String(),
						it.Get("id").String(), it.Get("call_id").String(), it.Get("name").String())
					if it.Get("type").String() == "tool_search_call" { fmt.Printf(" args=%s", it.Get("arguments").Raw) }
					fmt.Println()
				}
			}
		} else { fmt.Printf("  line 55 INVALID JSON: %.80s\n", d54[:min(80, len(d54))]) }

		// Find our tool_search events
		var ourAdded, ourDone *gjson.Result
		for _, evt := range allEvents {
			et := extractEventType(evt)
			ds := extractDataStr(evt)
			if et != "response.output_item.added" && et != "response.output_item.done" { continue }
			if !gjson.Valid(ds) { continue }
			p := gjson.Parse(ds)
			if p.Get("item.type").String() == "tool_search_call" {
				if et == "response.output_item.added" { ourAdded = &p } else { ourDone = &p }
			}
		}

		fmt.Println("\n  --- Key Differences ---")

		if ourAdded != nil {
			oi := ourAdded.Get("item")
			fmt.Println("  OUR output_item.added:")
			fmt.Printf("    type=%s id=%s name=%s status=%s call_id=%s args=%s\n",
				oi.Get("type").String(), oi.Get("id").String(), oi.Get("name").String(),
				oi.Get("status").String(), oi.Get("call_id").String(), oi.Get("arguments").String())
			if gjson.Valid(d50) {
				cp := gjson.Parse(d50)
				fmt.Println("  COMPARISON with codex:")
				fmt.Printf("    codex id=%s, our id=%s\n", cp.Get("item.id").String(), oi.Get("id").String())
				fmt.Printf("    codex args=%s, our args=%s\n", cp.Get("item.arguments").Raw, oi.Get("arguments").String())
				fmt.Printf("    codex execution=%v, we have=%v\n", cp.Get("item.execution").String(), ourAdded.Get("item.execution").Exists())
				fmt.Printf("    codex name=%v, our name=%s\n", cp.Get("item.name").String(), oi.Get("name").String())
				fmt.Printf("    codex type=%s, our type=%s\n", cp.Get("item.type").String(), oi.Get("type").String())
			}
		} else { fmt.Println("  *** WE PRODUCED NO tool_search_call output_item.added ***") }

		if ourDone != nil {
			oi := ourDone.Get("item")
			fmt.Println("\n  OUR output_item.done:")
			fmt.Printf("    type=%s id=%s name=%s status=%s call_id=%s args=%s\n",
				oi.Get("type").String(), oi.Get("id").String(), oi.Get("name").String(),
				oi.Get("status").String(), oi.Get("call_id").String(), oi.Get("arguments").String())
			if gjson.Valid(d52) {
				dp := gjson.Parse(d52)
				fmt.Println("  COMPARISON with codex:")
				fmt.Printf("    codex id=%s, our id=%s\n", dp.Get("item.id").String(), oi.Get("id").String())
				ca := dp.Get("item.arguments").Raw
				oa := oi.Get("arguments").Raw
				fmt.Printf("    codex args=%s, our args=%s\n", ca, oa)
				if ca != oa { fmt.Println("    *** DIFFERENT arguments ***") }
				fmt.Printf("    codex execution=%v, we have=%v\n", dp.Get("item.execution").String(), ourDone.Get("item.execution").Exists())

				// Codex lifecycle events
				fmt.Println("\n    Codex lifecycle events (lines 49-55):")
				for j := 48; j < 56; j++ {
					ln := strings.TrimSpace(cl[j])
					if strings.HasPrefix(ln, "event:") || strings.HasPrefix(ln, "data:") {
						fmt.Printf("      [%d] %s\n", j+1, ln[:min(200, len(ln))])
					}
				}
			}
		} else { fmt.Println("\n  *** WE PRODUCED NO tool_search_call output_item.done ***") }
	}

	// ========== 5 ==========
	fmt.Println("\n=== 5. Full Event Stream ===")
	for i, evt := range allEvents {
		et := extractEventType(evt)
		ds := extractDataStr(evt)
		fmt.Printf("[%2d] %s", i+1, et)
		if gjson.Valid(ds) {
			p := gjson.Parse(ds)
			switch et {
			case "response.output_item.added":
				it := p.Get("item")
				fmt.Printf(" -> type=%s id=%s name=%s status=%s oi=%d", it.Get("type").String(), it.Get("id").String(),
					it.Get("name").String(), it.Get("status").String(), int(p.Get("output_index").Int()))
			case "response.output_item.done":
				it := p.Get("item")
				fmt.Printf(" -> type=%s id=%s status=%s oi=%d", it.Get("type").String(), it.Get("id").String(),
					it.Get("status").String(), int(p.Get("output_index").Int()))
			case "response.completed":
				fmt.Printf(" -> id=%s status=%s outlen=%d", p.Get("response.id").String(),
					p.Get("response.status").String(), len(p.Get("response.output").Array()))
			case "response.created":
				fmt.Printf(" -> id=%s model=%s", p.Get("response.id").String(), p.Get("response.model").String())
			default:
				if len(ds) > 100 { fmt.Printf(" -> %.100s...", ds[:100]) } else { fmt.Printf(" -> %s", ds) }
			}
		}
		fmt.Println()
	}
	t.Log("Diagnosis complete")
}

func extractEventType(evt string) string {
	eIdx := strings.Index(evt, "event: ")
	if eIdx >= 0 {
		eEnd := strings.Index(evt[eIdx+7:], "\n")
		if eEnd >= 0 {
			return evt[eIdx+7 : eIdx+7+eEnd]
		}
	}
	return ""
}
func extractDataStr(evt string) string {
	dIdx := strings.Index(evt, "data: ")
	if dIdx >= 0 { return strings.TrimSpace(evt[dIdx+6:]) }
	return ""
}
func min(a, b int) int { if a < b { return a }; return b }
