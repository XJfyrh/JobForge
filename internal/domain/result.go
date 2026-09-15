package domain

import (
	"time"
	"unicode/utf8"
)

// MaxResultRefBytes bounds metadata kept on the hot jobs row. Business output
// belongs in the application's storage, never in this reference (PRD v0.6).
const MaxResultRefBytes = 2048

// ValidateResultRef accepts an optional opaque UTF-8 reference. Empty means no
// artifact. C0/DEL control characters are forbidden, including embedded NUL.
func ValidateResultRef(ref string) error {
	if !utf8.ValidString(ref) || len(ref) > MaxResultRefBytes {
		return NewError(CodeInvalidArgument, ErrInvalidArgument, "result_ref must be UTF-8 and at most 2048 bytes")
	}
	for _, r := range ref {
		if r < 0x20 || r == 0x7f {
			return NewError(CodeInvalidArgument, ErrInvalidArgument, "result_ref must not contain control characters")
		}
	}
	return nil
}

// CompleteWithResult applies the existing completion decision before attaching
// artifact metadata. Rejected transitions cannot mutate the reference.
func (j *Job) CompleteWithResult(workerID string, token int64, ref string, now time.Time) error {
	if err := ValidateResultRef(ref); err != nil {
		return err
	}
	if err := j.Complete(workerID, token, now); err != nil {
		return err
	}
	if ref != "" {
		j.ResultRef = &ref
	}
	return nil
}
