package colab

import "context"

// NoopMountPreflight is an offline placeholder that assumes Drive is already
// authorized and mounted. It exists so a Colab backend can be built and wired
// into the CLI without a real Drive mount adapter; a production mount
// preflight must replace it before live execution.
type NoopMountPreflight struct{}

func (NoopMountPreflight) CheckMount(context.Context, DriveMountRequest) error { return nil }

var _ DriveMountPreflight = NoopMountPreflight{}
