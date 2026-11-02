package main

import "time"

const (
	shortHashLength    = 12
	smallResponseLimit = 1 << 20
	largeResponseLimit = 8 << 20
	defaultLogTail     = 200
	devProbeTimeout    = 15 * time.Second
	devRetryDelay      = 200 * time.Millisecond
	tarFileMode        = 0o644
	deployPollTimeout  = 30 * time.Second
	deployPollDelay    = 250 * time.Millisecond
	devRebuildDelay    = 400 * time.Millisecond
	scaffoldFileMode   = 0o600
	privateFileMode    = 0o600
	privateDirMode     = 0o750
)
