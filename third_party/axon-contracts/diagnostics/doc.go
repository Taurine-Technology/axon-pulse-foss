// Package diagnostics provides bounded, transport-agnostic network
// measurements shared by Axon agents and Pulse sensors.
//
// The runners expose plain Go structs so they can be unit-tested without a
// message broker or generated protobuf types. This copy carries only the
// runners; the upstream module's protobuf orchestrator and MQTT handler are
// omitted because Pulse does not use them.
package diagnostics
