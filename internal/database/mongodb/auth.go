package mongodb

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/xdg-go/scram"
	"github.com/xdg-go/stringprep"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// SCRAM bounds. The library checks only a minimum iteration count, so the
// maximum is enforced before key derivation can spend server-chosen work.
const (
	scramMechanism = "SCRAM-SHA-256"
	minIterations  = 4096
	maxIterations  = 1 << 20
	maxSASLPayload = 4 << 10
)

// scramPassword applies SASLprep as MongoDB's SCRAM-SHA-256 requires. The
// error never includes the rejected character.
func scramPassword(a database.Access) (string, error) {
	password, err := stringprep.SASLprep.Prepare(a.Password())
	if err != nil {
		return "", database.Fail(contracts.ConfigInvalid, "invalid credential", false)
	}
	return password, nil
}

func scramVerificationFailed() error {
	return database.Fail(contracts.ConnectFailed, "MongoDB server failed SCRAM verification", false)
}

// saslError classifies a failed SASL command without reading its text.
func saslError(err error) error {
	var server serverError
	if !errors.As(err, &server) {
		return err
	}
	switch server.code {
	case 18:
		return database.Fail(contracts.ConnectFailed, "MongoDB authentication failed", false)
	case 334:
		return database.Fail(contracts.ConnectFailed, "MongoDB SCRAM-SHA-256 authentication is unavailable", false)
	}
	return database.Fail(contracts.ConnectFailed, "MongoDB connection setup failed", true)
}

// authenticate runs SCRAM-SHA-256 against the profile's authentication
// database and requires the server to prove it holds the account's key. The
// library's error text is always dropped.
func (s *session) authenticate(ctx context.Context, a database.Access, mechanisms []string) error {
	if mechanisms != nil && !slices.Contains(mechanisms, scramMechanism) {
		return database.Fail(contracts.ConnectFailed, "MongoDB account does not support SCRAM-SHA-256 authentication", false)
	}
	password, err := scramPassword(a)
	if err != nil {
		return err
	}
	p := a.Profile.Connection
	client, err := scram.SHA256.NewClientUnprepped(p.Username, password, "")
	if err != nil {
		return scramVerificationFailed()
	}
	conversation := client.WithMinIterations(minIterations).NewConversation()
	first, err := conversation.Step("")
	if err != nil {
		return scramVerificationFailed()
	}
	reply, err := s.control(ctx, p.Database, bson.D{
		kv("saslStart", int32(1)),
		kv("mechanism", scramMechanism),
		kv("payload", bson.Binary{Data: []byte(first)}),
		kv("options", bson.D{kv("skipEmptyExchange", true)}),
	})
	if err != nil {
		return saslError(err)
	}
	id, done, serverFirst, err := saslReply(reply)
	if err != nil || done {
		return protocolFailure()
	}
	if !iterations(string(serverFirst)) {
		return database.Fail(contracts.ConnectFailed, "MongoDB SCRAM iteration count exceeds the client limit", false)
	}
	final, err := conversation.Step(string(serverFirst))
	if err != nil {
		return scramVerificationFailed()
	}
	reply, err = s.control(ctx, p.Database, saslContinue(id, final))
	if err != nil {
		return saslError(err)
	}
	next, done, serverFinal, err := saslReply(reply)
	if err != nil || next != id {
		return protocolFailure()
	}
	if _, err := conversation.Step(string(serverFinal)); err != nil || !conversation.Done() || !conversation.Valid() {
		return scramVerificationFailed()
	}
	if done {
		return nil
	}
	// A server that ignores skipEmptyExchange expects one empty final step.
	reply, err = s.control(ctx, p.Database, saslContinue(id, ""))
	if err != nil {
		return saslError(err)
	}
	next, done, payload, err := saslReply(reply)
	if err != nil || next != id || !done || len(payload) != 0 {
		return protocolFailure()
	}
	return nil
}

func saslContinue(id int32, payload string) bson.D {
	return bson.D{kv("saslContinue", int32(1)), kv("conversationId", id), kv("payload", bson.Binary{Data: []byte(payload)})}
}

// saslReply reads a SASL step: conversationId, done and a bounded generic
// binary payload.
func saslReply(reply bson.Raw) (int32, bool, []byte, error) {
	top, err := fields(reply)
	if err != nil {
		return 0, false, nil, err
	}
	id, ok := top["conversationId"].Int32OK()
	done, okDone := top["done"].BooleanOK()
	subtype, data, okPayload := top["payload"].BinaryOK()
	if !ok || !okDone || !okPayload || subtype != 0 || len(data) > maxSASLPayload {
		return 0, false, nil, errProtocol
	}
	return id, done, data, nil
}

// iterations reports whether the server-first message has a single decimal
// iteration count within the client's bounds.
func iterations(serverFirst string) bool {
	count, value := 0, ""
	for _, attribute := range strings.Split(serverFirst, ",") {
		if v, ok := strings.CutPrefix(attribute, "i="); ok {
			count++
			value = v
		}
	}
	if count != 1 || value == "" || len(value) > 7 || strings.Trim(value, "0123456789") != "" {
		return false
	}
	n, err := strconv.Atoi(value)
	return err == nil && n >= minIterations && n <= maxIterations
}

// verifyIdentity requires the session to be authenticated as exactly the
// profile's account in its authentication database.
func (s *session) verifyIdentity(ctx context.Context, user, db string) error {
	reply, err := s.control(ctx, "admin", bson.D{kv("connectionStatus", int32(1))})
	if err != nil {
		return setupFailure(err, "MongoDB connection setup failed")
	}
	mismatch := database.Fail(contracts.ConnectFailed, "MongoDB authenticated identity does not match the profile", false)
	top, err := fields(reply)
	if err != nil {
		return mismatch
	}
	authInfo, ok := top["authInfo"].DocumentOK()
	if !ok {
		return mismatch
	}
	info, err := fields(authInfo)
	if err != nil || !singleUser(info["authenticatedUsers"], user, db) {
		return mismatch
	}
	return nil
}

// singleUser reports whether v is exactly [{user, db}], byte-equal to the
// profile.
func singleUser(v bson.RawValue, user, db string) bool {
	users, err := values(v, 1)
	if err != nil || len(users) != 1 {
		return false
	}
	doc, ok := users[0].DocumentOK()
	if !ok {
		return false
	}
	m, err := fields(doc)
	if err != nil || len(m) != 2 {
		return false
	}
	name, okName := m["user"].StringValueOK()
	source, okSource := m["db"].StringValueOK()
	return okName && okSource && name == user && source == db
}
