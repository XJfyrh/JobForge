package business

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// ActionHTTPStore accepts only a pre-registered action and immutable receipt.
type ActionHTTPStore interface {
	ApplyResolution(context.Context, string, SignedAction, map[string]TrustedActionKey) (ActionReceipt, error)
	Receipt(context.Context, string, string) (ActionReceipt, error)
}

// ActionKeyConfig is public deployment metadata, distinct from writer secrets.
type ActionKeyConfig struct {
	PublicKey string   `json:"public_key"`
	Tenants   []string `json:"tenants"`
}

// DecodeActionKeys validates tenant-bound Ed25519 public keys at startup.
func DecodeActionKeys(config map[string]ActionKeyConfig) (map[string]TrustedActionKey, error) {
	if len(config) < 1 || len(config) > 16 {
		return nil, ErrInvalidArgument
	}
	keys := make(map[string]TrustedActionKey, len(config))
	for id, value := range config {
		key, err := base64.RawURLEncoding.DecodeString(value.PublicKey)
		if !validID(id) || err != nil || len(key) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(key) != value.PublicKey || len(value.Tenants) < 1 || len(value.Tenants) > 128 {
			return nil, ErrInvalidArgument
		}
		trusted := TrustedActionKey{PublicKey: append(ed25519.PublicKey(nil), key...), Tenants: make(map[string]bool, len(value.Tenants))}
		for _, tenant := range value.Tenants {
			if !validID(tenant) || trusted.Tenants[tenant] {
				return nil, ErrInvalidArgument
			}
			trusted.Tenants[tenant] = true
		}
		keys[id] = trusted
	}
	return keys, nil
}

// NewActionHTTPHandler isolates action credentials from model/capture readers.
// The supplied pools have already passed CheckActionRole, without elevation.
func NewActionHTTPHandler(writer, reader ActionHTTPStore, identities map[string]Identity, publicKeys map[string]TrustedActionKey) (http.Handler, error) {
	if writer == nil || reader == nil || len(identities) == 0 || len(publicKeys) == 0 {
		return nil, ErrInvalidArgument
	}
	keys := make(map[string]Identity, len(identities))
	for token, identity := range identities {
		if len(token) < 16 || len(token) > 256 || !validID(identity.TenantID) || (identity.Role != "action_reader" && identity.Role != "action_writer") {
			return nil, ErrInvalidArgument
		}
		for _, c := range token {
			if c < 33 || c > 126 {
				return nil, ErrInvalidArgument
			}
		}
		keys[token] = identity
	}
	trusted := make(map[string]TrustedActionKey, len(publicKeys))
	for id, key := range publicKeys {
		if !validID(id) || len(key.PublicKey) != ed25519.PublicKeySize || len(key.Tenants) == 0 {
			return nil, ErrInvalidArgument
		}
		copyKey := TrustedActionKey{PublicKey: append(ed25519.PublicKey(nil), key.PublicKey...), Tenants: make(map[string]bool, len(key.Tenants))}
		for tenant, allowed := range key.Tenants {
			if !validID(tenant) || !allowed {
				return nil, ErrInvalidArgument
			}
			copyKey.Tenants[tenant] = true
		}
		trusted[id] = copyKey
	}
	slots := make(chan struct{}, 8)
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			parts := strings.Split(r.Header.Get("Authorization"), " ")
			if len(r.Header.Values("Authorization")) != 1 || len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
				writeBusinessCode(w, 401, "UNAUTHORIZED")
				return
			}
			identity, ok := keys[parts[1]]
			if !ok {
				writeBusinessCode(w, 401, "UNAUTHORIZED")
				return
			}
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			default:
				writeBusinessCode(w, 429, "RATE_LIMITED")
				return
			}
			ctx, cancel := context.WithTimeout(context.WithValue(r.Context(), identityKey{}, identity), 10*time.Second)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	})
	router.Post("/business/v1/actions/apply_ticket_resolution", func(w http.ResponseWriter, r *http.Request) {
		identity := requestIdentity(r)
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, ActionRequestMaxBytes))
		if err != nil {
			writeBusinessError(w, ErrInvalidArgument)
			return
		}
		action, err := DecodeSignedAction(body)
		if err != nil {
			writeBusinessError(w, err)
			return
		}
		if action.Authorization.TenantID != identity.TenantID {
			writeBusinessError(w, ErrNotFound)
			return
		}
		if identity.Role != "action_writer" {
			writeBusinessError(w, ErrActionForbidden)
			return
		}
		receipt, err := writer.ApplyResolution(r.Context(), identity.TenantID, action, trusted)
		writeBusinessResult(w, receipt, err)
	})
	router.Get("/business/v1/actions/{operation_id}/receipt", func(w http.ResponseWriter, r *http.Request) {
		identity := requestIdentity(r)
		if identity.Role != "action_reader" {
			writeBusinessError(w, ErrActionForbidden)
			return
		}
		receipt, err := reader.Receipt(r.Context(), identity.TenantID, chi.URLParam(r, "operation_id"))
		writeBusinessResult(w, receipt, err)
	})
	router.NotFound(func(w http.ResponseWriter, _ *http.Request) { writeBusinessError(w, ErrNotFound) })
	router.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) { writeBusinessError(w, ErrInvalidArgument) })
	return router, nil
}
