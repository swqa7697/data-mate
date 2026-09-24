package vault

import (
	aesgcmpb "github.com/tink-crypto/tink-go/v2/proto/aes_gcm_go_proto"
	"google.golang.org/protobuf/proto"
)

func aes256(raw []byte) bool {
	var key aesgcmpb.AesGcmKey
	if proto.Unmarshal(raw, &key) != nil {
		return false
	}
	defer clear(key.KeyValue)
	return key.Version == 0 && len(key.KeyValue) == 32
}
