package tray

import _ "embed"

// The PNGs are drawn by ./gen and committed, so building needs no image toolchain.

//go:embed online.png
var iconOnline []byte

//go:embed syncing.png
var iconSyncing []byte

//go:embed paused.png
var iconPaused []byte

//go:embed loggedout.png
var iconIdle []byte
