package runexecutor

import (
	"net/url"
	"strings"
)

func childEnvironment(e Environment) ([]string, error) {
	if !validOrigin(e.BusinessOrigin) || !validOrigin(e.OllamaOrigin) ||
		!validSecret(e.BusinessReadKey) || !validSecret(e.DeepSeekKey) ||
		(e.BusinessOrigin == "") != (e.BusinessReadKey == "") {
		return nil, ErrEnvironment
	}
	// No ambient environment is inherited: proxy, loader, Python module paths,
	// control-plane tokens, DSNs and unrelated tenant keys never reach Python.
	env := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8", "PYTHONDONTWRITEBYTECODE=1"}
	for _, entry := range []struct{ name, value string }{
		{"JOBFORGE_BUSINESS_ORIGIN", e.BusinessOrigin},
		{"JOBFORGE_BUSINESS_READ_KEY", e.BusinessReadKey},
		{"JOBFORGE_OLLAMA_ORIGIN", e.OllamaOrigin},
		{"DEEPSEEK_API_KEY", e.DeepSeekKey},
	} {
		if entry.value != "" {
			env = append(env, entry.name+"="+entry.value)
		}
	}
	return env, nil
}

func validOrigin(raw string) bool {
	if raw == "" {
		return true
	}
	if len(raw) > 2048 || strings.ContainsAny(raw, "\x00\r\n\t ") {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != "" &&
		u.User == nil && u.Path == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.Opaque == ""
}

func validSecret(value string) bool {
	return len(value) <= 8192 && !strings.ContainsAny(value, "\x00\r\n")
}
