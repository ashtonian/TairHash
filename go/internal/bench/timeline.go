package bench

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// TimelineSample is one interval of server activity during a window.
type TimelineSample struct {
	TSec          float64 `json:"t_sec"`
	Utilization   float64 `json:"utilization"`
	CPUPerSec     float64 `json:"cpu_per_sec"`
	CmdPerSec     float64 `json:"cmd_per_sec"`
	CyclesPerSec  float64 `json:"eventloop_cycles_per_sec"`
	NetInPerSec   float64 `json:"net_in_bytes_per_sec"`
	NetOutPerSec  float64 `json:"net_out_bytes_per_sec"`
	ExpiredPerSec float64 `json:"expired_fields_per_sec"`
	UsedMemory    int64   `json:"used_memory"`
	Clients       int64   `json:"connected_clients"`
}

// sampler records server activity every interval until stopped, so a window
// can be checked for steady state and for interference from other tenants.
type sampler struct {
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	samples []TimelineSample
	err     error
}

func startSampler(ctx context.Context, srv *Server, every time.Duration) *sampler {
	ctx, cancel := context.WithCancel(ctx)
	s := &sampler{cancel: cancel}
	if every <= 0 {
		return s
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		start := time.Now()
		prev, err := srv.quickStats(ctx)
		if err != nil {
			s.err = err
			return
		}
		prevAt := time.Now()
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			cur, err := srv.quickStats(ctx)
			if err != nil {
				if ctx.Err() == nil {
					s.err = err
				}
				return
			}
			now := time.Now()
			dt := now.Sub(prevAt).Seconds()
			rate := func(key string) float64 { return (infoFloat(cur, key) - infoFloat(prev, key)) / dt }
			s.samples = append(s.samples, TimelineSample{
				TSec:          now.Sub(start).Seconds(),
				Utilization:   busyRate(prev, cur, dt),
				CPUPerSec:     rate("used_cpu_user") + rate("used_cpu_sys"),
				CmdPerSec:     rate("total_commands_processed"),
				CyclesPerSec:  rate("eventloop_cycles"),
				NetInPerSec:   rate("total_net_input_bytes"),
				NetOutPerSec:  rate("total_net_output_bytes"),
				ExpiredPerSec: rate("expired_fields"),
				UsedMemory:    int64(infoFloat(cur, "used_memory")),
				Clients:       int64(infoFloat(cur, "connected_clients")),
			})
			prev, prevAt = cur, now
		}
	}()
	return s
}

// stop ends sampling and returns the samples.
func (s *sampler) stop() ([]TimelineSample, error) {
	s.cancel()
	s.wg.Wait()
	return s.samples, s.err
}

func infoFloat(info map[string]string, key string) float64 {
	v, _ := strconv.ParseFloat(info[key], 64)
	return v
}
