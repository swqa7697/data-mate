package transport

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const MaxKnownHostsBytes = 1 << 20

var (
	ErrUnknownHost = errors.New("SSH host key is unknown; enroll interactively with db add/edit --ssh-enroll")
	ErrChangedHost = errors.New("SSH host key changed; verify the server identity outside Data Mate")
	ErrKnownHosts  = errors.New("invalid owned SSH known_hosts")
)

// HostKey is a nonsecret exact endpoint pin staged until final confirmation.
// Wildcards, certificates, markers and ambient OpenSSH files are not supported.
type HostKey struct {
	Address string
	Key     ssh.PublicKey
}

func sshAddress(s config.SSH) string { return net.JoinHostPort(s.Host, strconv.Itoa(s.Port)) }

func parseHosts(raw []byte) (map[string]ssh.PublicKey, error) {
	if len(raw) > MaxKnownHostsBytes {
		return nil, ErrKnownHosts
	}
	hosts := make(map[string]ssh.PublicKey)
	for len(bytes.TrimSpace(raw)) > 0 {
		marker, names, key, _, rest, err := ssh.ParseKnownHosts(raw)
		if err != nil || marker != "" || len(names) != 1 {
			return nil, ErrKnownHosts
		}
		name := names[0]
		if strings.ContainsAny(name, "*?!|,\\") || hosts[name] != nil {
			return nil, ErrKnownHosts
		}
		if _, ok := key.(*ssh.Certificate); ok {
			return nil, ErrKnownHosts
		}
		hosts[name] = key
		raw = rest
	}
	return hosts, nil
}

func checkHost(hosts map[string]ssh.PublicKey, address string, key ssh.PublicKey) error {
	want := hosts[knownhosts.Normalize(address)]
	if want == nil {
		return ErrUnknownHost
	}
	if !bytes.Equal(want.Marshal(), key.Marshal()) {
		return ErrChangedHost
	}
	return nil
}

// ReadKnownHosts reads only the owner-checked installation file under a lease.
func ReadKnownHosts(l *config.Lease) ([]byte, error) {
	raw, err := l.Read("config/known_hosts", MaxKnownHostsBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_, err = parseHosts(raw)
	return raw, err
}

// SaveHostKey rechecks the current pin under the writer's lease; a changed key
// cannot be overwritten, even if enrollment raced another confirmed writer.
func SaveHostKey(l *config.Lease, pin HostKey) error {
	host, port, err := net.SplitHostPort(pin.Address)
	n, e := strconv.Atoi(port)
	if err != nil || e != nil || !validHost(host) || n < 1 || n > 65535 || pin.Key == nil {
		return ErrKnownHosts
	}
	if _, ok := pin.Key.(*ssh.Certificate); ok {
		return ErrKnownHosts
	}
	raw, err := ReadKnownHosts(l)
	if err != nil {
		return err
	}
	hosts, err := parseHosts(raw)
	if err != nil {
		return err
	}
	err = checkHost(hosts, pin.Address, pin.Key)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrUnknownHost) {
		return err
	}
	line := knownhosts.Line([]string{pin.Address}, pin.Key) + "\n"
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		raw = append(raw, '\n')
	}
	raw = append(raw, line...)
	if len(raw) > MaxKnownHostsBytes {
		return ErrKnownHosts
	}
	return l.Replace("config/known_hosts", raw)
}

// ProbeHostKey obtains a fingerprint candidate without sending credentials. The
// caller must confirm it interactively and save only after final confirmation.
// Existing pins are checked here and again during publication and every dial.
func ProbeHostKey(ctx context.Context, s config.SSH, raw []byte) (HostKey, error) {
	var pin HostKey
	if !validHost(s.Host) || s.Port < 1 || s.Port > 65535 {
		return pin, ErrKnownHosts
	}
	hosts, err := parseHosts(raw)
	if err != nil {
		return pin, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, err := (&net.Dialer{}).DialContext(ctx, "tcp", sshAddress(s))
	if err != nil {
		return pin, routeError(ctx, err)
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	observed := errors.New("host key observed")
	var hostErr error
	_, _, _, err = ssh.NewClientConn(c, sshAddress(s), &ssh.ClientConfig{User: s.User, HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
		hostErr = checkHost(hosts, sshAddress(s), key)
		if hostErr != nil && !errors.Is(hostErr, ErrUnknownHost) {
			return hostErr
		}
		if _, ok := key.(*ssh.Certificate); ok {
			hostErr = ErrKnownHosts
			return hostErr
		}
		pin = HostKey{Address: sshAddress(s), Key: key}
		return observed
	}})
	if hostErr != nil && !errors.Is(hostErr, ErrUnknownHost) {
		return HostKey{}, hostErr
	}
	if pin.Key == nil {
		return HostKey{}, routeError(ctx, err)
	}
	if ctx.Err() != nil {
		return HostKey{}, ctx.Err()
	}
	return pin, nil
}
