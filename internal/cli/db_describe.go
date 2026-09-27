package cli

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/spf13/cobra"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/service"
)

func describeDatabase(cmd *cobra.Command, root config.Root, revision config.Revision, p config.Profile, factory managementFactory) error {
	client, err := factory(cmd.Context(), root)
	if err != nil {
		return serviceError(err)
	}
	reply, err := client.Request(cmd.Context(), service.ManagementRequest{Operation: "describe", Interactive: hasTerminal(cmd), Expected: revision, ProfileID: p.ID, Alias: p.Alias})
	client.Close()
	if err != nil {
		if errors.Is(err, config.ErrRevision) {
			return storageError(err, false)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return failure(string(contracts.QueryTimeout) + ": database description timed out")
		}
		return serviceError(err)
	}
	if reply.Diagnostic != nil && reply.Diagnostic.Error != nil {
		e := reply.Diagnostic.Error
		message := string(e.Code) + ": " + e.Message
		switch e.Code {
		case contracts.Cancelled:
			return context.Canceled
		case contracts.ConfigInvalid, contracts.InvalidArgument:
			return invalid(message)
		default:
			return failure(message)
		}
	}
	if reply.Description == nil {
		return failure("database description unavailable")
	}
	description := reply.Description
	// Service work and cleanup have finished before potentially slow CLI output.
	if flag(cmd, "json") {
		err = json.NewEncoder(cmd.OutOrStdout()).Encode(description)
	} else {
		err = writeDescription(cmd, description)
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if err != nil {
		return failure("cannot write database description")
	}
	return nil
}
