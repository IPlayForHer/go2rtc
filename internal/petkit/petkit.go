package petkit

import (
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/petkit"
)

func Init() {
	streams.HandleFunc("petkit-shm", petkit.Dial)
}
