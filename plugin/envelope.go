// Package plugin defines the core abstractions for the SNMP Collector's
// Telegraf-style plugin architecture. Every Input and Transport plugin depends
// on this package; nothing here depends on any other internal package except
// [models].
//
// The package provides three things:
//
//  1. [Envelope] — the standard internal message that flows from Input plugins
//     through the pipeline (decode → produce → format) to Transport plugins.
//  2. [Input] — the interface every data-collection plugin must implement.
//  3. [Transport] — the interface every output/delivery plugin must implement.
package plugin

import (
	"time"

	"snmp/snmp-collector/models"
)

// Envelope is the standardised internal message that flows from Input plugins
// through the pipeline to Transport plugins.
//
// Inputs produce Envelopes; the engine/broker loop collects them from all
// active Inputs via a shared channel and fans them out to every configured
// Transport after formatting.
//
// The zero value is not useful — callers should always set at least Source,
// Timestamp, and Metric.
type Envelope struct {
	// Source is the Name() of the Input plugin that produced this message.
	// It allows downstream stages (formatters, transports, routers) to
	// discriminate by origin without inspecting the payload.
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
