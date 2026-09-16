package runprotocol

type meteredCall struct {
	permit                 Frame
	dispatched, overrun    bool
	dispatchedAt           int64
	observationDisposition string
	report                 *Usage
	reportEmitted          int64
	ackEmitted             int64
	settlement             string
}

// MeteringReceiver binds usage to consumed permits. Conversation initializes and
// feeds it; callers cannot register arbitrary calls. It grants no execution rights.
// Its emission clocks are direction-local, independent of ordinary pipe ordering.
type MeteringReceiver struct {
	requestID                           string
	binding                             Binding
	start, lastNow, lastReport, lastAck int64
	calls                               map[string]*meteredCall
}

// Accept validates a dedicated-FD frame including after ordinary execution ends.
// The caller performs SettleUsage only for successfully decoded report frames,
// then supplies an ACK reflecting its actual result, never an assumed success.
func (m *MeteringReceiver) Accept(f Frame, nowMonoMS int64) error {
	if _, err := Encode(f); err != nil {
		return err
	}
	if !oneOf(f.Kind, "metering_report", "metering_ack") || !validClock(f.EmittedMonoMS, nowMonoMS) || nowMonoMS < m.lastNow || f.EmittedMonoMS < m.start || f.RequestID != m.requestID || f.Binding != m.binding {
		return ErrProtocol
	}
	call := m.calls[f.PhysicalCallID]
	if call == nil || !call.dispatched || f.CallSequence != call.permit.CallSequence || f.EmittedMonoMS < call.dispatchedAt || !oneOf(call.permit.Subcall, "chat", "query_embedding") {
		return ErrProtocol
	}
	if f.Kind == "metering_report" {
		if f.ParameterHash != call.permit.ParameterHash || f.EmittedMonoMS < m.lastReport {
			return ErrProtocol
		}
		if call.report != nil && *call.report != *f.Usage {
			return ErrProtocol
		}
		if call.report == nil {
			copyUsage := *f.Usage
			call.report = &copyUsage
			// Duplicate delivery does not rewrite the first report's identity or
			// make a previously issued settlement confirmation invalid.
			call.reportEmitted = f.EmittedMonoMS
			call.overrun = copyUsage.InputTokens > call.permit.InputTokenLimit || copyUsage.OutputTokens > call.permit.OutputTokenLimit
		}
		m.lastReport = f.EmittedMonoMS
	} else {
		if f.EmittedMonoMS < m.lastAck || call.report == nil || f.EmittedMonoMS < call.reportEmitted || *f.UsageHash != call.report.UsageHash || (!oneOf(call.settlement, "", "unconfirmed") && call.settlement != f.Settlement) || (call.overrun && f.Settlement == "settled") {
			return ErrProtocol
		}
		// A repeated final ACK confirms the same settlement. It cannot move
		// the execution barrier past a result already produced after that ACK.
		if call.settlement != f.Settlement {
			call.ackEmitted = f.EmittedMonoMS
		}
		call.settlement, m.lastAck = f.Settlement, f.EmittedMonoMS
	}
	m.lastNow = nowMonoMS
	return nil
}
