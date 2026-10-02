package control

import (
	"context"
	"net/http"

	"messh/internal/state"
)

// UpdatePreparation holds admission closed on this node for a short update.
// Persist it with Paths.SaveUpdateStartup before stopping the old process.
// The candidate inherits the same lease until AbortUpdate commits admission.
type UpdatePreparation = state.UpdateStartup

// UpdateLeaseRequest proves possession of this node's current update lease.
// The local control token is required independently on every request.
type UpdateLeaseRequest struct {
	Lease string `json:"lease"`
}

func (c *Client) PrepareUpdate(ctx context.Context) (UpdatePreparation, error) {
	var result UpdatePreparation
	err := c.Do(ctx, http.MethodPost, "/v1/update/prepare", nil, &result)
	return result, err
}

func (c *Client) AbortUpdate(ctx context.Context, lease string) error {
	return c.Do(ctx, http.MethodDelete, "/v1/update/prepare", UpdateLeaseRequest{Lease: lease}, nil)
}

func (c *Client) StopUpdate(ctx context.Context, lease string) error {
	return c.Do(ctx, http.MethodPost, "/v1/update/stop", UpdateLeaseRequest{Lease: lease}, nil)
}
