package runprotocol

// Conversation validates one step without owning I/O, goroutines or authority.
// The caller serializes both FD readers, injects CLOCK_BOOTTIME milliseconds and
// drains/joins both readers before committing any result. Stop is irreversible.
type Conversation struct {
	started, closed, blocked                                                 bool
	phase                                                                    string
	requestID                                                                string
	binding                                                                  Binding
	deadline, lastNow, lastParent, lastChild, callDeadline, dispatchDeadline int64
	resumeFloor                                                              int64
	intent, permit, pending                                                  Frame
	callIndex                                                                int
	totalSequence                                                            int64
	toolID                                                                   string
	metering                                                                 MeteringReceiver
}

// Deadline returns the original step deadline, never a receive-time reset.
func (c *Conversation) Deadline() int64 { return c.deadline }

// Closed reports irreversible loss of local execution permission.
func (c *Conversation) Closed() bool { return c.closed }

// MeteringPending reports an observation awaiting its matching settled report.
func (c *Conversation) MeteringPending() bool { return c.phase == "metering" }

// Accept validates an ordinary frame. Dedicated metering frames must use
// AcceptMetering even after ordinary execution is stopped.
func (c *Conversation) Accept(frame Frame, nowMonoMS int64) error {
	if _, err := Encode(frame); err != nil {
		c.closed = true
		return err
	}
	if err := c.accept(frame, nowMonoMS); err != nil {
		c.closed = true
		return err
	}
	c.lastNow = nowMonoMS
	if oneOf(frame.Kind, "execute_step", "call_permit") {
		c.lastParent = frame.EmittedMonoMS
	} else {
		c.lastChild = frame.EmittedMonoMS
	}
	return nil
}

func (c *Conversation) accept(f Frame, now int64) error {
	lastEmitted := c.lastChild
	if oneOf(f.Kind, "execute_step", "call_permit") {
		lastEmitted = c.lastParent
	}
	if c.closed || !validClock(f.EmittedMonoMS, now) || (c.started && (now < c.lastNow || f.EmittedMonoMS < lastEmitted || f.EmittedMonoMS < c.metering.start)) {
		return ErrProtocol
	}
	if !c.started {
		if f.Kind != "execute_step" {
			return ErrProtocol
		}
		deadline, err := AnchoredDeadline(f.EmittedMonoMS, f.RemainingMS, now)
		if err != nil {
			return err
		}
		c.started, c.phase = true, "idle"
		c.requestID, c.binding, c.deadline = f.RequestID, f.Binding, deadline
		c.metering = MeteringReceiver{requestID: f.RequestID, binding: f.Binding, start: f.EmittedMonoMS, calls: map[string]*meteredCall{}}
		return nil
	}
	if f.RequestID != c.requestID || f.Binding != c.binding || now >= c.deadline {
		return ErrProtocol
	}
	if oneOf(f.Kind, "call_intent", "step_result") && f.EmittedMonoMS < c.resumeFloor {
		return ErrProtocol
	}
	switch f.Kind {
	case "call_intent":
		calls := subcalls(c.binding.StepKind)
		if c.phase != "idle" || c.blocked || c.callIndex >= len(calls) || f.Subcall != calls[c.callIndex] || f.CallSequence != c.totalSequence+1 {
			return ErrProtocol
		}
		if c.callIndex > 0 && f.ToolInvocationID != c.toolID {
			return ErrProtocol
		}
		c.toolID, c.intent, c.phase, c.totalSequence = f.ToolInvocationID, f, "intent", f.CallSequence
	case "call_permit":
		if c.phase != "intent" || f.EmittedMonoMS < c.intent.EmittedMonoMS || f.CallSequence != c.intent.CallSequence || f.Subcall != c.intent.Subcall || f.ParameterHash != c.intent.ParameterHash || f.ToolInvocationID != c.intent.ToolInvocationID {
			return ErrProtocol
		}
		if !f.Granted {
			c.blocked, c.phase = true, "idle"
			return nil
		}
		if _, exists := c.metering.calls[f.PhysicalCallID]; exists {
			return ErrProtocol
		}
		dispatch, err := AnchoredDeadline(f.EmittedMonoMS, f.DispatchMS, now)
		if err != nil {
			return err
		}
		deadline, err := AnchoredDeadline(f.EmittedMonoMS, f.CallMS, now)
		if err != nil || deadline > c.deadline {
			return ErrProtocol
		}
		c.permit, c.phase, c.dispatchDeadline, c.callDeadline = f, "call", dispatch, deadline
		c.metering.calls[f.PhysicalCallID] = &meteredCall{permit: f}
	case "call_observation":
		call := c.metering.calls[f.PhysicalCallID]
		if c.phase != "call" || call == nil || !call.dispatched || f.CallSequence != c.permit.CallSequence || f.PhysicalCallID != c.permit.PhysicalCallID || f.EmittedMonoMS < call.dispatchedAt || now >= c.callDeadline {
			return ErrProtocol
		}
		if f.UsageDisposition == "unknown" {
			if call.report != nil {
				return ErrProtocol
			}
			call.observationDisposition = "unknown"
			c.finishObservation(f)
			return nil
		}
		if !oneOf(call.permit.Subcall, "chat", "query_embedding") {
			return ErrProtocol
		}
		if call.report != nil && *f.UsageHash != call.report.UsageHash {
			return ErrProtocol
		}
		call.observationDisposition = "reported"
		c.pending, c.phase = f, "metering"
		if call.settlement == "settled" {
			c.finishObservation(f)
		}
	case "step_result":
		if c.phase != "idle" || (c.blocked && f.Outcome != "error") || (f.Outcome == "success" && c.callIndex != len(subcalls(c.binding.StepKind))) {
			return ErrProtocol
		}
		c.closed = true
	default:
		return ErrProtocol
	}
	return nil
}

