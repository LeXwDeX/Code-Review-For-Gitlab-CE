package dag

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestCascadeStatusTable(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name   string
		nodes  []NodeSpec
		status map[string]NodeStatus
		want   string
		stop   bool
	}{
		{
			name:   "required failed",
			nodes:  []NodeSpec{node("a"), node("b", "a")},
			status: map[string]NodeStatus{"a": NodeFailed},
			want:   "unsatisfied",
			stop:   true,
		},
		{
			name:   "required aborted",
			nodes:  []NodeSpec{node("a"), node("b", "a")},
			status: map[string]NodeStatus{"a": NodeAborted},
			want:   "unsatisfied",
			stop:   true,
		},
		{
			name:   "optional failed",
			nodes:  []NodeSpec{{ID: "a", Kind: "x"}, node("b", "a")},
			status: map[string]NodeStatus{"a": NodeFailed},
			stop:   false,
		},
		{
			name:   "all skipped",
			nodes:  []NodeSpec{node("a"), node("b", "a")},
			status: map[string]NodeStatus{"a": NodeSkipped},
			want:   "deps_skipped",
			stop:   true,
		},
		{
			name:   "skipped plus completed",
			nodes:  []NodeSpec{node("a"), node("c"), node("b", "a", "c")},
			status: map[string]NodeStatus{"a": NodeSkipped, "c": NodeCompleted},
			stop:   false,
		},
		{
			name:   "condition false",
			nodes:  []NodeSpec{node("a"), {ID: "b", Kind: "x", DependsOn: []string{"a"}, Condition: "a.output.ok == true"}},
			status: map[string]NodeStatus{"a": NodeCompleted},
			want:   "condition_false",
			stop:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wf := WorkflowView{Nodes: map[string]NodeView{}}
			for _, spec := range tc.nodes {
				status, ok := tc.status[spec.ID]
				if !ok {
					status = NodeCompleted
				}
				output := json.RawMessage(`{"ok":false}`)
				wf.Nodes[spec.ID] = NodeView{ID: spec.ID, Spec: spec, Status: status, Output: output}
			}
			target := tc.nodes[len(tc.nodes)-1]
			reason, stop := cascadeStatus(wf, target)
			if reason != tc.want || stop != tc.stop {
				t.Fatalf("got (%q,%v), want (%q,%v)", reason, stop, tc.want, tc.stop)
			}
		})
	}
	_ = now
}

func TestResolveInputPlaceholders(t *testing.T) {
	wf := WorkflowView{Nodes: map[string]NodeView{
		"ok":      {ID: "ok", Status: NodeCompleted, Output: json.RawMessage(`{"x":{"y":3}}`)},
		"badjson": {ID: "badjson", Status: NodeCompleted, Output: json.RawMessage(`{`)},
		"failed":  {ID: "failed", Status: NodeFailed},
		"aborted": {ID: "aborted", Status: NodeAborted},
		"skipped": {ID: "skipped", Status: NodeSkipped},
		"pending": {ID: "pending", Status: NodePending},
		"empty":   {ID: "empty", Status: NodeCompleted},
	}}
	tests := []struct {
		ref      string
		want     string
		reason   string
		hasValue bool
	}{
		{ref: "ok.output.x", hasValue: true},
		{ref: "ok.output.x.y", hasValue: true},
		{ref: "ok.output.missing", reason: "output_missing"},
		{ref: "badjson.output.x", reason: "output_missing"},
		{ref: "empty.output.x", reason: "output_missing"},
		{ref: "failed.output.x", reason: "dependency_failed"},
		{ref: "aborted.output.x", reason: "dependency_aborted"},
		{ref: "skipped.output.x", reason: "dependency_skipped"},
		{ref: "pending.output.x", reason: "dependency_pending"},
		{ref: "ghost.output.x", reason: "invalid_reference"},
		{ref: "nonsense", reason: "invalid_reference"},
	}
	for _, tc := range tests {
		raw := resolveInput(wf, tc.ref)
		if tc.hasValue {
			if string(raw) != `{"y":3}` && string(raw) != `3` {
				t.Fatalf("%s = %s", tc.ref, raw)
			}
			continue
		}
		var placeholder map[string]any
		if err := json.Unmarshal(raw, &placeholder); err != nil {
			t.Fatalf("%s: %v", tc.ref, err)
		}
		if placeholder["_unavailable"] != true || placeholder["reason"] != tc.reason {
			t.Fatalf("%s = %v, want reason %s", tc.ref, placeholder, tc.reason)
		}
	}
}

