package dag

import (
	"errors"
	"testing"
)

func TestNodeTransitionTable(t *testing.T) {
	all := []NodeStatus{NodePending, NodeQueued, NodeRunning, NodeCompleted, NodeFailed, NodeAborted, NodeSkipped}
	allowed := map[NodeStatus]map[NodeStatus]bool{
		NodePending: {NodeQueued: true, NodeRunning: true, NodeSkipped: true, NodeFailed: true},
		NodeQueued:  {NodeQueued: true, NodeRunning: true, NodePending: true, NodeSkipped: true, NodeFailed: true},
		NodeRunning: {NodeCompleted: true, NodeFailed: true, NodeAborted: true, NodePending: true, NodeSkipped: true},
	}
	for _, from := range all {
		for _, to := range all {
			want := allowed[from][to]
			if got := NodeTransitionAllowed(from, to); got != want {
				t.Errorf("node %s -> %s = %v, want %v", from, to, got, want)
			}
			err := nodeTransition(from, to)
			if want && err != nil {
				t.Errorf("node %s -> %s rejected: %v", from, to, err)
			}
			if !want {
				if from.Terminal() {
					if !errors.Is(err, ErrTerminalViolation) {
						t.Errorf("node %s -> %s terminal error = %v, want terminal violation", from, to, err)
					}
				} else if !errors.Is(err, ErrInvalidTransition) {
					t.Errorf("node %s -> %s error = %v, want invalid transition", from, to, err)
				}
			}
		}
	}
}

func TestWorkflowTransitionTable(t *testing.T) {
	all := []WorkflowStatus{WorkflowPending, WorkflowRunning, WorkflowCompleted, WorkflowFailed, WorkflowCancelled}
	allowed := map[WorkflowStatus]map[WorkflowStatus]bool{
		WorkflowPending: {WorkflowRunning: true, WorkflowCancelled: true},
		WorkflowRunning: {WorkflowCompleted: true, WorkflowFailed: true, WorkflowCancelled: true},
	}
	for _, from := range all {
		for _, to := range all {
			want := allowed[from][to]
			if got := WorkflowTransitionAllowed(from, to); got != want {
				t.Errorf("workflow %s -> %s = %v, want %v", from, to, got, want)
			}
			err := workflowTransition(from, to)
			if want && err != nil {
				t.Errorf("workflow %s -> %s rejected: %v", from, to, err)
			}
			if !want {
				if from == WorkflowCompleted || from == WorkflowFailed || from == WorkflowCancelled {
					if !errors.Is(err, ErrTerminalViolation) {
						t.Errorf("workflow %s -> %s terminal error = %v", from, to, err)
					}
				} else if !errors.Is(err, ErrInvalidTransition) {
					t.Errorf("workflow %s -> %s error = %v, want invalid transition", from, to, err)
				}
			}
		}
	}
}

func TestNodeStatusTerminal(t *testing.T) {
	for _, status := range []NodeStatus{NodePending, NodeQueued, NodeRunning} {
		if status.Terminal() {
			t.Errorf("%s must not be terminal", status)
		}
	}
	for _, status := range []NodeStatus{NodeCompleted, NodeFailed, NodeAborted, NodeSkipped} {
		if !status.Terminal() {
			t.Errorf("%s must be terminal", status)
		}
	}
}

func TestErrorMessages(t *testing.T) {
	if got := (&GraphError{Problem: "empty"}).Error(); got == "" || !contains(got, "empty") {
		t.Fatalf("graph error = %q", got)
	}
	if got := (&GraphError{Field: "nodes", Problem: "bad"}).Error(); !contains(got, "nodes") || !contains(got, "bad") {
		t.Fatalf("graph error = %q", got)
	}
	if got := (&TransitionError{Entity: "node", From: "running", To: "queued"}).Error(); !contains(got, "running") {
		t.Fatalf("transition error = %q", got)
	}
	attempt := &attemptError{entity: "a", want: 2, got: 1}
	if !contains(attempt.Error(), "a") || !errors.Is(attempt, ErrStaleAttempt) {
		t.Fatalf("attempt error = %v", attempt)
	}
	if !errors.Is(&GraphError{}, ErrInvalidGraph) {
		t.Fatal("GraphError must unwrap to ErrInvalidGraph")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
