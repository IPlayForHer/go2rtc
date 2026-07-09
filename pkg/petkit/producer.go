package petkit

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/aac"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h264/annexb"
	"github.com/pion/rtp"
)

type Producer struct {
	core.Connection

	reader     *Reader
	done       chan struct{}
	videoMask  uint16
	audioMask  uint16
	videoCodec *core.Codec
	audioCodec *core.Codec
}

func Dial(rawURL string) (core.Producer, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}

	videoMask, err := videoMaskFromQuery(u.Query().Get("video"))
	if err != nil {
		return nil, err
	}
	audioMask := uint16(0)
	if enabled(u.Query().Get("audio")) {
		audioMask = MaskAudio
	}

	name := u.Path
	if name == "" {
		name = u.Opaque
	}
	size, _ := strconv.Atoi(u.Query().Get("size"))

	reader, err := OpenReader(name, size)
	if err != nil {
		return nil, err
	}

	p := &Producer{
		reader:    reader,
		done:      make(chan struct{}),
		videoMask: videoMask,
		audioMask: audioMask,
		Connection: core.Connection{
			ID:         core.NewID(),
			FormatName: "petkit-shm",
			Protocol:   "shm",
			RemoteAddr: reader.String(),
			Source:     rawURL,
			URL:        rawURL,
			Transport:  reader,
		},
	}

	if err = p.probe(); err != nil {
		_ = reader.Close()
		return nil, err
	}
	return p, nil
}

func videoMaskFromQuery(value string) (uint16, error) {
	switch strings.ToLower(value) {
	case "", "main", "1", "true":
		return MaskMain, nil
	case "sub":
		return MaskSub, nil
	case "aux":
		return MaskAux, nil
	case "none", "0", "false", "off":
		return 0, nil
	default:
		return 0, fmt.Errorf("petkit: unknown video stream %q", value)
	}
}

func enabled(value string) bool {
	switch strings.ToLower(value) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func (p *Producer) probe() error {
	wantVideo := p.videoMask != 0
	wantAudio := p.audioMask != 0
	mask := p.videoMask | p.audioMask
	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) && (wantVideo || wantAudio) {
		frame, err := p.reader.ReadFrame(mask)
		if errors.Is(err, errNoFrame) {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if err != nil {
			return err
		}

		if wantVideo && frame.Mask&p.videoMask != 0 {
			if codec := h264Codec(frame.Data); codec != nil {
				p.videoCodec = codec
				p.Medias = append(p.Medias, &core.Media{
					Kind:      core.KindVideo,
					Direction: core.DirectionRecvonly,
					Codecs:    []*core.Codec{codec},
				})
				wantVideo = false
			}
		}

		if wantAudio && frame.Mask&p.audioMask != 0 {
			if codec := aac.ADTSToCodec(frame.Data); codec != nil {
				codec.PayloadType = core.PayloadTypeRAW
				p.audioCodec = codec
				p.Medias = append(p.Medias, &core.Media{
					Kind:      core.KindAudio,
					Direction: core.DirectionRecvonly,
					Codecs:    []*core.Codec{codec},
				})
				wantAudio = false
			}
		}
	}

	if len(p.Medias) == 0 {
		return errors.New("petkit: no supported H264/AAC frames in shared memory")
	}
	if wantVideo {
		return errors.New("petkit: H264 SPS not found in shared memory")
	}
	if wantAudio {
		return errors.New("petkit: AAC ADTS header not found in shared memory")
	}
	return nil
}

func h264Codec(payload []byte) *core.Codec {
	avcc := annexb.EncodeToAVCC(payload)
	if len(avcc) < 5 || h264.NALUType(avcc) != h264.NALUTypeSPS {
		return nil
	}
	codec := h264.AVCCToCodec(avcc)
	codec.PayloadType = core.PayloadTypeRAW
	return codec
}

func (p *Producer) Start() error {
	mask := p.videoMask | p.audioMask
	for {
		select {
		case <-p.done:
			return nil
		default:
		}

		frame, err := p.reader.ReadFrame(mask)
		if errors.Is(err, errNoFrame) {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if err != nil {
			return err
		}

		p.Recv += len(frame.Data)
		switch {
		case p.videoCodec != nil && frame.Mask&p.videoMask != 0:
			p.writeVideo(frame.Data)
		case p.audioCodec != nil && frame.Mask&p.audioMask != 0:
			p.writeAudio(frame.Data)
		}
	}
}

func (p *Producer) writeVideo(payload []byte) {
	if len(p.Receivers) == 0 {
		return
	}
	avcc := annexb.EncodeToAVCC(payload)
	if len(avcc) == 0 {
		return
	}
	p.write(p.videoCodec, avcc)
}

func (p *Producer) writeAudio(payload []byte) {
	if len(payload) < aac.ADTSHeaderSize || !aac.IsADTS(payload) {
		return
	}
	size := int(aac.ReadADTSSize(payload))
	if size <= 0 || size > len(payload) {
		return
	}
	header := aac.ADTSHeaderLen(payload)
	if header >= size {
		return
	}
	p.write(p.audioCodec, payload[header:size])
}

func (p *Producer) write(codec *core.Codec, payload []byte) {
	for _, receiver := range p.Receivers {
		if receiver.Codec == codec {
			receiver.WriteRTP(&rtp.Packet{
				Header:  rtp.Header{Timestamp: core.Now90000()},
				Payload: payload,
			})
		}
	}
}

func (p *Producer) Stop() error {
	select {
	case <-p.done:
	default:
		close(p.done)
	}
	return p.Connection.Stop()
}
