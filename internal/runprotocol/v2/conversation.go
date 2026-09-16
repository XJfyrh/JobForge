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
	resumeFloor, resumeCallDeadline                                          int64
	intent, permit, pending                                                  Frame
	pendingHash                                                              string
	pendingAck                                                               *Frame
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
// Waiting only for an ordinary observation ACK does not count as metering.
func (c *Conversation) MeteringPending() bool {
	if c.phase != "observation" || c.pending.UsageDisposition != "reported" {
		return false
	}
	return c.metering.calls[c.pending.PhysicalCallID].settlement != "settled"
}

// Accept validates an ordinary frame. Dedicated metering frames must use
// AcceptMetering even after ordinary execution is stopped.
func (c *Conversation) Accept(frame Frame, nowMonoMS int64) error {
	if _, err := Encode(frame); err != nil {
		c.Stop()
		return err
	}
	if err := c.accept(frame, nowMonoMS); err != nil {
		c.Stop()
		return err
	}
	c.lastNow = nowMonoMS
	if ordinaryParent(frame.Kind) {
		c.lastParent = frame.EmittedMonoMS
	} else {
		c.lastChild = frame.EmittedMonoMS
	}
	return nil
}

func (c *Conversation) accept(f Frame, now int64) error {
	lastEmitted := c.lastChild
	if ordinaryParent(f.Kind) {
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
	if oneOf(f.Kind, "call_intent", "step_result") {
		if f.EmittedMonoMS < c.resumeFloor || (c.resumeCallDeadline != 0 && now >= c.resumeCallDeadline) {
			return ErrProtocol
		}
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
		// Once the next intent is accepted, its new permit supplies the next
		// call deadline. The previous call must not constrain that whole call.
		c.resumeCallDeadline = 0
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
		return c.acceptObservation(f, now)
	case "call_observation_ack":
		if c.phase != "observation" || c.pendingAck != nil || f.CallSequence != c.pending.CallSequence || f.PhysicalCallID != c.pending.PhysicalCallID || f.ObservationHash != c.pendingHash || f.EmittedMonoMS < c.pending.EmittedMonoMS || now >= c.callDeadline {
			return ErrProtocol
		}
		c.pendingAck = &f
		return c.joinObservation(now)
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

func (c *Conversation) acceptObservation(f Frame, now int64) error {
	call := c.metering.calls[f.PhysicalCallID]
	if c.phase != "call" || call == nil || !call.dispatched || f.CallSequence != c.permit.CallSequence || f.PhysicalCallID != c.permit.PhysicalCallID || f.EmittedMonoMS < call.dispatchedAt || now >= c.callDeadline {
		return ErrProtocol
	}
	if f.UsageDisposition == "unknown" {
		if call.report != nil && call.report.ProviderAudit == nil {
			return ErrProtocol
		}
	} else {
		if !oneOf(call.permit.Subcall, "chat", "query_embedding") || (call.report != nil && (call.report.Usage == nil || *f.UsageHash != call.report.Usage.UsageHash)) {
			return ErrProtocol
		}
		// Preserve the original observation identity across independent reader
		// scheduling, even if its caller reuses or mutates the source frame.
		usageHash := *f.UsageHash
		f.UsageHash = &usageHash
	}
	if f.AuditHash != nil {
		auditHash := *f.AuditHash
		f.AuditHash = &auditHash
		if call.report != nil && (call.report.ProviderAudit == nil || call.report.ProviderAudit.AuditHash != auditHash) {
			return ErrProtocol
		}
	}
	call.observationDisposition = f.UsageDisposition
	c.pending, c.pendingHash, c.phase = f, observationHash(f), "observation"
	if chatStep(c.binding.StepKind) && f.UsageDisposition == "unknown" {
		c.Stop()
	}
	return nil
}

func (c *Conversation) joinObservation(now int64) error {
	call := c.metering.calls[c.pending.PhysicalCallID]
	if call.report != nil && ((c.pending.UsageDisposition == "reported" && (call.report.Usage == nil || call.report.Usage.UsageHash != *c.pending.UsageHash)) ||
		(c.pending.AuditHash != nil && (call.report.ProviderAudit == nil || call.report.ProviderAudit.AuditHash != *c.pending.AuditHash))) {
		// A bad ordinary declaration revokes execution without discarding a
		// valid report already accepted on the independent metering channel.
		c.Stop()
		return nil
	}
	if c.closed {
		return nil
	}
	if now >= c.callDeadline || now >= c.deadline {
		c.Stop()
		return nil
	}
	if c.pendingAck == nil || (c.pending.UsageDisposition == "reported" && call.settlement != "settled") {
		return nil
	}
	if c.pending.UsageDisposition == "reported" && c.pendingAck.EmittedMonoMS < call.ackEmitted {
		c.Stop()
		return ErrProtocol
	}
	c.resumeFloor = max(c.resumeFloor, c.pendingAck.EmittedMonoMS)
	c.resumeCallDeadline = c.callDeadline
	c.phase = "idle"
	c.callIndex++
	c.blocked = c.pending.BusinessOutcome != "accepted"
	c.pending, c.pendingHash, c.pendingAck = Frame{}, "", nil
	return nil
}

// CanDispatch consumes a permit once. Callers recheck this immediately before
// sending (or handing a possibly-sent command to the supervised adapter).
func (c *Conversation) CanDispatch(id string, nowMonoMS int64) error {
	call := c.metering.calls[id]
	if c.closed || !between(nowMonoMS, 0, MaxInteger) || nowMonoMS < c.lastNow || c.phase != "call" || id != c.permit.PhysicalCallID || call == nil || call.dispatched || nowMonoMS >= c.dispatchDeadline || nowMonoMS >= c.deadline {
		c.Stop()
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
		c.Stop()
		return ErrProtocol
	}
	if err := c.metering.Accept(frame, nowMonoMS); err != nil {
		c.Stop()
		return err
	}
	c.lastNow = nowMonoMS
	call := c.metering.calls[frame.PhysicalCallID]
	if nowMonoMS >= c.deadline || call.overrun || call.observationDisposition == "unknown" ||
		(call.report != nil && !reportAllowsContinuation(call.report)) || oneOf(call.settlement, "recorded", "anomaly", "conflict", "unconfirmed") {
		c.Stop()
	}
	if c.phase == "observation" && frame.PhysicalCallID == c.pending.PhysicalCallID {
		return c.joinObservation(nowMonoMS)
	}
	return nil
}

// Stop revokes only ordinary execution; original-call metering remains narrow.
func (c *Conversation) Stop() {
	c.closed = true
	c.pendingAck = nil
}

func ordinaryParent(kind string) bool {
	return oneOf(kind, "execute_step", "call_permit", "call_observation_ack")
}

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
