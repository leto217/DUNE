package runtime

import (
	"context"

	"github.com/gary/dune/internal/database/model"
)

type Runtime interface {
	Name() string

	AddInbound(ctx context.Context, ib *model.Inbound) error
	DelInbound(ctx context.Context, ib *model.Inbound) error
	UpdateInbound(ctx context.Context, oldIb, newIb *model.Inbound) error

	AddUser(ctx context.Context, ib *model.Inbound, userMap map[string]any) error
	RemoveUser(ctx context.Context, ib *model.Inbound, email string) error

	// Per-client operations that route through the node's clients API on
	// Remote (instead of pushing the whole inbound) so the node applies
	// per-user xray API calls without a DelInbound+AddInbound cycle.
	UpdateUser(ctx context.Context, ib *model.Inbound, email string, payload model.Client) error
	DeleteUser(ctx context.Context, ib *model.Inbound, email string) error
	AddClient(ctx context.Context, ib *model.Inbound, client model.Client) error
	// AddClients pushes several clients in one step. Remote uses bulkCreate;
	// local falls back to per-user xray API calls.
	AddClients(ctx context.Context, ib *model.Inbound, clients []model.Client) error
	// DeleteUsers detaches several clients from an inbound in one step.
	DeleteUsers(ctx context.Context, ib *model.Inbound, emails []string) error
	// AdjustClients shifts expiry/total for several clients in one step.
	AdjustClients(ctx context.Context, ib *model.Inbound, emails []string, addDays int, addBytes int64) error

	RestartXray(ctx context.Context) error

	ResetClientTraffic(ctx context.Context, ib *model.Inbound, email string) error
	// ResetClientsTraffic resets usage for several clients in one step.
	ResetClientsTraffic(ctx context.Context, ib *model.Inbound, emails []string) error
	ResetInboundTraffic(ctx context.Context, ib *model.Inbound) error
	ResetAllTraffics(ctx context.Context) error
}
