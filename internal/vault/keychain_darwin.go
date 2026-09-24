package vault

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#cgo CFLAGS: -Wno-deprecated-declarations
#include "keychain_darwin.h"
*/
import "C"

import (
	"context"
	"fmt"
	"unsafe"
)

// Keychain uses one generic-password item per installation-bound account. Interactive
// callers may permit native prompts; background/native unattended probes can
// disallow interaction and receive ErrLocked instead. OS prompts are synchronous.
type Keychain struct{ Interactive bool }

// The file-based API uses a process interaction switch, unlike the data-
// protection keychain's per-query UI key. Serialize and restore it for every call.
var nativeGate = make(chan struct{}, 1)

func (k Keychain) interaction(ctx context.Context) (func(), error) {
	select {
	case nativeGate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		<-nativeGate
		return nil, err
	}
	var previous C.Boolean
	status := C.SecKeychainGetUserInteractionAllowed(&previous)
	if status == C.errSecSuccess {
		status = C.SecKeychainSetUserInteractionAllowed(k.interactive(ctx))
	}
	if err := keychainStatus(status); err != nil {
		<-nativeGate
		return nil, err
	}
	return func() { C.SecKeychainSetUserInteractionAllowed(previous); <-nativeGate }, nil
}

func keychainStatus(status C.OSStatus) error {
	switch status {
	case C.errSecSuccess:
		return nil
	case C.errSecItemNotFound:
		return ErrMissing
	case C.errSecAuthFailed, C.errSecUserCanceled:
		return ErrDenied
	case C.errSecInteractionNotAllowed, C.errSecInteractionRequired:
		return ErrLocked
	default:
		return fmt.Errorf("%w (OSStatus %d)", ErrUnavailable, int32(status))
	}
}
func (k Keychain) interactive(ctx context.Context) C.Boolean {
	allowed, specified := ctx.Value(interactionKey{}).(bool)
	if (specified && allowed) || (!specified && k.Interactive) {
		return 1
	}
	return 0
}

// Load retrieves exactly one bounded serialized Tink keyset. Errors contain no native data.
func (k Keychain) Load(ctx context.Context, account string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validFingerprint(account) {
		return nil, ErrUnavailable
	}
	a := C.CString(account)
	defer C.free(unsafe.Pointer(a))
	key := make([]byte, MaxKeysetBytes)
	var length C.size_t
	restore, err := k.interaction(ctx)
	if err != nil {
		clear(key)
		return nil, err
	}
	status := C.dm_load(0, a, k.interactive(ctx), (*C.uchar)(unsafe.Pointer(&key[0])), C.size_t(len(key)), &length)
	restore()
	err = keychainStatus(status)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		clear(key)
		return nil, err
	}
	return key[:int(length)], nil
}

// CreateIfAbsent never updates an existing item; duplicates are loaded and checked
// by the vault against the durably initialized usage record.
func (k Keychain) CreateIfAbsent(ctx context.Context, account string, key []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validFingerprint(account) || len(key) == 0 || len(key) > MaxKeysetBytes {
		return nil, ErrUnavailable
	}
	a := C.CString(account)
	defer C.free(unsafe.Pointer(a))
	restore, err := k.interaction(ctx)
	if err != nil {
		return nil, err
	}
	status := C.dm_create(0, a, k.interactive(ctx), (*C.uchar)(unsafe.Pointer(&key[0])), C.size_t(len(key)))
	restore()
	if status == C.errSecDuplicateItem {
		return k.Load(ctx, account)
	}
	if err := keychainStatus(status); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]byte(nil), key...), nil
}

// Delete removes only the exact service/account in the default keychain.
func (k Keychain) Delete(ctx context.Context, account string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validFingerprint(account) {
		return ErrUnavailable
	}
	a := C.CString(account)
	defer C.free(unsafe.Pointer(a))
	restore, err := k.interaction(ctx)
	if err != nil {
		return err
	}
	status := C.dm_delete(0, a, k.interactive(ctx))
	restore()
	if status == C.errSecItemNotFound {
		return nil
	}
	return keychainStatus(status)
}