func TestValidateExtensionTable(t *testing.T) {
	base := WorkflowView{
		Spec: GraphSpec{Nodes: []NodeSpec{node("plan"), node("gather", "plan")}},
		Nodes: map[string]NodeView{
			"plan":   {ID: "plan", Spec: node("plan"), Status: NodeCompleted},
			"gather": {ID: "gather", Spec: node("gather", "plan"), Status: NodePending},
		},
		ExtendsUsed: 0,
	}
	base.Spec.Budget = NormalizeBudget(base.Spec.Budget)
	tests := []struct {
		name string
		ext  *Extension
		err  error
	}{
		{name: "fan out", ext: &Extension{Nodes: []NodeSpec{node("cr.p1", "plan")}, DepPatches: []DepPatch{{Node: "gather", Add: []string{"cr.p1"}}}}},
		{name: "explicit extend", ext: &Extension{Nodes: []NodeSpec{node("cr.p1", "plan")}, Source: ExtensionSourceExtend}, err: nil},
		{name: "unknown source", ext: &Extension{Source: "nope"}, err: ErrInvalidGraph},
		{name: "patch unknown", ext: &Extension{DepPatches: []DepPatch{{Node: "ghost", Add: []string{"plan"}}}}, err: ErrInvalidGraph},
		{name: "duplicate", ext: &Extension{Nodes: []NodeSpec{node("plan")}}, err: ErrInvalidGraph},
		{name: "missing dep", ext: &Extension{Nodes: []NodeSpec{node("x", "ghost")}}, err: ErrInvalidGraph},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := validateExtension(base, tc.ext)
			if tc.err == nil {
				if err != nil {
					t.Fatal(err)
				}
				if payload.Source == "" {
					t.Fatal("source not normalized")
				}
				return
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	// non-pending patch target
	queued := cloneView(base)
	view := queued.Nodes["gather"]
	view.Status = NodeQueued
	queued.Nodes["gather"] = view
	if _, err := validateExtension(queued, &Extension{DepPatches: []DepPatch{{Node: "gather", Add: []string{"plan"}}}}); err == nil {
		t.Fatal("queued patch target accepted")
	}
	// exhausted extend budget
	exhausted := cloneView(base)
	exhausted.ExtendsUsed = exhausted.Spec.Budget.MaxExtends
	if _, err := validateExtension(exhausted, &Extension{Nodes: []NodeSpec{node("x", "plan")}, Source: ExtensionSourceExtend}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("error = %v", err)
	}
	// max node budget
	small := cloneView(base)
	small.Spec.Budget.MaxNodes = 2
	if _, err := validateExtension(small, &Extension{Nodes: []NodeSpec{node("x", "plan")}}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("error = %v", err)
	}
}

func TestPlanScheduleQueueTimeout(t *testing.T) {
	engine := NewEngine(NewMemoryEventStore(), nil, nil, Options{DefaultNodeTimeout: time.Minute})
	wf := WorkflowView{
		Spec: GraphSpec{Nodes: []NodeSpec{node("a")}},
		Nodes: map[string]NodeView{
			"a": {ID: "a", Spec: node("a"), Status: NodeQueued, QueuedAt: testNow, Attempt: 1},
		},
		Order: []string{"a"},
	}
	events, starts := engine.planSchedule(wf, testNow.Add(2*time.Minute))
	if len(starts) != 0 || len(events) != 1 || events[0].Type != EventNodeFailed {
		t.Fatalf("events=%v starts=%v", events, starts)
	}
	events, starts = engine.planSchedule(wf, testNow)
	if len(events) != 0 || len(starts) != 1 {
		t.Fatalf("events=%v starts=%v", events, starts)
	}
}

func TestSchedulerHelpers(t *testing.T) {
	wf := WorkflowView{
		Spec:  GraphSpec{Nodes: []NodeSpec{node("a"), node("b", "a")}},
		Nodes: map[string]NodeView{"a": {ID: "a", Spec: node("a"), Status: NodeCompleted}, "b": {ID: "b", Spec: node("b", "a"), Status: NodeRunning}},
	}
	if dependenciesTerminal(wf, node("b", "a")) != true {
		t.Fatal("terminal deps")
	}
	if dependenciesTerminal(wf, node("c", "missing")) {
		t.Fatal("missing dep")
	}
	if allTerminal(wf) {
		t.Fatal("running node")
	}
	wf.Nodes["b"] = NodeView{ID: "b", Spec: node("b"), Status: NodeCompleted}
	if !allTerminal(wf) {
		t.Fatal("all terminal")
	}
}

func TestBuildNodeInputCopies(t *testing.T) {
	wf := WorkflowView{
		ID:   "wf",
		Meta: map[string]string{"m": "v"},
		Nodes: map[string]NodeView{
			"a": {ID: "a", Status: NodeCompleted, Output: json.RawMessage(`{"v":1}`)},
		},
	}
	spec := NodeSpec{ID: "b", Kind: "x", DependsOn: []string{"a"}, InputMapping: map[string]string{"in": "a.output.v"}}
	in := buildNodeInput(wf, spec, 2)
	if in.WorkflowID != "wf" || in.Attempt != 2 || string(in.Inputs["in"]) != "1" || in.Meta["m"] != "v" {
		t.Fatalf("input: %+v", in)
	}
	in.Node.DependsOn[0] = "changed"
	if spec.DependsOn[0] != "a" {
		t.Fatal("spec was not copied")
	}
	in.Meta["m"] = "changed"
	if wf.Meta["m"] != "v" {
		t.Fatal("meta was not copied")
	}
}

func TestTokenBudgetEventsOptional(t *testing.T) {
	wf := WorkflowView{
		Spec:  GraphSpec{Nodes: []NodeSpec{node("a"), {ID: "b", Kind: "x", DependsOn: []string{"a"}}}},
		Nodes: map[string]NodeView{"a": {ID: "a", Spec: node("a"), Status: NodeCompleted}, "b": {ID: "b", Spec: NodeSpec{ID: "b", Kind: "x"}, Status: NodePending}},
		Order: []string{"a", "b"},
	}
	events := tokenBudgetEvents(wf, testNow)
	if len(events) != 1 || events[0].Type != EventNodeSkipped {
		t.Fatalf("events: %v", events)
	}
}

func TestEngineOptionalTokenBudgetCompletes(t *testing.T) {
	exec := funcExecutor(func(ctx context.Context, in NodeInput) (NodeOutput, error) {
		return NodeOutput{Data: json.RawMessage(`{}`), Usage: Usage{OutputTokens: 9}}, nil
	})
	engine := fastEngine(exec, Options{})
	wfID, _ := engine.Create(context.Background(), GraphSpec{
		Nodes: []NodeSpec{
			node("a"),
			{ID: "b", Kind: "x", DependsOn: []string{"a"}},
		},
		Budget: Budget{MaxTokens: 1},
	}, nil)
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatal(err)
	}
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.Nodes["b"].Status != NodeSkipped || wf.Nodes["b"].Reason != "budget_exhausted" {
		t.Fatalf("b: %+v", wf.Nodes["b"])
	}
	if wf.Status != WorkflowCompleted {
		t.Fatalf("workflow: %s", wf.Status)
	}
}
