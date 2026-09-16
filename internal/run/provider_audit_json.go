package run

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

// MaxCallReportBytes caps a typed report before parsing; its enclosing metering
// frame independently retains the stricter 8 KiB limit including all fields/LF.
const MaxCallReportBytes = 8 * 1024

var providerAuditFields = map[string]bool{
	"schema_version": false, "provider": false, "response_complete": false, "http_status": false,
	"response_sha256": true, "identity_state": false, "response_id": true, "response_model": true,
	"system_fingerprint": true, "created": true, "usage_evidence": false, "reasoning_state": false,
	"reasoning_tokens": true, "mode_state": false, "audit_hash": false,
}

// DecodeProviderAudit rejects missing, duplicate, unknown and scalar-null fields
// before decoding typed values. It does not parse or verify any provider body.
func DecodeProviderAudit(body []byte) (ProviderAudit, error) {
	var result ProviderAudit
	if len(body) > MaxProviderAuditBytes {
		return result, ErrCheckpointTooLarge
	}
	if _, err := auditObject(body, providerAuditFields); err != nil {
		return result, err
	}
	type plain ProviderAudit
	var value plain
	if json.Unmarshal(body, &value) != nil {
		return result, ErrInvalidArgument
	}
	result = ProviderAudit(value)
	if _, err := ProviderAuditJSON(result); err != nil {
		return ProviderAudit{}, err
	}
	return result, nil
}

// UnmarshalJSON keeps nested audit decoding on the same strict path.
func (a *ProviderAudit) UnmarshalJSON(body []byte) error {
	decoded, err := DecodeProviderAudit(body)
	if err != nil {
		return err
	}
	*a = decoded
	return nil
}

// ProviderAuditJSON checks typed facts and bounds the final encoded object.
func ProviderAuditJSON(audit ProviderAudit) ([]byte, error) {
	if err := audit.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(audit)
	if err != nil {
		return nil, ErrInvalidArgument
	}
	if len(body) > MaxProviderAuditBytes {
		return nil, ErrCheckpointTooLarge
	}
	return body, nil
}

// DecodeCallReport reads the bounded persisted shape. Original reservation and
// receipt verification still require CallReport.Verify before accepting a write.
func DecodeCallReport(body []byte) (CallReport, error) {
	var result CallReport
	if len(body) > MaxCallReportBytes {
		return result, ErrCheckpointTooLarge
	}
	raw, err := auditObject(body, map[string]bool{"usage": true, "provider_audit": true})
	if err != nil {
		return result, err
	}
	if !auditNull(raw["provider_audit"]) {
		audit, err := DecodeProviderAudit(raw["provider_audit"])
		if err != nil {
			return result, err
		}
		result.ProviderAudit = &audit
	}
	if !auditNull(raw["usage"]) {
		fields := map[string]bool{"input_tokens": false, "output_tokens": false, "cached_input_tokens": false, "receipt_hash": false, "usage_hash": false}
		if _, err := auditObject(raw["usage"], fields); err != nil {
			return result, err
		}
		var usage UsageReport
		if json.Unmarshal(raw["usage"], &usage) != nil {
			return result, ErrInvalidArgument
		}
		result.Usage = &usage
	}
	if _, err := CallReportJSON(result); err != nil {
		return CallReport{}, err
	}
	return result, nil
}

// UnmarshalJSON enforces strict report shape even when nested in another object.
func (r *CallReport) UnmarshalJSON(body []byte) error {
	decoded, err := DecodeCallReport(body)
	if err != nil {
		return err
	}
	*r = decoded
	return nil
}

// CallReportJSON serializes bounded typed report content, never raw provider JSON.
func CallReportJSON(report CallReport) ([]byte, error) {
	if err := report.validateContent(); err != nil {
		return nil, err
	}
	if report.ProviderAudit != nil {
		if _, err := ProviderAuditJSON(*report.ProviderAudit); err != nil {
			return nil, err
		}
	}
	body, err := json.Marshal(report)
	if err != nil {
		return nil, ErrInvalidArgument
	}
	if len(body) > MaxCallReportBytes {
		return nil, ErrCheckpointTooLarge
	}
	return body, nil
}

func auditNull(raw []byte) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

// auditObject is deliberately limited to closed typed objects. Nested values
// are validated by their own closed decoder, rather than decoded into any/map.
func auditObject(body []byte, fields map[string]bool) (map[string]json.RawMessage, error) {
	if !utf8.Valid(body) {
		return nil, ErrInvalidArgument
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, ErrInvalidArgument
	}
	result := make(map[string]json.RawMessage, len(fields))
	for decoder.More() {
		token, err = decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, ErrInvalidArgument
		}
		nullable, known := fields[key]
		if _, exists := result[key]; !known || exists {
			return nil, ErrInvalidArgument
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil || (!nullable && auditNull(value)) {
			return nil, ErrInvalidArgument
		}
		result[key] = value
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') || len(result) != len(fields) {
		return nil, ErrInvalidArgument
	}
	if _, err = decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, ErrInvalidArgument
	}
	return result, nil
}
