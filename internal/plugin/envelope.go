// Package plugin defines the core abstractions for the SNMP Collector plugin
// surface. This package is intentionally small and currently exposes:
//
//  1. [Envelope] — the internal message shape used between pipeline stages.
//  2. [Transport] — the interface every output/delivery plugin must implement.
//
// Nothing here depends on any other internal package except [models].
package plugin

import (
	"time"

	"snmp/snmp-collector/internal/models"
)

// Envelope is the standardised internal message that flows through the
// collection pipeline before formatting and transport delivery.
//
// The zero value is not useful — callers should always set at least Source,
// Timestamp, and Metric.
type Envelope struct {
	// Source identifies the producer of this message (for example,
	// "snmp_poller"). Downstream stages can use it for attribution without
	// inspecting the payload.
	Source string

	// Timestamp records when the data was collected (not when it entered the
	// pipeline). For polled metrics this is the start of the poll cycle.
	Timestamp time.Time

	// Metric is the fully enriched metric payload ready for formatting.
	// A nil Metric means the envelope is empty and should be dropped.
	Metric *models.SNMPMetric
}

// Valid reports whether the envelope carries a non-nil payload.
func (e Envelope) Valid() bool {
	return e.Metric != nil
}
