package connectorprotocol

// DataCarrierMaxStreamWindow is how far a data-carrier stream's flow-control window may grow
// (yamux and QUIC auto-tuning) on both the edge and the machine. Streams start at their
// configured initial window; with the maximum pinned to that, one preview or tunnel stream was
// limited to window/RTT (256 KiB at 50 ms: ~38 Mbit/s). 4 MiB carries ~250 Mbit/s at 50 ms.
const DataCarrierMaxStreamWindow = 4 << 20
