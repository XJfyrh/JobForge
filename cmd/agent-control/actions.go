package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"os"

	"github.com/xjfyrh/jobforge/internal/business"
	"github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/run/businessclient"
)

func loadActionSigners(profiles []run.Profile) (map[string]ed25519.PrivateKey, error) {
	result := make(map[string]ed25519.PrivateKey)
	var seeds map[string]string
	if os.Getenv("JOBFORGE_AGENT_ACTION_SIGNING_KEYS") != "" {
		if readSecretJSON("JOBFORGE_AGENT_ACTION_SIGNING_KEYS", &seeds) != nil {
			return nil, errors.New("invalid action signing configuration")
		}
		for id, encoded := range seeds {
			seed, err := base64.RawURLEncoding.DecodeString(encoded)
			if !run.ValidIdentifier(id) || err != nil || len(seed) != ed25519.SeedSize || base64.RawURLEncoding.EncodeToString(seed) != encoded {
				return nil, errors.New("invalid action signing configuration")
			}
			result[id] = ed25519.NewKeyFromSeed(seed)
		}
	}
	for _, profile := range profiles {
		if !profile.ApprovalEnabled() || !profile.Executable {
			continue
		}
		d, _ := run.DecodeSupportDefinition(profile.Definition)
		key := result[d.Action.KeyID]
		if len(key) != ed25519.PrivateKeySize || business.PublicKeyHash(key.Public().(ed25519.PublicKey)) != d.Action.PublicKeySHA256 {
			return nil, errors.New("action signing key does not match registered profile")
		}
	}
	return result, nil
}

func loadActionReader(profiles []run.Profile, tenants []string, configured credentials) (run.ReceiptReader, error) {
	origins := make(map[string]string)
	for _, p := range profiles {
		if !p.ApprovalEnabled() {
			continue
		}
		d, _ := run.DecodeSupportDefinition(p.Definition)
		for _, tenant := range tenants {
			if prior := origins[tenant]; prior != "" && prior != d.Action.Origin {
				return nil, errors.New("conflicting action receiver origins")
			}
			origins[tenant] = d.Action.Origin
		}
	}
	if len(origins) == 0 {
		return nil, nil
	}
	var keys map[string]string
	if readSecretJSON("JOBFORGE_AGENT_ACTION_RECEIPT_KEYS", &keys) != nil || len(keys) != len(origins) {
		return nil, errors.New("invalid action receipt credentials")
	}
	seen := make(map[string]bool)
	for key := range configured.public {
		seen[key] = true
	}
	for _, key := range configured.workers {
		seen[key] = true
	}
	for _, key := range configured.business {
		seen[key] = true
	}
	config := make(map[string]businessclient.ActionCredentials)
	for tenant, origin := range origins {
		key := keys[tenant]
		if seen[key] {
			return nil, errors.New("overlapping action receipt credentials")
		}
		seen[key] = true
		config[tenant] = businessclient.ActionCredentials{Origin: origin, ReaderKey: key}
	}
	client, err := businessclient.NewActionClient(config)
	if err != nil {
		return nil, errors.New("invalid action receipt configuration")
	}
	return client, nil
}
