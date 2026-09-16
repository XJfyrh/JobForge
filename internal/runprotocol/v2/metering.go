package runprotocol

import (
	"reflect"

	"github.com/xjfyrh/jobforge/internal/run"
)

type meteredCall struct {
	permit                 Frame
	dispatched, overrun    bool
	dispatchedAt           int64
	observationDisposition string
	report                 *run.CallReport
	reportHash             string
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
		report := Report(f)
		if call.report != nil && (call.reportHash != f.ReportHash || !reflect.DeepEqual(*call.report, report)) {
			return ErrProtocol
		}
		if call.report == nil {
			raw, err := run.CallReportJSON(report)
			if err != nil {
				return ErrProtocol
			}
			copyReport, err := run.DecodeCallReport(raw)
			if err != nil {
				return ErrProtocol
			}
			call.report, call.reportHash = &copyReport, f.ReportHash
			// Duplicate delivery does not rewrite the first report's identity or
			// make a previously issued settlement confirmation invalid.
			call.reportEmitted = f.EmittedMonoMS
			call.overrun = report.Usage != nil && (report.Usage.InputTokens > call.permit.InputTokenLimit || report.Usage.OutputTokens > call.permit.OutputTokenLimit ||
				(call.permit.Subcall == "query_embedding" && report.Usage.CachedInputTokens != 0))
		}
		m.lastReport = f.EmittedMonoMS
	} else {
		if f.EmittedMonoMS < m.lastAck || call.report == nil || f.EmittedMonoMS < call.reportEmitted || f.ReportHash != call.reportHash || (!oneOf(call.settlement, "", "unconfirmed") && call.settlement != f.Settlement) ||
			(f.Settlement == "settled" && (call.overrun || !priceable(call.report))) || (f.Settlement == "recorded" && priceable(call.report)) {
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
