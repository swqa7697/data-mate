package service

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/godbus/dbus/v5"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/testsupport/dbusfixture"
)

// Existing fakeLaunch scenarios own lifecycle races but bypass the systemd wire
// boundary. Exercise exact unit ownership and structured ExecStart decoding on
// a private bus, including refusal to replace or stop a competing installation.
func TestSystemdOwnership(t *testing.T) {
	bus := dbusfixture.Start(t)
	conn, err := bus.Connect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.RequestName(systemdName, dbus.NameFlagDoNotQueue); err != nil {
		t.Fatal(err)
	}
	c, _, store := controllerFixture(t)
	lease, err := store.Lifecycle(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := config.NewID()
	if err != nil {
		t.Fatal(err)
	}
	r := record{3, installation(c.Root, lease.Identity()), c.Build, nonce, 0, lease.Identity().Executable}
	if err = saveRecord(lease, r); err != nil {
		t.Fatal(err)
	}
	lease.Release()
	unitPath := dbus.ObjectPath("/org/freedesktop/systemd1/unit/com_2edata_2dmate_2eservice")
	var mu sync.Mutex
	present := false
	props := map[string]dbus.Variant{}
	transient := true
	collectDuringRead := false
	export := func(path dbus.ObjectPath, iface string, methods map[string]any) {
		t.Helper()
		if err := conn.ExportMethodTable(methods, path, iface); err != nil {
			t.Fatal(err)
		}
	}
	export(managerPath, systemdManager, map[string]any{
		"GetUnit": func(name string) (dbus.ObjectPath, *dbus.Error) {
			mu.Lock()
			defer mu.Unlock()
			if !present {
				return "/", dbus.NewError(systemdName+".NoSuchUnit", nil)
			}
			return unitPath, nil
		},
		"StartTransientUnit": func(name, mode string, properties []unitProperty, aux []auxiliaryUnit) (dbus.ObjectPath, *dbus.Error) {
			mu.Lock()
			defer mu.Unlock()
			if name != systemdUnit || mode != "fail" || present {
				return "/", dbus.NewError(systemdName+".UnitExists", nil)
			}
			for _, p := range properties {
				props[p.Name] = p.Value
			}
			var commands []execCommand
			if props["ExecStart"].Store(&commands) != nil || len(commands) != 1 {
				return "/", dbus.NewError(systemdName+".InvalidArgs", nil)
			}
			props["ExecStart"] = dbus.MakeVariant([]execStatus{{Path: commands[0].Path, Args: commands[0].Args}})
			props["MainPID"] = dbus.MakeVariant(uint32(os.Getpid()))
			present = true
			return "/job/one", nil
		},
		"StopUnit": func(name, mode string) (dbus.ObjectPath, *dbus.Error) {
			mu.Lock()
			defer mu.Unlock()
			present = false
			return "/job/two", nil
		},
	})
	export(unitPath, "org.freedesktop.DBus.Properties", map[string]any{
		"GetAll": func(iface string) (map[string]dbus.Variant, *dbus.Error) {
			mu.Lock()
			defer mu.Unlock()
			if iface == systemdName+".Unit" {
				return map[string]dbus.Variant{"Transient": dbus.MakeVariant(transient), "Id": dbus.MakeVariant(systemdUnit)}, nil
			}
			if collectDuringRead {
				present = false
				return map[string]dbus.Variant{}, nil
			}
			copy := map[string]dbus.Variant{}
			for k, v := range props {
				copy[k] = v
			}
			return copy, nil
		},
	})
	launcher := systemd{connect: bus.Connect}
	if j, err := launcher.Inspect(t.Context(), c.Root); err != nil || j.Present {
		t.Fatal("absent", j, err)
	}
	if err = launcher.Bootstrap(t.Context(), c.Root); err != nil {
		t.Fatal("bootstrap", err)
	}
	if j, err := launcher.Inspect(t.Context(), c.Root); err != nil || !matching(j, r) || j.PID != os.Getpid() {
		t.Fatal("owner", j, err)
	}
	if err = launcher.Bootstrap(t.Context(), c.Root); err == nil {
		t.Fatal("replaced existing unit")
	}
	foreign, err := config.ResolveRoot(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err = launcher.Bootout(t.Context(), foreign); !errors.Is(err, ErrConflict) {
		t.Fatal("stopped foreign unit", err)
	}
	mu.Lock()
	transient = false
	mu.Unlock()
	if _, err = launcher.Inspect(t.Context(), c.Root); !errors.Is(err, ErrConflict) {
		t.Fatal("adopted persistent unit", err)
	}
	mu.Lock()
	transient = true
	props["Restart"] = dbus.MakeVariant("always")
	mu.Unlock()
	if err = launcher.Bootout(t.Context(), c.Root); !errors.Is(err, ErrConflict) {
		t.Fatal("stopped altered unit", err)
	}
	mu.Lock()
	props["Restart"] = dbus.MakeVariant("no")
	mu.Unlock()
	if err = launcher.Bootout(t.Context(), c.Root); err != nil {
		t.Fatal("stop", err)
	}
	if err = launcher.Bootstrap(t.Context(), c.Root); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	collectDuringRead = true
	mu.Unlock()
	if j, err := launcher.Inspect(t.Context(), c.Root); err != nil || j.Present {
		t.Fatal("collected unit caused a false ownership conflict", j, err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = launcher.Inspect(canceled, c.Root); err == nil {
		t.Fatal("ignored cancellation")
	}
}
