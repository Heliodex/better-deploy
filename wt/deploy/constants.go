package main

import "time"

const (
	defaultLogTail = 200
	deployPollDelay = 250 * time.Millisecond
	deployPollTimeout = 30 * time.Second
	devProbeTimeout = 15 * time.Second
	devRebuildDelay = 400 * time.Millisecond
	devRetryDelay = 200 * time.Millisecond
	largeResponseLimit = 8 << 20
	privateDirMode = 0o750
	privateFileMode = 0o600
	scaffoldFileMode = 0o600
	shortHashLength = 12
	smallResponseLimit = 1 << 20
	tarFileMode = 0o644
)
