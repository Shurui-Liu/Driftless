// lincheck — Porcupine linearizability checker for Experiment 3.
//
// Reads a history.jsonl produced by exp3.py and verifies that the sequence of
// observed task statuses is consistent with a system that only moves each task
// forward through the state machine:
//
//   PENDING (1) → ASSIGNED (2) → COMPLETED / FAILED (3)
//
// Each task_id's poll_observe events form an independent series of concurrent
// read operations on that task's status register. Porcupine checks that for
// every task there exists a valid sequential linearization — i.e., no observed
// status code is lower than a previously observed one (no backward transition).
//
// Usage:
//
//	go run . -history <path/to/history.jsonl> [-out <path/to/result.json>]
//	go build -o lincheck . && ./lincheck -history history.jsonl
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/anishathalye/porcupine"
)

// ── Status codes ─────────────────────────────────────────────────────────────

const (
	codeUnknown   = 0
	codePending   = 1
	codeAssigned  = 2
	codeTerminal  = 3 // COMPLETED or FAILED
)

func statusCode(s string) int {
	switch s {
	case "PENDING":
		return codePending
	case "ASSIGNED":
		return codePending + 1 // same as codeAssigned
	case "COMPLETED", "FAILED":
		return codeTerminal
	default:
		return codeUnknown
	}
}

// ── Porcupine model ───────────────────────────────────────────────────────────
//
// State    int   — maximum status code observed for this task so far.
// Input    int   — (unused; all ops on the same task share the same model)
// Output   int   — the status code returned by this poll.
//
// Step is valid iff output >= state (status never goes backward).

type pollInput struct{ TaskID string }
type pollOutput struct{ Code int }

var monotoneModel = porcupine.Model{
	Init: func() interface{} { return 0 },

	Step: func(state, input, output interface{}) (bool, interface{}) {
		cur := state.(int)
		code := output.(pollOutput).Code
		if code == codeUnknown {
			// Unknown / null status from DynamoDB — treat as non-violating skip.
			return true, cur
		}
		if code < cur {
			// Backward transition — linearizability violation.
			return false, cur
		}
		newMax := code
		if cur > newMax {
			newMax = cur
		}
		return true, newMax
	},

	Equal: func(s1, s2 interface{}) bool {
		return s1.(int) == s2.(int)
	},

	DescribeOperation: func(input, output interface{}) string {
		in := input.(pollInput)
		out := output.(pollOutput)
		return fmt.Sprintf("poll(%s…) → %d", in.TaskID[:8], out.Code)
	},

	DescribeState: func(state interface{}) string {
		return fmt.Sprintf("max_code=%d", state.(int))
	},
}

// ── History parsing ───────────────────────────────────────────────────────────

type rawEvent struct {
	Op       string `json:"op"`
	TaskID   string `json:"task_id"`
	CallNs   int64  `json:"call_ns"`
	ReturnNs int64  `json:"return_ns"`
	Status   string `json:"status"`
}

// perTaskOps groups Porcupine operations by task_id.
func loadHistory(path string) (map[string][]porcupine.Operation, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	groups := make(map[string][]porcupine.Operation)
	clientID := make(map[string]int)
	nextID := 0

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev rawEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Op != "poll_observe" || ev.TaskID == "" {
			continue
		}
		code := statusCode(ev.Status)
		if code == codeUnknown {
			continue
		}

		if _, ok := clientID[ev.TaskID]; !ok {
			clientID[ev.TaskID] = nextID
			nextID++
		}

		groups[ev.TaskID] = append(groups[ev.TaskID], porcupine.Operation{
			ClientId: clientID[ev.TaskID],
			Input:    pollInput{TaskID: ev.TaskID},
			Output:   pollOutput{Code: code},
			Call:     ev.CallNs,
			Return:   ev.ReturnNs,
		})
	}
	return groups, scanner.Err()
}

// ── Result ────────────────────────────────────────────────────────────────────

type CheckResult struct {
	TotalTasks          int      `json:"total_tasks"`
	TotalOperations     int      `json:"total_operations"`
	Violations          int      `json:"violations"`
	ViolatingTaskSample []string `json:"violating_task_sample"`
	Linearizable        bool     `json:"linearizable"`
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	historyFlag := flag.String("history", "", "path to history.jsonl (required)")
	outFlag := flag.String("out", "", "write JSON result to this file (optional)")
	flag.Parse()

	if *historyFlag == "" {
		fmt.Fprintln(os.Stderr, "usage: lincheck -history <history.jsonl> [-out result.json]")
		os.Exit(2)
	}

	groups, err := loadHistory(*historyFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to read history: %v\n", err)
		os.Exit(1)
	}

	var violating []string
	totalOps := 0

	// Check each task independently — tasks are key-disjoint so this is
	// equivalent to a single global Porcupine check but O(n) instead of
	// O(n²) in state-space size.
	for taskID, ops := range groups {
		totalOps += len(ops)
		// Sort by Call time so sequential polls from the same client are ordered.
		sort.Slice(ops, func(i, j int) bool { return ops[i].Call < ops[j].Call })
		if !porcupine.CheckOperations(monotoneModel, ops) {
			violating = append(violating, taskID)
		}
	}

	sort.Strings(violating)
	sample := violating
	if len(sample) > 20 {
		sample = sample[:20]
	}

	res := CheckResult{
		TotalTasks:          len(groups),
		TotalOperations:     totalOps,
		Violations:          len(violating),
		ViolatingTaskSample: sample,
		Linearizable:        len(violating) == 0,
	}

	out, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(out))

	if *outFlag != "" {
		if err := os.WriteFile(*outFlag, out, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write output file: %v\n", err)
		}
	}

	if !res.Linearizable {
		fmt.Fprintf(os.Stderr,
			"VIOLATION: %d task(s) showed backward status transitions\n",
			res.Violations,
		)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "OK: all task status transitions are linearizable")
}
