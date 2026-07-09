package petkit

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

const (
	defaultShmName = "/media_buffer_frame_buf"

	shmHeaderSize = 0x3E8
	ringSize      = 0x200000
	frameHeader   = 0x3D

	offOldestSeq = 0x18
	offNewestSeq = 0x1C
	offNewestOff = 0x24
	offOldestOff = 0x28
)

const (
	MaskAudio = 0x0001
	MaskMain  = 0x0004
	MaskSub   = 0x0008
	MaskAux   = 0x0010
)

var errNoFrame = errors.New("petkit: no frame")

type Frame struct {
	Seq     uint32
	TimeLow uint32
	TimeHi  uint32
	Type    byte
	Mask    uint16
	Data    []byte
}

type Reader struct {
	mu     sync.Mutex
	file   *os.File
	data   []byte
	last   uint32
	off    uint32
	closed bool
}

func OpenReader(name string, size int) (*Reader, error) {
	path := shmPath(name)
	if size <= 0 {
		size = shmHeaderSize + ringSize
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	data, err := unix.Mmap(int(file.Fd()), 0, size, unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		_ = file.Close()
		return nil, err
	}

	r := &Reader{
		file: file,
		data: data,
	}
	newest := binary.LittleEndian.Uint32(data[offNewestSeq:])
	if newest != 0 {
		r.last = newest - 1
	}
	r.off = binary.LittleEndian.Uint32(data[offNewestOff:]) & (ringSize - 1)
	return r, nil
}

func shmPath(name string) string {
	if name == "" {
		name = defaultShmName
	}
	if strings.HasPrefix(name, "/dev/shm/") {
		return name
	}
	if strings.HasPrefix(name, "/") {
		return filepath.Join("/dev/shm", strings.TrimPrefix(name, "/"))
	}
	return filepath.Join("/dev/shm", name)
}

func (r *Reader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return nil
	}
	r.closed = true

	var err error
	if r.data != nil {
		err = unix.Munmap(r.data)
		r.data = nil
	}
	if r.file != nil {
		if e := r.file.Close(); err == nil {
			err = e
		}
		r.file = nil
	}
	return err
}

func (r *Reader) ReadFrame(mask uint16) (*Frame, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed || len(r.data) < shmHeaderSize+ringSize {
		return nil, os.ErrClosed
	}

	newest := binary.LittleEndian.Uint32(r.data[offNewestSeq:])
	oldest := binary.LittleEndian.Uint32(r.data[offOldestSeq:])
	if oldest == 0 || r.last >= oldest {
		return nil, errNoFrame
	}

	if int32((1-newest)+r.last) < 0 {
		r.last = newest - 1
		r.off = binary.LittleEndian.Uint32(r.data[offNewestOff:]) & (ringSize - 1)
	}

	if int32(oldest-r.last) < 0 {
		r.last = oldest
		r.off = binary.LittleEndian.Uint32(r.data[offOldestOff:]) & (ringSize - 1)
	}

	for int32(r.last-oldest) < 0 {
		frame, ok := r.nextFrame()
		if !ok {
			return nil, errNoFrame
		}
		if mask == 0 || frame.Mask&mask != 0 {
			return frame, nil
		}
		oldest = binary.LittleEndian.Uint32(r.data[offOldestSeq:])
	}
	return nil, errNoFrame
}

func (r *Reader) nextFrame() (*Frame, bool) {
	var h [frameHeader]byte
	r.copyRing(r.off, h[:])
	size := int(binary.LittleEndian.Uint32(h[0x04:]))
	seq := binary.LittleEndian.Uint32(h[0x00:])
	if size <= 0 || size > ringSize-frameHeader {
		return nil, false
	}

	r.last = seq
	r.off = (r.off + frameHeader) & (ringSize - 1)

	payload := make([]byte, size)
	r.copyRing(r.off, payload)
	r.off = (r.off + uint32(size)) & (ringSize - 1)

	return &Frame{
		Seq:     seq,
		TimeLow: binary.LittleEndian.Uint32(h[0x10:]),
		TimeHi:  binary.LittleEndian.Uint32(h[0x14:]),
		Type:    h[0x20],
		Mask:    binary.LittleEndian.Uint16(h[0x22:]),
		Data:    payload,
	}, true
}

func (r *Reader) copyRing(off uint32, dst []byte) {
	off &= ringSize - 1
	n := uint32(len(dst))
	if off+n > ringSize {
		first := ringSize - off
		copy(dst, r.data[shmHeaderSize+off:shmHeaderSize+ringSize])
		copy(dst[first:], r.data[shmHeaderSize:shmHeaderSize+n-first])
		return
	}
	copy(dst, r.data[shmHeaderSize+off:shmHeaderSize+off+n])
}

func (r *Reader) String() string {
	if r == nil || r.file == nil {
		return "petkit-shm"
	}
	return fmt.Sprintf("petkit-shm:%s", r.file.Name())
}
