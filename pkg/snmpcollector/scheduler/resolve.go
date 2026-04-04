// Package scheduler coordinates interval-based SNMP poll job dispatch.
// It resolves the config hierarchy (devices → device groups → object groups →
// object definitions) into concrete PollJob values, maintains a per-device
// timer, and fires jobs into the poller WorkerPool at the configured cadence.
package scheduler

import (
	"log/slog"
	"sort"

	"snmp/snmp-collector/models"
	"snmp/snmp-collector/pkg/snmpcollector/config"
	"snmp/snmp-collector/pkg/snmpcollector/poller"
)

// ResolveJobs walks the config hierarchy for every device and returns a flat
// list of PollJob values. Objects that appear via multiple groups are
// deduplicated per device.
func ResolveJobs(cfg *config.LoadedConfig, logger *slog.Logger) []poller.PollJob {
	if cfg == nil {
		return nil
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(noopWriter{}, nil))
	}

	// Sort hostnames for deterministic output (helps testing + debugging).
	hostnames := make([]string, 0, len(cfg.Devices))
	for h := range cfg.Devices {
		hostnames = append(hostnames, h)
	}
	sort.Strings(hostnames)

	var jobs []poller.PollJob
	for _, hostname := range hostnames {
		devCfg := cfg.Devices[hostname]
		dev := models.Device{
			Hostname:    hostname,
			IPAddress:   devCfg.IP,
			SNMPVersion: devCfg.Version,
		}

		seen := make(map[string]bool)
		for _, dgName := range devCfg.DeviceGroups {
			dg, ok := cfg.DeviceGroups[dgName]
			if !ok {
				logger.Warn("scheduler: unknown device group", "hostname", hostname, "group", dgName)
				continue
			}
			for _, ogName := range dg.ObjectGroups {
				og, ok := cfg.ObjectGroups[ogName]
				if !ok {
					logger.Warn("scheduler: unknown object group", "hostname", hostname, "objectGroup", ogName)
					continue
				}
				for _, objKey := range og.Objects {
					if seen[objKey] {
						continue
					}
					seen[objKey] = true

					objDef, ok := cfg.ObjectDefs[objKey]
					if !ok {
						logger.Warn("scheduler: unknown object definition", "hostname", hostname, "object", objKey)
						continue
					}
					jobs = append(jobs, poller.PollJob{
						Hostname:     hostname,
						Device:       dev,
						DeviceConfig: devCfg,
						ObjectDef:    objDef,
					})
				}
			}
		}
	}
	return jobs
}

// jobsByHostname groups a flat job slice by hostname.
func jobsByHostname(jobs []poller.PollJob) map[string][]poller.PollJob {
	m := make(map[string][]poller.PollJob)
	for _, j := range jobs {
		m[j.Hostname] = append(m[j.Hostname], j)
	}
	return m
}
