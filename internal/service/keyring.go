package service

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/vault"
)

// These frames exist only after the authenticated management hello and request.
// A challenge ID is scoped to this connection and this original operation.
type managementFrame struct {
	Challenge *vault.KeyringChallenge `json:"challenge,omitempty"`
	ID        string                  `json:"id,omitempty"`
	Reply     *ManagementReply        `json:"reply,omitempty"`
}
type keyringAnswer struct {
	ID       string `json:"id"`
	Password []byte `json:"password,omitempty"`
	Cancel   bool   `json:"cancel,omitempty"`
	Invalid  bool   `json:"invalid,omitempty"`
}

func exchangeManagement(ctx context.Context, conn *net.UnixConn, interactive bool) (ManagementReply, error) {
	promptCtx, stopPrompt := context.WithCancel(ctx)
	defer stopPrompt()
	frames := make(chan managementFrame, 1)
	readDone := make(chan struct{})
	defer func() { stopPrompt(); conn.Close(); <-readDone }()
	go func() {
		defer close(readDone)
		defer close(frames)
		defer stopPrompt()
		for {
			var f managementFrame
			if readFrame(conn, &f) != nil {
				return
			}
			select {
			case frames <- f:
			case <-promptCtx.Done():
				return
			}
			if f.Reply != nil {
				return
			}
		}
	}()
	seen := make(map[string]bool)
	var promptError error
	for f := range frames {
		if f.Reply != nil {
			if f.Challenge != nil || f.ID != "" {
				return ManagementReply{}, ErrState
			}
			if promptError != nil {
				return *f.Reply, promptError
			}
			return *f.Reply, f.Reply.ResultError()
		}
		if f.Challenge == nil || !f.Challenge.Valid() || !config.ValidUUID(f.ID) || seen[f.ID] || len(seen) >= 8 || !interactive {
			return ManagementReply{}, ErrState
		}
		seen[f.ID] = true
		prep, cancel := context.WithTimeout(vault.WithInteraction(promptCtx, true), vault.PreparationTimeout)
		password, err := vault.AskKeyring(prep, *f.Challenge)
		cancel()
		promptError = err
		if promptCtx.Err() != nil {
			promptError = nil
		}
		answer := keyringAnswer{ID: f.ID, Password: password, Cancel: err != nil}
		if errors.Is(err, vault.ErrPassword) && f.Challenge.Kind == "unlock" {
			answer.Cancel = false
			answer.Invalid = true
			promptError = nil
		}
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		writeErr := writeFrame(conn, answer)
		clear(password)
		_ = conn.SetWriteDeadline(time.Time{})
		if writeErr != nil {
			// A final timeout/shutdown reply can arrive while the terminal is
			// still waiting. The reader cancels input; drain its final frame.
			if promptCtx.Err() != nil {
				continue
			}
			break
		}
	}
	if ctx.Err() != nil {
		return ManagementReply{}, ctx.Err()
	}
	return ManagementReply{}, ErrUnavailable
}

func serveManagementExchange(ctx context.Context, conn *net.UnixConn, m *Manager, q ManagementRequest) {
	operation, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var output sync.Mutex
	finished := false
	var pending string
	var next int
	answers := make(chan keyringAnswer)
	done := make(chan struct{})
	defer func() { cancel(); conn.Close(); <-done }()
	go func() {
		defer close(done)
		for {
			var a keyringAnswer
			if readFrameLimit(conn, &a, 2*vault.MaxSecretBytes+256) != nil {
				clear(a.Password)
				cancel()
				return
			}
			mu.Lock()
			valid := pending != "" && a.ID == pending && ((a.Cancel != a.Invalid && len(a.Password) == 0) || (!a.Cancel && !a.Invalid && len(a.Password) > 0 && len(a.Password) <= vault.MaxSecretBytes))
			pending = ""
			mu.Unlock()
			if !valid {
				clear(a.Password)
				cancel()
				return
			}
			select {
			case answers <- a:
			case <-operation.Done():
				clear(a.Password)
				return
			}
		}
	}()
	promptOperation := vault.WithKeyringPrompt(operation, func(ctx context.Context, challenge vault.KeyringChallenge) ([]byte, error) {
		output.Lock()
		if finished || ctx.Err() != nil {
			output.Unlock()
			return nil, context.Canceled
		}
		id, idError := config.NewID()
		if idError != nil || next >= 8 {
			output.Unlock()
			return nil, ErrUnavailable
		}
		mu.Lock()
		next++
		pending = id
		mu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		err := writeFrame(conn, managementFrame{Challenge: &challenge, ID: id})
		_ = conn.SetWriteDeadline(time.Time{})
		output.Unlock()
		if err != nil {
			cancel()
			return nil, context.Canceled
		}
		select {
		case a := <-answers:
			if a.Invalid {
				return nil, vault.ErrPassword
			}
			if a.Cancel {
				return nil, context.Canceled
			}
			return a.Password, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	reply := m.HandleManagement(promptOperation, q)
	output.Lock()
	defer output.Unlock()
	finished = true
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = writeFrame(conn, managementFrame{Reply: &reply})
}
