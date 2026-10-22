package dag

// nodeTransitions is the complete node transition table. A transition that is
// absent from this table is rejected without writing an event.
var nodeTransitions = map[NodeStatus][]NodeStatus{
	NodePending:   {NodeQueued, NodeRunning, NodeSkipped, NodeFailed},
	NodeQueued:    {NodeQueued, NodeRunning, NodePending, NodeSkipped, NodeFailed},
	NodeRunning:   {NodeCompleted, NodeFailed, NodeAborted, NodePending, NodeSkipped},
	NodeCompleted: {},
	NodeFailed:    {},
	NodeAborted:   {},
	NodeSkipped:   {},
}

var workflowTransitions = map[WorkflowStatus][]WorkflowStatus{
	WorkflowPending:   {WorkflowRunning, WorkflowCancelled},
	WorkflowRunning:   {WorkflowCompleted, WorkflowFailed, WorkflowCancelled},
	WorkflowCompleted: {},
	WorkflowFailed:    {},
	WorkflowCancelled: {},
}

// NodeTransitionAllowed reports whether from -> to is in the node table.
func NodeTransitionAllowed(from, to NodeStatus) bool {
	for _, candidate := range nodeTransitions[from] {
		if candidate == to {
			return true
		}
	}
	return false
}

// WorkflowTransitionAllowed reports whether from -> to is in the workflow table.
func WorkflowTransitionAllowed(from, to WorkflowStatus) bool {
	for _, candidate := range workflowTransitions[from] {
		if candidate == to {
			return true
		}
	}
	return false
}

func nodeTransition(from, to NodeStatus) error {
	if NodeTransitionAllowed(from, to) {
		return nil
	}
	return &TransitionError{
		Entity:   "node",
		From:     string(from),
		To:       string(to),
		Terminal: from.Terminal(),
	}
}

func workflowTransition(from, to WorkflowStatus) error {
	if WorkflowTransitionAllowed(from, to) {
		return nil
	}
	terminal := from == WorkflowCompleted || from == WorkflowFailed || from == WorkflowCancelled
	return &TransitionError{
		Entity:   "workflow",
		From:     string(from),
		To:       string(to),
		Terminal: terminal,
	}
}
