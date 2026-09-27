package service

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/nativebus"
)

const systemdName = "org.freedesktop.systemd1"
const systemdManager = systemdName + ".Manager"
const systemdUnit = "com.data-mate.service"
const managerPath dbus.ObjectPath = "/org/freedesktop/systemd1"

type systemd struct {
	connect func(context.Context) (*dbus.Conn, error)
}

func nativeLauncher() launchManager { return systemd{connect: nativebus.Connect} }

// Persist the exact intended job properties under the existing lifecycle lease.
// systemd owns the transient unit; no persistent unit or login link is installed.
func serviceDefinition(root config.Root, r record) []byte {
	raw, _ := json.Marshal(struct {
		Unit string   `json:"unit"`
		Args []string `json:"args"`
	}{systemdUnit, r.args()})
	return raw
}

type unitProperty struct {
	Name  string
	Value dbus.Variant
}
type execCommand struct {
	Path          string
	Args          []string
	IgnoreFailure bool
}
type execStatus struct {
	Path                                                       string
	Args                                                       []string
	IgnoreFailure                                              bool
	StartRealtime, StartMonotonic, ExitRealtime, ExitMonotonic uint64
	PID                                                        uint32
	Code, Status                                               int32
}
type auxiliaryUnit struct {
	Name       string
	Properties []unitProperty
}

func systemdError(err error, name string) bool {
	var e dbus.Error
	return errors.As(err, &e) && e.Name == systemdName+"."+name
}
func getProperties(ctx context.Context, conn *dbus.Conn, path dbus.ObjectPath, iface string) (map[string]dbus.Variant, error) {
	call := conn.Object(systemdName, path).CallWithContext(ctx, "org.freedesktop.DBus.Properties.GetAll", 0, iface)
	if call.Err != nil {
		return nil, call.Err
	}
	if len(call.Body) != 1 {
		return nil, ErrConflict
	}
	// Keep the wire signatures: dbus.Store converts nested struct arrays into
	// generic Go slices when copying a variant map, losing their exact shape.
	props, ok := call.Body[0].(map[string]dbus.Variant)
	if !ok {
		return nil, ErrConflict
	}
	return props, nil
}

