package businessclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/xjfyrh/jobforge/internal/business"
	"github.com/xjfyrh/jobforge/internal/jsonstrict"
	"github.com/xjfyrh/jobforge/internal/run"
)

// ActionCredentials never enter Python environment or public/step output.
type ActionCredentials struct {
	Origin    string `json:"origin"`
	ReaderKey string `json:"reader_key"`
	WriterKey string `json:"writer_key,omitempty"`
}

// ActionClient sends exactly one HTTP request, with no redirects or reused
// connections (net/http retries only reusable connections). Proxy env is ignored.
type ActionClient struct {
	tenants map[string]ActionCredentials
	client  *http.Client
}

// NewActionClient validates fixed origins and distinct receipt/write identities.
func NewActionClient(config map[string]ActionCredentials) (*ActionClient, error) {
	if len(config) < 1 || len(config) > 128 {
		return nil, run.ErrInvalidArgument
	}
	c := &ActionClient{tenants: make(map[string]ActionCredentials, len(config)), client: &http.Client{Timeout: 10 * time.Second,
		Transport:     &http.Transport{DisableKeepAlives: true, MaxConnsPerHost: 8, ResponseHeaderTimeout: 10 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	keys := make(map[string]bool)
	for tenant, identity := range config {
		u, err := url.Parse(identity.Origin)
		if !run.ValidIdentifier(tenant) || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.String() != identity.Origin {
			return nil, run.ErrInvalidArgument
		}
		for _, key := range []string{identity.ReaderKey, identity.WriterKey} {
			if key == "" && identity.WriterKey == "" {
				continue
			}
			if len(key) < 16 || len(key) > 256 || keys[key] {
				return nil, run.ErrInvalidArgument
			}
			for _, character := range key {
				if character < 33 || character > 126 {
					return nil, run.ErrInvalidArgument
				}
			}
			keys[key] = true
		}
		if identity.ReaderKey == "" {
			return nil, run.ErrInvalidArgument
		}
		c.tenants[tenant] = identity
	}
	return c, nil
}

func (c *ActionClient) credentials(tenant string, p run.Profile) (ActionCredentials, error) {
	i, ok := c.tenants[tenant]
	if !ok {
		return i, run.ErrNotFound
	}
	d, err := run.DecodeSupportDefinition(p.Definition)
	if err != nil || !p.ApprovalEnabled() || d.Action.Origin != i.Origin || !slices.ContainsFunc(d.Resources.Tenants, func(t run.SupportTenantDefinition) bool { return t.TenantID == tenant }) {
		return i, run.ErrProfileUnavailable
	}
	return i, nil
}

// ActionObservation is content-free and safe to persist in the action ledger.
type ActionObservation struct {
	PhysicalCallID string
	Transport      string
	Hash           string
	Receipt        business.ActionReceipt
	Found          bool
}

func (c *ActionClient) request(ctx context.Context, method, endpoint, key string, body []byte, action business.SignedAction) (ActionObservation, error) {
	observation := ActionObservation{Transport: "network_error"}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return observation, run.ErrInvalidArgument
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			observation.Transport = "timeout"
		}
		observation.Hash = run.Fingerprint("jobforge.run.action-observation.v1", observation.Transport)
		return observation, run.ErrDependencyUnavailable
	}
	defer func() { _ = response.Body.Close() }()
	observation.Transport = "response"
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	digest := sha256.Sum256(raw)
	observation.Hash = hex.EncodeToString(digest[:])
	if err != nil || len(raw) > 4096 {
		return observation, run.ErrDependencyUnavailable
	}
	if response.StatusCode != http.StatusOK {
		var envelope struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if jsonstrict.Decode(raw, &envelope) == nil {
			if response.StatusCode == http.StatusNotFound && method == http.MethodGet && envelope.Error.Code == "NOT_FOUND" && envelope.Error.Message == "NOT_FOUND" {
				return observation, nil
			}
			if response.StatusCode != http.StatusConflict {
				return observation, run.ErrDependencyUnavailable
			}
			if envelope.Error.Code == string(run.ErrActionConflict) {
				return observation, run.ErrActionConflict
			}
			if envelope.Error.Code == string(run.ErrActionAuthorizationExpired) {
				return observation, run.ErrActionAuthorizationExpired
			}
		}
		return observation, run.ErrDependencyUnavailable
	}
	if jsonstrict.Decode(raw, &observation.Receipt) != nil || observation.Receipt.Validate(action) != nil {
		return observation, run.ErrActionConflict
	}
	observation.Found = true
	observation.Hash = run.Fingerprint("jobforge.run.action-receipt-observation.v1", observation.Receipt.ReceiptHash)
	return observation, nil
}

// LookupReceipt makes one receipt-only exchange for public recovery.
func (c *ActionClient) LookupReceipt(ctx context.Context, tenant string, p run.Profile, action business.SignedAction) (business.ActionReceipt, bool, error) {
	i, err := c.credentials(tenant, p)
	if err != nil {
		return business.ActionReceipt{}, false, err
	}
	if action.Authorization.TenantID != tenant {
		return business.ActionReceipt{}, false, run.ErrActionConflict
	}
	view, err := c.request(ctx, http.MethodGet, i.Origin+"/business/v1/actions/"+action.Authorization.OperationID+"/receipt", i.ReaderKey, nil, action)
	return view.Receipt, view.Found, err
}

// Query returns original action transport evidence from one GET.
func (c *ActionClient) Query(ctx context.Context, tenant string, p run.Profile, action business.SignedAction) (ActionObservation, error) {
	i, err := c.credentials(tenant, p)
	if err != nil {
		return ActionObservation{}, err
	}
	return c.request(ctx, http.MethodGet, i.Origin+"/business/v1/actions/"+action.Authorization.OperationID+"/receipt", i.ReaderKey, nil, action)
}

// Apply sends one fixed signed action using only the writer identity.
func (c *ActionClient) Apply(ctx context.Context, tenant string, p run.Profile, action business.SignedAction) (ActionObservation, error) {
	i, err := c.credentials(tenant, p)
	if err != nil {
		return ActionObservation{}, err
	}
	if i.WriterKey == "" {
		return ActionObservation{}, run.ErrForbidden
	}
	raw, err := json.Marshal(action)
	if err != nil || len(raw) > business.ActionRequestMaxBytes {
		return ActionObservation{}, run.ErrInvalidArgument
	}
	return c.request(ctx, http.MethodPost, i.Origin+"/business/v1/actions/apply_ticket_resolution", i.WriterKey, raw, action)
}
