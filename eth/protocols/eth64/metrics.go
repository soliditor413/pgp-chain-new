// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package eth64

import "github.com/ethereum/go-ethereum/metrics"

var (
	peerCountMeter    = metrics.NewRegisteredMeter("eth64/peers", nil)
	handshakeMeter    = metrics.NewRegisteredMeter("eth64/handshake", nil)
	headerReqMeter    = metrics.NewRegisteredMeter("eth64/req/headers", nil)
	bodyReqMeter      = metrics.NewRegisteredMeter("eth64/req/bodies", nil)
	headerRespMeter   = metrics.NewRegisteredMeter("eth64/resp/headers", nil)
	bodyRespMeter     = metrics.NewRegisteredMeter("eth64/resp/bodies", nil)
	importedMeter     = metrics.NewRegisteredMeter("eth64/imported/blocks", nil)
	discardedMeter    = metrics.NewRegisteredMeter("eth64/discarded/messages", nil)
	ancestorHitMeter  = metrics.NewRegisteredMeter("eth64/ancestor/hit", nil)
	ancestorMissMeter = metrics.NewRegisteredMeter("eth64/ancestor/miss", nil)
	boundaryStopMeter = metrics.NewRegisteredMeter("eth64/sync/boundary-stops", nil)

	// bodyTruncatedMeter counts body responses the peer cut short at its 2 MiB
	// ceiling. A high rate is normal on a chain with large blocks; it is not an
	// error, the sync simply imports the served prefix and asks for the rest.
	bodyTruncatedMeter = metrics.NewRegisteredMeter("eth64/resp/bodies-truncated", nil)
)