func (c *Conversation) finishObservation(f Frame) {
	if call := c.metering.calls[f.PhysicalCallID]; call != nil && call.settlement == "settled" {
		c.resumeFloor = call.ackEmitted
	}
	c.phase = "idle"
	c.callIndex++
	c.blocked = f.BusinessOutcome != "accepted"
}

// CanDispatch consumes a permit once. Callers recheck this immediately before
// sending (or handing a possibly-sent command to the supervised adapter).
func (c *Conversation) CanDispatch(id string, nowMonoMS int64) error {
	call := c.metering.calls[id]
	if c.closed || !between(nowMonoMS, 0, MaxInteger) || nowMonoMS < c.lastNow || c.phase != "call" || id != c.permit.PhysicalCallID || call == nil || call.dispatched || nowMonoMS >= c.dispatchDeadline || nowMonoMS >= c.deadline {
		c.closed = true
		return ErrProtocol
	}
	call.dispatched = true
	call.dispatchedAt = nowMonoMS
	c.lastNow = nowMonoMS
	return nil
}

// AcceptMetering has no execution deadline or lease authority. A validated report
// remains usable for ledger settlement after Stop, EOF, or ordinary decode error.
// Overrun is retained but immediately stops ordinary execution before any ACK.
func (c *Conversation) AcceptMetering(frame Frame, nowMonoMS int64) error {
	if !c.started || !between(nowMonoMS, 0, MaxInteger) || nowMonoMS < c.lastNow {
		c.closed = true
		return ErrProtocol
	}
	if err := c.metering.Accept(frame, nowMonoMS); err != nil {
		c.closed = true
		return err
	}
	c.lastNow = nowMonoMS
	call := c.metering.calls[frame.PhysicalCallID]
	if nowMonoMS >= c.deadline || call.overrun || call.observationDisposition == "unknown" || oneOf(call.settlement, "anomaly", "unconfirmed") {
		c.closed = true
	}
	if c.phase == "metering" && frame.PhysicalCallID == c.pending.PhysicalCallID {
		if call.report != nil && call.report.UsageHash != *c.pending.UsageHash {
			c.closed = true
			// The ordinary hash mismatch revokes execution, but must not discard
			// independently valid original-call usage from the dedicated FD.
			return nil
		}
		if !c.closed && call.settlement == "settled" {
			if nowMonoMS >= c.callDeadline || nowMonoMS >= c.deadline {
				c.closed = true
				return nil
			}
			c.finishObservation(c.pending)
		}
	}
	return nil
}

// Stop revokes only ordinary execution; original-call metering remains narrow.
func (c *Conversation) Stop() { c.closed = true }

// AnchoredDeadline adds a bounded relative lifetime to its emission time and
// deducts IPC delay by checking against the actual receiver clock. Equality expires.
func AnchoredDeadline(emittedMonoMS, remainingMS, nowMonoMS int64) (int64, error) {
	if !validClock(emittedMonoMS, nowMonoMS) || !between(remainingMS, 1, MaxInteger) || emittedMonoMS > MaxInteger-remainingMS {
		return 0, ErrProtocol
	}
	deadline := emittedMonoMS + remainingMS
	if nowMonoMS >= deadline {
		return 0, ErrProtocol
	}
	return deadline, nil
}

func validClock(emitted, now int64) bool {
	return between(emitted, 0, MaxInteger) && between(now, 0, MaxInteger) && emitted <= now
}

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
