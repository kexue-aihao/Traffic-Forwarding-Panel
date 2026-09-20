package agent

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

const walVersion = 1
const walHeaderSize = 20
const recordHeaderSize = 24
const maxWALRecord = 4 << 20
const maxSnapshot = 64 << 20
const walCheckpointBytes = 8 << 20

var walMagic = []byte("TFPWAL01")
var recordMagic = []byte("EV01")
var crcTable = crc32.MakeTable(crc32.Castagnoli)

type checkpoint struct {
	Version  int             `json:"format_version"`
	Sequence uint64          `json:"sequence"`
	State    json.RawMessage `json:"state"`
	CRC      uint32          `json:"crc32c"`
}

func checksum(seq uint64, b []byte) uint32 {
	var h [8]byte
	binary.BigEndian.PutUint64(h[:], seq)
	crc := crc32.Update(0, crcTable, h[:])
	return crc32.Update(crc, crcTable, b)
}
func walHeader(base uint64) []byte {
	b := make([]byte, walHeaderSize)
	copy(b, walMagic)
	binary.BigEndian.PutUint64(b[8:16], base)
	binary.BigEndian.PutUint32(b[16:], crc32.Checksum(b[:16], crcTable))
	return b
}
func encodeRecord(seq uint64, event stateEvent) ([]byte, error) {
	payload, e := json.Marshal(event)
	if e != nil {
		return nil, e
	}
	if len(payload) > maxWALRecord {
		return nil, errors.New("WAL event too large")
	}
	b := make([]byte, recordHeaderSize+len(payload))
	copy(b, recordMagic)
	binary.BigEndian.PutUint32(b[4:8], uint32(len(payload)))
	binary.BigEndian.PutUint64(b[8:16], seq)
	copy(b[recordHeaderSize:], payload)
	crc := crc32.Update(0, crcTable, b[:16])
	crc = crc32.Update(crc, crcTable, payload)
	binary.BigEndian.PutUint32(b[16:20], crc)
	binary.BigEndian.PutUint32(b[20:24], crc32.Checksum(b[:20], crcTable))
	return b, nil
}
func (s *Store) initializeMaps() {
	if s.state.Used == nil {
		s.state.Used = map[string]int64{}
	}
	if s.state.Retired == nil {
		s.state.Retired = map[string]bool{}
	}
	if s.state.Leases == nil {
		s.state.Leases = map[string]contract.Lease{}
	}
	for _, r := range s.state.Config.Rules {
		if r.Lease != nil {
			if _, ok := s.state.Leases[r.Lease.ID]; !ok {
				s.state.Leases[r.Lease.ID] = *r.Lease
			}
		}
	}
}
func (s *Store) openDurable() error {
	legacy := false
	versioned := false
	b, e := os.ReadFile(s.path)
	if e == nil {
		if len(b) > maxSnapshot {
			return errors.New("snapshot too large")
		}
		var c checkpoint
		if e = json.Unmarshal(b, &c); e != nil {
			return e
		}
		if c.Version == 0 {
			legacy = true
			if e = json.Unmarshal(b, &s.state); e != nil {
				return e
			}
		} else {
			versioned = true
			if c.Version != walVersion || checksum(c.Sequence, c.State) != c.CRC {
				return errors.New("unsupported or corrupt checkpoint")
			}
			s.seq = c.Sequence
			if e = json.Unmarshal(c.State, &s.state); e != nil {
				return e
			}
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	s.initializeMaps()
	if len(s.state.Pending) > MaxPendingRecords {
		return errors.New("usage spool over limit")
	}
	file, e := os.OpenFile(s.path+".wal", os.O_RDWR, 0600)
	if os.IsNotExist(e) {
		if versioned {
			return errors.New("checkpoint WAL missing; restore the complete state bundle")
		}
		if e = s.replaceWAL(s.seq); e != nil {
			return e
		}
	} else if e != nil {
		return e
	} else {
		s.wal = file
		if e = s.replay(); e != nil {
			return e
		}
	}
	if legacy {
		return s.checkpointLocked()
	}
	return nil
}
func (s *Store) replay() error {
	stat, e := s.wal.Stat()
	if e != nil {
		return e
	}
	size := stat.Size()
	if size < walHeaderSize {
		return errors.New("incomplete WAL header")
	}
	header := make([]byte, walHeaderSize)
	if _, e = s.wal.ReadAt(header, 0); e != nil {
		return e
	}
	if !bytes.Equal(header[:8], walMagic) || binary.BigEndian.Uint32(header[16:]) != crc32.Checksum(header[:16], crcTable) {
		return errors.New("corrupt or unsupported WAL header")
	}
	base := binary.BigEndian.Uint64(header[8:16])
	if base > s.seq {
		return errors.New("WAL checkpoint missing; refusing to restore a fresh budget")
	}
	expected := base + 1
	offset := int64(walHeaderSize)
	for offset < size {
		remaining := size - offset
		if remaining < recordHeaderSize {
			tail := make([]byte, remaining)
			s.wal.ReadAt(tail, offset)
			prefix := len(tail)
			if prefix > len(recordMagic) {
				prefix = len(recordMagic)
			}
			if !bytes.Equal(tail[:prefix], recordMagic[:prefix]) {
				return errors.New("corrupt WAL tail header")
			}
			return s.truncateTail(offset)
		}
		h := make([]byte, recordHeaderSize)
		if _, e = s.wal.ReadAt(h, offset); e != nil {
			return e
		}
		if !bytes.Equal(h[:4], recordMagic) {
			return fmt.Errorf("WAL corruption at %d", offset)
		}
		if binary.BigEndian.Uint32(h[20:24]) != crc32.Checksum(h[:20], crcTable) {
			return fmt.Errorf("WAL header checksum corruption at %d", offset)
		}
		n := int64(binary.BigEndian.Uint32(h[4:8]))
		seq := binary.BigEndian.Uint64(h[8:16])
		if n <= 0 || n > maxWALRecord || seq != expected {
			return fmt.Errorf("invalid WAL length/sequence at %d", offset)
		}
		if n > remaining-recordHeaderSize {
			has, e := s.hasFollowingRecord(offset+1, size, seq)
			if e != nil {
				return e
			}
			if has {
				return errors.New("interior WAL length corruption")
			}
			return s.truncateTail(offset)
		}
		payload := make([]byte, n)
		if _, e = s.wal.ReadAt(payload, offset+recordHeaderSize); e != nil {
			return e
		}
		crc := crc32.Update(0, crcTable, h[:16])
		crc = crc32.Update(crc, crcTable, payload)
		if crc != binary.BigEndian.Uint32(h[16:20]) {
			return fmt.Errorf("WAL checksum corruption at %d (complete records are never discarded)", offset)
		}
		if seq > s.seq {
			var event stateEvent
			decoder := json.NewDecoder(bytes.NewReader(payload))
			decoder.DisallowUnknownFields()
			if e = decoder.Decode(&event); e != nil {
				return fmt.Errorf("WAL event: %w", e)
			}
			if e = s.applyEvent(event); e != nil {
				return fmt.Errorf("WAL replay: %w", e)
			}
			s.seq = seq
		}
		expected++
		offset += recordHeaderSize + n
	}
	s.walBytes = size
	_, e = s.wal.Seek(0, io.SeekEnd)
	return e
}

// A damaged interior length must not disguise later intact events as a tail.
func (s *Store) hasFollowingRecord(start, size int64, seq uint64) (bool, error) {
	if size-start > walCheckpointBytes+maxWALRecord {
		return false, errors.New("WAL exceeds recovery bound")
	}
	b := make([]byte, size-start)
	if _, e := s.wal.ReadAt(b, start); e != nil {
		return false, e
	}
	for i := 0; i+recordHeaderSize <= len(b); i++ {
		if !bytes.Equal(b[i:i+4], recordMagic) {
			continue
		}
		h := b[i : i+recordHeaderSize]
		if binary.BigEndian.Uint32(h[20:24]) != crc32.Checksum(h[:20], crcTable) {
			continue
		}
		n := int(binary.BigEndian.Uint32(h[4:8]))
		if n <= 0 || n > maxWALRecord || n > len(b)-i-recordHeaderSize || binary.BigEndian.Uint64(h[8:16]) <= seq {
			continue
		}
		crc := crc32.Update(0, crcTable, h[:16])
		crc = crc32.Update(crc, crcTable, b[i+recordHeaderSize:i+recordHeaderSize+n])
		if crc == binary.BigEndian.Uint32(h[16:20]) {
			return true, nil
		}
	}
	return false, nil
}
func (s *Store) truncateTail(offset int64) error {
	if e := s.wal.Truncate(offset); e != nil {
		return e
	}
	if e := s.wal.Sync(); e != nil {
		return e
	}
	s.walBytes = offset
	_, e := s.wal.Seek(0, io.SeekEnd)
	return e
}
func (s *Store) point(name string) error {
	if s.fault != nil {
		return s.fault(name)
	}
	return nil
}
func (s *Store) appendEvents(events []stateEvent) error {
	var batch bytes.Buffer
	for i, event := range events {
		seq := s.seq + uint64(i) + 1
		if seq <= s.seq {
			return errors.New("WAL sequence overflow")
		}
		b, e := encodeRecord(seq, event)
		if e != nil {
			return e
		}
		batch.Write(b)
	}
	if e := s.point("append_before"); e != nil {
		return e
	}
	p := batch.Bytes()
	for len(p) > 0 {
		var n int
		var e error
		if s.writeWAL != nil {
			n, e = s.writeWAL(p)
		} else {
			n, e = s.wal.Write(p)
		}
		if e != nil {
			return e
		}
		if n <= 0 || n > len(p) {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	if e := s.point("append_after"); e != nil {
		return e
	}
	if e := s.point("sync_before"); e != nil {
		return e
	}
	if e := s.wal.Sync(); e != nil {
		return e
	}
	if e := s.point("sync_after"); e != nil {
		return e
	}
	s.seq += uint64(len(events))
	s.walBytes += int64(batch.Len())
	return nil
}
func syncDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer dir.Close()
	return dir.Sync()
}
func (s *Store) checkpointLocked() error {
	state, e := json.Marshal(s.state)
	if e != nil {
		return e
	}
	if len(state) > maxSnapshot-1024 {
		return errors.New("checkpoint metadata limit reached")
	}
	b, e := json.Marshal(checkpoint{Version: walVersion, Sequence: s.seq, State: state, CRC: checksum(s.seq, state)})
	if e != nil {
		return e
	}
	if e = s.point("snapshot_before_write"); e != nil {
		return e
	}
	f, e := os.OpenFile(s.path+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = f.Write(b)
	if e == nil {
		e = s.point("snapshot_after_write")
	}
	if e == nil {
		e = f.Sync()
	}
	if e == nil {
		e = s.point("snapshot_after_sync")
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = os.Rename(s.path+".tmp", s.path); e != nil {
		return e
	}
	if e = s.point("snapshot_after_rename"); e != nil {
		return e
	}
	if e = syncDirectory(s.path); e != nil {
		return e
	}
	if e = s.point("snapshot_after_dirsync"); e != nil {
		return e
	}
	return s.replaceWAL(s.seq)
}

// Replacement is ordered AFTER the checkpoint rename and directory sync.
// Either old WAL or new WAL can survive a crash; both replay from checkpoint seq.
func (s *Store) replaceWAL(base uint64) error {
	path := s.path + ".wal"
	f, e := os.OpenFile(path+".next", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = f.Write(walHeader(base))
	if e == nil {
		e = s.point("rotate_after_write")
	}
	if e == nil {
		e = f.Sync()
	}
	if e == nil {
		e = s.point("rotate_after_sync")
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if s.wal != nil {
		if e = s.wal.Close(); e != nil {
			return e
		}
		s.wal = nil
	}
	if e = s.point("rotate_before_rename"); e != nil {
		return e
	}
	if e = os.Rename(path+".next", path); e != nil {
		return e
	}
	if e = s.point("rotate_after_rename"); e != nil {
		return e
	}
	if e = syncDirectory(path); e != nil {
		return e
	}
	if e = s.point("rotate_after_dirsync"); e != nil {
		return e
	}
	s.wal, e = os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0600)
	if e != nil {
		return e
	}
	s.walBytes = walHeaderSize
	return nil
}
