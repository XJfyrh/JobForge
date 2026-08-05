package ctl

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"
)

// Output formats supported by the CLI.
const (
	OutputTable = "table"
	OutputJSON  = "json"
)

// RenderList renders a list result in the requested format. The table view
// intentionally omits job payloads (PRD v0.2 NFR-205 log discipline); the
// json view includes full server responses for the operator's own tenant.
func RenderList(w io.Writer, format string, res *ListResult) error {
	if format == OutputJSON {
		return writeIndentedJSON(w, res)
	}

	lw := &lineWriter{}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	lw.printf(tw, "ID\tSTATE\tQUEUE\tTYPE\tATTEMPT\tUPDATED\n")
	for i := range res.Jobs {
		j := &res.Jobs[i]
		lw.printf(tw, "%s\t%s\t%s\t%s\t%d/%d\t%s\n",
			j.ID, j.State, j.Queue, j.Type, j.Attempt, j.MaxAttempts,
			formatTime(j.UpdatedAt))
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("flush table: %w", err)
	}
	if res.NextCursor != "" {
		lw.printf(w, "\nnext_cursor: %s\n", res.NextCursor)
	}
	return lw.err
}

// RenderJob renders job details plus the attempt timeline.
func RenderJob(w io.Writer, format string, job *Job) error {
	if format == OutputJSON {
		return writeIndentedJSON(w, job)
	}

	lw := &lineWriter{}

	// Detail section. trace_id is shown; trace_context and payload are not
	// (NFR-205; use --output json for the full record).
	lw.printf(w, "id:             %s\n", job.ID)
	lw.printf(w, "state:          %s\n", job.State)
	lw.printf(w, "tenant:         %s\n", job.TenantID)
	lw.printf(w, "queue:          %s\n", job.Queue)
	lw.printf(w, "type:           %s\n", job.Type)
	lw.printf(w, "priority:       %d\n", job.Priority)
	lw.printf(w, "attempt:        %d/%d\n", job.Attempt, job.MaxAttempts)
	lw.printf(w, "timeout:        %ds\n", job.TimeoutSeconds)
	lw.printf(w, "run_at:         %s\n", formatTime(job.RunAt))
	lw.printf(w, "created_at:     %s\n", formatTime(job.CreatedAt))
	lw.printf(w, "updated_at:     %s\n", formatTime(job.UpdatedAt))
	if job.LeaseOwner != nil {
		lw.printf(w, "lease_owner:    %s\n", *job.LeaseOwner)
	}
	if job.LeaseUntil != nil {
		lw.printf(w, "lease_until:    %s\n", formatTime(*job.LeaseUntil))
	}
	if job.IdempotencyKey != nil {
		lw.printf(w, "idempotency:    %s\n", *job.IdempotencyKey)
	}
	if job.RetryOfJobID != nil {
		lw.printf(w, "retry_of:       %s\n", *job.RetryOfJobID)
	}
	if job.TraceID != nil {
		lw.printf(w, "trace_id:       %s\n", *job.TraceID)
	}

	// Attempt timeline.
	lw.printf(w, "\nattempts (%d):\n", len(job.Attempts))
	if len(job.Attempts) == 0 {
		lw.printf(w, "  (none)\n")
		return lw.err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	lw.printf(tw, "  #\tOUTCOME\tWORKER\tSTARTED\tDURATION\tERROR\n")
	for i := range job.Attempts {
		a := &job.Attempts[i]
		duration := "-"
		if a.DurationMs != nil {
			duration = fmt.Sprintf("%dms", *a.DurationMs)
		}
		errCol := "-"
		if a.ErrorCode != nil {
			errCol = *a.ErrorCode
			if a.ErrorMessage != nil {
				errCol += ": " + truncate(*a.ErrorMessage, 60)
			}
		}
		lw.printf(tw, "  %d\t%s\t%s\t%s\t%s\t%s\n",
			a.AttemptNo, a.Outcome, a.WorkerID, formatTime(a.StartedAt), duration, errCol)
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("flush attempts table: %w", err)
	}
	return lw.err
}

// RenderSubmitResult renders a retry/submit acknowledgement.
func RenderSubmitResult(w io.Writer, format, action string, res *SubmitResult) error {
	if format == OutputJSON {
		return writeIndentedJSON(w, res)
	}
	lw := &lineWriter{}
	lw.printf(w, "%s accepted: job_id=%s state=%s\n", action, res.JobID, res.State)
	return lw.err
}

// RenderOutboxStatus renders the outbox backlog summary.
func RenderOutboxStatus(w io.Writer, format string, s *OutboxStatus) error {
	if format == OutputJSON {
		return writeIndentedJSON(w, s)
	}
	lw := &lineWriter{}
	lw.printf(w, "pending:             %d\n", s.Pending)
	oldest := "none"
	if s.OldestUnpublished != nil {
		oldest = fmt.Sprintf("%s (age %s)", formatTime(*s.OldestUnpublished),
			time.Since(*s.OldestUnpublished).Truncate(time.Second))
	}
	lw.printf(w, "oldest unpublished:   %s\n", oldest)
	return lw.err
}

// lineWriter accumulates the first write error so renderers stay readable.
type lineWriter struct {
	err error
}

// printf writes a formatted line unless a previous write already failed.
func (lw *lineWriter) printf(dst io.Writer, format string, args ...any) {
	if lw.err != nil {
		return
	}
	_, lw.err = fmt.Fprintf(dst, format, args...)
}

// writeIndentedJSON writes v as indented JSON.
func writeIndentedJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	return nil
}

// formatTime renders a timestamp in a compact, stable layout.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02 15:04:05")
}

// truncate limits a string to n runes for table display.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}
