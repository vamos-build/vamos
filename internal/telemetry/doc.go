// Package telemetry exports a coordinator's spans and metrics, and a worker's metrics, over OpenTelemetry.
//
// # Invariants
//
// This package imports tracing and OpenTelemetry, and no other vamos package;
// sched, api and tracing never import it or any of OpenTelemetry,
// and remote's non-test code never imports OpenTelemetry's SDK.
// Exported spans are made through the SDK with IDs and times that this package chooses:
// a span's context carries the IDs that chosenIDs gives it once, and its start and end are vamos's.
//
// # Changes
//
// ids.go owns how a span is given the IDs chosen for it; TestChosenIDs proves that the pinned SDK keeps them.
package telemetry
