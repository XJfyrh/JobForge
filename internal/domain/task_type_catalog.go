package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var taskTypeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

var defaultTaskTypeNames = []string{
	"demo.echo",
	"demo.fail",
	"demo.http",
	"demo.idempotent_effect",
	"demo.sleep",
	"pagewise.reindex",
}

// TaskTypeCatalog is an immutable deployment allowlist for task types. Its
// canonical ordering and fingerprint let independently deployed API and
// Gateway processes detect configuration drift without logging payloads.
type TaskTypeCatalog struct {
	types       []string
	allowed     map[string]struct{}
	fingerprint string
}

// DefaultTaskTypeNames returns a copy of the built-in development catalog.
func DefaultTaskTypeNames() []string {
	return append([]string(nil), defaultTaskTypeNames...)
}

// ValidateTaskTypeName verifies the deployment-safe task type syntax.
func ValidateTaskTypeName(taskType string) error {
	if !taskTypeNamePattern.MatchString(taskType) {
		return fmt.Errorf("task type %q must match %s", taskType, taskTypeNamePattern.String())
	}
	return nil
}

// NewTaskTypeCatalog validates and freezes a task type allowlist.
func NewTaskTypeCatalog(taskTypes []string) (*TaskTypeCatalog, error) {
	if len(taskTypes) == 0 {
		return nil, fmt.Errorf("task type catalog must not be empty")
	}

	canonical := make([]string, 0, len(taskTypes))
	allowed := make(map[string]struct{}, len(taskTypes))
	for _, taskType := range taskTypes {
		if err := ValidateTaskTypeName(taskType); err != nil {
			return nil, err
		}
		if _, exists := allowed[taskType]; exists {
			return nil, fmt.Errorf("task type catalog contains duplicate %q", taskType)
		}
		allowed[taskType] = struct{}{}
		canonical = append(canonical, taskType)
	}

	sort.Strings(canonical)
	digest := sha256.Sum256([]byte(strings.Join(canonical, "\n")))
	return &TaskTypeCatalog{
		types:       canonical,
		allowed:     allowed,
		fingerprint: hex.EncodeToString(digest[:]),
	}, nil
}

// Contains reports whether taskType is allowed by this deployment.
func (c *TaskTypeCatalog) Contains(taskType string) bool {
	if c == nil {
		return false
	}
	_, ok := c.allowed[taskType]
	return ok
}

// Types returns a sorted copy of the catalog.
func (c *TaskTypeCatalog) Types() []string {
	if c == nil {
		return nil
	}
	return append([]string(nil), c.types...)
}

// Size returns the number of task types in the catalog.
func (c *TaskTypeCatalog) Size() int {
	if c == nil {
		return 0
	}
	return len(c.types)
}

// Fingerprint returns the SHA-256 hex digest of the newline-delimited,
// lexicographically sorted catalog.
func (c *TaskTypeCatalog) Fingerprint() string {
	if c == nil {
		return ""
	}
	return c.fingerprint
}
