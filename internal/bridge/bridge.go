// Package bridge maps Yandex smart home devices onto HomeKit accessories and
// keeps the two in sync.
//
// Two invariants hold the whole package together, and both exist because the
// reference implementation lost its accessories over time:
//
//   - A device's HomeKit accessory id never changes. See registry.go.
//   - The accessory set is rebuilt only when several consecutive successful
//     polls agree that it changed. See supervisor.go.
package bridge

import "time"

// Version is reported to HomeKit as the accessory firmware revision.
const Version = "1.0.0"

// writeTimeout bounds a single HomeKit-initiated write. HomeKit gives an
// accessory a few seconds to answer before it shows the control as failed, so
// there is no point waiting much longer than the user will.
const writeTimeout = 15 * time.Second
