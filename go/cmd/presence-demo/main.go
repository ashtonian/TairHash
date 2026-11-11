// Command presence-demo shows the presence registry on a fieldhash backend
// (the TairHash module by default). Several devices of one user heartbeat; one
// stops partway through, as a crashed client would, and drops off the online
// list on its own once its TTL lapses. The rest disconnect cleanly on exit.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/valkey-io/valkey-go"

	"github.com/ashtonian/TairHash/go/fieldhash"
	"github.com/ashtonian/TairHash/go/fieldhash/lua"
	"github.com/ashtonian/TairHash/go/fieldhash/module"
	"github.com/ashtonian/TairHash/go/fieldhash/native"
	"github.com/ashtonian/TairHash/go/presence"
	"github.com/ashtonian/TairHash/go/tairhash"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:6379", "Valkey address")
	backend := flag.String("backend", "module", "fieldhash backend: module, lua or native")
	user := flag.String("user", "alice", "user whose devices are simulated")
	devices := flag.Int("devices", 3, "devices to simulate")
	ttl := flag.Duration("ttl", 3*time.Second, "presence lease per heartbeat")
	crashAfter := flag.Duration("crash-after", 4*time.Second, "when device-0 stops heartbeating")
	runFor := flag.Duration("run", 12*time.Second, "how long to run before disconnecting")
	shutdownTimeout := flag.Duration("shutdown-timeout", 5*time.Second, "bound on clean disconnects at exit")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log, *addr, *backend, *user, *devices, *ttl, *crashAfter, *runFor, *shutdownTimeout); err != nil {
		log.Error("presence-demo failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func run(log *slog.Logger, addr, backend, user string, devices int, ttl, crashAfter, runFor, shutdownTimeout time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, runFor)
	defer cancel()

	vk, err := valkey.NewClient(valkey.ClientOption{
		InitAddress:      []string{addr},
		DisableCache:     true,
		ConnWriteTimeout: 5 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	defer vk.Close()

	var store fieldhash.Store
	switch backend {
	case "module":
		c := tairhash.New(vk)
		if err := c.Ping(ctx); err != nil {
			return err
		}
		store = module.New(c)
	case "lua":
		if store, err = lua.New(ctx, vk); err != nil {
			return err
		}
	case "native":
		store = native.New(vk)
	default:
		return fmt.Errorf("unknown backend %q", backend)
	}

	reg, err := presence.New(store, presence.WithTTL(ttl))
	if err != nil {
		return err
	}
	log.Info("starting", slog.String("backend", backend), slog.String("addr", addr),
		slog.String("user", user), slog.Int("devices", devices), slog.Duration("ttl", ttl))

	var wg sync.WaitGroup
	for i := range devices {
		device := fmt.Sprintf("device-%d", i)
		lifetime := ctx
		if i == 0 {
			var crash context.CancelFunc
			lifetime, crash = context.WithTimeout(ctx, crashAfter)
			defer crash()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			heartbeat(lifetime, log, reg, user, presence.Session{Device: device, Node: "gw-1", Status: "active"})
		}()
	}

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for done := false; !done; {
		select {
		case <-ctx.Done():
			done = true
		case <-tick.C:
			online, err := reg.Online(ctx, user)
			if err != nil {
				if ctx.Err() == nil {
					log.Warn("list online devices", slog.String("user", user), slog.Any("error", err))
				}
				continue
			}
			names := make([]string, len(online))
			for i, s := range online {
				names[i] = s.Device
			}
			fmt.Printf("%s online: [%s]\n", time.Now().Format(time.TimeOnly), strings.Join(names, " "))
		}
	}
	wg.Wait()

	// The run context is done; disconnects get their own bounded context.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()
	for i := 1; i < devices; i++ {
		device := fmt.Sprintf("device-%d", i)
		if _, err := reg.Disconnect(shutdownCtx, user, device); err != nil {
			log.Warn("disconnect", slog.String("user", user), slog.String("device", device), slog.Any("error", err))
		}
	}
	log.Info("stopped")
	return nil
}

// heartbeat renews the device's lease at a third of the TTL until ctx ends.
func heartbeat(ctx context.Context, log *slog.Logger, reg *presence.Registry, user string, s presence.Session) {
	tick := time.NewTicker(reg.TTL() / 3)
	defer tick.Stop()
	for {
		if err := reg.Heartbeat(ctx, user, s); err != nil && ctx.Err() == nil {
			log.Warn("heartbeat", slog.String("user", user), slog.String("device", s.Device), slog.Any("error", err))
		}
		select {
		case <-ctx.Done():
			if s.Device == "device-0" {
				log.Info("device stopped heartbeating", slog.String("device", s.Device))
			}
			return
		case <-tick.C:
		}
	}
}
