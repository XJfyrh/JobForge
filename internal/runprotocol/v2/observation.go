package runprotocol

import (
	"strconv"

	"github.com/xjfyrh/jobforge/internal/run"
)

// ObservationErrorCode maps a wire observation error to its ledger error code.
// Control rejections are not provider observations and cannot be hashed as one.
func ObservationErrorCode(wireCode string) (string, error) {
	switch wireCode {
	case "OUTPUT_INVALID":
		return "MODEL_PROTOCOL_ERROR", nil
	case "INPUT_INVALID":
		return "INVALID_ARGUMENT", nil
	case "PROTOCOL_ERROR":
		return "EXECUTOR_PROTOCOL_ERROR", nil
	case "", "TIMEOUT", "DEPENDENCY_UNAVAILABLE", "PROFILE_UNAVAILABLE", "BUDGET_EXHAUSTED":
		return wireCode, nil
	default:
		return "", ErrProtocol
	}
}

// ObservationHash validates an observation and returns its authoritative ledger
// fingerprint. Binding and call identity must still be matched independently.
func ObservationHash(observation Frame) (string, error) {
	if observation.Kind != "call_observation" {
		return "", ErrProtocol
	}
	if _, err := EncodeOrdinary(observation); err != nil {
		return "", err
	}
	return observationHash(observation), nil
}

// observationHash is only used after ordinary frame validation has accepted the
// closed observation error set, including its wire-to-ledger mapping.
func observationHash(observation Frame) string {
	code, _ := ObservationErrorCode(observation.ErrorCode)
	usageHash := ""
	if observation.UsageHash != nil {
		usageHash = *observation.UsageHash
	}
	auditHash := ""
	if observation.AuditHash != nil {
		auditHash = *observation.AuditHash
	}
	return run.Fingerprint("jobforge.run.observation.v2", observation.TransportOutcome,
		strconv.FormatInt(observation.HTTPStatus, 10), code, observation.BusinessOutcome, usageHash, auditHash)
}
