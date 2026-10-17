package provider

import (
	"context"
	"sort"
	"strings"

	"zende.local/harness/internal/protocol"
	"zende.local/harness/internal/resource"
)

const maxPreparedModels = 4096
const maxPreparedVariants = 16384

// PreparedConfig is a local publication candidate. Its resolver owns immutable
// metadata; credentials remain references and are resolved again on dispatch.
// Preparation never sends a model request or proves remote authentication.
type PreparedConfig struct {
	sources  *resource.PreparedConfig
	resolver *Resolver
}

func PrepareConfig(ctx context.Context, sources *resource.PreparedConfig, env func(string) string) (*PreparedConfig, error) {
	if sources == nil {
		return nil, protocol.E("invalid_config", "configuration candidate is required")
	}
	snapshot := sources.Snapshot()
	if err := snapshot.Validate(); err != nil {
		return nil, err
	}
	r, err := New(snapshot.Effective.Config, env)
	if err != nil {
		return nil, protocol.E("invalid_config", "provider configuration is invalid")
	}
	ids := make([]string, 0, len(r.config.Connections))
	for id := range r.config.Connections {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	models, variants := 0, 0
	for _, id := range ids {
		connection := r.config.Connections[id]
		for _, m := range connection.Models {
			models++
			variants += 1 + len(m.Efforts)
			if models > maxPreparedModels || variants > maxPreparedVariants {
				return nil, protocol.E("resource_limit", "provider catalog exceeds 4096 models or 16384 model/effort variants")
			}
			// The empty selection is always available; validate every declared
			// option too, rather than publishing unusable picker entries.
			efforts := append([]string{""}, m.Efforts...)
			seen := map[string]bool{}
			for _, effort := range efforts {
				if seen[effort] {
					return nil, protocol.E("invalid_config", "model effort options must be nonempty and unique")
				}
				seen[effort] = true
				if _, err := r.ResolveSelection(ctx, id+"/"+m.ID, effort, ""); err != nil {
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					return nil, protocol.E("invalid_config", "configured model adapter or effort could not be prepared")
				}
			}
		}
	}
	p := &PreparedConfig{sources: sources, resolver: r}
	if err := p.Verify(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *PreparedConfig) Snapshot() resource.ConfigSnapshot { return p.sources.Snapshot() }
func (p *PreparedConfig) Resolver() *Resolver               { return p.resolver }

// Verify is the final local publication check after any slower preparation.
// It does not retain secret values or return raw credential reader errors.
func (p *PreparedConfig) Verify(ctx context.Context) error {
	if p == nil || p.sources == nil || p.resolver == nil {
		return protocol.E("invalid_config", "configuration candidate is incomplete")
	}
	if err := p.sources.Verify(ctx); err != nil {
		return err
	}
	for _, connection := range p.resolver.config.Connections {
		if err := ctx.Err(); err != nil {
			return err
		}
		value, err := resource.ResolveCredential(connection.Credential, p.resolver.env)
		if err != nil || len(value) > 65536 || strings.IndexFunc(value, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
			return protocol.E("credential_unavailable", "configured provider credential reference is missing, unreadable or invalid")
		}
	}
	return p.sources.Verify(ctx)
}