func (s systemd) Inspect(parent context.Context, root config.Root) (job, error) {
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	conn, err := s.connect(ctx)
	if err != nil {
		return job{}, ErrUnavailable
	}
	defer conn.Close()
	return inspectSystemd(ctx, conn)
}
func inspectSystemd(ctx context.Context, conn *dbus.Conn) (result job, resultErr error) {
	// A transient unit can be collected between GetUnit and the property calls.
	// Confirm absence with the manager before classifying a torn snapshot as an
	// ownership conflict; a still-present or replacement unit remains a conflict.
	defer func() {
		if resultErr == nil || ctx.Err() != nil {
			return
		}
		var current dbus.ObjectPath
		err := conn.Object(systemdName, managerPath).CallWithContext(ctx, systemdManager+".GetUnit", 0, systemdUnit).Store(&current)
		if systemdError(err, "NoSuchUnit") {
			result, resultErr = job{}, nil
		}
	}()
	var path dbus.ObjectPath
	err := conn.Object(systemdName, managerPath).CallWithContext(ctx, systemdManager+".GetUnit", 0, systemdUnit).Store(&path)
	if systemdError(err, "NoSuchUnit") {
		return job{}, nil
	}
	if err != nil {
		return job{}, ErrUnavailable
	}
	unit, err := getProperties(ctx, conn, path, systemdName+".Unit")
	if err != nil {
		return job{}, ErrUnavailable
	}
	if unit["Transient"].Value() != true || unit["Id"].Value() != systemdUnit {
		return job{}, ErrConflict
	}
	props, err := getProperties(ctx, conn, path, systemdName+".Service")
	if err != nil {
		return job{}, ErrUnavailable
	}
	var commands []execStatus
	start, ok := props["ExecStart"]
	if !ok || start.Signature().String() != "a(sasbttttuii)" || start.Store(&commands) != nil || len(commands) != 1 || commands[0].IgnoreFailure {
		return job{}, ErrConflict
	}
	args := commands[0].Args
	if len(args) != 4 && len(args) != 6 {
		return job{}, ErrConflict
	}
	if commands[0].Path != args[0] || !filepath.IsAbs(args[0]) || args[1] != "__service" || args[2] != "--instance" || !config.ValidUUID(args[3]) {
		return job{}, ErrConflict
	}
	var root config.Root
	if len(args) == 6 {
		if args[4] != "--root" {
			return job{}, ErrConflict
		}
		root, err = config.ResolveRoot(args[5], "")
	} else {
		var home string
		home, err = config.AccountHome()
		if err == nil {
			root, err = config.ProductionRoot(home)
		}
	}
	if err != nil || !safePath(root) || args[0] != config.ExecutablePath(root) {
		return job{}, ErrConflict
	}
	if props["Restart"].Value() != "no" || props["Type"].Value() != "exec" || props["UMask"].Value() != uint32(0077) || props["WorkingDirectory"].Value() != "/" || props["StandardOutput"].Value() != "null" || props["StandardError"].Value() != "null" || props["TimeoutStopUSec"].Value() != uint64(5000000) {
		return job{}, ErrConflict
	}
	pid, ok := props["MainPID"].Value().(uint32)
	if !ok || pid > 1<<31-1 {
		return job{}, ErrConflict
	}
	return job{Present: true, PID: int(pid), Path: filepath.Join(root.Path, config.ServiceFile()), Args: args}, nil
}
func (s systemd) Bootstrap(parent context.Context, root config.Root) error {
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	conn, err := s.connect(ctx)
	if err != nil {
		return ErrUnavailable
	}
	defer conn.Close()
	store, err := config.OpenExisting(ctx, root)
	if err != nil {
		return ErrState
	}
	defer store.Close()
	lease, err := store.ReadLease(ctx)
	if err != nil {
		return err
	}
	r, err := readRecord(lease.Read, root, lease.Identity())
	lease.Release()
	if err != nil {
		return err
	}
	props := []unitProperty{}
	add := func(name string, value any) { props = append(props, unitProperty{name, dbus.MakeVariant(value)}) }
	add("Description", "Data Mate")
	add("Type", "exec")
	add("ExecStart", []execCommand{{r.Executable, r.args(), false}})
	add("Restart", "no")
	add("CollectMode", "inactive-or-failed")
	add("UMask", uint32(0077))
	add("WorkingDirectory", "/")
	add("StandardOutput", "null")
	add("StandardError", "null")
	add("TimeoutStopUSec", uint64(5000000))
	// fail never replaces the unit belonging to a competing installation.
	err = conn.Object(systemdName, managerPath).CallWithContext(ctx, systemdManager+".StartTransientUnit", 0, systemdUnit, "fail", props, []auxiliaryUnit{}).Err
	if err != nil {
		return ErrStartup
	}
	return nil
}
func (s systemd) Bootout(parent context.Context, root config.Root) error {
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	conn, err := s.connect(ctx)
	if err != nil {
		return ErrUnavailable
	}
	defer conn.Close()
	j, err := inspectSystemd(ctx, conn)
	if err != nil {
		return err
	}
	if !j.Present {
		return nil
	}
	if j.Path != filepath.Join(root.Path, config.ServiceFile()) {
		return ErrConflict
	}
	// The controller already verified the durable record and nonce while holding
	// the lifecycle lease. Stop only this checked unit, never a recorded PID.
	if err = conn.Object(systemdName, managerPath).CallWithContext(ctx, systemdManager+".StopUnit", 0, systemdUnit, "fail").Err; err != nil {
		return ErrUnavailable
	}
	return nil
}
