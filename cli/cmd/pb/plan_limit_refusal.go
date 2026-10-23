package main

import (
	"errors"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// planLimitRefusal surfaces the control plane's bandwidth_quota_exhausted refusal when it ended a
// command mid-way: a renewal of a transfer, exec or ssh authorization refused at the plan limit is wrapped
// with the transport error it caused (a closed stream), so the command would otherwise report only an
// internal failure without the limit and the Billing link.
func planLimitRefusal(err error) error {
	var refused *api.APIError
	if errors.As(err, &refused) && refused.Code == "bandwidth_quota_exhausted" {
		return refused
	}
	return err
}
