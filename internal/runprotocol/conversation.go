package runprotocol

import "time"

// Conversation validates one supervised step exchange. Its caller serializes
// access and supplies a monotonic local clock; no goroutine or I/O is started.
// A rejected frame permanently closes the conversation, preventing later sends.
type Conversation struct {
	started       bool
	closed        bool
	blocked       bool
	phase         string
	requestID     string
	binding       Binding
	deadline      time.Time
	lastTime      time.Time
	callDeadline  time.Time
	intent        Frame
	permit        Frame
	callIndex     int
	seenCalls     map[string]bool
	totalSequence int64
	toolID        string
}

// Accept validates a decoded or locally built frame before any action occurs.
// Permits must still be checked with CanDispatch immediately before HTTP send.
func (c *Conversation) Accept(frame Frame, now time.Time) error {
	if _, err := Encode(frame); err != nil {
		c.closed = true
		return err
	}
	if err := c.accept(frame, now); err != nil {
		c.closed = true
		return err
	}
	c.lastTime = now
	return nil
}

func (c *Conversation) accept(frame Frame, now time.Time) error {
	if c.closed || (!c.lastTime.IsZero() && now.Before(c.lastTime)) {
		return ErrProtocol
	}
	if !c.started {
		if frame.Kind != "execute_step" {
			return ErrProtocol
		}
		c.started, c.phase = true, "idle"
		c.requestID, c.binding = frame.RequestID, frame.Binding
		c.deadline = now.Add(time.Duration(frame.RemainingMS) * time.Millisecond)
		c.seenCalls = map[string]bool{}
		return nil
	}
	if frame.RequestID != c.requestID || frame.Binding != c.binding || !now.Before(c.deadline) {
		return ErrProtocol
	}
	switch frame.Kind {
	case "call_intent":
		calls := subcalls(c.binding.StepKind)
		if c.phase != "idle" || c.blocked || c.callIndex >= len(calls) || frame.Subcall != calls[c.callIndex] || frame.CallSequence != c.totalSequence+1 {
			return ErrProtocol
		}
		if c.callIndex > 0 && frame.ToolInvocationID != c.toolID {
			return ErrProtocol
		}
		c.toolID = frame.ToolInvocationID
		c.intent, c.phase = frame, "intent"
		c.totalSequence = frame.CallSequence
	case "call_permit":
		if c.phase != "intent" || frame.CallSequence != c.intent.CallSequence || frame.Subcall != c.intent.Subcall ||
			frame.ParameterHash != c.intent.ParameterHash || frame.ToolInvocationID != c.intent.ToolInvocationID {
			return ErrProtocol
		}
		if !frame.Granted {
			c.blocked, c.phase = true, "idle"
			return nil
		}
		if c.seenCalls[frame.PhysicalCallID] || now.Add(time.Duration(frame.CallMS)*time.Millisecond).After(c.deadline) {
			return ErrProtocol
		}
		c.seenCalls[frame.PhysicalCallID] = true
		c.permit, c.phase = frame, "call"
		c.callDeadline = now.Add(time.Duration(frame.CallMS) * time.Millisecond)
	case "call_observation":
		if c.phase != "call" || c.permit.DispatchMS != 0 || frame.CallSequence != c.permit.CallSequence || frame.PhysicalCallID != c.permit.PhysicalCallID || !now.Before(c.callDeadline) {
			return ErrProtocol
		}
		if frame.Usage != nil {
			if !oneOf(c.permit.Subcall, "chat", "query_embedding") || frame.Usage.InputTokens > c.permit.InputTokenLimit || frame.Usage.OutputTokens > c.permit.OutputTokenLimit {
				return ErrProtocol
			}
		}
		c.phase = "idle"
		c.callIndex++
		c.blocked = frame.BusinessOutcome != "accepted"
	case "step_result":
		if c.phase != "idle" || (c.blocked && frame.Outcome != "error") || (frame.Outcome == "success" && c.callIndex != len(subcalls(c.binding.StepKind))) {
			return ErrProtocol
		}
		c.closed = true
	default:
		return ErrProtocol
	}
	return nil
}

// CanDispatch consumes local permission exactly once, strictly before dispatch
// and step expiry. It is a local guard only, never an atomic provider-side cancel.
func (c *Conversation) CanDispatch(physicalCallID string, now time.Time) error {
	if c.closed || c.phase != "call" || physicalCallID != c.permit.PhysicalCallID ||
		now.Before(c.lastTime) || !now.Before(c.lastTime.Add(time.Duration(c.permit.DispatchMS)*time.Millisecond)) ||
		!now.Before(c.deadline) || c.permit.DispatchMS == 0 {
		c.closed = true
		return ErrProtocol
	}
	// Consume without extending expiry; observation remains the only next frame.
	c.permit.DispatchMS = 0
	c.lastTime = now
	return nil
}

// Stop permanently invalidates local permission on cancellation or lease loss.
func (c *Conversation) Stop() { c.closed = true }

func subcalls(step string) []string {
	switch step {
	case "get_order", "get_delivery":
		return []string{step}
	case "search_policy":
		return []string{"profile_version", "profile_tags", "query_embedding", "search_policy"}
	case "model_proposal", "protocol_correction":
		return []string{"chat"}
	default:
		return nil
	}
}
