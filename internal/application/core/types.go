package core

import (
	"context"
	"net/http"

	"github.com/agentclouddrive/agent-clouddrive/internal/config"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Deps struct {
	DB               *pgxpool.Pool
	Config           config.Config
	Original         ports.OriginalStore
	Artifacts        ports.ArtifactStore
	Identity         ports.IdentityAdapter
	Search           ports.SearchIndex
	Converter        ports.Converter
	Preview          ports.Converter
	Delegation       *DelegationConfig
	MasterKeyStatus  *MasterKeyStatusCache
	SpaceProvisioner ports.SpaceProvisioner
}

type Object = map[string]any

type Actor struct {
	Principal   domain.Principal
	Subject     ports.Subject
	Token       string
	Interactive bool
}

type DelegationConfig struct {
	ClientID, ClientSecret, AuthorizationURL, TokenURL, CallbackURL string
	Key                                                             []byte
	HTTP                                                            *http.Client
}

type Querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}
